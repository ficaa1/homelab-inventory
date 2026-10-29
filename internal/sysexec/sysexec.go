// Package sysexec runs external commands on behalf of collectors.
//
// It exists to make "read-only" a property the code enforces rather than a
// promise in a comment. Every command must name a binary on the allowlist
// below; anything else fails before a process is created. Remediation — the
// mutating side of this tooling — will use a separate, explicitly-privileged
// runner, so there is never a moment where the discovery path could mutate
// the host.
//
// Two other conventions worth internalising:
//
//   - A non-zero exit status is not a Go error. `systemctl status` exits 3 for
//     an inactive unit; `which docker` exits 1 when docker is absent. Those are
//     answers, not failures. Only a failure to *run* the command (missing
//     binary, context deadline, fork failure) returns an error.
//   - Every call takes a context. A hung `docker inspect` against a wedged
//     daemon should not hang the whole inventory run.
package sysexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ErrNotAllowed is returned when a command is not on the read-only allowlist.
var ErrNotAllowed = errors.New("sysexec: command not on read-only allowlist")

// ErrNotFound is returned when an allowed binary is not installed.
var ErrNotFound = errors.New("sysexec: binary not found in PATH")

// rule describes what a binary is permitted to do.
type rule struct {
	// subcommands restricts the first non-flag argument. An empty list means
	// the binary has no destructive subcommands and needs no such guard.
	//
	// This second dimension exists because binary-level allowlisting is not
	// enough for the tools Proxmox ships: `pvesh get` reads the API and
	// `pvesh set` writes to it; `qm config` prints a guest's configuration and
	// `qm destroy` deletes the guest. Same binary, opposite consequences.
	subcommands []string
}

// allowed is the set of binaries this tool may execute. Keep it small,
// alphabetical, and justify additions: every entry widens the blast radius.
//
// Convention that this guard depends on: no separated flag value may precede
// the subcommand. `systemctl -t service list-units` would be read as the
// subcommand "service" and refused; write `systemctl list-units --type=service`
// instead. Flags after the subcommand are unaffected, so
// `docker version --format X` is fine. Every call site in this repo complies.
var allowed = map[string]rule{
	// Read-only container queries. Deliberately not `volume`, `network` or
	// `image`, whose own subcommands include rm and prune.
	"docker": {subcommands: []string{"version", "ps", "inspect"}},

	// Entire argument vectors are checked before process creation.
	"journalctl": {}, // Fixed kernel window only.
	"smartctl":   {}, // Fixed health reads only.

	// Proxmox. `get` is the only read verb pvesh has; create/set/delete are
	// the others, which is exactly why this guard exists.
	"pct":        {subcommands: []string{"config", "list", "status"}},
	"pvesh":      {subcommands: []string{"get"}},
	"pveversion": {},
	"qm":         {subcommands: []string{"config", "list", "status"}},

	// systemctl's write verbs (start, stop, enable, mask...) are excluded by
	// omission. A bare `systemctl --version` has no subcommand and is allowed.
	"systemctl": {subcommands: []string{"list-units", "list-unit-files", "list-timers", "show", "cat"}},

	// ZFS. `zpool destroy`, `zpool remove`, `zfs destroy` and `zfs rollback`
	// are all one typo away from the read verbs, which is what the guard is
	// for. `get` is included because it is the scriptable form of `list`.
	"zfs":   {subcommands: []string{"list", "get", "version"}},
	"zpool": {subcommands: []string{"list", "status", "get", "version"}},

	// Firewalls. `nft` is the sharpest tool on this list: `nft flush ruleset`
	// on a remote box takes the machine off the network, and `add`, `delete`,
	// `insert` and `replace` all sit one word away from `list`. Exactly one
	// subcommand is permitted, and a test asserts the destructive ones are
	// refused before a process is created.
	"nft": {subcommands: []string{"list"}},

	// pve-firewall's other verbs are start, stop, restart and localnet.
	// `status` says enabled/disabled, which is the whole question; `compile`
	// prints the ruleset it would apply without applying it.
	"pve-firewall": {subcommands: []string{"status", "compile"}},

	// Fixed argument vectors below exclude file output and socket destruction.
	"ip6tables-save":      {},
	"iptables-save":       {},
	"netstat":             {},
	"ss":                  {},
	"systemd-detect-virt": {},
	"uname":               {},
}

// permits reports whether args are acceptable for this rule, returning the
// subcommand it judged so the error can name it.
func (r rule) permits(args []string) (sub string, ok bool) {
	if len(r.subcommands) == 0 {
		return "", true
	}
	sub = firstNonFlag(args)
	if sub == "" {
		// No subcommand at all: `systemctl --version`, `docker`. Harmless.
		return "", true
	}
	for _, s := range r.subcommands {
		if s == sub {
			return sub, true
		}
	}
	return sub, false
}

func firstNonFlag(args []string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			return "<separated flag value>"
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// Result is the outcome of a command that ran to completion.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// OK reports whether the command exited zero.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Lines splits Stdout into non-empty trimmed lines, which is what most
// parsers want.
func (r Result) Lines() []string {
	var out []string
	for _, l := range strings.Split(r.Stdout, "\n") {
		if l = strings.TrimRight(l, "\r"); strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

// Runner executes allowlisted commands with a per-command timeout.
//
// The zero value is usable; DefaultTimeout is applied when Timeout is zero.
type Runner struct {
	Timeout time.Duration

	// MaxOutputBytes caps captured stdout. A runaway command should not be
	// able to exhaust memory on a small VM.
	MaxOutputBytes int
}

const (
	// DefaultTimeout is generous enough for `docker inspect` on a loaded box
	// but short enough that a wedged daemon does not stall the run.
	DefaultTimeout = 15 * time.Second
	// DefaultMaxOutput caps a single command's stdout at 8 MiB.
	DefaultMaxOutput = 8 << 20
)

// New returns a Runner with sensible defaults.
func New() *Runner {
	return &Runner{Timeout: DefaultTimeout, MaxOutputBytes: DefaultMaxOutput}
}

// Available reports whether an allowlisted binary exists in PATH. Collectors
// use this to degrade gracefully instead of emitting spurious errors on hosts
// that simply do not run Docker.
func (r *Runner) Available(name string) bool {
	if _, ok := allowed[name]; !ok {
		return false
	}
	_, err := exec.LookPath(name)
	return err == nil
}

// Subcommands returns the read-only subcommands permitted for a binary, for
// tests and diagnostics.
func Subcommands(name string) []string { return allowed[name].subcommands }

// Run executes name with args and returns its output.
//
// The returned error is non-nil only when the command could not be run to
// completion. Check Result.ExitCode for the command's own verdict.
func (r *Runner) Run(ctx context.Context, name string, args ...string) (Result, error) {
	spec, ok := allowed[name]
	if !ok {
		return Result{}, fmt.Errorf("%w: %q (allowed: %s)", ErrNotAllowed, name, strings.Join(allowedNames(), ", "))
	}
	if vectors, fixed := fixedArgs[name]; fixed {
		match := false
		for _, vector := range vectors {
			if strings.Join(args, "\x00") == strings.Join(vector, "\x00") {
				match = true
				break
			}
		}
		if !match {
			return Result{}, fmt.Errorf("%w: %s arguments", ErrNotAllowed, name)
		}
	}

	if name == "journalctl" && strings.Join(args, "\x00") != strings.Join(KernelJournalArgs(), "\x00") {
		return Result{}, fmt.Errorf("%w: journalctl arguments", ErrNotAllowed)
	}
	if name == "smartctl" && !smartRead(args) {
		return Result{}, fmt.Errorf("%w: smartctl arguments", ErrNotAllowed)
	}
	// Docker global flags can consume a read verb as their value and hide
	// the real command. Collectors never use global options; refuse them.
	if name == "docker" && len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return Result{}, fmt.Errorf("%w: docker global options", ErrNotAllowed)
	}
	if sub, ok := spec.permits(args); !ok {
		return Result{}, fmt.Errorf("%w: %q %q (allowed subcommands: %s)",
			ErrNotAllowed, name, sub, strings.Join(spec.subcommands, ", "))
	}
	if _, err := exec.LookPath(name); err != nil {
		return Result{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	// A predictable, minimal environment keeps output parseable: locale-aware
	// tools will happily translate their column headers otherwise.
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LC_ALL=C",
		"LANG=C",
	}
	// Never let a child read from the terminal; a prompt would hang the run.
	cmd.Stdin = nil

	// WaitDelay bounds the gap between killing the process and Wait returning.
	//
	// Without it, CommandContext kills the child on timeout but Wait still
	// blocks until every writer to the stdout pipe closes — and a grandchild
	// that inherited the descriptor keeps it open indefinitely. The symptom is
	// a collector that hangs for its full timeout and returns nothing, which
	// is what an node1 container did: every exec hit the deadline while
	// plain file reads worked fine. This turns that into a prompt error.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	res := Result{
		Stdout: truncate(stdout.String(), r.maxOutput()),
		Stderr: truncate(strings.TrimSpace(stderr.String()), 4096),
	}

	switch {
	case err == nil:
		res.ExitCode = 0
		return res, nil
	case ctx.Err() != nil:
		return res, fmt.Errorf("running %s: %w", name, ctx.Err())
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Ran fine, disagreed with us. Not an error.
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("running %s: %w", name, err)
	}
}

func (r *Runner) maxOutput() int {
	if r.MaxOutputBytes <= 0 {
		return DefaultMaxOutput
	}
	return r.MaxOutputBytes
}

func truncate(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "\n[truncated]"
	}
	return s
}

func allowedNames() []string {
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// KernelJournalArgs fixes the source, boot and rolling window. No journal writes.
func KernelJournalArgs() []string {
	return []string{"--dmesg", "--boot=0", "--since=-6h", "--no-pager", "--output=json", "--quiet"}
}

var smartDevice = regexp.MustCompile(`^/dev/(sd[a-z]+|hd[a-z]+|vd[a-z]+|nvme[0-9]+(n[0-9]+)?|sg[0-9]+)$`)

// SmartDevice restricts scan output to direct disk nodes, never options or paths.
func SmartDevice(s string) bool { return smartDevice.MatchString(s) }
func smartRead(args []string) bool {
	if len(args) == 1 && args[0] == "--scan" {
		return true
	}
	return len(args) == 6 && args[0] == "-H" && args[1] == "-A" && args[2] == "-j" && args[3] == "-n" && args[4] == "standby" && SmartDevice(args[5])
}

// No file output, destructive socket flags or arbitrary operands.
var fixedArgs = map[string][][]string{
	"ss":             {{"-tulpn"}, {"-tuln"}},
	"netstat":        {{"-tulpn"}, {"-tuln"}},
	"iptables-save":  {{}},
	"ip6tables-save": {{}},
	"uname":          {{"-s"}, {"-r"}, {"-m"}, {"-a"}},
}
