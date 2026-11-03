package execallow

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const containerStateFormat = `{"name":{{json .Name}},"state":{{json .State.Status}},"health":{{with index .State "Health"}}{{json .Status}}{{else}}""{{end}},"startedAt":{{json .State.StartedAt}}}`
const probeMaxOutput = 64 << 10

func isProbe(command string) bool {
	return command == "vpn-wg-dump" || command == "vpn-exit-check" || command == "container-state"
}

// boundedOutput discards excess bytes while the child is still writing.
// Returning the full count keeps a verbose child from blocking on a broken pipe.
type boundedOutput struct {
	data     []byte
	max      int
	overflow bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.max - len(w.data)
	if len(p) > remaining {
		p = p[:remaining]
		w.overflow = true
	}
	w.data = append(w.data, p...)
	return n, nil
}
func (w *boundedOutput) String() string { return string(w.data) }

// wgOutput never retains the key columns (1 and 2 in `wg show all dump`).
// Only validated public fields survive String; malformed output is discarded.
type wgOutput struct {
	rows       []string
	fields     [9]string
	col, total int
	bad        bool
}

func (w *wgOutput) Write(p []byte) (int, error) {
	for _, b := range p {
		w.total++
		if w.total > probeMaxOutput {
			w.bad = true
			continue
		}
		switch b {
		case '\n':
			w.finish()
		case '\t':
			w.col++
			if w.col >= len(w.fields) {
				w.bad = true
			}
		default:
			if w.col >= len(w.fields) {
				continue
			}
			// Columns 4 and 8 are unnecessary allowed-IPs/keepalive metadata.
			if w.col == 1 || w.col == 2 || w.col == 4 || w.col == 8 {
				continue
			}
			if len(w.fields[w.col]) >= 256 {
				w.bad = true
				continue
			}
			w.fields[w.col] += string(b)
		}
	}
	return len(p), nil
}
func (w *wgOutput) finish() {
	f := w.fields
	if !safeName.MatchString(f[0]) {
		w.bad = true
	}
	switch w.col {
	case 4:
		// A keyless interface row; the discarded port/fwmark are not probe facts.
		w.rows = append(w.rows, f[0]+"\t_\t_\t0\toff")
	case 8:
		endpoint := f[3]
		if endpoint != "(none)" {
			host, port, err := net.SplitHostPort(endpoint)
			n, e := strconv.ParseUint(port, 10, 16)
			if err != nil || net.ParseIP(host) == nil || e != nil || n == 0 {
				w.bad = true
			}
		}
		stamp, err := strconv.ParseInt(f[5], 10, 64)
		_, rxErr := strconv.ParseUint(f[6], 10, 64)
		_, txErr := strconv.ParseUint(f[7], 10, 64)
		if err != nil || stamp < 0 || rxErr != nil || txErr != nil {
			w.bad = true
		}
		w.rows = append(w.rows, strings.Join([]string{f[0], "_", "_", endpoint, "_", f[5], f[6], f[7], "0"}, "\t"))
	default:
		w.bad = true
	}
	w.fields = [9]string{}
	w.col = 0
}
func (w *wgOutput) String() string {
	if w.col != 0 || w.fields[0] != "" {
		w.finish()
	}
	if w.bad {
		return ""
	}
	return strings.Join(w.rows, "\n")
}

// VPNCheckURL returns the shared server-configured endpoint for exit checks.
func VPNCheckURL() (string, error) {
	value := os.Getenv("VPN_CHECK_URL")
	if value == "" {
		value = "https://ipinfo.io/json"
	}
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || len(value) > 2048 || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("invalid VPN check URL")
	}
	return value, nil
}
func exitArgv(container, tool, target string) []string {
	if tool == "wget" {
		return []string{"docker", "exec", container, "wget", "-qO-", "-T", "5", target}
	}
	return []string{"docker", "exec", container, "curl", "-fsS", "--max-time", "5", target}
}
func missingCurl(res Result) bool {
	if res.ExitCode == 0 && res.Error == "" {
		return false
	}
	if res.ExitCode == 126 || res.ExitCode == 127 {
		return true
	}
	text := strings.ToLower(res.Stderr + " " + res.Error)
	return strings.Contains(text, "curl") && (strings.Contains(text, "executable file not found") || strings.Contains(text, "curl: not found") || strings.Contains(text, "curl: no such file") || strings.Contains(text, `exec: "curl": stat`) && strings.Contains(text, "no such file"))
}
func (r *Runner) runProbe(ctx context.Context, command string, args map[string]string) Result {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	var res Result
	switch command {
	case "vpn-wg-dump":
		res = r.runCaptured(ctx, buildArgv(command, args), true)
	case "vpn-exit-check":
		target, err := VPNCheckURL()
		if err != nil {
			return Result{Command: command, ExitCode: -1, Error: err.Error()}
		}
		res = r.runCaptured(ctx, exitArgv(args["container"], "curl", target), false)
		if missingCurl(res) {
			res = r.runCaptured(ctx, exitArgv(args["container"], "wget", target), false)
		}
	default:
		res = r.runCaptured(ctx, buildArgv(command, args), false)
	}
	return sanitizeProbeResult(command, res)
}
func sanitizeProbeResult(command string, res Result) Result {
	if !isProbe(command) {
		return res
	}
	res.Command = command
	if command == "vpn-wg-dump" {
		w := &wgOutput{}
		_, _ = w.Write([]byte(res.Stdout))
		res.Stdout = w.String()
	}
	if len(res.Stdout) > probeMaxOutput {
		res.Stdout = ""
		res.ExitCode = -1
		res.Error = "probe output exceeded limit"
	}
	// Never expose container stderr or process errors: a tool can echo keys.
	res.Stderr = ""
	if res.ExitCode != 0 || res.Error != "" {
		res.Stdout = ""
		res.Error = "container probe failed"
	}
	return res
}
