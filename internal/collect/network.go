package collect

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// NetworkCollector enumerates listening TCP/UDP sockets.
//
// `ss` is preferred and `netstat` is the fallback: net-tools is deprecated and
// absent from minimal images, but plenty of long-lived homelab boxes still
// have it and not much else. Source is recorded in the output so you can tell
// which parser produced a surprising row.
//
// Process attribution (which PID owns the socket) requires root. Without it
// the ports are still listed, just unattributed, and a diagnostic says so —
// which is much better than silently returning a less useful answer.
type NetworkCollector struct{}

func (NetworkCollector) Name() string { return "network" }

func (c NetworkCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	// Network is only attached once a listing succeeded: a failed ss means
	// "not looked at" (null), not "nothing is listening".
	n := &inventory.Network{Listeners: []inventory.Listener{}}

	switch {
	case env.Run.Available("ss"):
		n.Source = "ss"
		out, err := listSockets(ctx, env, "ss", "-tulpn")
		if err != nil {
			// Owner lookup walks every process's fds and stalls behind processes
			// stuck on a busy NFS mount. The bare listing does not.
			env.Warnf("ss -tulpn failed (%v); listed ports without owners", err)
			if out, err = listSockets(ctx, env, "ss", "-tuln"); err != nil {
				return err
			}
			n.Unattributed = true
		}
		n.Listeners = parseSS(out)

	case env.Run.Available("netstat"):
		n.Source = "netstat"
		out, err := listSockets(ctx, env, "netstat", "-tulpn")
		if err != nil {
			env.Warnf("netstat -tulpn failed (%v); listed ports without owners", err)
			if out, err = listSockets(ctx, env, "netstat", "-tuln"); err != nil {
				return err
			}
			n.Unattributed = true
		}
		n.Listeners = parseNetstat(out)

	default:
		env.Warnf("neither ss nor netstat is installed; cannot enumerate listening ports")
		inv.Network = n
		return nil
	}
	inv.Network = n

	if n.Listeners == nil {
		n.Listeners = []inventory.Listener{}
	}

	attributed := 0
	for _, l := range n.Listeners {
		if l.PID != 0 {
			attributed++
		}
	}
	if len(n.Listeners) > 0 && attributed == 0 && !n.Unattributed {
		env.Warnf("no listening socket could be attributed to a process; re-run as root for process names")
	}

	resolveProcesses(env, n.Listeners)
	sortListeners(n.Listeners)
	return nil
}

// listTimeout bounds one socket listing (normally ~40 ms), leaving the
// collector's budget for the fallback.
const listTimeout = 3 * time.Second

// listSockets runs one listing under its own timeout. A non-zero exit is a
// failure: its output cannot be trusted as the full list.
func listSockets(ctx context.Context, env *Env, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	res, err := env.Run.Run(ctx, name, args...)
	if err != nil {
		return "", err
	}
	if !res.OK() {
		return "", fmt.Errorf("%s exited %d: %s", name, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// commMaxLen is the kernel's limit on a process's comm name (TASK_COMM_LEN-1).
// Both ss and netstat report comm, so anything longer is silently cut: the
// names such as "Plex Media Serv" and "pvedaemon worke" are truncated.
const commMaxLen = 15

// resolveProcesses fills in the full command line for attributed sockets and
// repairs names the kernel truncated.
//
// /proc/<pid>/cmdline is NUL-separated argv. Reading it needs no privileges
// for your own processes and root for everything else, which is the same
// condition under which we got a PID at all.
func resolveProcesses(env *Env, listeners []inventory.Listener) {
	// One process usually owns several sockets; cache so a Plex with 60
	// listeners is one read, not sixty.
	cache := map[int][]string{}

	for i := range listeners {
		pid := listeners[i].PID
		if pid == 0 {
			continue
		}
		argv, ok := cache[pid]
		if !ok {
			argv = readCmdline(env, pid)
			cache[pid] = argv
		}
		if len(argv) == 0 {
			continue
		}
		listeners[i].Command = redact.String(joinArgs(argv))
		// Only override the name when the kernel actually truncated it;
		// otherwise comm is the more accurate answer, since a process that
		// rewrote its title can say anything it likes in argv.
		if len(listeners[i].Process) >= commMaxLen {
			if full := filepath.Base(argv[0]); len(full) > len(listeners[i].Process) {
				listeners[i].Process = full
			}
		}
	}
}

func readCmdline(env *Env, pid int) []string {
	raw, err := env.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	var argv []string
	for _, a := range strings.Split(strings.TrimRight(raw, "\x00"), "\x00") {
		if a != "" {
			argv = append(argv, a)
		}
	}
	return argv
}

// joinArgs caps the stored command line. Some containers are launched with
// enormous argument lists and the inventory does not need all of it.
func joinArgs(argv []string) string {
	s := strings.Join(argv, " ")
	const max = 400
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// ssProcess pulls the first "name",pid=N pair out of ss's users:(...) column.
// A socket can have several owners (pre-forking servers); the first is
// representative and the rest add noise to an inventory.
var ssProcess = regexp.MustCompile(`\(\("([^"]+)",pid=(\d+)`)

// netstatProcess matches netstat's "1234/sshd" column.
var netstatProcess = regexp.MustCompile(`^(\d+)/(.+)$`)

func parseSS(out string) []inventory.Listener {
	var listeners []inventory.Listener
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if f[0] == "Netid" || f[0] == "State" { // header
			continue
		}
		proto := f[0]
		if proto != "tcp" && proto != "udp" {
			continue
		}
		state := f[1]
		if state != "LISTEN" && state != "UNCONN" {
			continue
		}
		l := newListener(proto, f[4])
		if m := ssProcess.FindStringSubmatch(line); m != nil {
			l.Process = m[1]
			l.PID = atoi(m[2])
		}
		listeners = append(listeners, l)
	}
	return listeners
}

func parseNetstat(out string) []inventory.Listener {
	var listeners []inventory.Listener
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		proto := f[0]
		if !strings.HasPrefix(proto, "tcp") && !strings.HasPrefix(proto, "udp") {
			continue
		}
		// netstat's udp rows have no State column, so the presence of LISTEN
		// cannot be required; -l already restricted us to listeners.
		if strings.HasPrefix(proto, "tcp") && !containsField(f, "LISTEN") {
			continue
		}
		base := "tcp"
		if strings.HasPrefix(proto, "udp") {
			base = "udp"
		}
		l := newListener(base, f[3])
		if m := netstatProcess.FindStringSubmatch(f[len(f)-1]); m != nil {
			l.PID = atoi(m[1])
			l.Process = m[2]
		}
		listeners = append(listeners, l)
	}
	return listeners
}

func containsField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// newListener splits an address:port token into its parts.
//
// The shapes ss and netstat emit:
//
//	1.2.3.4:80                     plain IPv4
//	*:80                           dual-stack wildcard
//	[::1]:80                       bracketed IPv6
//	127.0.0.53%lo:53               IPv4 with a zone
//	[ff02::fb]%veth1a2b3c:5353     bracketed IPv6 with a zone
//
// The last shape is why the zone is parsed out rather than left glued to the
// address: naive bracket-trimming leaves a stray "]" in the middle
// ("ff02::fb]%veth1a2b3c"), and an mDNS listener bound on fifty interfaces
// then produces fifty distinct-looking addresses instead of one.
func newListener(proto, addrPort string) inventory.Listener {
	l := inventory.Listener{Protocol: proto}

	addr, portStr := addrPort, ""
	if i := strings.LastIndex(addrPort, ":"); i >= 0 {
		addr, portStr = addrPort[:i], addrPort[i+1:]
	}

	if strings.HasPrefix(addr, "[") {
		if end := strings.Index(addr, "]"); end >= 0 {
			l.Zone = strings.TrimPrefix(addr[end+1:], "%")
			addr = addr[1:end]
		} else {
			addr = strings.TrimPrefix(addr, "[")
		}
	} else if i := strings.Index(addr, "%"); i >= 0 {
		l.Zone = addr[i+1:]
		addr = addr[:i]
	}

	l.Address = addr
	if p, err := strconv.Atoi(portStr); err == nil {
		l.Port = p
	}

	switch {
	case addr == "*" || addr == "":
		l.Family = "any"
	case strings.Contains(addr, ":"):
		l.Family = "ipv6"
	default:
		l.Family = "ipv4"
	}

	// Wildcard binds are reachable from every network the host is attached
	// to. This flag is the first thing to look at when auditing exposure.
	switch addr {
	case "0.0.0.0", "::", "*", "":
		l.Exposed = true
	}
	return l
}

func sortListeners(ls []inventory.Listener) {
	sort.Slice(ls, func(a, b int) bool {
		if ls[a].Port != ls[b].Port {
			return ls[a].Port < ls[b].Port
		}
		if ls[a].Protocol != ls[b].Protocol {
			return ls[a].Protocol < ls[b].Protocol
		}
		return ls[a].Address < ls[b].Address
	})
}
