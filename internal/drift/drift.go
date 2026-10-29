// Package drift answers "what changed on this host?".
//
// Every run since the agents went live is stored, and until now nothing read
// two of them together — which was the stated reason for building ingest before
// any UI. A diff between runs is the one question none of the other tools on
// a snapshot alone can answer: a metrics tool knows a number over time, a monitor knows
// whether something responds, and neither knows that a container appeared, a
// port moved, or a unit that used to be enabled no longer is.
//
// # What counts as a change
//
// Only things a person would recognise as an event. A diff of the raw JSON
// would be technically complete and useless: uptime, load average and free
// memory change on every single run, so a naive comUTCon reports a hundred
// differences per hour and buries the one that matters. Each section here picks
// the fields where a change means somebody did something — or something broke.
//
// # Why absence is reported carefully
//
// A section that is nil means "did not look", not "found nothing" (see the
// schema's invariant). So a collector that failed on one run would otherwise
// look like every container on the host being removed at once. Sections are
// compared only when both runs actually looked, and a section that appeared or
// disappeared is reported as exactly that, rather than as its entire contents
// being added or removed.
package drift

import (
	"fmt"
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Kind is what happened to a thing.
type Kind string

const (
	KindAdded   Kind = "added"
	KindRemoved Kind = "removed"
	KindChanged Kind = "changed"
	KindSkipped Kind = "skipped" // a section one of the runs did not collect
)

// Change is one difference worth reporting.
type Change struct {
	// Section is the inventory section it came from: docker, network, systemd,
	// zfs, proxmox, host. It is what a UI groups on.
	Section string `json:"section"`
	Kind    Kind   `json:"kind"`
	// Subject names the thing — a container, a port, a unit, a pool.
	Subject string `json:"subject"`
	// Summary is the human sentence.
	Summary string `json:"summary"`
	// From and To are the old and new values, set only on a change.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Notable marks changes that are usually worth a second look: a new exposed
	// port, a unit that stopped being enabled, a pool that lost redundancy.
	Notable bool `json:"notable,omitempty"`
}

// Report is the whole diff.
type Report struct {
	Host string `json:"host"`
	From struct {
		ID          string    `json:"id"`
		CollectedAt time.Time `json:"collected_at"`
	} `json:"from"`
	To struct {
		ID          string    `json:"id"`
		CollectedAt time.Time `json:"collected_at"`
	} `json:"to"`
	Changes []Change `json:"changes"`
	// Counts is a per-section tally, so a UI can say "docker: 3" without
	// walking the list.
	Counts map[string]int `json:"counts"`
}

// Compare diffs two runs of one host, oldest first.
func Compare(from, to *inventory.Inventory, fromID, toID string) *Report {
	r := &Report{Host: to.Host.Hostname, Changes: []Change{}, Counts: map[string]int{}}
	r.From.ID, r.From.CollectedAt = fromID, from.CollectedAt
	r.To.ID, r.To.CollectedAt = toID, to.CollectedAt

	d := &differ{}
	d.host(from, to)
	d.docker(from, to)
	d.network(from, to)
	d.systemd(from, to)
	d.zfs(from, to)
	d.proxmox(from, to)

	r.Changes = d.out
	sort.SliceStable(r.Changes, func(i, j int) bool {
		a, b := r.Changes[i], r.Changes[j]
		if a.Notable != b.Notable {
			return a.Notable // the things worth a second look first
		}
		if a.Section != b.Section {
			return a.Section < b.Section
		}
		return a.Subject < b.Subject
	})
	for _, c := range r.Changes {
		r.Counts[c.Section]++
	}
	return r
}

type differ struct{ out []Change }

func (d *differ) add(c Change) { d.out = append(d.out, c) }

// section reports whether both runs looked, and records a note when only one
// did. A nil section means "did not look" — treating that as "everything was
// removed" would turn one failed collector into a page of phantom deletions.
func (d *differ) section(name string, fromLooked, toLooked bool) bool {
	switch {
	case fromLooked && toLooked:
		return true
	case !fromLooked && toLooked:
		d.add(Change{Section: name, Kind: KindSkipped, Subject: name,
			Summary: "the earlier run did not collect " + name + ", so there is nothing to compare"})
	case fromLooked && !toLooked:
		d.add(Change{Section: name, Kind: KindSkipped, Subject: name, Notable: true,
			Summary: "the later run did not collect " + name + " — the collector may have failed"})
	}
	return false
}

func (d *differ) host(from, to *inventory.Inventory) {
	if from.Host.Kernel != to.Host.Kernel {
		d.add(Change{Section: "host", Kind: KindChanged, Subject: "kernel", Notable: true,
			Summary: "kernel changed", From: from.Host.Kernel, To: to.Host.Kernel})
	}
	if from.Host.OS.Pretty != to.Host.OS.Pretty {
		d.add(Change{Section: "host", Kind: KindChanged, Subject: "os", Notable: true,
			Summary: "operating system changed", From: from.Host.OS.Pretty, To: to.Host.OS.Pretty})
	}
	// A reboot is the single most useful host-level event, and it is the one
	// thing uptime is good for — as a step change, not as a number.
	if !from.Host.BootTime.IsZero() && !to.Host.BootTime.IsZero() &&
		to.Host.BootTime.Sub(from.Host.BootTime) > 2*time.Minute {
		d.add(Change{Section: "host", Kind: KindChanged, Subject: "boot", Notable: true,
			Summary: "the host rebooted",
			From:    from.Host.BootTime.Format(time.RFC3339),
			To:      to.Host.BootTime.Format(time.RFC3339)})
	}

	fromFS := map[string]inventory.Filesystem{}
	for _, f := range from.Host.Filesystems {
		if f.Kind == inventory.FSKindReal || f.Kind == inventory.FSKindNetwork {
			fromFS[f.MountPoint] = f
		}
	}
	for _, f := range to.Host.Filesystems {
		if f.Kind != inventory.FSKindReal && f.Kind != inventory.FSKindNetwork {
			continue
		}
		old, ok := fromFS[f.MountPoint]
		if !ok {
			d.add(Change{Section: "host", Kind: KindAdded, Subject: f.MountPoint,
				Summary: "mounted " + f.MountPoint + " (" + f.FSType + ")"})
			continue
		}
		delete(fromFS, f.MountPoint)
		// Only a meaningful swing: a filesystem creeps by a fraction of a
		// percent between every run and nobody wants to read about it.
		if delta := f.UsedPct - old.UsedPct; delta >= 5 || delta <= -5 {
			d.add(Change{Section: "host", Kind: KindChanged, Subject: f.MountPoint,
				Summary: fmt.Sprintf("%s usage moved %+.0f points", f.MountPoint, delta),
				From:    fmt.Sprintf("%.0f%%", old.UsedPct),
				To:      fmt.Sprintf("%.0f%%", f.UsedPct),
				Notable: f.UsedPct >= 85,
			})
		}
	}
	for mount, f := range fromFS {
		d.add(Change{Section: "host", Kind: KindRemoved, Subject: mount, Notable: true,
			Summary: "unmounted " + mount + " (" + f.FSType + ")"})
	}
}

func (d *differ) docker(from, to *inventory.Inventory) {
	fromOK := from.Docker != nil && from.Docker.Available
	toOK := to.Docker != nil && to.Docker.Available
	if !d.section("docker", fromOK, toOK) {
		return
	}

	old := map[string]inventory.Container{}
	for _, c := range from.Docker.Containers {
		old[c.Name] = c
	}
	for _, c := range to.Docker.Containers {
		prev, ok := old[c.Name]
		if !ok {
			d.add(Change{Section: "docker", Kind: KindAdded, Subject: c.Name,
				Summary: "container " + c.Name + " appeared", To: c.Image})
			continue
		}
		delete(old, c.Name)
		if prev.Image != c.Image {
			// The single most useful line in the whole diff: this is what
			// Watchtower did to you at 04:00.
			d.add(Change{Section: "docker", Kind: KindChanged, Subject: c.Name, Notable: true,
				Summary: "container " + c.Name + " changed image", From: prev.Image, To: c.Image})
		}
		if prev.State != c.State {
			d.add(Change{Section: "docker", Kind: KindChanged, Subject: c.Name,
				Notable: c.State != "running",
				Summary: "container " + c.Name + " is now " + c.State,
				From:    prev.State, To: c.State})
		}
	}
	for name, c := range old {
		d.add(Change{Section: "docker", Kind: KindRemoved, Subject: name, Notable: true,
			Summary: "container " + name + " is gone", From: c.Image})
	}

	oldProj := map[string]bool{}
	for _, p := range from.Docker.ComposeProjects {
		oldProj[p.Name] = true
	}
	for _, p := range to.Docker.ComposeProjects {
		if !oldProj[p.Name] {
			d.add(Change{Section: "docker", Kind: KindAdded, Subject: p.Name,
				Summary: "compose project " + p.Name + " appeared"})
		}
		delete(oldProj, p.Name)
	}
	for name := range oldProj {
		d.add(Change{Section: "docker", Kind: KindRemoved, Subject: name, Notable: true,
			Summary: "compose project " + name + " is gone"})
	}
}

func (d *differ) network(from, to *inventory.Inventory) {
	// A failed scan is "not looked at", even from an old agent that stored an
	// empty list: otherwise every port reads as closed, then reopened.
	if !d.section("network", from.Network != nil && !from.NetworkFailed(), to.Network != nil && !to.NetworkFailed()) {
		return
	}
	ownersKnown := !from.Network.Unattributed && !to.Network.Unattributed

	// Ephemeral ports are excluded: the kernel hands out a different one on
	// every restart, so including them would make every diff a wall of noise
	// about rpcbind. This mirrors what portmap does for the same reason.
	key := func(l inventory.Listener) string { return fmt.Sprintf("%s/%d", l.Protocol, l.Port) }
	old := map[string]inventory.Listener{}
	for _, l := range from.Network.Listeners {
		if !l.Ephemeral {
			old[key(l)] = l
		}
	}
	seen := map[string]bool{}
	for _, l := range to.Network.Listeners {
		if l.Ephemeral || seen[key(l)] {
			continue
		}
		seen[key(l)] = true
		prev, ok := old[key(l)]
		if !ok {
			d.add(Change{Section: "network", Kind: KindAdded, Subject: key(l),
				// A new port reachable from off the machine is the one network
				// change that is always worth knowing about.
				Notable: l.Exposed,
				Summary: fmt.Sprintf("%s/%d opened by %s%s", l.Protocol, l.Port,
					ownerName(l), exposedSuffix(l.Exposed))})
			continue
		}
		delete(old, key(l))
		if ownersKnown && ownerName(prev) != ownerName(l) {
			d.add(Change{Section: "network", Kind: KindChanged, Subject: key(l),
				Summary: fmt.Sprintf("%s/%d changed hands", l.Protocol, l.Port),
				From:    ownerName(prev), To: ownerName(l)})
		}
		if prev.Exposed != l.Exposed {
			d.add(Change{Section: "network", Kind: KindChanged, Subject: key(l), Notable: l.Exposed,
				Summary: fmt.Sprintf("%s/%d is now %s", l.Protocol, l.Port,
					map[bool]string{true: "exposed to the network", false: "loopback only"}[l.Exposed]),
				From: boolWord(prev.Exposed), To: boolWord(l.Exposed)})
		}
	}
	for k, l := range old {
		d.add(Change{Section: "network", Kind: KindRemoved, Subject: k,
			Summary: k + " closed", From: ownerName(l)})
	}
}

func ownerName(l inventory.Listener) string {
	if l.Owner != nil && l.Owner.Name != "" {
		return l.Owner.Name
	}
	if l.Process != "" {
		return l.Process
	}
	return "unattributed"
}

func exposedSuffix(exposed bool) string {
	if exposed {
		return ", exposed"
	}
	return ""
}

func boolWord(b bool) string {
	if b {
		return "exposed"
	}
	return "loopback"
}

func (d *differ) systemd(from, to *inventory.Inventory) {
	fromOK := from.Systemd != nil && from.Systemd.Available
	toOK := to.Systemd != nil && to.Systemd.Available
	if !d.section("systemd", fromOK, toOK) {
		return
	}

	old := map[string]inventory.Unit{}
	for _, u := range from.Systemd.Units {
		old[u.Name] = u
	}
	for _, u := range to.Systemd.Units {
		prev, ok := old[u.Name]
		if !ok {
			d.add(Change{Section: "systemd", Kind: KindAdded, Subject: u.Name,
				Summary: "unit " + u.Name + " appeared", To: u.Active})
			continue
		}
		delete(old, u.Name)
		// Only failure transitions and enablement changes. Active/inactive
		// churn on oneshot units would otherwise dominate the diff.
		wasFailed, isFailed := prev.Active == "failed", u.Active == "failed"
		if !wasFailed && isFailed {
			d.add(Change{Section: "systemd", Kind: KindChanged, Subject: u.Name, Notable: true,
				Summary: "unit " + u.Name + " started failing", From: prev.Active, To: u.Active})
		}
		if wasFailed && !isFailed {
			d.add(Change{Section: "systemd", Kind: KindChanged, Subject: u.Name,
				Summary: "unit " + u.Name + " recovered", From: prev.Active, To: u.Active})
		}
		if prev.UnitFileState != u.UnitFileState && prev.UnitFileState != "" && u.UnitFileState != "" {
			d.add(Change{Section: "systemd", Kind: KindChanged, Subject: u.Name,
				Notable: u.UnitFileState == "disabled" || u.UnitFileState == "masked",
				Summary: "unit " + u.Name + " is now " + u.UnitFileState,
				From:    prev.UnitFileState, To: u.UnitFileState})
		}
	}
	for name, u := range old {
		d.add(Change{Section: "systemd", Kind: KindRemoved, Subject: name,
			Notable: u.Active == "active",
			Summary: "unit " + name + " is gone"})
	}
}

func (d *differ) zfs(from, to *inventory.Inventory) {
	fromOK := from.ZFS != nil && from.ZFS.Available
	toOK := to.ZFS != nil && to.ZFS.Available
	if !d.section("zfs", fromOK, toOK) {
		return
	}

	old := map[string]inventory.ZPool{}
	for _, p := range from.ZFS.Pools {
		old[p.Name] = p
	}
	for _, p := range to.ZFS.Pools {
		prev, ok := old[p.Name]
		if !ok {
			d.add(Change{Section: "zfs", Kind: KindAdded, Subject: p.Name,
				Summary: "pool " + p.Name + " appeared", To: p.Health})
			continue
		}
		delete(old, p.Name)
		if prev.Health != p.Health {
			d.add(Change{Section: "zfs", Kind: KindChanged, Subject: p.Name,
				Notable: p.Health != "ONLINE",
				Summary: "pool " + p.Name + " is now " + p.Health, From: prev.Health, To: p.Health})
		}
		if prev.Scan != p.Scan {
			d.add(Change{Section: "zfs", Kind: KindChanged, Subject: p.Name,
				Summary: "pool " + p.Name + " scrub state changed", From: prev.Scan, To: p.Scan})
		}
		if delta := p.CapacityPct - prev.CapacityPct; delta >= 5 || delta <= -5 {
			d.add(Change{Section: "zfs", Kind: KindChanged, Subject: p.Name,
				Notable: p.CapacityPct >= 80,
				Summary: fmt.Sprintf("pool %s capacity moved %+d points", p.Name, delta),
				From:    fmt.Sprintf("%d%%", prev.CapacityPct), To: fmt.Sprintf("%d%%", p.CapacityPct)})
		}
	}
	for name := range old {
		d.add(Change{Section: "zfs", Kind: KindRemoved, Subject: name, Notable: true,
			Summary: "pool " + name + " is gone"})
	}
}

func (d *differ) proxmox(from, to *inventory.Inventory) {
	fromOK := from.Proxmox != nil && from.Proxmox.Available
	toOK := to.Proxmox != nil && to.Proxmox.Available
	if !d.section("proxmox", fromOK, toOK) {
		return
	}

	old := map[int]inventory.Guest{}
	for _, g := range from.Proxmox.Guests {
		old[g.VMID] = g
	}
	for _, g := range to.Proxmox.Guests {
		prev, ok := old[g.VMID]
		if !ok {
			d.add(Change{Section: "proxmox", Kind: KindAdded, Subject: guestName(g),
				Summary: fmt.Sprintf("guest %s appeared", guestName(g)), To: g.Status})
			continue
		}
		delete(old, g.VMID)
		if prev.Status != g.Status {
			d.add(Change{Section: "proxmox", Kind: KindChanged, Subject: guestName(g),
				Notable: g.Status != "running",
				Summary: fmt.Sprintf("guest %s is now %s", guestName(g), g.Status),
				From:    prev.Status, To: g.Status})
		}
		if prev.OnBoot != g.OnBoot {
			d.add(Change{Section: "proxmox", Kind: KindChanged, Subject: guestName(g), Notable: !g.OnBoot,
				Summary: fmt.Sprintf("guest %s autostart %s", guestName(g),
					map[bool]string{true: "enabled", false: "disabled"}[g.OnBoot]),
				From: boolOnOff(prev.OnBoot), To: boolOnOff(g.OnBoot)})
		}
	}
	for _, g := range old {
		d.add(Change{Section: "proxmox", Kind: KindRemoved, Subject: guestName(g), Notable: true,
			Summary: fmt.Sprintf("guest %s is gone", guestName(g))})
	}

	oldStore := map[string]inventory.Storage{}
	for _, st := range from.Proxmox.Storages {
		oldStore[st.Name] = st
	}
	for _, st := range to.Proxmox.Storages {
		prev, ok := oldStore[st.Name]
		if !ok || st.TotalBytes == 0 || prev.TotalBytes == 0 {
			continue
		}
		was := 100 * float64(prev.UsedBytes) / float64(prev.TotalBytes)
		is := 100 * float64(st.UsedBytes) / float64(st.TotalBytes)
		if delta := is - was; delta >= 5 || delta <= -5 {
			d.add(Change{Section: "proxmox", Kind: KindChanged, Subject: st.Name,
				// A thin pool is the sharp edge: at 100% writes fail and guests
				// go read-only, so its movement is always worth a look.
				Notable: st.Type == "lvmthin" && is >= 75,
				Summary: fmt.Sprintf("storage %s usage moved %+.0f points", st.Name, delta),
				From:    fmt.Sprintf("%.0f%%", was), To: fmt.Sprintf("%.0f%%", is)})
		}
	}
}

func guestName(g inventory.Guest) string {
	if g.Name != "" {
		return fmt.Sprintf("%d (%s)", g.VMID, g.Name)
	}
	return fmt.Sprint(g.VMID)
}

func boolOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
