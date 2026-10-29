package overview

import (
	"context"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"testing"
	"time"
)

// Selected metadata from read-only production GETs on 2026-10-08. Full
// inventories remain outside git; no config, disks, addresses or archive paths.
func TestProtectionProductionShapes(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	var p, z inventory.Inventory
	if err := json.Unmarshal([]byte(`{
  "host": {
    "hostname": "host-b"
  },
  "proxmox": {
    "available": true,
    "guests": [
      {
        "vmid": 100,
        "name": "host-c",
        "disks": [
          {
            "name": "scsi0"
          },
          {
            "name": "scsi1",
            "backup": false
          },
          {
            "name": "scsi2",
            "backup": false
          },
          {
            "name": "scsi3",
            "backup": false
          }
        ]
      },
      {
        "vmid": 101,
        "name": "host-f",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      },
      {
        "vmid": 102,
        "name": "ubuntu",
        "disks": [
          {
            "name": "scsi0"
          },
          {
            "name": "scsi1"
          },
          {
            "name": "scsi2",
            "backup": false
          }
        ]
      },
      {
        "vmid": 103,
        "name": "node2",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      },
      {
        "vmid": 104,
        "name": "windows",
        "disks": [
          {
            "name": "ide0"
          },
          {
            "name": "scsi0"
          }
        ]
      },
      {
        "vmid": 105,
        "name": "node3",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      },
      {
        "vmid": 107,
        "name": "host-e",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      },
      {
        "vmid": 108,
        "name": "node1",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      },
      {
        "vmid": 109,
        "name": "host-d",
        "disks": [
          {
            "name": "rootfs"
          }
        ]
      }
    ],
    "backup_jobs": [
      {
        "id": "backup-23b63e75-2b23",
        "enabled": true,
        "schedule": "sun 03:00",
        "storage": "backup-nfs",
        "mode": "snapshot",
        "node": "host-b",
        "vmids": [
          102
        ],
        "next_run": "2026-10-11T03:00:00+02:00",
        "retention": "keep-monthly=1"
      }
    ],
    "backups": [
      {
        "vmid": 100,
        "created": "2024-08-13T22:22:13+02:00",
        "size_bytes": 2684233661
      },
      {
        "vmid": 101,
        "created": "2024-08-13T22:30:59+02:00",
        "size_bytes": 433447090
      },
      {
        "vmid": 102,
        "created": "2026-10-04T03:00:01+02:00",
        "size_bytes": 257074284922
      },
      {
        "vmid": 103,
        "created": "2024-08-13T22:59:32+02:00",
        "size_bytes": 54145397333
      },
      {
        "vmid": 105,
        "created": "2024-08-13T22:53:50+02:00",
        "size_bytes": 1126256796
      }
    ]
  }
}`), &p); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{
  "host": {
    "hostname": "host-c"
  },
  "zfs": {
    "available": true,
    "pools": [
      {
        "name": "boot-pool",
        "health": "ONLINE",
        "capacity_pct": 23,
        "scan": "scrub repaired 0B in 00:00:24 with 0 errors on Thu Oct  8 03:45:26 2026"
      },
      {
        "name": "media",
        "health": "ONLINE",
        "capacity_pct": 70,
        "scan": "scrub repaired 0B in 13:03:54 with 0 errors on Tue Oct  6 13:03:56 2026"
      },
      {
        "name": "secure",
        "health": "ONLINE",
        "capacity_pct": 66,
        "scan": "scrub repaired 0B in 07:44:24 with 0 errors on Mon Oct  5 07:44:26 2026"
      }
    ],
    "snapshots": [
      {
        "dataset": "boot-pool/ROOT/22.12.2",
        "count": 1,
        "newest": "2023-05-06T19:05:09+02:00"
      },
      {
        "dataset": "media",
        "count": 16,
        "newest": "2026-10-08T00:00:01+02:00"
      },
      {
        "dataset": "secure",
        "count": 16,
        "newest": "2026-10-08T00:00:00+02:00"
      }
    ]
  }
}`), &z); err != nil {
		t.Fatal(err)
	}
	book := silence.Book{}
	for _, id := range []string{"100", "101", "103", "104", "105", "107", "108"} {
		if _, err := book.Add(silence.Silence{Host: "host-b", Rule: "proxmox.backup.unscheduled", Subject: id, Reason: "Deliberate: not enough backup storage to cover every guest"}, at); err != nil {
			t.Fatal(err)
		}
	}
	rep := silence.Apply(alert.Evaluate([]*inventory.Inventory{&p}, alert.DefaultThresholds(), at), &book, at)
	b := buildBackups([]*inventory.Inventory{&p}, rep, at, &book).(*Backups)
	if b.Counts != (BackupCounts{Covered: 1, Skipped: 7, Uncovered: 1}) {
		t.Fatalf("coverage: %+v", b.Counts)
	}
	for _, g := range b.Guests {
		switch g.VMID {
		case 102:
			if g.Coverage != "covered" || len(g.Jobs) != 1 || len(g.ExcludedDisks) != 1 || *g.NewestArchiveAgeDays != 4 || *g.NewestArchiveSizeBytes != 257074284922 {
				t.Fatalf("covered: %+v", g)
			}
		case 109:
			if g.Coverage != "uncovered" || g.NewestArchiveAgeDays != nil || g.NewestArchiveSizeBytes != nil {
				t.Fatalf("uncovered: %+v", g)
			}
		case 100:
			if g.Coverage != "skipped" || g.SilenceReason == "" || *g.NewestArchiveAgeDays != 785 {
				t.Fatalf("historical archive: %+v", g)
			}
		}
	}
	pools := buildZFS([]*inventory.Inventory{&z}, at).([]Pool)
	if len(pools) != 3 {
		t.Fatal(pools)
	}
	for _, p := range pools {
		if p.Status != "ok" || p.Scrub.Status != "ok" || *p.Scrub.Errors != 0 {
			t.Fatalf("pool: %+v", p)
		}
		switch p.Pool {
		case "media":
			if *p.Scrub.AgeDays != 2 || p.Snapshots.Count != 16 || *p.Snapshots.NewestAgeDays != 0 || p.Snapshots.Status != "ok" {
				t.Fatal(p)
			}
		case "secure":
			if *p.Scrub.AgeDays != 3 {
				t.Fatal(p)
			}
		case "boot-pool":
			if *p.Scrub.AgeDays != 0 || p.Snapshots.Status != "stale" {
				t.Fatal(p)
			}
		}
	}
}

func TestProtectionScrubAndSnapshotStates(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	// Historical production scan, before the October completed scrub.
	s := parseScrub("scrub canceled on Sun Jul 20 17:06:06 2025", at)
	if s.Status != "stale" || *s.State != "canceled" || *s.AgeDays != 444 || s.Errors != nil {
		t.Fatalf("cancelled: %+v", s)
	}
	for _, tc := range []struct{ scan, status string }{
		{"scrub repaired 0B in 01:00:00 with 2 errors on Tue Oct  6 13:03:56 2026", "errors"},
		{"scrub repaired 0B in 01:00:00 with 0 errors on Sun Aug 23 03:00:00 2026", "stale"},
		{"none requested", "stale"}, {"resilvered 0B", "unknown"}, {"scrub in progress since Thu Oct 8", "running"},
	} {
		if got := parseScrub(tc.scan, at); got.Status != tc.status {
			t.Fatalf("%s: %+v", tc.scan, got)
		}
	}
	for _, tc := range []struct {
		count  int
		newest time.Time
		status string
	}{
		{0, time.Time{}, "none"}, {16, time.Time{}, "unknown"}, {16, at.Add(-15 * 24 * time.Hour), "stale"}, {16, at.Add(-time.Hour), "ok"},
	} {
		if got := snapshotStatus(Snapshots{Count: tc.count}, tc.newest, at); got != tc.status {
			t.Fatal(got)
		}
	}
}

func TestProtectionCoverageAmbiguityAndSilence(t *testing.T) {
	at := now
	p := inv("host-b")
	p.Proxmox = &inventory.Proxmox{Available: true, Guests: []inventory.Guest{{VMID: 102}}, Backups: []inventory.Backup{}, BackupJobs: []inventory.BackupJob{
		{ID: "a", Enabled: true, All: true}, {ID: "b", Enabled: true, VMIDs: []int{102}},
	}}
	rep := &alert.Report{Alerts: []alert.Alert{{Host: "host-b", Rule: "proxmox.backup.unscheduled", Subject: "102", Silenced: true, SilenceReason: "accepted"}}}
	b := buildBackups([]*inventory.Inventory{p}, rep, at).(*Backups)
	if b.Guests[0].Coverage != "covered" || len(b.Guests[0].Jobs) != 2 || b.Counts.Covered != 1 {
		t.Fatal(b)
	}
	p.Proxmox.BackupJobs = []inventory.BackupJob{{ID: "pool", Enabled: true, Pool: "unresolved"}}
	if b = buildBackups([]*inventory.Inventory{p}, rep, at).(*Backups); b.Guests[0].Coverage != "unknown" {
		t.Fatal(b)
	}
	p.Proxmox.BackupJobs = []inventory.BackupJob{{ID: "disabled", Enabled: false, All: true}, {ID: "excluded", Enabled: true, All: true, Exclude: []int{102}}}
	if b = buildBackups([]*inventory.Inventory{p}, rep, at).(*Backups); b.Guests[0].Coverage != "skipped" {
		t.Fatal(b)
	}
	rep.Alerts[0].SilenceReason = ""
	if b = buildBackups([]*inventory.Inventory{p}, rep, at).(*Backups); b.Guests[0].Coverage != "uncovered" {
		t.Fatal(b)
	}
	rep.Alerts[0].Rule = "proxmox.backup.stale"
	rep.Alerts[0].SilenceReason = "accepted"
	if b = buildBackups([]*inventory.Inventory{p}, rep, at).(*Backups); b.Guests[0].Coverage != "uncovered" {
		t.Fatal(b)
	}
	p.Proxmox.Backups = nil
	if _, ok := buildBackups([]*inventory.Inventory{p}, rep, at).(Unavailable); !ok {
		t.Fatal("missing archives became empty")
	}
}

// Protection follows the latest inventory, including after a storage-cache hit.
func TestProtectionUsesLatestScrub(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	old := inv("host-c")
	old.CollectedAt = at.Add(-time.Hour)
	old.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{Name: "media", Health: "ONLINE", Scan: "scrub canceled on Sun Jul 20 17:06:06 2025"}}}
	s := saved(t, old)
	b := Builder{Store: s}
	first := b.Build(context.Background(), at).ZFS.([]Pool)[0]
	if first.Scrub.Status != "stale" || *first.Scrub.AgeDays != 444 {
		t.Fatal(first)
	}
	latest := *old
	latest.CollectedAt = at
	latest.ZFS = &inventory.ZFS{Available: true, Pools: []inventory.ZPool{{Name: "media", Health: "ONLINE", Scan: "scrub repaired 0B in 13:03:54 with 0 errors on Tue Oct  6 13:03:56 2026"}}, Snapshots: []inventory.SnapshotSummary{}}
	if _, err := s.Save(&latest); err != nil {
		t.Fatal(err)
	}
	current := b.Build(context.Background(), at).ZFS.([]Pool)[0]
	if current.Scrub.Status != "ok" || *current.Scrub.State != "completed" || *current.Scrub.AgeDays != 2 || current.Snapshots == nil || current.Snapshots.Count != 0 {
		t.Fatal("historical scrub survived a new inventory", current)
	}
}
