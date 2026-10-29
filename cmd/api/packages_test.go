package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/packages"
	"github.com/ficaa1/homelab-inventory/internal/softstore"
)

func TestPackageEndpoints(t *testing.T) {
	s := testServer(t, "inventory-token")
	h := s.routes()
	get := func(path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer inventory-token")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for _, path := range []string{"/hosts/host-a/packages", "/packages?name=libssl3", "/hosts/host-a/packages/changes"} {
		if rec := get(path, false); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unguarded %s: %d", path, rec.Code)
		}
	}
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	inv := &inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now, Tool: inventory.Tool{Revision: 14}}
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
	rec := get("/hosts/host-a/packages", true)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"software": null`) {
		t.Fatalf("old agent must be unknown: %d %s", rec.Code, rec.Body)
	}
	set := inventory.PackageSet{Manager: "dpkg", Status: "ok", CollectedAt: now, Distro: inventory.Distro{ID: "debian", VersionID: "12"}, Count: 2, Packages: []inventory.Package{{Name: "libssl3", Version: "1:3.0-1", Source: "openssl"}, {Name: "libssl-dev", Version: "1:3.0-1", Source: "openssl"}}}
	inv.CollectedAt = now.Add(time.Second)
	inv.Software = &inventory.Software{Status: "partial", CollectedAt: now, Host: set, ImagesStatus: "partial", ImagesError: "root unreadable", Runtimes: []inventory.RuntimeVersion{}, Images: []inventory.ImagePackages{
		{ImageID: "sha256:abc", ImageRef: "example:latest", Containers: []string{"web"}, SampleContainer: "web", PackageSet: set},
		{ImageID: "sha256:bad", Containers: []string{"broken"}, PackageSet: inventory.PackageSet{Status: "failed", Error: "permission denied", Packages: []inventory.Package{}}},
	}}
	body, _ := json.Marshal(inv)
	req := httptest.NewRequest("POST", "/inventory", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer inventory-token")
	posted := httptest.NewRecorder()
	h.ServeHTTP(posted, req)
	if posted.Code != 201 {
		t.Fatalf("ingest: %d %s", posted.Code, posted.Body)
	}
	stored, err := s.store.Latest("host-a")
	if err != nil || stored.Software != nil {
		t.Fatalf("run retains software: %+v %v", stored, err)
	}
	// Subsequent ordinary runs must not hide the daily package snapshot.
	inv.Software = nil
	inv.CollectedAt = now.Add(15 * time.Minute)
	if _, err = s.store.Save(inv); err != nil {
		t.Fatal(err)
	}

	var response packages.Response
	rec = get("/hosts/host-a/packages?q=ssl&scope=web&limit=1&offset=1", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || response.Total != 2 || response.AgentRevision != 14 || response.Software == nil || len(response.Software.Sets) != 1 || len(response.Software.Sets[0].Packages) != 1 || response.Software.Sets[0].Packages[0].Name != "libssl3" || response.ConfirmedAt.IsZero() || !response.CollectedAt.Equal(now) {
		t.Fatalf("pagination/scope: %s", rec.Body)
	}
	rec = get("/hosts/host-a/packages?scope=sha256:bad&limit=5000", true)
	json.Unmarshal(rec.Body.Bytes(), &response)
	if response.Limit != 1000 || response.Software.Sets[0].Status != "failed" || response.Software.Sets[0].Error != "permission denied" {
		t.Fatalf("failed set hidden: %s", rec.Body)
	}
	for _, query := range []string{"limit=0", "offset=-1", "limit=wat"} {
		if r := get("/hosts/host-a/packages?"+query, true); r.Code != 400 {
			t.Fatalf("invalid %s: %d", query, r.Code)
		}
	}
	// Other features keep auxiliary directories under the same data root.
	if err := os.MkdirAll(filepath.Join(s.store.Dir(), "tv-requests"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		count int
	}{{"libssl3", 2}, {"libssl*", 4}, {"absent", 0}} {
		rec = get("/packages?name="+tc.name, true)
		var result packages.SearchResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if rec.Code != 200 || len(result.Matches) != tc.count || len(result.Coverage) != 1 || len(result.Coverage[0].Software.Sets) != 3 {
			t.Fatalf("fleet search %s: %s", tc.name, rec.Body)
		}
	}
	if rec := get("/hosts/host-a/packages/changes?limit=bad", true); rec.Code != 400 {
		t.Fatalf("invalid changes limit: %d", rec.Code)
	}
	if rec := get("/hosts/host-a/packages/changes", true); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("baseline changes: %s", rec.Body)
	}
	if rec := get("/packages", true); rec.Code != 400 {
		t.Fatalf("missing name: %d", rec.Code)
	}
	if rec := get("/hosts/missing/packages", true); rec.Code != 404 {
		t.Fatalf("missing host: %d", rec.Code)
	}
}

func TestSoftwareIngestFailurePreservesRun(t *testing.T) {
	s := testServer(t, "")
	// A file where the shared blob directory must be forces a storage failure.
	if err := os.WriteFile(filepath.Join(s.store.Dir(), "packagesets"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	inv := inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: time.Now(), Software: &inventory.Software{Host: inventory.PackageSet{Packages: []inventory.Package{}}}}
	b, _ := json.Marshal(inv)
	rec := doRequest(t, s.routes(), "POST", "/inventory", b)
	if rec.Code != 201 || !strings.Contains(rec.Body.String(), "software_error") {
		t.Fatalf("run lost on software failure: %d %s", rec.Code, rec.Body)
	}
	got, err := s.store.Latest("host-a")
	if err != nil || got.Software != nil {
		t.Fatalf("stored=%+v err=%v", got, err)
	}
}

func TestPackageChangesEndpoint(t *testing.T) {
	s := testServer(t, "")
	now := time.Now().UTC()
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now, Software: &inventory.Software{CollectedAt: now, Host: inventory.PackageSet{Status: "ok", Packages: []inventory.Package{{Name: "example", Version: "1"}}}}}
	post := func() {
		t.Helper()
		b, _ := json.Marshal(inv)
		if rec := doRequest(t, s.routes(), "POST", "/inventory", b); rec.Code != 201 {
			t.Fatalf("post: %s", rec.Body)
		}
	}
	post()
	inv.CollectedAt = now.Add(24 * time.Hour)
	inv.Software.CollectedAt = inv.CollectedAt
	inv.Software.Host.Packages[0].Version = "2"
	post()
	rec := doRequest(t, s.routes(), "GET", "/hosts/host-a/packages/changes?limit=1", nil)
	var entries []softstore.Entry
	if err := json.Unmarshal(rec.Body.Bytes(), &entries); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(entries) != 1 || len(entries[0].Changes) != 1 || entries[0].Changes[0].Name != "example" {
		t.Fatalf("changes: %s", rec.Body)
	}
}
