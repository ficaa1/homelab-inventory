package drift

import (
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func base(host string) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   time.Now(),
		Host:          inventory.Host{Hostname: host},
	}
}

func compare(from, to *inventory.Inventory) *Report {
	return Compare(from, to, "a", "b")
}

func has(r *Report, substr string) bool {
	for _, c := range r.Changes {
		if strings.Contains(c.Summary, substr) {
			return true
		}
	}
	return false
}

func get(r *Report, substr string) (Change, bool) {
	for _, c := range r.Changes {
		if strings.Contains(c.Summary, substr) {
			return c, true
		}
	}
	return Change{}, false
}

// The thing this exists for. Watchtower updates a container overnight and the
// only record that it happened is two stored runs.
func TestContainerImageChangeIsReported(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "immich_server", Image: "ghcr.io/immich-app/immich-server:v1.118", State: "running"},
		{Name: "plex", Image: "plexinc/pms-docker:1.40", State: "running"},
	}}
	to.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "immich_server", Image: "ghcr.io/immich-app/immich-server:v1.119", State: "running"},
		{Name: "plex", Image: "plexinc/pms-docker:1.40", State: "running"},
		{Name: "newthing", Image: "some/image:1", State: "running"},
	}}

	r := compare(from, to)
	c, ok := get(r, "immich_server changed image")
	if !ok {
		t.Fatalf("no image change reported: %+v", r.Changes)
	}
	if !c.Notable {
		t.Error("an image change should be notable — it is what an auto-updater did to you")
	}
	if !strings.Contains(c.From, "v1.118") || !strings.Contains(c.To, "v1.119") {
		t.Errorf("from/to = %q -> %q", c.From, c.To)
	}
	if !has(r, "newthing appeared") {
		t.Error("a new container was not reported")
	}
	// The unchanged container must produce nothing at all.
	if has(r, "plex changed") {
		t.Error("an unchanged container produced a change")
	}
}

// The single most important correctness property: a nil section means "did not
// look". Treating it as "everything was removed" would turn one failed
// collector into a page of phantom deletions.
func TestFailedCollectorIsNotReportedAsMassDeletion(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "a", Image: "i", State: "running"},
		{Name: "b", Image: "i", State: "running"},
		{Name: "c", Image: "i", State: "running"},
	}}
	to.Docker = nil // the collector did not run

	r := compare(from, to)
	for _, c := range r.Changes {
		if c.Kind == KindRemoved {
			t.Fatalf("a skipped section produced a removal: %+v", c)
		}
	}
	c, ok := get(r, "did not collect docker")
	if !ok {
		t.Fatalf("the skipped section was not reported at all: %+v", r.Changes)
	}
	if c.Kind != KindSkipped || !c.Notable {
		t.Errorf("a section that stopped being collected should be notable: %+v", c)
	}

	// Available:false is the same situation — the section exists but says it
	// could not look.
	to.Docker = &inventory.Docker{Available: false}
	for _, c := range compare(from, to).Changes {
		if c.Kind == KindRemoved {
			t.Fatalf("an unavailable section produced a removal: %+v", c)
		}
	}
}

// The kernel hands out a different ephemeral port on every restart. Including
// them would make every diff a wall of noise about rpcbind.
func TestEphemeralPortsAreIgnored(t *testing.T) {
	t.Parallel()
	from, to := base("host-c"), base("host-c")
	from.Network = &inventory.Network{Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 22, Process: "sshd"},
		{Protocol: "tcp", Port: 45123, Process: "rpc.statd", Ephemeral: true},
	}}
	to.Network = &inventory.Network{Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 22, Process: "sshd"},
		{Protocol: "tcp", Port: 51999, Process: "rpc.statd", Ephemeral: true},
	}}

	r := compare(from, to)
	if len(r.Changes) != 0 {
		t.Errorf("ephemeral churn produced %d changes: %+v", len(r.Changes), r.Changes)
	}
}

// A port that becomes reachable from off the machine is the network change that
// always matters.
func TestNewlyExposedPortIsNotable(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Network = &inventory.Network{Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 8096, Address: "127.0.0.1", Process: "jellyfin"},
	}}
	to.Network = &inventory.Network{Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 8096, Address: "0.0.0.0", Exposed: true, Process: "jellyfin"},
		{Protocol: "tcp", Port: 9000, Address: "0.0.0.0", Exposed: true, Process: "newthing"},
	}}

	r := compare(from, to)
	c, ok := get(r, "tcp/8096 is now exposed")
	if !ok {
		t.Fatalf("a port becoming exposed was not reported: %+v", r.Changes)
	}
	if !c.Notable {
		t.Error("a port becoming exposed should be notable")
	}
	if c2, ok := get(r, "tcp/9000 opened"); !ok || !c2.Notable {
		t.Errorf("a new exposed port should be reported and notable: %+v", c2)
	}
}

// Only failure transitions and enablement changes: active/inactive churn on
// oneshot units would otherwise dominate every diff.
func TestSystemdReportsFailuresAndEnablementNotChurn(t *testing.T) {
	t.Parallel()
	from, to := base("host-b"), base("host-b")
	from.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{
		{Name: "a.service", Active: "active", UnitFileState: "enabled"},
		{Name: "b.service", Active: "failed", UnitFileState: "enabled"},
		{Name: "oneshot.service", Active: "inactive", UnitFileState: "enabled"},
	}}
	to.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{
		{Name: "a.service", Active: "failed", UnitFileState: "enabled"},
		{Name: "b.service", Active: "active", UnitFileState: "enabled"},
		{Name: "oneshot.service", Active: "active", UnitFileState: "disabled"},
	}}

	r := compare(from, to)
	if c, ok := get(r, "a.service started failing"); !ok || !c.Notable {
		t.Errorf("a unit starting to fail should be notable: %+v", c)
	}
	if !has(r, "b.service recovered") {
		t.Error("a recovery was not reported")
	}
	if c, ok := get(r, "oneshot.service is now disabled"); !ok || !c.Notable {
		t.Errorf("a unit being disabled should be notable: %+v", c)
	}
	// The oneshot going active is churn and must not appear on its own.
	if has(r, "oneshot.service started failing") {
		t.Error("churn was reported as a failure")
	}
}

// Small movements are noise; a real swing is an event.
func TestOnlyMeaningfulCapacitySwingsAreReported(t *testing.T) {
	t.Parallel()
	from, to := base("host-c"), base("host-c")
	from.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{
		{Name: "media", Health: "ONLINE", CapacityPct: 76},
		{Name: "secure", Health: "ONLINE", CapacityPct: 55},
	}}
	to.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{
		{Name: "media", Health: "ONLINE", CapacityPct: 78},    // noise
		{Name: "secure", Health: "DEGRADED", CapacityPct: 68}, // both real
	}}

	r := compare(from, to)
	if has(r, "media capacity") {
		t.Error("a 2-point drift was reported; that is every run")
	}
	if c, ok := get(r, "secure capacity moved +13"); !ok {
		t.Errorf("a 13-point swing was not reported: %+v", r.Changes)
	} else if c.From != "55%" || c.To != "68%" {
		t.Errorf("from/to = %q -> %q", c.From, c.To)
	}
	if c, ok := get(r, "secure is now DEGRADED"); !ok || !c.Notable {
		t.Errorf("a pool leaving ONLINE should be notable: %+v", c)
	}
}

func TestProxmoxGuestChanges(t *testing.T) {
	t.Parallel()
	from, to := base("host-b"), base("host-b")
	from.Proxmox = &inventory.Proxmox{Available: true, Guests: []inventory.Guest{
		{VMID: 101, Name: "host-f", Status: "running", OnBoot: true},
		{VMID: 105, Name: "node3", Status: "running", OnBoot: true},
	}}
	to.Proxmox = &inventory.Proxmox{Available: true, Guests: []inventory.Guest{
		{VMID: 101, Name: "host-f", Status: "stopped", OnBoot: true},
		{VMID: 105, Name: "node3", Status: "running", OnBoot: false},
	}}

	r := compare(from, to)
	if c, ok := get(r, "101 (host-f) is now stopped"); !ok || !c.Notable {
		t.Errorf("a guest stopping should be notable: %+v", c)
	}
	if c, ok := get(r, "105 (node3) autostart disabled"); !ok || !c.Notable {
		t.Errorf("autostart being turned off should be notable: %+v", c)
	}
}

// Two identical runs must produce absolutely nothing. If this ever fails, some
// field that changes on every collection has crept into the comUTCon and the
// whole feature becomes noise.
func TestIdenticalRunsProduceNoChanges(t *testing.T) {
	t.Parallel()
	inv := base("host-a")
	inv.Host.Kernel = "5.15.0-71-generic"
	inv.Host.BootTime = time.Now().Add(-30 * 24 * time.Hour)
	inv.Host.Filesystems = []inventory.Filesystem{
		{MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal, UsedPct: 65},
	}
	inv.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "plex", Image: "i", State: "running"},
	}}
	inv.Network = &inventory.Network{Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 32400, Process: "plex"},
	}}
	inv.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{
		{Name: "a.service", Active: "active", UnitFileState: "enabled"},
	}}

	r := compare(inv, inv)
	if len(r.Changes) != 0 {
		t.Errorf("comparing a run with itself produced %d changes: %+v", len(r.Changes), r.Changes)
	}
}

// A reboot is the host event worth surfacing, and uptime is only good for it as
// a step change rather than as a number.
func TestRebootIsReportedButClockDriftIsNot(t *testing.T) {
	t.Parallel()
	boot := time.Now().Add(-10 * 24 * time.Hour)
	from, to := base("host-b"), base("host-b")
	from.Host.BootTime = boot
	to.Host.BootTime = boot.Add(30 * time.Second) // sub-minute jitter, not a reboot
	if has(compare(from, to), "rebooted") {
		t.Error("clock jitter was reported as a reboot")
	}

	to.Host.BootTime = time.Now()
	if c, ok := get(compare(from, to), "rebooted"); !ok || !c.Notable {
		t.Errorf("a real reboot was not reported: %+v", c)
	}
}

func TestNotableChangesSortFirst(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "zzz", Image: "old", State: "running"},
	}}
	to.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "zzz", Image: "new", State: "running"},
		{Name: "aaa", Image: "i", State: "running"},
	}}
	r := compare(from, to)
	if len(r.Changes) < 2 {
		t.Fatalf("expected two changes, got %+v", r.Changes)
	}
	if !r.Changes[0].Notable {
		t.Errorf("the notable change did not sort first: %+v", r.Changes)
	}
	if r.Counts["docker"] != 2 {
		t.Errorf("counts = %+v", r.Counts)
	}
}

// An old agent's failed scan stored an empty list plus an error diagnostic. It
// must read as "not collected", not as every port closing.
func TestFailedNetworkScanIsNotMassClosure(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Network = &inventory.Network{Listeners: []inventory.Listener{{Protocol: "tcp", Port: 22, Process: "sshd"}}}
	to.Network = &inventory.Network{Listeners: []inventory.Listener{}}
	to.Diagnostics = []inventory.Diagnostic{{Collector: "network", Severity: inventory.SeverityError, Message: "collector failed"}}
	r := compare(from, to)
	if has(r, "closed") || !has(r, "did not collect network") {
		t.Fatalf("failed scan diffed as port changes: %+v", r.Changes)
	}
}

// A fallback listing without owners must not report every port changing hands.
func TestUnattributedListingHidesOwnerChanges(t *testing.T) {
	t.Parallel()
	from, to := base("host-a"), base("host-a")
	from.Network = &inventory.Network{Listeners: []inventory.Listener{{Protocol: "tcp", Port: 22, Process: "sshd"}}}
	to.Network = &inventory.Network{Unattributed: true, Listeners: []inventory.Listener{
		{Protocol: "tcp", Port: 22},
		{Protocol: "tcp", Port: 9000},
	}}
	r := compare(from, to)
	if has(r, "changed hands") {
		t.Fatalf("unknown owners reported as changes: %+v", r.Changes)
	}
	if !has(r, "tcp/9000 opened") {
		t.Fatalf("new port missed in a fallback listing: %+v", r.Changes)
	}
}

// Daily package history lives separately from the 15-minute run diff.
func TestDriftIgnoresSoftware(t *testing.T) {
	from, to := base("host-a"), base("host-a")
	to.Software = &inventory.Software{Host: inventory.PackageSet{Status: "ok"}}
	for _, c := range compare(from, to).Changes {
		if c.Section == "packages" {
			t.Fatalf("run drift includes software: %+v", c)
		}
	}
}
