package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func testInventory(host string, at time.Time) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   at,
		Host:          inventory.Host{Hostname: host, OS: inventory.OSRelease{Pretty: "Debian GNU/Linux 12"}},
	}
}

func TestSaveAndLatest(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	older := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	if _, err := s.Save(testInventory("node1", older)); err != nil {
		t.Fatalf("save older: %v", err)
	}
	if _, err := s.Save(testInventory("node1", newer)); err != nil {
		t.Fatalf("save newer: %v", err)
	}

	got, err := s.Latest("node1")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if !got.CollectedAt.Equal(newer) {
		t.Errorf("latest.CollectedAt = %v, want %v", got.CollectedAt, newer)
	}
}

func TestHistoryNewestFirst(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	times := []time.Time{
		time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC),
	}
	for _, at := range times {
		if _, err := s.Save(testInventory("host-a", at)); err != nil {
			t.Fatal(err)
		}
	}

	records, err := s.History("host-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 {
		t.Fatalf("len(records) = %d, want 3", len(records))
	}
	want := []time.Time{times[1], times[2], times[0]} // 25th, 24th, 23rd
	for i, w := range want {
		if !records[i].CollectedAt.Equal(w) {
			t.Errorf("records[%d].CollectedAt = %v, want %v", i, records[i].CollectedAt, w)
		}
	}
}

func TestAtRoundTripsRecordID(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 25, 12, 30, 0, 123456789, time.UTC)
	rec, err := s.Save(testInventory("host-c", at))
	if err != nil {
		t.Fatal(err)
	}

	inv, err := s.At("host-c", rec.ID)
	if err != nil {
		t.Fatalf("At(%q): %v", rec.ID, err)
	}
	if !inv.CollectedAt.Equal(at) {
		t.Errorf("CollectedAt = %v, want %v", inv.CollectedAt, at)
	}
}

func TestAtRejectsMalformedID(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(testInventory("host-c", time.Now())); err != nil {
		t.Fatal(err)
	}

	_, err = s.At("host-c", "../../etc/passwd")
	if !errors.Is(err, ErrInvalidID) {
		t.Errorf("At with path-traversal id: err = %v, want ErrInvalidID", err)
	}
}

func TestNotFound(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.Latest("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Latest(unknown host): err = %v, want ErrNotFound", err)
	}
	if _, err := s.History("ghost"); !errors.Is(err, ErrNotFound) {
		t.Errorf("History(unknown host): err = %v, want ErrNotFound", err)
	}
}

func TestHostsListsEveryHostWithARun(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"host-a", "node1", "host-b"} {
		if _, err := s.Save(testInventory(h, time.Now())); err != nil {
			t.Fatal(err)
		}
	}

	hosts, err := s.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"host-a", "host-b", "node1"} // sorted
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
	for i, w := range want {
		if hosts[i] != w {
			t.Errorf("hosts[%d] = %q, want %q", i, hosts[i], w)
		}
	}
}

func TestLatestAllSkipsNothingButActualHosts(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(testInventory("host-a", time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(testInventory("host-b", time.Now())); err != nil {
		t.Fatal(err)
	}

	invs, err := s.LatestAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("len(invs) = %d, want 2", len(invs))
	}
}

func TestSaveRejectsUnsafeHostname(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{"../escape", "a/b", "", ".", "..", "agent-offers.jsonl", "actions-audit.jsonl", "settings", "packagesets", "state-notify.json", "state-settings.json", ".actions-temp"} {
		if _, err := s.Save(testInventory(host, time.Now())); err == nil {
			t.Errorf("Save(hostname=%q): expected error, got nil", host)
		}
	}
}

func TestSaveDefaultsMissingCollectedAt(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	inv := testInventory("node1", time.Time{})
	before := time.Now().Add(-time.Second)
	rec, err := s.Save(inv)
	if err != nil {
		t.Fatal(err)
	}
	if rec.CollectedAt.Before(before) {
		t.Errorf("CollectedAt = %v, want approximately now", rec.CollectedAt)
	}
}

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}

	type blob struct {
		Seen  map[string]int `json:"seen"`
		Count int            `json:"count"`
	}

	// Never written: not an error, and v is left alone. "Nothing has happened
	// yet" is a normal state, not a failure.
	var got blob
	if err := s.ReadState("notify", &got); err != nil {
		t.Fatalf("reading unwritten state: %v", err)
	}
	if got.Count != 0 || got.Seen != nil {
		t.Errorf("unwritten state returned %+v, want the zero value", got)
	}

	want := blob{Seen: map[string]int{"host-b|zfs.pool.scrub|media": 1}, Count: 3}
	if err := s.WriteState("notify", want); err != nil {
		t.Fatal(err)
	}
	if err := s.ReadState("notify", &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != want.Count || got.Seen["host-b|zfs.pool.scrub|media"] != 1 {
		t.Errorf("round trip gave %+v, want %+v", got, want)
	}

	// Unlike a run, state is meant to be replaced.
	want.Count = 9
	if err := s.WriteState("notify", want); err != nil {
		t.Fatalf("overwriting state: %v", err)
	}
	if err := s.ReadState("notify", &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 9 {
		t.Errorf("Count = %d after overwrite, want 9", got.Count)
	}
}

// State lives as a file in the store root precisely so that Hosts(), which
// lists directories, cannot mistake it for a host. If that ever changes,
// GET /hosts starts reporting a host called "notify".
func TestStateIsNotMistakenForAHost(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save(testInventory("host-a", time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteState("notify", map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}

	hosts, err := s.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0] != "host-a" {
		t.Fatalf("Hosts() = %v, want [host-a]", hosts)
	}
	invs, err := s.LatestAll()
	if err != nil {
		t.Fatalf("LatestAll: %v", err)
	}
	if len(invs) != 1 {
		t.Fatalf("LatestAll returned %d inventories, want 1", len(invs))
	}
}

func TestStateNameIsValidated(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "../escape", "a/b", "Notify", "note.json"} {
		if err := s.WriteState(name, map[string]string{}); err == nil {
			t.Errorf("WriteState(%q): expected an error, got nil", name)
		}
		if err := s.ReadState(name, &struct{}{}); err == nil {
			t.Errorf("ReadState(%q): expected an error, got nil", name)
		}
	}
}
