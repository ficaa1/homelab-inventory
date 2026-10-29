package main

import (
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/overview"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOverviewAuthenticatedContract(t *testing.T) {
	s := testServer(t, "test-token")
	v := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: time.Now().UTC(), Host: inventory.Host{Hostname: "host"}, Tool: inventory.Tool{Revision: inventory.AgentRevision}}
	if _, e := s.store.Save(&v); e != nil {
		t.Fatal(e)
	}
	h := s.routes()
	if r := doRequest(t, h, "GET", "/overview", nil); r.Code != http.StatusUnauthorized {
		t.Fatal(r.Code)
	}
	req := httptest.NewRequest("GET", "/overview", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code)
	}
	var out map[string]json.RawMessage
	if e := json.Unmarshal(w.Body.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"generated_at", "fleet", "attention", "storage", "zfs", "backups", "units_failed"} {
		if _, ok := out[field]; !ok {
			t.Errorf("missing %s", field)
		}
	}
	if string(out["backups"]) != "null" || string(out["attention"]) != "[]" {
		t.Fatalf("null/empty: %s", w.Body)
	}
	for _, field := range []string{"streams", "archive", "arr", "plex_load"} {
		if _, ok := out[field]; ok {
			t.Errorf("removed media field %s is present", field)
		}
	}

}

func TestOverviewCacheInvalidatedByOlderIngest(t *testing.T) {
	s := testServer(t, "")
	now := time.Now().UTC()
	v := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: now, Host: inventory.Host{Hostname: "disk", Filesystems: []inventory.Filesystem{{Kind: inventory.FSKindReal, MountPoint: "/", FSType: "ext4", TotalBytes: 1000, UsedBytes: 200, UsedPct: 20}}}}
	if _, err := s.store.Save(&v); err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	storage := func() []overview.Storage {
		t.Helper()
		w := doRequest(t, h, "GET", "/overview", nil)
		var result struct {
			Storage []overview.Storage `json:"storage"`
		}
		if w.Code != http.StatusOK {
			t.Fatal(w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Storage
	}
	if rows := storage(); len(rows) != 1 || rows[0].Delta24hBytes != nil {
		t.Fatal(rows)
	}
	v.CollectedAt = now.Add(-24 * time.Hour)
	v.Host.Filesystems[0].UsedBytes = 100
	v.Host.Filesystems[0].UsedPct = 10
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	w := doRequest(t, h, "POST", "/inventory", body)
	if w.Code != http.StatusCreated {
		t.Fatal(w.Code)
	}
	if rows := storage(); rows[0].Delta24hBytes == nil || *rows[0].Delta24hBytes != 100 {
		t.Fatalf("older ingest failed to invalidate: %+v", rows)
	}
}
