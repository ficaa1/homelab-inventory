// Package collect implements the discovery collectors and the runner that
// drives them.
package collect

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
)

// Collector gathers one section of the inventory.
//
// Design note — why Collect mutates *Inventory instead of returning a value:
// the alternative signature, Collect(ctx) (any, error), would keep collectors
// independent of the schema, but it forces the inventory into a
// map[string]any and throws away the compile-time types that the ingest API
// code wants. Handing each collector the document keeps everything typed. The
// cost is that a collector *could* scribble on another's section; the
// convention is that it writes exactly one top-level field plus diagnostics,
// and that convention is cheap to check in review.
type Collector interface {
	// Name is the stable identifier used by -only/-skip and in diagnostics.
	Name() string
	// Collect fills in its section. It should return an error only when the
	// section is wholly unobtainable; partial results belong in the inventory
	// with a Diagnostic attached via Env.
	Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error
}

// Env carries the shared dependencies handed to every collector. Passing this
// explicitly (rather than reaching for package-level globals) is what makes
// collectors testable: a test supplies a Runner pointed at fixtures and a
// RootFS pointed at testdata.
type Env struct {
	Run *sysexec.Runner

	// ForcePackages bypasses the daily window for an explicit -only packages.
	ForcePackages bool
	// Now supplies local wall time; nil uses time.Now.
	Now func() time.Time
	// Executable is discover's own binary, whose mtime marks a fresh
	// install; "" uses os.Executable.
	Executable string

	// RootFS is prefixed to absolute paths the collectors read. It is "" in
	// production and a testdata directory in tests. This is the poor man's
	// filesystem abstraction; it beats an interface here because every read is
	// a plain os.ReadFile and io/fs would buy nothing but ceremony.
	RootFS string

	// MaxFileBytes caps how much of any single discovered file we store.
	MaxFileBytes int64

	diags *[]inventory.Diagnostic
	name  string
}

// Path resolves an absolute host path through RootFS.
func (e *Env) Path(p string) string {
	if e.RootFS == "" {
		return p
	}
	return e.RootFS + p
}

// ReadFile reads a host file, honouring RootFS and MaxFileBytes.
func (e *Env) ReadFile(p string) (string, error) {
	f, err := os.Open(e.Path(p))
	if err != nil {
		return "", err
	}
	defer f.Close()

	limit := e.MaxFileBytes
	if limit <= 0 {
		limit = DefaultMaxFileBytes
	}
	// Read one byte past the limit so we can tell "exactly at the cap" from
	// "truncated".
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > limit {
		return string(b[:limit]) + "\n[truncated]", nil
	}
	return string(b), nil
}

// DefaultMaxFileBytes caps stored file contents at 256 KiB. Compose files are
// tiny; anything larger is almost certainly not what we meant to capture.
const DefaultMaxFileBytes = 256 << 10

// Infof records an informational diagnostic ("docker not installed").
func (e *Env) Infof(format string, args ...any) {
	e.diag(inventory.SeverityInfo, fmt.Sprintf(format, args...), "")
}

// Warnf records a partial-result diagnostic ("could not read /var/spool/cron").
func (e *Env) Warnf(format string, args ...any) {
	e.diag(inventory.SeverityWarn, fmt.Sprintf(format, args...), "")
}

// Errorf records a collector failure.
func (e *Env) Errorf(format string, args ...any) {
	e.diag(inventory.SeverityError, fmt.Sprintf(format, args...), "")
}

// WarnErr is the common case: something failed, keep going, record why.
func (e *Env) WarnErr(msg string, err error) {
	if err == nil {
		return
	}
	e.diag(inventory.SeverityWarn, msg, err.Error())
}

func (e *Env) diag(sev inventory.Severity, msg, detail string) {
	if e.diags == nil {
		return
	}
	*e.diags = append(*e.diags, inventory.Diagnostic{
		Collector: e.name,
		Severity:  sev,
		Message:   msg,
		Detail:    detail,
		At:        time.Now().UTC(),
	})
}

// All returns every collector in a deterministic order. Host runs first
// because later diagnostics are easier to read once you know what box you are
// looking at.
func All() []Collector {
	return []Collector{
		HostCollector{},
		DiskHealthCollector{},
		KernelIOCollector{},
		SystemdCollector{},
		NetworkCollector{},
		DockerCollector{},
		PackagesCollector{},
		// Proxmox runs after host because it needs the node name, which is
		// the hostname. That is the one ordering dependency between
		// collectors; everything else that spans sections lives in
		// correlate.go.
		ProxmoxCollector{},
		ZFSCollector{},
		SharesCollector{},
		SchedulingCollector{},
		FirewallCollector{},
	}
}

// Names returns the identifiers accepted by -only and -skip.
func Names() []string {
	var out []string
	for _, c := range All() {
		out = append(out, c.Name())
	}
	sort.Strings(out)
	return out
}

// Options controls a run.
type Options struct {
	// Only, when non-empty, restricts the run to these collector names.
	Only []string
	// Skip removes collector names from the run.
	Skip []string
	// PerCollectorTimeout bounds each collector independently, so one slow
	// section cannot consume the whole budget.
	PerCollectorTimeout time.Duration
	// RootFS is for testing; see Env.RootFS.
	RootFS string
}

// Select filters All() according to opts.
func Select(opts Options) ([]Collector, error) {
	only := toSet(opts.Only)
	skip := toSet(opts.Skip)
	known := toSet(Names())

	for n := range only {
		if _, ok := known[n]; !ok {
			return nil, fmt.Errorf("unknown collector %q (known: %s)", n, strings.Join(Names(), ", "))
		}
	}
	for n := range skip {
		if _, ok := known[n]; !ok {
			return nil, fmt.Errorf("unknown collector %q (known: %s)", n, strings.Join(Names(), ", "))
		}
	}

	var out []Collector
	for _, c := range All() {
		if len(only) > 0 {
			if _, ok := only[c.Name()]; !ok {
				continue
			}
		}
		if _, ok := skip[c.Name()]; ok {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}

func toSet(ss []string) map[string]struct{} {
	m := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = struct{}{}
		}
	}
	return m
}

// Run executes the selected collectors and returns a populated Inventory.
//
// Collectors run sequentially. Running them in parallel would shave a second
// or two off a run, but sequential execution gives deterministic diagnostic
// ordering and makes a hang trivially attributable to one collector — worth
// more than the wall-clock during the early period where we are still
// establishing whether the output is correct. Revisit if a run ever gets slow
// enough to notice.
func Run(ctx context.Context, opts Options) (*inventory.Inventory, error) {
	collectors, err := Select(opts)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   start.UTC(),
		Diagnostics:   []inventory.Diagnostic{},
	}

	runner := sysexec.New()
	if opts.PerCollectorTimeout > 0 && opts.PerCollectorTimeout < sysexec.DefaultTimeout {
		runner.Timeout = opts.PerCollectorTimeout
	}

	_, forcePackages := toSet(opts.Only)["packages"]
	newEnv := func(name string) *Env {
		return &Env{
			Run:           runner,
			ForcePackages: forcePackages,
			RootFS:        opts.RootFS,
			MaxFileBytes:  DefaultMaxFileBytes,
			diags:         &inv.Diagnostics,
			name:          name,
		}
	}

	for _, c := range collectors {
		env := newEnv(c.Name())

		cctx := ctx
		var cancel context.CancelFunc
		if opts.PerCollectorTimeout > 0 {
			cctx, cancel = context.WithTimeout(ctx, opts.PerCollectorTimeout)
		}
		err := c.Collect(cctx, env, inv)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			env.Errorf("collector failed: %v", err)
		}
		if ctx.Err() != nil {
			env.Errorf("run cancelled: %v", ctx.Err())
			break
		}
	}

	// Correlation runs last and reads across sections. See correlate.go for
	// why it is a step of its own rather than work inside a collector.
	correlate(newEnv("correlate"), inv)

	inv.DurationMS = time.Since(start).Milliseconds()
	return inv, nil
}

// HasErrors reports whether any diagnostic was recorded at error severity.
// The CLI uses this for its -strict exit code.
func HasErrors(inv *inventory.Inventory) bool {
	for _, d := range inv.Diagnostics {
		if d.Severity == inventory.SeverityError {
			return true
		}
	}
	return false
}
