// Command portmap answers "what is on this port, and which ports are free?"
// from inventories already collected by discover.
//
// It runs on your laptop against inventories/, not on the servers. That
// separation is deliberate: discover is a read-only agent you copy onto a
// host, portmap is analysis over its output.
//
//	portmap                      # reserved ports per host
//	portmap -port 8096           # who owns 8096, on every host
//	portmap -free 5              # five ports free on every host
//	portmap -host host-a -all      # include kernel-assigned ephemeral ports
//	portmap -json                # the same data the API serves
//
// This file is deliberately thin. All the aggregation lives in
// internal/portmap so the API and web UI can reuse it rather than
// reimplementing the same collapsing and filtering rules.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/ficaa1/homelab-inventory/internal/portmap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "portmap:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir       = flag.String("dir", "inventories", "directory of inventory JSON files")
		hostFlag  = flag.String("host", "", "limit output to one host")
		portFlag  = flag.Int("port", 0, "show what owns this port on every host")
		freeFlag  = flag.Int("free", 0, "suggest this many free ports")
		rangeFlag = flag.String("range", "8000-8999", "range to suggest free ports from")
		showAll   = flag.Bool("all", false, "include kernel-assigned ephemeral ports")
		asJSON    = flag.Bool("json", false, "emit the aggregated port map as JSON")
	)
	flag.Parse()

	invs, err := portmap.LoadDir(*dir)
	if err != nil {
		return err
	}
	if len(invs) == 0 {
		return fmt.Errorf("no inventories found in %s (run 'make collect' first)", *dir)
	}

	pm := portmap.Build(invs)
	if *hostFlag != "" {
		pm = pm.FilterHost(*hostFlag)
		if len(pm.Hosts) == 0 {
			return fmt.Errorf("no inventory for host %q", *hostFlag)
		}
	}
	for _, w := range pm.Warnings {
		fmt.Fprintln(os.Stderr, "portmap:", w)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(pm)
	}

	out := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer out.Flush()

	switch {
	case *portFlag != 0:
		reportPort(out, pm, *portFlag)
	case *freeFlag > 0:
		lo, hi, err := parseRange(*rangeFlag)
		if err != nil {
			return err
		}
		return reportFree(out, pm, *freeFlag, lo, hi)
	default:
		reportAll(out, pm, *showAll)
	}
	return nil
}

func reportAll(w *tabwriter.Writer, pm *portmap.PortMap, showAll bool) {
	for _, h := range pm.Hosts {
		shown, hidden := h.Reserved()
		if showAll {
			shown, hidden = h.Ports, 0
		}

		fmt.Fprintf(w, "\n%s\t%s\n", strings.ToUpper(h.Name), h.OS)
		fmt.Fprintf(w, "%d reserved port(s)", len(shown))
		if hidden > 0 {
			fmt.Fprintf(w, ", %d ephemeral hidden (-all to show)", hidden)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, "PORT\tPROTO\tOWNER\tKIND\tBIND\tEXPOSED")

		for _, p := range shown {
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
				p.Port, p.Protocol, owner(p), p.Kind, binds(p), yesNo(p.Exposed))
		}
	}

	if len(pm.Shared) > 0 {
		fmt.Fprintf(w, "\nIN USE ON MORE THAN ONE HOST\n")
		for _, s := range pm.Shared {
			fmt.Fprintf(w, "%d\t%s\n", s.Port, strings.Join(s.Hosts, ", "))
		}
	}
}

func reportPort(w *tabwriter.Writer, pm *portmap.PortMap, port int) {
	matches := pm.Lookup(port)
	fmt.Fprintf(w, "PORT %d\n", port)
	if len(matches) == 0 {
		fmt.Fprintf(w, "free on all %d host(s): %s\n", len(pm.Hosts), strings.Join(pm.HostNames(), ", "))
		return
	}
	fmt.Fprintln(w, "HOST\tPROTO\tOWNER\tKIND\tBIND\tEXPOSED")
	for _, m := range matches {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			m.Host, m.Port.Protocol, owner(m.Port), m.Port.Kind, binds(m.Port), yesNo(m.Port.Exposed))
	}
}

func reportFree(w *tabwriter.Writer, pm *portmap.PortMap, want, lo, hi int) error {
	free := pm.Free(lo, hi, want)
	if len(free) == 0 {
		return fmt.Errorf("no free ports in %d-%d across %d host(s)", lo, hi, len(pm.Hosts))
	}
	fmt.Fprintf(w, "free on %s (range %d-%d)\n", strings.Join(pm.HostNames(), ", "), lo, hi)
	for _, p := range free {
		fmt.Fprintf(w, "%d\n", p)
	}
	return nil
}

// owner renders the owner for a terminal. The structured fields stay separate
// in the data; only the display joins them.
func owner(p portmap.Port) string {
	name := p.Owner
	if name == "" {
		name = p.Process
	}
	if name == "" {
		return "?"
	}
	if p.ComposeProject != "" {
		name = fmt.Sprintf("%s (%s/%s)", name, p.ComposeProject, p.ComposeService)
	}
	if p.ContainerPort != 0 {
		name = fmt.Sprintf("%s ->:%d", name, p.ContainerPort)
	}
	return name
}

// maxBindsShown caps the BIND column. tabwriter sizes a column to its widest
// cell, so one mDNS socket bound across fifty interfaces would otherwise push
// every other column off the screen.
const maxBindsShown = 3

func binds(p portmap.Port) string {
	shown, suffix := p.Addresses, ""
	if len(shown) > maxBindsShown {
		shown = shown[:maxBindsShown]
		suffix = fmt.Sprintf(" +%d", len(p.Addresses)-maxBindsShown)
	}
	s := strings.Join(shown, ",") + suffix
	// Multicast groups are bound per interface; the count is the interesting
	// part, not the list.
	switch n := len(p.Zones); {
	case n > 1:
		s += fmt.Sprintf(" (%d ifaces)", n)
	case n == 1:
		s += "%" + p.Zones[0]
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

func parseRange(s string) (lo, hi int, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("range %q is not lo-hi", s)
	}
	if lo, err = strconv.Atoi(strings.TrimSpace(a)); err != nil {
		return 0, 0, fmt.Errorf("range %q: %w", s, err)
	}
	if hi, err = strconv.Atoi(strings.TrimSpace(b)); err != nil {
		return 0, 0, fmt.Errorf("range %q: %w", s, err)
	}
	if lo < 1 || hi > 65535 || hi < lo {
		return 0, 0, fmt.Errorf("range %q is out of bounds", s)
	}
	return lo, hi, nil
}
