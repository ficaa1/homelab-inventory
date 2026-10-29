package softstore

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/packages"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func TestRoundTripAndChangeOnlyHistory(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	now := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)
	set := inventory.PackageSet{Manager: "dpkg", Status: "ok", CollectedAt: now, Packages: []inventory.Package{{Name: "stable", Version: "1"}, {Name: "changed", Version: "1", Arch: "amd64"}}}
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now, Tool: inventory.Tool{Revision: 14}, Software: &inventory.Software{CollectedAt: now, Status: "ok", Host: set, Images: []inventory.ImagePackages{{ImageID: "sha256:example", Containers: []string{"web"}, SampleContainer: "web", PackageSet: set}}, Runtimes: []inventory.RuntimeVersion{{Name: "kernel", Version: "6.1"}}}}
	ingest := func() {
		t.Helper()
		if err := s.Ingest(inv); err != nil {
			t.Fatal(err)
		}
	}
	history := func() []byte {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "host-a/software/history.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	blobs := func() map[string]time.Time {
		t.Helper()
		entries, err := os.ReadDir(filepath.Join(dir, "packagesets"))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]time.Time{}
		for _, e := range entries {
			st, err := e.Info()
			if err != nil {
				t.Fatal(err)
			}
			out[e.Name()] = st.ModTime()
		}
		return out
	}
	ingest()
	baseline := history()
	originalBlobs := blobs()
	if len(originalBlobs) != 1 || bytes.Contains(baseline, []byte(`"packages":`)) || !bytes.Contains(baseline, []byte(`"packages_ref":`)) {
		t.Fatalf("snapshot contains package lists or duplicated shared blob: %s", baseline)
	}
	inv.Software.CollectedAt = now.Add(15 * time.Minute)
	inv.Software.DurationMS = 99
	inv.Software.Host.CollectedAt = inv.Software.CollectedAt
	inv.Software.Host.DurationMS = 10
	// Order and elapsed collection time do not change content identity.
	inv.Software.Host.Packages = []inventory.Package{set.Packages[1], set.Packages[0]}
	ingest()
	if !bytes.Equal(history(), baseline) {
		t.Fatal("identical ingest appended history")
	}
	for name, at := range blobs() {
		if before, ok := originalBlobs[name]; !ok || !at.Equal(before) {
			t.Fatal("identical ingest wrote a blob")
		}
	}
	response, err := s.Host("host-a", packages.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Software == nil || len(response.Software.Sets) != 2 || len(response.Software.Sets[0].Packages) != 2 || response.AgentRevision != 14 || response.ConfirmedAt.IsZero() {
		t.Fatalf("round trip: %+v", response)
	}
	result, err := s.Search([]string{"host-a", "uncollected"}, "changed")
	if err != nil || len(result.Matches) != 2 || result.Coverage[1].Software != nil || result.Coverage[1].AgentRevision != 0 || !result.Matches[0].ConfirmedAt.Equal(response.ConfirmedAt) {
		t.Fatalf("search: %+v %v", result, err)
	}
	inv.CollectedAt = now.Add(24 * time.Hour)
	inv.Software.CollectedAt = inv.CollectedAt
	inv.Software.Host.Packages[0].Version = "2"
	ingest()
	if len(blobs()) != 2 || len(strings.Split(strings.TrimSpace(string(history())), "\n")) != 2 {
		t.Fatal("changed package must add exactly one blob and history line")
	}
	entries, err := s.Changes("host-a", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || len(entries[0].Changes) != 1 {
		t.Fatalf("changes=%+v", entries)
	}
	change := entries[0].Changes[0]
	if change.Scope != "host" || change.Name != "changed" || change.Arch != "amd64" || change.Kind != "changed" || change.From.Version != "1" || change.To.Version != "2" {
		t.Fatalf("change=%+v", change)
	}
	// An unchanged image must not even open its blob while computing deltas.
	var snap Snapshot
	if err = json.Unmarshal(baseline, &snap); err != nil {
		t.Fatal(err)
	}
	// The host's previous list shares that blob, so use a status-only transition
	// to check that neither unchanged image nor unchanged host references load.
	inv.Software.Host.Status = "partial"
	ingest()
	blob := filepath.Join(dir, "packagesets", snap.Software.Images[0].PackageSet.PackagesRef+".json.gz")
	if err = os.Rename(blob, blob+".unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Changes("host-a", 1); err != nil {
		t.Fatalf("status transition loaded package blobs: %v", err)
	}
	if err = os.Rename(blob+".unavailable", blob); err != nil {
		t.Fatal(err)
	}
	// Store.Save must omit software even if a caller forgets to clear the field.
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Save(inv); err != nil {
		t.Fatal(err)
	}
	stored, err := st.Latest("host-a")
	if err != nil || stored.Software != nil {
		t.Fatalf("run retains software: %+v %v", stored, err)
	}
	hosts, err := st.Hosts()
	if err != nil || len(hosts) != 1 || hosts[0] != "host-a" {
		t.Fatalf("blob directory treated as host: %v %v", hosts, err)
	}
	if err = st.ForgetHost("host-a", now.Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, "host-a/software")); !os.IsNotExist(err) {
		t.Fatalf("forgotten software remains: %v", err)
	}
	if len(blobs()) != 2 {
		t.Fatal("forget removed shared blobs")
	}
}
