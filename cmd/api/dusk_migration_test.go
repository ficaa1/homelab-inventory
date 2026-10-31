package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/deviceauth"
	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

func TestMigratedLegacyReceiptBlocksNewHouseholdOperation(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state-migration", "tv-requests")
	if e := os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	name := fmt.Sprintf("%x.json", sha256.Sum256([]byte("movie:123:0")))
	b := []byte(`{"operation":"legacy-operation-0001","state":"unknown","at":"2024-01-01T00:00:00Z","future":true}`)
	if e := os.WriteFile(filepath.Join(root, name), b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := migrateBackupDusk(dir, map[string]string{}, "", 0); e != nil {
		t.Fatal(e)
	}
	d := duskState{path: filepath.Join(dir, "plugins", "dusk", "core", "policy.json")}
	state, ok := d.legacyRequest(mediadata.Request{Kind: "movie", ID: 123, Operation: "new-household-operation"})
	if !ok || state != "unknown" {
		t.Fatal(state, ok)
	}
	copied, e := os.ReadFile(filepath.Join(filepath.Dir(d.path), "legacy-requests", name))
	if e != nil || !bytes.Equal(b, copied) {
		t.Fatal("receipt bytes changed", e)
	}
	if _, ok = d.legacyRequest(mediadata.Request{Kind: "movie", ID: 124}); ok {
		t.Fatal("unrelated request blocked")
	}
	if e = os.WriteFile(filepath.Join(filepath.Dir(d.path), "legacy-requests", name), []byte("broken"), 0600); e != nil {
		t.Fatal(e)
	}
	state, ok = d.legacyRequest(mediadata.Request{Kind: "movie", ID: 123})
	if !ok || state != "unknown" {
		t.Fatal("corrupt receipt did not fail closed")
	}
}
func TestExplicitMigrationProfilePreservesVerdictsAndCreatesNoDevices(t *testing.T) {
	dir := t.TempDir()
	b := []byte("{\n\"owner\":7,\"titles\":{\"movie:6\":{\"verdict\":\"loved\",\"title\":\"Synthetic\",\"at\":\"2024-01-01T00:00:00Z\"}}}")
	if e := os.WriteFile(filepath.Join(dir, "taste-feedback.json"), b, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := migrateBackupDusk(dir, map[string]string{"TV_PLEX_USER_ID": "7"}, "household", 8); e == nil {
		t.Fatal("owner mismatch allowed")
	}
	if _, e := migrateBackupDusk(dir, map[string]string{"TV_PLEX_USER_ID": "7"}, "household", 7); e != nil {
		t.Fatal(e)
	}
	copied, e := os.ReadFile(filepath.Join(dir, "plugins", "taste", "profiles", "household", "taste-feedback.json"))
	if e != nil || !bytes.Equal(b, copied) {
		t.Fatal("verdict bytes changed", e)
	}
	devices, e := deviceauth.New(filepath.Join(dir, "plugins", "dusk", "core"))
	if e != nil || len(devices.Devices()) != 0 || len(devices.Profiles()) != 1 || devices.Profiles()[0].ID != "household" {
		t.Fatal("invalid household migration", e)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if e != nil {
		t.Fatal(e)
	}
	var state map[string]any
	if e = json.Unmarshal(raw, &state); e != nil {
		t.Fatal(e)
	}
}

func TestDuskTasteFeedbackUsesPairedHousehold(t *testing.T) {
	s := testServer(t, "synthetic-admin-token")
	var e error
	s.plugins, e = openPlugins(s.store.Dir(), s.log)
	if e != nil {
		t.Fatal(e)
	}
	defer s.plugins.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"pageInfo":{"results":0},"results":[]}`) }))
	defer upstream.Close()
	h := duskHTTPTest{t: t, s: s, http: httptest.NewServer(s.routes())}
	defer h.http.Close()
	h.call("PUT", "/plugins/dusk", s.token, map[string]any{"enabled": true, "settings": map[string]any{"discovery": true, "taste": true}}, 200)
	h.call("PUT", "/plugins/media", s.token, map[string]any{"enabled": true, "settings": map[string]any{"overseerr_url": upstream.URL, "overseerr_key": "synthetic-key"}}, 200)
	first := h.profile("First")
	second := h.profile("Second")
	bindings, _ := json.Marshal(map[string]any{first: map[string]int{"owner": 7}, second: map[string]int{"owner": 8}})
	h.call("PUT", "/plugins/taste", s.token, map[string]any{"enabled": true, "settings": map[string]any{"bindings": string(bindings)}}, 200)
	token, _ := h.pair(first, []string{"taste", "preferences", "discover"})
	other, _ := h.pair(second, []string{"taste", "preferences", "discover"})
	h.call("POST", "/tv/v1/taste/verdict", token, map[string]any{"kind": "movie", "id": 17, "verdict": "loved", "title": "Synthetic first", "year": "2024"}, 200)
	h.call("POST", "/tv/v1/taste/verdict", token, map[string]any{"profile": second, "kind": "movie", "id": 18, "verdict": "loved", "title": "Synthetic forbidden"}, 400)
	h.call("POST", "/tv/v1/taste/verdict", other, map[string]any{"kind": "movie", "id": 19, "verdict": "disliked", "title": "Synthetic second", "year": "2023"}, 200)
	path := filepath.Join(s.store.Dir(), "plugins", "taste", "profiles")
	for id, want := range map[string]string{first: "movie:17", second: "movie:19"} {
		f := engine.Feedback{Path: filepath.Join(path, id, "taste-feedback.json"), Owner: map[string]int{first: 7, second: 8}[id]}
		rows, e := f.Load()
		if e != nil || len(rows) != 1 || rows[want].Title == "" {
			t.Fatalf("feedback crossed profiles: %#v %v", rows, e)
		}
	}
}

func TestPortableTwinsAutomaticallyActivatedWithoutGobDecode(t *testing.T) {
	dir := t.TempDir()
	portable := []byte(`{"Source":"synthetic licensed artifact","Films":[{"TMDB":1,"Title":"Fixture","Count":5,"Mean":4}],"Rows":[[{"To":0,"W":0.5}]]}`)
	for name, b := range map[string][]byte{"tastetwins.portable.json": portable, "tastetwins.gob": {0xff, 0, 1}} {
		if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	issues, e := migrateBackupDusk(dir, map[string]string{}, "household", 7)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(dir, "plugins", "taste", "twins.json"))
	if e != nil || !bytes.Equal(raw, portable) {
		t.Fatal("portable weights lost", e)
	}
	settings, e := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if e != nil || !bytes.Contains(settings, []byte(`"twins_artifact":"twins.json"`)) {
		t.Fatal(string(settings), issues, e)
	}
}
