package overview

import (
	"context"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)

func inv(host string) *inventory.Inventory {
	return &inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: now, Host: inventory.Host{Hostname: host}, Tool: inventory.Tool{Revision: inventory.AgentRevision}}
}
func saved(t *testing.T, vs ...*inventory.Inventory) *store.Store {
	t.Helper()
	s, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range vs {
		if _, e = s.Save(v); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func TestFleetAttentionAndSilences(t *testing.T) {
	v := inv("old")
	v.CollectedAt = now.Add(-3 * time.Hour)
	v.Tool.Revision = 0
	v.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{{Name: "failed.service", Active: "failed"}}}
	s := saved(t, v)
	sb := silence.Book{}
	sb.Add(silence.Silence{Host: "old", Rule: "systemd.unit.failed", Subject: "failed.service"}, now)
	s.WriteState(silence.StateName, sb)
	first := now.Add(-24 * time.Hour)
	s.WriteState(seen.StateName, seen.Book{Entries: map[string]seen.Entry{seen.Key(alert.Alert{Host: "old", Rule: "host.stale", Subject: "old"}): {First: first, Last: now}}})
	r := (Builder{Store: s}).Build(context.Background(), now)
	f := r.Fleet.(Fleet)
	if f.HostsTotal != 1 || f.HostsReporting != 0 || len(f.Stale) != 1 || f.Stale[0].Severity != "critical" || *f.Stale[0].AgeS != 10800 || len(f.AgentsOutdated) != 1 || f.Alerts.Silenced != 1 {
		t.Fatalf("fleet: %+v", f)
	}
	a := r.Attention.([]Attention)
	if len(a) != 1 || a[0].Rule != "host.stale" || !a[0].Since.Equal(first) {
		t.Fatalf("attention: %+v", a)
	}
	u := r.UnitsFailed.([]Unit)
	if len(u) != 1 || !u[0].Silenced {
		t.Fatalf("units: %+v", u)
	}
}
func TestAttentionWorstThenOldestAndLimit(t *testing.T) {
	vs := []*inventory.Inventory{}
	for i := 0; i < 15; i++ {
		v := inv(string(rune('a' + i)))
		v.CollectedAt = now.Add(-time.Duration(60+i) * time.Minute)
		vs = append(vs, v)
	}
	v := inv("critical")
	v.CollectedAt = now.Add(-3 * time.Hour)
	vs = append(vs, v)
	s := saved(t, vs...)
	book := seen.Book{Entries: map[string]seen.Entry{}}
	for i, v := range vs {
		book.Entries[seen.Key(alert.Alert{Host: v.Host.Hostname, Rule: "host.stale", Subject: v.Host.Hostname})] = seen.Entry{First: now.Add(-time.Duration(i+1) * 24 * time.Hour), Last: now}
	}
	s.WriteState(seen.StateName, book)
	a := (Builder{Store: s}).Build(context.Background(), now).Attention.([]Attention)
	if len(a) != 12 || a[0].Severity != "critical" || a[1].Host != "o" {
		t.Fatalf("order/limit: %+v", a)
	}
}
func TestStorageForecastGatingAndDelta(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		step float64
		want string
	}{{"short", 2, 1, "insufficient"}, {"flat", 20, 0, "flat"}, {"growth", 20, 1, "ok"}} {
		t.Run(tc.name, func(t *testing.T) {
			vs := []*inventory.Inventory{}
			for i := 0; i < tc.n; i++ {
				v := inv("disk")
				v.CollectedAt = now.Add(-time.Duration(tc.n-1-i) * 24 * time.Hour)
				pct := 20 + tc.step*float64(i)
				v.Host.Filesystems = []inventory.Filesystem{{Kind: inventory.FSKindReal, MountPoint: "/", FSType: "ext4", TotalBytes: 10000, UsedBytes: uint64(pct * 100), UsedPct: pct}}
				vs = append(vs, v)
			}
			r := (Builder{Store: saved(t, vs...)}).Build(context.Background(), now)
			rows := r.Storage.([]Storage)
			if len(rows) != 1 || rows[0].Forecast.Status != tc.want || rows[0].Delta24hBytes == nil || *rows[0].Delta24hBytes != int64(tc.step*100) {
				t.Fatalf("row: %+v", rows)
			}
			if tc.want == "ok" && rows[0].Forecast.FullAt == nil {
				t.Fatal("missing forecast")
			}
			if tc.want != "ok" && rows[0].Forecast.FullAt != nil {
				t.Fatal("ungated forecast")
			}
		})
	}
}
func TestStorageSparkActualSamples(t *testing.T) {
	vs := []*inventory.Inventory{}
	for i := 0; i < 40; i++ {
		v := inv("disk")
		v.CollectedAt = now.Add(-time.Duration(39-i) * time.Hour)
		v.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{Name: "pool", SizeBytes: 100, AllocBytes: uint64(i), CapacityPct: i}}}
		vs = append(vs, v)
	}
	rows := (Builder{Store: saved(t, vs...)}).Build(context.Background(), now).Storage.([]Storage)
	if len(rows[0].Spark) != 30 || rows[0].Spark[0] != 0 || rows[0].Spark[29] != 39 {
		t.Fatalf("spark: %+v", rows[0])
	}
}
func TestZFSSections(t *testing.T) {
	v := inv("nas")
	v.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{Name: "media", Health: "ONLINE", CapacityPct: 72, Scan: "scrub repaired 0B in 01:00:00 with 0 errors on Tue Oct  6 03:00:00 2026"}}, Snapshots: []inventory.SnapshotSummary{{Dataset: "media/share", Count: 16, Newest: now.Add(-time.Hour)}}}
	// Hosts without a ZFS section do not contribute fabricated pools.
	p := inv("host-b")
	p.ZFS = nil
	z := buildZFS([]*inventory.Inventory{p, v}, now).([]Pool)
	if len(z) != 1 || *z[0].Scrub.State != "completed" || *z[0].Scrub.Errors != 0 || z[0].Snapshots.Count != 16 || *z[0].Snapshots.NewestAgeDays != 0 {
		t.Fatalf("zfs: %+v", z)
	}
	for _, tc := range []struct{ raw, want string }{{"scrub canceled on Tue Oct  6 03:00:00 2026", "canceled"}, {"scrub in progress since Tue Oct 6", "running"}, {"none requested", "none"}, {"resilvered 0B", ""}, {"unknown", ""}} {
		s := parseScrub(tc.raw, now)
		if tc.want == "" {
			if s.State != nil {
				t.Fatal(s)
			}
		} else if s.State == nil || *s.State != tc.want {
			t.Fatal(s)
		}
	}
	v.ZFS.Snapshots = nil
	z = buildZFS([]*inventory.Inventory{v}, now).([]Pool)
	if z[0].Snapshots != nil {
		t.Fatal("uncollected snapshots became empty")
	}
}
func TestBackups(t *testing.T) {
	v := inv("host-b")
	v.Proxmox = &inventory.Proxmox{Available: true, BackupJobs: []inventory.BackupJob{{ID: "weekly", Enabled: true, All: true, Exclude: []int{109}, NextRun: now.Add(24 * time.Hour)}}, Guests: []inventory.Guest{{VMID: 102, Name: "host-a"}, {VMID: 109, Name: "host-d"}}, Backups: []inventory.Backup{{VMID: 102, Created: now.Add(-3 * 24 * time.Hour)}}}
	rep := &alert.Report{Alerts: []alert.Alert{{Host: "host-b", Rule: "proxmox.backup.unscheduled", Subject: "109", Silenced: true}}}
	b := buildBackups([]*inventory.Inventory{v}, rep, now).(*Backups)
	if len(b.Jobs) != 1 || b.Jobs[0].NextRun == nil || *b.Guests[0].Job != "weekly" || *b.Guests[0].NewestArchiveAgeDays != 3 || b.Guests[1].Job != nil || b.Guests[1].NewestArchiveAgeDays != nil || !b.Guests[1].Silenced {
		t.Fatalf("backups: %+v", b)
	}
	if buildBackups([]*inventory.Inventory{inv("vm")}, rep, now) != nil {
		t.Fatal("no Proxmox must be null")
	}
}

func TestUnavailableLocalSectionsAndEmptySlices(t *testing.T) {
	r := (Builder{Store: saved(t, inv("vm"))}).Build(context.Background(), now)
	for _, v := range []any{r.Storage, r.ZFS, r.UnitsFailed} {
		if _, ok := v.(Unavailable); !ok {
			t.Fatalf("missing source represented as %+v", v)
		}
	}
	v := inv("empty")
	v.Host.Filesystems = []inventory.Filesystem{}
	v.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{}, Snapshots: []inventory.SnapshotSummary{}}
	v.Systemd = &inventory.Systemd{Available: true, Units: []inventory.Unit{}}
	r = (Builder{Store: saved(t, v)}).Build(context.Background(), now)
	for _, v := range []any{r.Storage, r.ZFS, r.UnitsFailed, r.Attention} {
		body, _ := json.Marshal(v)
		if string(body) != "[]" {
			t.Fatalf("looked/found nothing: %s", body)
		}
	}
}
func TestExplicitlyUnavailableInventorySources(t *testing.T) {
	v := inv("failed")
	v.ZFS = &inventory.ZFS{Available: false}
	v.Proxmox = &inventory.Proxmox{Available: false}
	v.Systemd = &inventory.Systemd{Available: false}
	r := (Builder{Store: saved(t, v)}).Build(context.Background(), now)
	for _, section := range []any{r.Storage, r.ZFS, r.Backups, r.UnitsFailed} {
		if _, ok := section.(Unavailable); !ok {
			t.Fatalf("unavailable source became %+v", section)
		}
	}
}

func intPointer(v int) *int { return &v }

func TestStorageUnavailableMeasurements(t *testing.T) {
	v := inv("host-b")
	v.Proxmox = &inventory.Proxmox{Available: true, Storages: []inventory.Storage{
		{Name: "inactive", Active: false, TotalBytes: 100, UsedBytes: 50},
		{Name: "unreadable", Active: true},
		{Name: "empty", Active: true, TotalBytes: 100},
	}}
	rows := (Builder{Store: saved(t, v)}).Build(context.Background(), now).Storage.([]Storage)
	if rows[0].Name != "empty" || rows[0].Pct == nil || *rows[0].Pct != 0 || rows[0].Status != "ok" {
		t.Fatalf("readable empty: %+v", rows)
	}
	for _, row := range rows[1:] {
		body, _ := json.Marshal(row)
		var fields map[string]any
		json.Unmarshal(body, &fields)
		if row.Status != "unavailable" || row.Error == "" || fields["pct"] != nil || fields["used_bytes"] != nil || fields["size_bytes"] != nil || row.Spark != nil || row.Delta24hBytes != nil || row.Forecast.Status != "unavailable" {
			t.Fatalf("unavailable: %s", body)
		}
	}
}
func TestZFSPoolsNotCollected(t *testing.T) {
	v := inv("nas")
	v.ZFS = &inventory.ZFS{Available: true}
	for _, hosts := range [][]*inventory.Inventory{{v}, {v, inv("vm")}} {
		body, _ := json.Marshal(buildZFS(hosts, now))
		if string(body) != "null" {
			t.Fatalf("uncollected pools: %s", body)
		}
	}
	v.ZFS.Pools = []inventory.ZPool{}
	body, _ := json.Marshal(buildZFS([]*inventory.Inventory{v}, now))
	if string(body) != "[]" {
		t.Fatalf("collected empty pools: %s", body)
	}
}
