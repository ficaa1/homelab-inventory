// Command alerts reports what is wrong across the homelab, from inventories
// already collected by discover.
//
//	alerts                      # everything worth looking at
//	alerts -level critical      # just the fires
//	alerts -host host-c        # one machine
//	alerts -json                # the same data GET /alerts serves
//
// Like portmap, this runs on your laptop against inventories/ and is
// deliberately thin: every rule lives in internal/alert so this command, the
// API and a future dashboard cannot drift apart on what counts as a problem.
//
// Exit status is 1 when anything critical is outstanding, so this can be the
// last line of a cron job without needing a wrapper to interpret it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/portmap"
)

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "alerts:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run() (int, error) {
	var (
		dir       = flag.String("dir", "inventories", "directory of inventory JSON files")
		hostFlag  = flag.String("host", "", "limit output to one host")
		levelFlag = flag.String("level", "", "only show alerts at this level or worse (critical, warning, info)")
		asJSON    = flag.Bool("json", false, "emit the report as JSON")
		// Off by default here, unlike the API. These files are a snapshot you
		// collected by hand, so "this host has not reported recently" says
		// something about when you last ran collect, not about the host — and
		// with eight of them it buries everything that is actually wrong. The
		// age is still reported, once, in the header. Pass a duration to turn
		// the per-host rule on.
		staleAfter = flag.Duration("stale", 0, "alert per-host when an inventory is older than this (0 disables)")
	)
	flag.Parse()

	// portmap.LoadDir is the existing "read inventories/ into memory" helper.
	// Reusing it keeps one definition of how a directory of runs is loaded,
	// rather than a second copy that quietly disagrees about, say, filling in
	// a missing hostname from the filename.
	invs, err := portmap.LoadDir(*dir)
	if err != nil {
		return 0, err
	}
	if len(invs) == 0 {
		return 0, fmt.Errorf("no inventories found in %s (run 'make collect' first)", *dir)
	}

	th := alert.DefaultThresholds()
	th.StaleAfter, th.StaleCritAfter = *staleAfter, *staleAfter*2

	now := time.Now()
	rep := alert.Evaluate(invs, th, now)
	oldest := oldestCollectedAt(invs)

	if *hostFlag != "" {
		rep = rep.FilterHost(*hostFlag)
		if len(rep.Hosts) == 0 {
			return 0, fmt.Errorf("no inventory for host %q", *hostFlag)
		}
	}
	if *levelFlag != "" {
		min, ok := alert.ParseLevel(*levelFlag)
		if !ok {
			return 0, fmt.Errorf("level %q is not one of: critical, warning, info", *levelFlag)
		}
		rep = rep.FilterLevel(min)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 0, err
		}
	} else {
		report(rep, oldest, now)
	}

	if rep.Counts.Critical > 0 {
		return 1, nil
	}
	return 0, nil
}

func report(rep *alert.Report, oldest time.Time, now time.Time) {
	age := ""
	if !oldest.IsZero() {
		age = fmt.Sprintf(" (oldest inventory %s old)", roundAge(now.Sub(oldest)))
	}

	if len(rep.Alerts) == 0 {
		fmt.Printf("nothing to report across %d host(s)%s: %s\n",
			len(rep.Hosts), age, strings.Join(rep.Hosts, ", "))
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()

	fmt.Fprintf(w, "%d alert(s) across %d host(s)%s\t%d critical, %d warning, %d info\n",
		rep.Counts.Total, len(rep.Hosts), age,
		rep.Counts.Critical, rep.Counts.Warning, rep.Counts.Info)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "LEVEL\tHOST\tRULE\tWHAT")

	for _, a := range rep.Alerts {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", marker(a.Level), a.Host, a.Rule, a.Message)
	}
}

// oldestCollectedAt is what the header reports instead of a per-host stale
// alert: one line saying how old the data is, rather than one alert per host
// saying the same thing.
func oldestCollectedAt(invs []*inventory.Inventory) time.Time {
	var oldest time.Time
	for _, inv := range invs {
		if inv == nil || inv.CollectedAt.IsZero() {
			continue
		}
		if oldest.IsZero() || inv.CollectedAt.Before(oldest) {
			oldest = inv.CollectedAt
		}
	}
	return oldest
}

func roundAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// marker renders a level for a terminal. The data keeps the level as a plain
// string; only this display layer decides it deserves punctuation.
func marker(l alert.Level) string {
	switch l {
	case alert.LevelCritical:
		return "!! crit"
	case alert.LevelWarning:
		return " ! warn"
	default:
		return "   info"
	}
}
