// Command discover produces a read-only inventory of the machine it runs on.
//
// It is meant to be built once, copied to each host (Proxmox node, Ubuntu VM,
// an LXC), and run there. It writes a single JSON document describing what is
// installed, running, listening, and scheduled. It never modifies the host:
// every external command goes through an allowlist in internal/sysexec.
//
//	discover                      # JSON to stdout
//	discover -o proxmox.json      # ...or to a file, mode 0600
//	discover -only network,docker # just those sections
//	discover -list                # show collector names
//
// Exit codes: 0 success (possibly with warnings), 1 fatal error, 2 when
// -strict is set and any collector reported an error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"os/user"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/collect"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// version is overridden at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always --dirty)"
var version = "dev"

func main() {
	if err := run(); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			fmt.Fprintln(os.Stderr, "discover:", ec.msg)
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "discover:", err)
		os.Exit(1)
	}
}

// exitCode lets run() return a specific status without calling os.Exit from
// deep inside the program, which would skip deferred cleanup.
type exitCode struct {
	code int
	msg  string
}

func (e exitCode) Error() string { return e.msg }

func run() error {
	var (
		outPath     = flag.String("o", "", "write JSON to this file instead of stdout (mode 0600)")
		only        = flag.String("only", "", "comma-separated collectors to run exclusively")
		skip        = flag.String("skip", "", "comma-separated collectors to skip")
		timeout     = flag.Duration("timeout", 30*time.Second, "per-collector timeout")
		totalBudget = flag.Duration("deadline", 5*time.Minute, "overall wall-clock budget for the run")
		compact     = flag.Bool("compact", false, "emit compact JSON instead of indented")
		list        = flag.Bool("list", false, "list collector names and exit")
		strict      = flag.Bool("strict", false, "exit 2 if any collector reported an error")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = usage
	flag.Parse()

	switch {
	case *showVersion:
		fmt.Println("discover", version)
		return nil
	case *list:
		for _, n := range collect.Names() {
			fmt.Println(n)
		}
		return nil
	}

	// Ctrl-C should produce a partial inventory, not a truncated file. The
	// context is cancelled, Run returns what it has, and we still write it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *totalBudget)
	defer cancel()

	inv, err := collect.Run(ctx, collect.Options{
		Only:                splitCSV(*only),
		Skip:                splitCSV(*skip),
		PerCollectorTimeout: *timeout,
	})
	if err != nil {
		return err
	}
	inv.Tool = toolInfo()

	data, err := marshal(inv, !*compact)
	if err != nil {
		return fmt.Errorf("encoding inventory: %w", err)
	}

	if err := emit(*outPath, data); err != nil {
		return err
	}
	summarize(os.Stderr, inv, *outPath)

	if *strict && collect.HasErrors(inv) {
		return exitCode{code: 2, msg: "collectors reported errors (see diagnostics)"}
	}
	return nil
}

func marshal(inv *inventory.Inventory, indent bool) ([]byte, error) {
	if indent {
		return json.MarshalIndent(inv, "", "  ")
	}
	return json.Marshal(inv)
}

// emit writes to stdout, or atomically to a file.
//
// Atomic means write-temp-then-rename: a reader (or a future uploader
// watching the directory) never sees a half-written document, and an
// interrupted run leaves the previous inventory intact. Mode 0600 because even
// a redacted inventory maps every open port on the host.
func emit(path string, data []byte) error {
	if path == "" {
		if _, err := os.Stdout.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("writing stdout: %w", err)
		}
		return nil
	}

	if err := fsx.WriteAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func toolInfo() inventory.Tool {
	t := inventory.Tool{
		Name:      "discover",
		Version:   version,
		Revision:  inventory.AgentRevision,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Args:      os.Args[1:],
		UID:       os.Geteuid(),
		Elevated:  os.Geteuid() == 0,
	}
	if u, err := user.Current(); err == nil {
		t.User = u.Username
	}
	return t
}

// summarize prints a human-readable digest to stderr so that `discover > x.json`
// still tells you what happened.
func summarize(w *os.File, inv *inventory.Inventory, outPath string) {
	var warn, errs int
	for _, d := range inv.Diagnostics {
		switch d.Severity {
		case inventory.SeverityWarn:
			warn++
		case inventory.SeverityError:
			errs++
		}
	}

	dest := "stdout"
	if outPath != "" {
		dest = outPath
	}
	fmt.Fprintf(w, "discover %s: host=%s in %dms -> %s\n",
		version, inv.Host.Hostname, inv.DurationMS, dest)

	var real, hidden, full int
	for _, fs := range inv.Host.Filesystems {
		switch fs.Kind {
		case inventory.FSKindReal, inventory.FSKindNetwork:
			real++
			if fs.UsedPct >= 80 {
				full++
			}
		default:
			hidden++
		}
	}
	fmt.Fprintf(w, "  storage:    %d real/network filesystems (%d at 80%%+), %d container & snap mounts\n",
		real, full, hidden)

	if inv.Systemd != nil && inv.Systemd.Available {
		var failed []string
		for _, u := range inv.Systemd.Units {
			if u.Active == "failed" || u.Sub == "failed" {
				failed = append(failed, u.Name)
			}
		}
		fmt.Fprintf(w, "  systemd:    %d units (%d failed), %d timers\n",
			len(inv.Systemd.Units), inv.Systemd.FailedCount, len(inv.Systemd.Timers))
		if len(failed) > 0 {
			fmt.Fprintf(w, "              failed: %s\n", strings.Join(failed, ", "))
		}
	}
	if inv.Network != nil {
		exposed := 0
		for _, l := range inv.Network.Listeners {
			if l.Exposed {
				exposed++
			}
		}
		fmt.Fprintf(w, "  network:    %d listeners (%d on wildcard addresses) via %s\n",
			len(inv.Network.Listeners), exposed, inv.Network.Source)
	}
	if inv.Docker != nil && inv.Docker.Available {
		running := 0
		var unhealthy []string
		for _, c := range inv.Docker.Containers {
			switch c.State {
			case "running":
				running++
			case "restarting", "dead", "paused":
				unhealthy = append(unhealthy, c.Name+" ("+c.State+")")
			}
		}
		readable := 0
		files := 0
		for _, p := range inv.Docker.ComposeProjects {
			for _, f := range p.ConfigFiles {
				files++
				if f.Exists {
					readable++
				}
			}
		}
		fmt.Fprintf(w, "  docker:     %d containers (%d running), %d compose projects, %d/%d compose files readable\n",
			len(inv.Docker.Containers), running, len(inv.Docker.ComposeProjects), readable, files)
		if len(unhealthy) > 0 {
			fmt.Fprintf(w, "              not running: %s\n", strings.Join(unhealthy, ", "))
		}
	}
	if inv.Proxmox != nil && inv.Proxmox.Available {
		var running int
		var notAutostart []string
		for _, g := range inv.Proxmox.Guests {
			if g.Status == "running" {
				running++
				if !g.OnBoot {
					notAutostart = append(notAutostart, fmt.Sprintf("%d/%s", g.VMID, g.Name))
				}
			}
		}
		fmt.Fprintf(w, "  proxmox:    %d guests (%d running), %d storages\n",
			len(inv.Proxmox.Guests), running, len(inv.Proxmox.Storages))
		if len(notAutostart) > 0 {
			fmt.Fprintf(w, "              running but not set to start on boot: %s\n",
				strings.Join(notAutostart, ", "))
		}
	}

	if inv.ZFS != nil && inv.ZFS.Available {
		var unhealthy []string
		for _, p := range inv.ZFS.Pools {
			if p.Health != "ONLINE" {
				unhealthy = append(unhealthy, fmt.Sprintf("%s (%s)", p.Name, p.Health))
			}
		}
		fmt.Fprintf(w, "  zfs:        %d pool(s), %d dataset(s)\n",
			len(inv.ZFS.Pools), len(inv.ZFS.Datasets))
		for _, p := range inv.ZFS.Pools {
			fmt.Fprintf(w, "              %-12s %s  %d%% full, %d%% fragmented\n",
				p.Name, p.Health, p.CapacityPct, p.FragmentationPct)
		}
		if len(unhealthy) > 0 {
			fmt.Fprintf(w, "              NOT HEALTHY: %s\n", strings.Join(unhealthy, ", "))
		}
	}

	if inv.Shares != nil {
		fmt.Fprintf(w, "  shares:     %d NFS export(s), %d SMB share(s)\n",
			len(inv.Shares.NFS), len(inv.Shares.SMB))
	}

	if inv.Scheduling != nil {
		n := len(inv.Scheduling.CronD) + len(inv.Scheduling.UserCrontabs)
		if inv.Scheduling.SystemCrontab != nil {
			n++
		}
		fmt.Fprintf(w, "  scheduling: %d crontab files, %d periodic dirs\n", n, len(inv.Scheduling.PeriodicDirs))
	}

	if warn+errs > 0 {
		fmt.Fprintf(w, "  %d warning(s), %d error(s) — see .diagnostics in the output\n", warn, errs)
	}
	if !inv.Tool.Elevated {
		fmt.Fprintln(w, "  note: not running as root; process names and user crontabs may be missing")
	}
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func usage() {
	fmt.Fprintf(os.Stderr, `discover %s — read-only host inventory

Usage:
  discover [flags]

Collectors: %s

Flags:
`, version, strings.Join(collect.Names(), ", "))
	flag.PrintDefaults()
}
