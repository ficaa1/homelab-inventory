// Package execallow validates and carries out allowlisted Docker commands on
// a remote host. It is the mutating sibling of internal/sysexec (which is
// read-only by invariant) and does not import it — the exec pattern is copied,
// not shared, so the two packages cannot accidentally grow a shared mutable
// surface.
//
// Every operation validates its args against a hardcoded allowlist before
// touching the host. Unknown commands and disallowed paths are rejected before
// a process is created.
package execallow

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

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// safeName matches names safe for use as a service/container identifier.
// Shell metacharacters and argument-injection sequences are rejected.
var safeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

const (
	// DefaultTimeout bounds a restart against a hung daemon.
	DefaultTimeout = 120 * time.Second

	// DefaultMaxOutput caps captured stdout at 8 MiB.
	DefaultMaxOutput = 8 << 20
)

// Result is the outcome of an allowlisted operation.
type Result struct {
	Command  string `json:"command"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

// Runner executes allowlisted commands with a per-command timeout.
//
// The zero value is usable; DefaultTimeout is applied when Timeout is zero.
type Runner struct {
	Policy  Policy
	Timeout time.Duration
	// Inventory must return the latest target host inventory. Missing inventory fails closed.
	Inventory func() (*inventory.Inventory, error)
	// Execute overrides process execution for offline tests, after validation.
	Execute func(context.Context, string, map[string]string) Result
}

// command describes one allowlisted operation.
type command struct {
	// required lists argument names that must be non-empty.
	required []string
	// optional lists argument names that, when set, must match safeName.
	optional []string
}

// allowed is the set of named operations. Every entry maps to a command that
// is safe to carry out given validated arguments.
var allowed = map[string]command{
	"compose-restart": {
		required: []string{"compose_file"},
		optional: []string{"service", "project"},
	},
	"container-restart": {
		required: []string{"container"},
	},
}

// Run validates and carries out one allowlisted command. It never returns a Go
// error; every outcome is folded into Result (ExitCode -1 = rejected/could not
// run, 0 = success, >0 = ran but exited non-zero).
func (r *Runner) Run(ctx context.Context, command string, args map[string]string) Result {
	if err := r.Validate(command, args); err != nil {
		return Result{Command: command, ExitCode: -1, Error: err.Error()}
	}
	if r.Execute != nil {
		return r.Execute(ctx, command, args)
	}

	if command == "container-restart" {
		name := args["container"]
		inspected := r.runArgv(ctx, []string{"docker", "inspect", "--type=container", "--format", "{{.Name}}", "--", name})
		if inspected.Error != "" || inspected.ExitCode != 0 || strings.TrimSpace(inspected.Stdout) != "/"+name {
			return Result{Command: command, ExitCode: -1, Error: "live Docker target is not the exact container name"}
		}
	}
	return r.runCommand(ctx, command, args)
}

// Validate applies the same guard used by Run, without executing anything.
func (r *Runner) Validate(command string, args map[string]string) error {
	res := r.validate(command, args)
	if res.Error != "" {
		return errors.New(res.Error)
	}
	return nil
}

func (r *Runner) validate(command string, args map[string]string) Result {
	cmd, ok := allowed[command]
	if !ok {
		return Result{Command: command, ExitCode: -1, Error: fmt.Sprintf("unknown command %q", command)}
	}

	// Validate required args.
	for _, name := range cmd.required {
		if strings.TrimSpace(args[name]) == "" {
			return Result{Command: command, ExitCode: -1, Error: fmt.Sprintf("%s is required for %s", name, command)}
		}
	}

	// Validate compose_file against the allowlist (when present).
	if cf := args["compose_file"]; cf != "" {
		if !r.Policy.ComposeFileAllowed(cf) {
			return Result{Command: command, ExitCode: -1, Error: fmt.Sprintf("compose_file %q is not allowed", cf)}
		}
	} else {
		// compose_file is required for compose-* commands, but if empty
		// the required-args check above already caught it. For non-compose
		// commands it's fine.
	}

	// Validate optional args against safeName.
	for _, name := range cmd.optional {
		if v := args[name]; v != "" && !safeName.MatchString(v) {
			return Result{Command: command, ExitCode: -1, Error: fmt.Sprintf("%s %q is not a valid name", name, v)}
		}
	}

	// Validate container against safeName (container-restart).
	if v := args["container"]; v != "" && !safeName.MatchString(v) {
		return Result{Command: command, ExitCode: -1, Error: fmt.Sprintf("container %q is not a valid name", v)}
	}

	if command == "container-restart" || command == "compose-restart" {
		if r.Inventory == nil {
			return Result{Error: "latest target host inventory is unavailable"}
		}
		inv, err := r.Inventory()
		if err != nil {
			return Result{Error: "latest target host inventory is unavailable"}
		}
		if err := r.Policy.CheckInventory(inv); err != nil {
			return Result{Error: err.Error()}
		}
		if command == "container-restart" {
			if _, err := FindContainer(inv, args["container"]); err != nil {
				return Result{Error: err.Error()}
			}
		}
	}
	if command == "compose-restart" && (args["project"] == "" || r.Policy.ComposeFiles[args["project"]] != args["compose_file"]) {
		return Result{Error: "compose project is not allowlisted"}
	}
	return Result{}
}

// runCommand builds the docker/docker compose argv and runs it.
func (r *Runner) runCommand(ctx context.Context, name string, args map[string]string) Result {
	return r.runArgv(ctx, buildArgv(name, args))
}

func (r *Runner) runArgv(ctx context.Context, argv []string) Result {
	label := strings.Join(argv, " ")

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin := argv[0]
	binArgs := argv[1:]

	cmd := exec.CommandContext(ctx, bin, binArgs...)
	cmd.Env = []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"LC_ALL=C",
		"LANG=C",
	}
	cmd.Stdin = nil
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	res := Result{
		Command: label,
		Stdout:  truncate(stdout.String(), DefaultMaxOutput),
		Stderr:  truncate(strings.TrimSpace(stderr.String()), 4096),
	}

	switch {
	case err == nil:
		res.ExitCode = 0
		return res
	case ctx.Err() != nil:
		res.ExitCode = -1
		res.Error = fmt.Sprintf("timed out after %s", timeout)
		return res
	default:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
			return res
		}
		res.ExitCode = -1
		res.Error = err.Error()
		return res
	}
}

// buildArgv constructs the docker/docker compose argv for a shell-exec command.
// It is a pure function for testability.
func buildArgv(name string, args map[string]string) []string {
	switch name {
	case "compose-restart":
		a := []string{"docker", "compose", "-f", args["compose_file"]}
		if p := args["project"]; p != "" {
			a = append(a, "-p", p)
		}
		a = append(a, "restart")
		if s := args["service"]; s != "" {
			a = append(a, s)
		}
		return a
	case "container-restart":
		return []string{"docker", "restart", "/" + args["container"]}
	default:
		return nil
	}
}

// Commands returns the allowed command names sorted, for startup logging.
func Commands() []string {
	names := make([]string, 0, len(allowed))
	for n := range allowed {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func truncate(s string, max int) string {
	if max > 0 && len(s) > max {
		return s[:max] + "\n[truncated]"
	}
	return s
}
