package triage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/silence"
)

// This optional integration test reads complete GET captures outside git.
// TRIAGE_LIVE_FIXTURES=/tmp/units-silences-live go test ./internal/triage -run Live
func TestLiveSnapshot(t *testing.T) {
	dir := os.Getenv("TRIAGE_LIVE_FIXTURES")
	if dir == "" {
		t.Skip("set TRIAGE_LIVE_FIXTURES to read-only fleet captures")
	}
	read := func(name string, dest any) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(b, dest); err != nil {
			t.Fatal(err)
		}
	}
	var hosts []struct {
		Name string `json:"name"`
	}
	read("hosts", &hosts)
	invs := []*inventory.Inventory{}
	for _, h := range hosts {
		var inv inventory.Inventory
		read(h.Name, &inv)
		invs = append(invs, &inv)
	}
	var rep alert.Report
	read("alerts", &rep)
	var ss []silence.Silence
	read("silences", &ss)
	b := silence.Book{Silences: map[string]silence.Silence{}}
	for _, s := range ss {
		b.Silences[s.ID] = s
	}
	now := rep.GeneratedAt
	out := Build(invs, rep.Alerts, &b, now)
	expected := map[string]int{"node1": 0, "host-d": 8, "node2": 0, "host-e": 1, "host-a": 0, "host-b": 1, "host-f": 2, "host-c": 1, "node3": 0}
	for _, h := range out.Hosts {
		if h.Status != "ok" || h.FailedCount == nil || *h.FailedCount != expected[h.Host] {
			t.Fatalf("host %s: %+v", h.Host, h)
		}
		for _, u := range h.Units {
			if u.Result != nil || u.StateChangedAt != nil || u.Alert == nil {
				t.Fatalf("invented detail or missing alert: %+v", u)
			}
		}
	}
	counts := map[string]int{}
	for _, s := range out.Silences {
		counts[s.Condition]++
	}
	if counts["still_true"] != 17 || counts["cleared"] != 2 || counts["unknown"] != 0 {
		t.Fatalf("conditions: %v", counts)
	}
}

func TestUnavailableSourcesAndExactIdentity(t *testing.T) {
	now := time.Date(2026, 10, 8, 13, 0, 0, 0, time.UTC)
	// Synthetic systemd data carries a description but no exit result.
	var sd inventory.Systemd
	if err := json.Unmarshal([]byte(`{"available":true,"units":[{"name":"example.service","load":"loaded","active":"failed","sub":"failed","description":"Example service","unit_file_state":"static","preset":"-"}],"timers":[],"failed_count":8}`), &sd); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-d"}, Systemd: &sd, CollectedAt: now}
	a := alert.Alert{Host: "host-d", Rule: "systemd.unit.failed", Subject: "example.service", FirstSeen: &now}
	book := silence.Book{}
	for _, s := range []silence.Silence{
		{Host: "host-d", Rule: a.Rule, Subject: a.Subject},
		{Host: "host-d", Rule: a.Rule, Subject: "other.service"},
		{Host: "missing", Rule: a.Rule, Subject: a.Subject},
		{Host: "host-d", Rule: "zfs.snapshot.none", Subject: "media"},
	} {
		if _, err := book.Add(s, now); err != nil {
			t.Fatal(err)
		}
	}
	out := Build([]*inventory.Inventory{inv}, []alert.Alert{a}, &book, now)
	if *out.Hosts[0].FailedCount != 1 || !*out.Hosts[0].Units[0].Silenced {
		t.Fatal("failed count must derive from units and silence identity must match")
	}
	if out.Hosts[0].Units[0].StateChangedAt != nil {
		t.Fatal("observed age became failure onset")
	}
	conditions := map[string]string{}
	for _, s := range out.Silences {
		conditions[silence.ID(s.Host, s.Rule, s.Subject)] = s.Condition
	}
	for _, s := range book.List(now) {
		want := "unknown"
		if s.Host == "host-d" && s.Rule == a.Rule {
			want = "cleared"
			if s.Subject == a.Subject {
				want = "still_true"
			}
		}
		if conditions[s.ID] != want {
			t.Errorf("%s: got %s want %s", s.ID, conditions[s.ID], want)
		}
	}
	sd.Units = nil
	out = Build([]*inventory.Inventory{inv}, nil, nil, now)
	if out.Hosts[0].FailedCount != nil || out.Hosts[0].Units != nil || out.Silences != nil || out.SilenceStatus != "unavailable" {
		t.Fatal("missing sources reported zeros")
	}
	sd.Units = []inventory.Unit{}
	out = Build([]*inventory.Inventory{inv}, nil, &silence.Book{}, now)
	if *out.Hosts[0].FailedCount != 0 || out.Hosts[0].Units == nil || out.Silences == nil {
		t.Fatal("collected empty not preserved")
	}
}

func TestSilenceRecoveryRequiresCollectedSections(t *testing.T) {
	at := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-b"}, Proxmox: &inventory.Proxmox{Available: true}}
	book := &silence.Book{}
	book.Add(silence.Silence{Host: "host-b", Rule: "proxmox.backup.unscheduled", Subject: "109"}, at)
	if got := Build([]*inventory.Inventory{inv}, nil, book, at); got.Counts.Unknown != 1 || got.Counts.Cleared != 0 {
		t.Fatal("uncollected backup sections proved recovery", got)
	}
	inv.Proxmox.Guests = []inventory.Guest{}
	inv.Proxmox.Storages = []inventory.Storage{}
	inv.Proxmox.BackupJobs = []inventory.BackupJob{}
	inv.Proxmox.Backups = []inventory.Backup{}
	if got := Build([]*inventory.Inventory{inv}, nil, book, at); got.Counts.Cleared != 1 {
		t.Fatal("collected empty source did not prove recovery", got)
	}
	for _, rule := range []string{"firewall.no_inbound_filtering", "host.filesystem.capacity", "unknown.rule"} {
		if conditionCollected(inv, rule) {
			t.Fatalf("missing source for %s proved recovery", rule)
		}
	}
}
