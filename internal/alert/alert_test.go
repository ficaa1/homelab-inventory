package alert

import (
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// now is a fixed clock so age-based rules are deterministic. It is set just
// after the example scrub timestamp pinned below.
var now = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

func find(t *testing.T, alerts []Alert, rule, subject string) Alert {
	t.Helper()
	for _, a := range alerts {
		if a.Rule == rule && a.Subject == subject {
			return a
		}
	}
	t.Fatalf("no %s alert for %q in %v", rule, subject, rules(alerts))
	return Alert{}
}

func absent(t *testing.T, alerts []Alert, rule, subject string) {
	t.Helper()
	for _, a := range alerts {
		if a.Rule == rule && a.Subject == subject {
			t.Fatalf("unexpected %s alert for %q: %s", rule, subject, a.Message)
		}
	}
}

func rules(alerts []Alert) []string {
	out := make([]string, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, a.Rule+"/"+a.Subject)
	}
	return out
}

func evalOne(inv *inventory.Inventory) []Alert {
	return Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), now).Alerts
}

// base returns an inventory that is fresh and boring, so each test only has to
// introduce the one thing it is about.
func base() *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   now.Add(-2 * time.Minute),
		Host:          inventory.Host{Hostname: "host-c"},
		Tool:          inventory.Tool{Revision: inventory.AgentRevision},
	}
}

// ---------------------------------------------------------------- scan parsing

// Exercise completed, cancelled and incomplete scan output.
func TestParseScanExamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		scan      string
		wantState scanState
		wantTime  time.Time
		wantOK    bool
	}{
		{
			name:      "completed scrub, as boot-pool reports it",
			scan:      "scrub repaired 0B in 00:00:23 with 0 errors on Fri Jul 24 03:45:24 2026",
			wantState: scanCompleted,
			wantTime:  time.Date(2026, 7, 24, 3, 45, 24, 0, time.UTC),
			wantOK:    true,
		},
		{
			name:      "cancelled scrub",
			scan:      "scrub canceled on Wed Jan  1 17:06:06 2025",
			wantState: scanCanceled,
			wantTime:  time.Date(2025, 1, 1, 17, 6, 6, 0, time.UTC),
			wantOK:    true,
		},
		{
			name:      "never scrubbed",
			scan:      "none requested",
			wantState: scanNone,
			wantOK:    false,
		},
		{
			name:      "scrub running now",
			scan:      "scrub in progress since Sat Jul 25 03:00:00 2026",
			wantState: scanInProgress,
			wantTime:  time.Date(2026, 7, 25, 3, 0, 0, 0, time.UTC),
			wantOK:    false, // "since", not " on " — no trailing timestamp to find
		},
		{
			name:      "space-padded single-digit day",
			scan:      "scrub repaired 0B in 00:01:00 with 0 errors on Thu Jul  3 01:02:03 2025",
			wantState: scanCompleted,
			wantTime:  time.Date(2025, 7, 3, 1, 2, 3, 0, time.UTC),
			wantOK:    true,
		},
		{
			name:      "empty",
			scan:      "",
			wantState: scanUnknown,
			wantOK:    false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state, at, ok := parseScan(tc.scan)
			if state != tc.wantState {
				t.Errorf("state = %v, want %v", state, tc.wantState)
			}
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && !at.Equal(tc.wantTime) {
				t.Errorf("time = %v, want %v", at, tc.wantTime)
			}
		})
	}
}

// ---------------------------------------------------------------- zfs

// A cancelled scrub should be flagged while a recent completed scrub is not.
func TestCancelledScrubIsFlaggedWhileHealthyPoolIsNot(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.ZFS = &inventory.ZFS{
		Available: true,
		Pools: []inventory.ZPool{
			{
				Name: "boot-pool", Health: "ONLINE", CapacityPct: 21,
				Scan:   "scrub repaired 0B in 00:00:23 with 0 errors on Fri Jul 24 03:45:24 2026",
				Errors: "No known data errors",
			},
			{
				Name: "media", Health: "ONLINE", CapacityPct: 76,
				Scan:   "scrub canceled on Wed Jan  1 17:06:06 2025",
				Errors: "No known data errors",
			},
		},
	}

	alerts := evalOne(inv)

	// boot-pool scrubbed hours ago and is 21% full: nothing to say about it.
	absent(t, alerts, "zfs.pool.scrub", "boot-pool")
	absent(t, alerts, "zfs.pool.capacity", "boot-pool")

	a := find(t, alerts, "zfs.pool.scrub", "media")
	if a.Level != LevelWarning {
		t.Errorf("level = %v, want warning", a.Level)
	}
	if a.Since == nil || a.Since.Year() != 2025 {
		t.Errorf("Since = %v, want the 2025 cancellation date", a.Since)
	}
	// Over a year stale; the message should say so rather than just "old".
	if a.Value < 365*24 {
		t.Errorf("Value = %v hours, expected more than a year", a.Value)
	}
}

func TestPoolHealthAndErrorsAreCritical(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.ZFS = &inventory.ZFS{
		Available: true,
		Pools: []inventory.ZPool{{
			Name: "media", Health: "DEGRADED", CapacityPct: 10,
			Errors: "1 data errors, use '-v' for a list",
			Vdevs: []inventory.Vdev{
				{Name: "sda", State: "ONLINE"},
				{Name: "sdb", State: "FAULTED", Read: 0, Write: 3, Cksum: 12},
			},
		}},
	}

	alerts := evalOne(inv)
	for _, rule := range []struct{ r, s string }{
		{"zfs.pool.health", "media"},
		{"zfs.pool.errors", "media"},
		{"zfs.vdev.errors", "media/sdb"},
	} {
		if a := find(t, alerts, rule.r, rule.s); a.Level != LevelCritical {
			t.Errorf("%s: level = %v, want critical", rule.r, a.Level)
		}
	}
	absent(t, alerts, "zfs.vdev.errors", "media/sda")
}

func TestPoolCapacityEscalates(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.ZFS = &inventory.ZFS{
		Available: true,
		Pools: []inventory.ZPool{
			{Name: "ok", Health: "ONLINE", CapacityPct: 55, Errors: "No known data errors"},
			{Name: "warn", Health: "ONLINE", CapacityPct: 82, Errors: "No known data errors"},
			{Name: "crit", Health: "ONLINE", CapacityPct: 94, Errors: "No known data errors"},
		},
	}

	alerts := evalOne(inv)
	absent(t, alerts, "zfs.pool.capacity", "ok")
	if a := find(t, alerts, "zfs.pool.capacity", "warn"); a.Level != LevelWarning {
		t.Errorf("82%%: level = %v, want warning", a.Level)
	}
	if a := find(t, alerts, "zfs.pool.capacity", "crit"); a.Level != LevelCritical {
		t.Errorf("94%%: level = %v, want critical", a.Level)
	}
}

// ---------------------------------------------------------------- proxmox

// The real numbers from host-b: local-lvm is an lvmthin pool at 96%, which is the
// dangerous one, while nvme is plain lvm at 97%, which is just a fully
// allocated volume group. Treating those two the same would either cry wolf
// about nvme or under-report local-lvm.
func TestThinPoolIsCriticalButFullVolumeGroupIsNot(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages: []inventory.Storage{
			{Name: "local-lvm", Type: "lvmthin", Active: true, UsedBytes: 51102823638, TotalBytes: 52978253824},
			{Name: "nvme", Type: "lvm", Active: true, UsedBytes: 998579896320, TotalBytes: 1024207093760},
			{Name: "local", Type: "dir", Active: true, UsedBytes: 25482006528, TotalBytes: 39524352000},
		},
	}

	alerts := evalOne(inv)

	thin := find(t, alerts, "proxmox.storage.thinpool", "local-lvm")
	if thin.Level != LevelCritical {
		t.Errorf("local-lvm at 96%%: level = %v, want critical", thin.Level)
	}
	if thin.Value < 95 || thin.Value > 97 {
		t.Errorf("local-lvm Value = %v, want ~96", thin.Value)
	}

	// A 97% volume group is normal: allocation, not consumption.
	vg := find(t, alerts, "proxmox.storage.allocation", "nvme")
	if vg.Level != LevelInfo {
		t.Errorf("nvme at 97%%: level = %v, want info", vg.Level)
	}

	// 64% on a dir storage is unremarkable.
	absent(t, alerts, "proxmox.storage.capacity", "local")
}

func TestRunningGuestWithoutAutostart(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Guests: []inventory.Guest{
			{VMID: 101, Name: "host-f", Type: inventory.GuestLXC, Status: "running", OnBoot: true},
			{VMID: 110, Name: "kali", Type: inventory.GuestQEMU, Status: "running", OnBoot: false},
			// Stopped and not set to autostart is a choice, not a landmine.
			{VMID: 120, Name: "archived", Type: inventory.GuestQEMU, Status: "stopped", OnBoot: false},
		},
	}

	alerts := evalOne(inv)
	find(t, alerts, "proxmox.guest.autostart", "110/kali")
	absent(t, alerts, "proxmox.guest.autostart", "101/host-f")
	absent(t, alerts, "proxmox.guest.autostart", "120/archived")
}

// ---------------------------------------------------------------- units, containers

func TestFailedUnitsAndUnhealthyContainers(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-a"
	inv.Systemd = &inventory.Systemd{
		Available: true,
		Units: []inventory.Unit{
			{Name: "ssh.service", Active: "active", Sub: "running"},
			{Name: "zfs-import-scan.service", Active: "failed", Sub: "failed"},
		},
	}
	inv.Docker = &inventory.Docker{
		Available: true,
		Containers: []inventory.Container{
			{Name: "plex", State: "running"},
			{Name: "immich_server", State: "restarting", Status: "Restarting (1) 8 seconds ago"},
			// A container that ran once and stopped is not a problem.
			{Name: "backup-oneshot", State: "exited", Status: "Exited (0) 2 hours ago"},
		},
	}

	alerts := evalOne(inv)
	find(t, alerts, "systemd.unit.failed", "zfs-import-scan.service")
	absent(t, alerts, "systemd.unit.failed", "ssh.service")

	c := find(t, alerts, "docker.container.unhealthy", "immich_server")
	if c.Detail != "Restarting (1) 8 seconds ago" {
		t.Errorf("Detail = %q, want the docker status line", c.Detail)
	}
	absent(t, alerts, "docker.container.unhealthy", "backup-oneshot")
	absent(t, alerts, "docker.container.unhealthy", "plex")
}

// ---------------------------------------------------------------- filesystems

// The documented trap: ZFS datasets share their pool's free space, so a
// per-dataset statfs reports the same pool many times over. zfs.pools is the
// only source that gets capacity right, so filesystem rules must skip zfs.
func TestFilesystemRuleSkipsZFSAndNoise(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Filesystems = []inventory.Filesystem{
		{MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal, TotalBytes: 100, UsedBytes: 90, UsedPct: 90},
		{MountPoint: "/mnt/tank", FSType: "zfs", Kind: inventory.FSKindReal, TotalBytes: 100, UsedBytes: 99, UsedPct: 99},
		{MountPoint: "/snap/x", FSType: "squashfs", Kind: inventory.FSKindSnap, TotalBytes: 100, UsedBytes: 100, UsedPct: 100},
		{MountPoint: "/var/lib/docker/overlay2/abc", FSType: "overlay", Kind: inventory.FSKindContainer, TotalBytes: 100, UsedBytes: 100, UsedPct: 100},
	}

	alerts := evalOne(inv)
	find(t, alerts, "host.filesystem.capacity", "/")
	absent(t, alerts, "host.filesystem.capacity", "/mnt/tank")
	absent(t, alerts, "host.filesystem.capacity", "/snap/x")
	absent(t, alerts, "host.filesystem.capacity", "/var/lib/docker/overlay2/abc")
}

// ---------------------------------------------------------------- staleness

// Push-based ingest fails silently: a host whose timer dies simply stops
// appearing in fresh data. Silence has to be the alert.
func TestStalenessEscalates(t *testing.T) {
	t.Parallel()

	fresh := base()
	absent(t, evalOne(fresh), "host.stale", "host-c")

	stale := base()
	stale.CollectedAt = now.Add(-90 * time.Minute)
	if a := find(t, evalOne(stale), "host.stale", "host-c"); a.Level != LevelWarning {
		t.Errorf("90m: level = %v, want warning", a.Level)
	}

	veryStale := base()
	veryStale.CollectedAt = now.Add(-6 * time.Hour)
	if a := find(t, evalOne(veryStale), "host.stale", "host-c"); a.Level != LevelCritical {
		t.Errorf("6h: level = %v, want critical", a.Level)
	}
}

// StaleAfter of 0 turns the rule off, which is what cmd/alerts does when
// reading a directory of hand-collected files: there, every host is "stale"
// as an artefact of how the snapshot was taken, and eight such alerts would
// bury the real findings.
func TestStalenessCanBeDisabled(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.CollectedAt = now.Add(-30 * 24 * time.Hour) // a month old

	th := DefaultThresholds()
	th.StaleAfter, th.StaleCritAfter = 0, 0

	rep := Evaluate([]*inventory.Inventory{inv}, th, now)
	if len(rep.Alerts) != 0 {
		t.Errorf("StaleAfter=0 still produced %v", rules(rep.Alerts))
	}
}

// ---------------------------------------------------------------- report shape

func TestReportSortsWorstFirstAndCounts(t *testing.T) {
	t.Parallel()
	a := base()
	a.Host.Hostname = "alpha"
	a.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{
		{Name: "x.service", Active: "failed"},
	}}

	b := base()
	b.Host.Hostname = "bravo"
	b.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{
		{Name: "tank", Health: "DEGRADED", Errors: "No known data errors"},
	}}

	// The pool in this fixture has no snapshots, which a later rule correctly
	// reports. This test is about ordering and counting, so it asserts on the
	// two rules it set up rather than on the total — otherwise every new rule
	// breaks it for the wrong reason.
	rep := Evaluate([]*inventory.Inventory{a, b}, DefaultThresholds(), now)

	if !hasRule(rep.Alerts, "zfs.pool.health") || !hasRule(rep.Alerts, "systemd.unit.failed") {
		t.Fatalf("expected both seeded rules, got %v", rules(rep.Alerts))
	}
	if rep.Alerts[0].Level != LevelCritical {
		t.Errorf("first alert = %v, want the critical one first", rep.Alerts[0].Level)
	}
	if rep.Counts.Critical != 1 {
		t.Errorf("counts = %+v, want exactly one critical", rep.Counts)
	}
	if rep.Counts.Total != len(rep.Alerts) {
		t.Errorf("Total = %d but there are %d alerts", rep.Counts.Total, len(rep.Alerts))
	}
	if len(rep.Hosts) != 2 || rep.Hosts[0] != "alpha" {
		t.Errorf("hosts = %v, want sorted [alpha bravo]", rep.Hosts)
	}
}

func TestFilters(t *testing.T) {
	t.Parallel()
	a := base()
	a.Host.Hostname = "alpha"
	a.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{{Name: "x.service", Active: "failed"}}}

	b := base()
	b.Host.Hostname = "bravo"
	b.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{
		{Name: "tank", Health: "DEGRADED", Errors: "No known data errors"},
	}}

	rep := Evaluate([]*inventory.Inventory{a, b}, DefaultThresholds(), now)
	before := len(rep.Alerts)

	byHost := rep.FilterHost("alpha")
	for _, x := range byHost.Alerts {
		if x.Host != "alpha" {
			t.Errorf("FilterHost let %s through: %v", x.Host, rules(byHost.Alerts))
		}
	}
	if byHost.Counts.Total != len(byHost.Alerts) {
		t.Errorf("FilterHost counts not recomputed: %+v", byHost.Counts)
	}

	byLevel := rep.FilterLevel(LevelCritical)
	for _, x := range byLevel.Alerts {
		if x.Level != LevelCritical {
			t.Errorf("FilterLevel let %s through: %v", x.Level, rules(byLevel.Alerts))
		}
	}

	// The original must be untouched by either filter. Asserted as "unchanged"
	// rather than as a fixed number, so a new rule does not fail this for a
	// reason that has nothing to do with filtering.
	if len(rep.Alerts) != before {
		t.Errorf("filters mutated the original report: %v", rules(rep.Alerts))
	}
}

func TestParseLevel(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"critical", "WARNING", " info "} {
		if _, ok := ParseLevel(s); !ok {
			t.Errorf("ParseLevel(%q) failed", s)
		}
	}
	if _, ok := ParseLevel("nope"); ok {
		t.Error("ParseLevel(\"nope\") should fail")
	}
}

// A nil section means "did not look" and must not produce alerts, which is the
// distinction the whole schema is built around.
func TestNilSectionsProduceNothing(t *testing.T) {
	t.Parallel()
	if alerts := evalOne(base()); len(alerts) != 0 {
		t.Errorf("bare inventory produced %v", rules(alerts))
	}
	if rep := Evaluate([]*inventory.Inventory{nil}, DefaultThresholds(), now); len(rep.Alerts) != 0 {
		t.Errorf("nil inventory produced %v", rules(rep.Alerts))
	}
}

// A crash-looping container is only in state "restarting" during the gap
// between exit and restart, which is a fraction of each cycle — so the state
// check alone misses it. immich_server on the real fleet restarted for weeks
// while every inventory caught it as `running` / `Up 1 second`. These strings
// are copied verbatim from that host's JSON.
func TestRestartingContainerIsCaughtWhileRunning(t *testing.T) {
	t.Parallel()
	now := time.Now()

	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   now,
		Host:          inventory.Host{Hostname: "host-a"},
		Docker: &inventory.Docker{
			Available: true,
			Containers: []inventory.Container{
				{Name: "immich_server", State: "running", Status: "Up 1 second (health: starting)",
					CreatedAt: now.Add(-22 * 24 * time.Hour).Format(dockerCreatedLayout)},
				{Name: "immich_postgres", State: "running", Status: "Up 4 weeks (healthy)",
					CreatedAt: now.Add(-30 * 24 * time.Hour).Format(dockerCreatedLayout)},
				// Just deployed: short uptime, but short age too. Restarting a
				// stack must not fire an alert for every container in it.
				{Name: "freshly-deployed", State: "running", Status: "Up 3 seconds",
					CreatedAt: now.Add(-4 * time.Second).Format(dockerCreatedLayout)},
			},
		},
	}

	rep := Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), now)

	var subjects []string
	for _, a := range rep.Alerts {
		if a.Rule == "docker.container.restarting" {
			subjects = append(subjects, a.Subject)
		}
	}
	if len(subjects) != 1 || subjects[0] != "immich_server" {
		t.Fatalf("docker.container.restarting fired for %v, want just [immich_server]", subjects)
	}
}

func TestUnhealthyContainerIsCaught(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   now,
		Host:          inventory.Host{Hostname: "host-a"},
		Docker: &inventory.Docker{
			Available: true,
			Containers: []inventory.Container{
				{Name: "sick", State: "running", Status: "Up 2 days (unhealthy)",
					CreatedAt: now.Add(-10 * 24 * time.Hour).Format(dockerCreatedLayout)},
			},
		},
	}
	rep := Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), now)
	var found bool
	for _, a := range rep.Alerts {
		if a.Rule == "docker.container.unhealthy" && a.Subject == "sick" {
			found = true
		}
	}
	if !found {
		t.Errorf("no unhealthy alert in %+v", rep.Alerts)
	}
}

func TestShortUptime(t *testing.T) {
	t.Parallel()
	// Docker's humanised Status strings, as they actually appear.
	cases := []struct {
		status string
		want   bool
	}{
		{"Up 1 second (health: starting)", true},
		{"Up 45 seconds", true},
		{"Up Less than a second", true},
		{"Up 2 minutes", true},
		{"Up 5 minutes", true},
		{"Up 6 minutes", false},
		{"Up 20 minutes (healthy)", false},
		{"Up About an hour", false},
		{"Up 4 weeks (healthy)", false},
		{"Up 3 days", false},
		{"Exited (0) 2 hours ago", false},
		{"Restarting (1) 5 seconds ago", false},
		{"", false},
	}
	for _, tc := range cases {
		if _, got := shortUptime(tc.status); got != tc.want {
			t.Errorf("shortUptime(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// hasRule reports whether a rule fired, so a test can assert on what it set up
// rather than on a total that every new rule changes.
func hasRule(as []Alert, rule string) bool {
	for _, a := range as {
		if a.Rule == rule {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- backups

// The rule most likely to matter, because its failure mode is silent and
// total: a backup job that stopped two years ago produces exactly the same
// evidence as one that ran last night — archives on a storage — until somebody
// needs to restore. These are the real numbers from host-b: one guest backed up
// this morning, four last touched 712 days ago.
func TestStaleAndMissingBackups(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages: []inventory.Storage{
			{Name: "backup-nfs", Type: "nfs", Content: []string{"backup"}, Active: true},
		},
		Guests: []inventory.Guest{
			{VMID: 100, Name: "host-c", Status: "running", OnBoot: true},
			{VMID: 102, Name: "host-a", Status: "running", OnBoot: true},
			{VMID: 109, Name: "scratch", Status: "stopped", OnBoot: false},
		},
		Backups: []inventory.Backup{
			{Storage: "backup-nfs", VMID: 100, Created: now.Add(-712 * 24 * time.Hour)},
			{Storage: "backup-nfs", VMID: 102, Created: now.Add(-9 * time.Hour)},
		},
	}

	alerts := evalOne(inv)

	// Backed up this morning: nothing to say.
	absent(t, alerts, "proxmox.backup.stale", "102")
	absent(t, alerts, "proxmox.backup.missing", "102")

	stale := find(t, alerts, "proxmox.backup.stale", "100")
	if stale.Level != LevelWarning {
		t.Errorf("a two-year-old backup of a running guest is %v, want warning", stale.Level)
	}
	if stale.Since == nil || stale.Since.Year() != 2024 {
		t.Errorf("Since = %v, want the 2024 backup date", stale.Since)
	}

	// A guest with no backup at all, and switched off, is worth knowing but is
	// not the same alarm as a running one going unprotected.
	missing := find(t, alerts, "proxmox.backup.missing", "109")
	if missing.Level != LevelInfo {
		t.Errorf("a guest that does not autostart is %v, want info", missing.Level)
	}
}

func TestRunningGuestWithNoBackupIsAWarning(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages:  []inventory.Storage{{Name: "backup-nfs", Content: []string{"backup"}}},
		Guests:    []inventory.Guest{{VMID: 101, Name: "host-f", Status: "running", OnBoot: true}},
		Backups:   []inventory.Backup{},
	}
	if a := find(t, evalOne(inv), "proxmox.backup.missing", "101"); a.Level != LevelWarning {
		t.Errorf("level = %v, want warning", a.Level)
	}
}

// "No storage holds backups" is a different statement from "backups are old",
// and is worth saying once rather than once per guest.
func TestNoBackupStorageIsSaidOnce(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages:  []inventory.Storage{{Name: "local-lvm", Content: []string{"rootdir", "images"}}},
		Guests: []inventory.Guest{
			{VMID: 100, OnBoot: true}, {VMID: 101, OnBoot: true}, {VMID: 102, OnBoot: true},
		},
	}
	alerts := evalOne(inv)
	find(t, alerts, "proxmox.backup.no_storage", "backups")
	absent(t, alerts, "proxmox.backup.missing", "100")
}

// ---------------------------------------------------------------- snapshots

// Exercise both absent and stale snapshots.
func TestSnapshotsAbsentOrStale(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.ZFS = &inventory.ZFS{
		Available: true,
		Pools: []inventory.ZPool{
			{Name: "media", Health: "ONLINE", Errors: "No known data errors"},
			{Name: "secure", Health: "ONLINE", Errors: "No known data errors"},
			{Name: "fresh", Health: "ONLINE", Errors: "No known data errors"},
			// A boot pool is rebuilt by the installer; nobody restores a file
			// from one, so it is not worth an alert.
			{Name: "boot-pool", Health: "ONLINE", Errors: "No known data errors"},
		},
		Snapshots: []inventory.SnapshotSummary{
			{Dataset: "secure", Count: 15, Newest: now.Add(-372 * 24 * time.Hour)},
			{Dataset: "fresh/data", Count: 40, Newest: now.Add(-6 * time.Hour)},
		},
	}

	alerts := evalOne(inv)

	find(t, alerts, "zfs.snapshot.none", "media")
	absent(t, alerts, "zfs.snapshot.none", "boot-pool")
	absent(t, alerts, "zfs.snapshot.stale", "boot-pool")

	// A child dataset's snapshots count for its pool.
	absent(t, alerts, "zfs.snapshot.none", "fresh")
	absent(t, alerts, "zfs.snapshot.stale", "fresh")

	stale := find(t, alerts, "zfs.snapshot.stale", "secure")
	if stale.Level != LevelWarning {
		t.Errorf("level = %v, want warning", stale.Level)
	}
	if stale.Value < 365*24 {
		t.Errorf("Value = %v hours, expected more than a year", stale.Value)
	}
}

// An unscheduled guest differs from a guest whose backup job is failing.
func TestGuestsOutsideAnyBackupJob(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages:  []inventory.Storage{{Name: "backup-nfs", Content: []string{"backup"}}},
		Guests: []inventory.Guest{
			{VMID: 100, Name: "host-c", Status: "running", OnBoot: true},
			{VMID: 102, Name: "host-a", Status: "running", OnBoot: true},
		},
		BackupJobs: []inventory.BackupJob{{
			ID: "backup-23b63e75", Enabled: true, Schedule: "sun 03:00",
			Storage: "backup-nfs", VMIDs: []int{102}, Retention: "keep-monthly=1",
		}},
		Backups: []inventory.Backup{
			// An old archive from a one-off manual run: a red herring, because
			// nothing will ever replace it.
			{Storage: "backup-nfs", VMID: 100, Created: now.Add(-712 * 24 * time.Hour)},
			{Storage: "backup-nfs", VMID: 102, Created: now.Add(-3 * 24 * time.Hour)},
		},
	}

	alerts := evalOne(inv)

	a := find(t, alerts, "proxmox.backup.unscheduled", "100")
	if a.Level != LevelWarning {
		t.Errorf("level = %v, want warning", a.Level)
	}
	// The stale wording would be actively misleading here: nothing is failing.
	absent(t, alerts, "proxmox.backup.stale", "100")
	if !strings.Contains(a.Detail, "nothing will replace it") {
		t.Errorf("detail should explain the old archive is a red herring: %q", a.Detail)
	}

	// The covered, recently backed up guest says nothing at all.
	absent(t, alerts, "proxmox.backup.unscheduled", "102")
	absent(t, alerts, "proxmox.backup.stale", "102")
	absent(t, alerts, "proxmox.backup.missing", "102")
}

// A job that is scheduled but whose archives are old is a different problem
// with a different fix, and must not be confused with an unscheduled guest.
func TestScheduledButFailingBackupIsCalledStale(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available: true,
		Storages:  []inventory.Storage{{Name: "backup-nfs", Content: []string{"backup"}}},
		Guests:    []inventory.Guest{{VMID: 102, Name: "host-a", OnBoot: true}},
		BackupJobs: []inventory.BackupJob{
			{ID: "j", Enabled: true, Schedule: "sun 03:00", VMIDs: []int{102}},
		},
		Backups: []inventory.Backup{
			{Storage: "backup-nfs", VMID: 102, Created: now.Add(-40 * 24 * time.Hour)},
		},
	}
	a := find(t, evalOne(inv), "proxmox.backup.stale", "102")
	if !strings.Contains(a.Detail, "failing") {
		t.Errorf("detail should distinguish failing from missing: %q", a.Detail)
	}
}

func TestAllAndExcludeAndDisabledJobs(t *testing.T) {
	t.Parallel()
	mk := func(jobs []inventory.BackupJob) []Alert {
		inv := base()
		inv.Host.Hostname = "host-b"
		inv.Proxmox = &inventory.Proxmox{
			Available:  true,
			Storages:   []inventory.Storage{{Name: "s", Content: []string{"backup"}}},
			Guests:     []inventory.Guest{{VMID: 100, OnBoot: true}, {VMID: 101, OnBoot: true}},
			BackupJobs: jobs,
			Backups:    []inventory.Backup{},
		}
		return evalOne(inv)
	}

	// all:1 covers everything, so nothing is unscheduled.
	a := mk([]inventory.BackupJob{{ID: "j", Enabled: true, All: true}})
	absent(t, a, "proxmox.backup.unscheduled", "100")
	absent(t, a, "proxmox.backup.unscheduled", "101")

	// all:1 with an exclusion covers everything but the exclusion.
	a = mk([]inventory.BackupJob{{ID: "j", Enabled: true, All: true, Exclude: []int{101}}})
	absent(t, a, "proxmox.backup.unscheduled", "100")
	find(t, a, "proxmox.backup.unscheduled", "101")

	// A disabled job covers nothing, and with no enabled job at all that is
	// said once rather than once per guest.
	a = mk([]inventory.BackupJob{{ID: "j", Enabled: false, All: true}})
	find(t, a, "proxmox.backup.no_job", "backups")
	absent(t, a, "proxmox.backup.unscheduled", "100")

	// A pool-selected job cannot be expanded here, so coverage is unknown and
	// claiming "not backed up" would be a confident wrong answer.
	a = mk([]inventory.BackupJob{{ID: "j", Enabled: true, Pool: "prod"}})
	absent(t, a, "proxmox.backup.unscheduled", "100")
}

// With no enabled job at all, an old archive must not be described as
// "scheduled but failing" — that sends somebody hunting for a job that does
// not exist, which is the wrong diagnosis this rule was rewritten to avoid.
func TestStaleWordingDoesNotClaimAScheduleThatDoesNotExist(t *testing.T) {
	t.Parallel()
	inv := base()
	inv.Host.Hostname = "host-b"
	inv.Proxmox = &inventory.Proxmox{
		Available:  true,
		Storages:   []inventory.Storage{{Name: "s", Content: []string{"backup"}}},
		Guests:     []inventory.Guest{{VMID: 100, Name: "host-c", OnBoot: true}},
		BackupJobs: []inventory.BackupJob{},
		Backups: []inventory.Backup{
			{Storage: "s", VMID: 100, Created: now.Add(-712 * 24 * time.Hour)},
		},
	}
	a := find(t, evalOne(inv), "proxmox.backup.stale", "100")
	if strings.Contains(a.Message, "scheduled") {
		t.Errorf("claimed a schedule with no job defined: %q", a.Message)
	}
	if !strings.Contains(a.Detail, "no enabled job") {
		t.Errorf("detail = %q, want it to say nothing will replace the archive", a.Detail)
	}
}

func TestAgentOutdated(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		revision int
		outdated bool
	}{
		{"pre-revision", 0, true},
		{"lower", inventory.AgentRevision - 1, true},
		{"negative", -1, true},
		{"current", inventory.AgentRevision, false},
		{"newer", inventory.AgentRevision + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			inv := base()
			inv.Tool = inventory.Tool{Version: "57a6815-dirty", Revision: tt.revision}
			alerts := evalOne(inv)
			if !tt.outdated {
				absent(t, alerts, "agent.outdated", "agent")
				return
			}
			a := find(t, alerts, "agent.outdated", "agent")
			if a.Level != LevelInfo || a.Host != inv.Host.Hostname {
				t.Errorf("agent alert = %+v, want info for %s", a, inv.Host.Hostname)
			}
			if !strings.Contains(a.Message, "latest ") || !strings.Contains(a.Message, "re-run make install-agent") {
				t.Errorf("message lacks latest revision or upgrade command: %s", a.Message)
			}
			if tt.revision == 0 && (!strings.Contains(a.Message, "unknown/pre-revision") || !strings.Contains(a.Message, inv.Tool.Version)) {
				t.Errorf("pre-revision message lacks provenance: %s", a.Message)
			}
			count := 0
			for _, a := range alerts {
				if a.Rule == "agent.outdated" {
					count++
				}
			}
			if count != 1 {
				t.Errorf("got %d agent alerts, want one", count)
			}
		})
	}
}

func TestKeyBytesArePersisted(t *testing.T) {
	a := Alert{Host: "host-a", Rule: "zfs.pool.scrub", Subject: "media"}
	if got, want := a.Key(), "host-a\x00zfs.pool.scrub\x00media"; got != want {
		t.Fatalf("Key = %q, want %q", got, want)
	}
}
