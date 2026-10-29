package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func TestPruneExtendsServerWriteDeadline(t *testing.T) {
	s := testServer(t, "")
	now := time.Now().UTC()
	for i := 0; i < 10; i++ {
		_, err := s.store.Save(&inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now.Add(-time.Duration(40*24+i) * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	ts := httptest.NewUnstartedServer(withLogging(s.log, s.routes()))
	ts.Config.WriteTimeout = 5 * time.Millisecond // Prune's pacing alone exceeds this.
	ts.Start()
	defer ts.Close()
	res, err := ts.Client().Post(ts.URL+"/storage/runs/prune", "application/json", bytes.NewBufferString(`{"dryRun":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("prune returned %s", res.Status)
	}
	var result store.PruneResult
	if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
		t.Fatal("prune response expired", err)
	}
}

func storageRequest(s *server, method, path, body, ct, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	out := httptest.NewRecorder()
	s.routes().ServeHTTP(out, req)
	return out
}
func TestRunsStorageEndpoints(t *testing.T) {
	s := testServer(t, "secret-test-token")
	now := time.Now().UTC()
	for _, at := range []time.Time{now.AddDate(0, 0, -40), now.AddDate(0, 0, -40).Add(time.Minute), now} {
		if _, err := s.store.Save(&inventory.Inventory{Host: inventory.Host{Hostname: "active"}, CollectedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	old := &inventory.Inventory{Host: inventory.Host{Hostname: "stale"}, CollectedAt: now.AddDate(0, 0, -8)}
	if _, err := s.store.Save(old); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/storage/runs", ""}, {"POST", "/storage/runs/prune", `{"dryRun":false}`}, {"DELETE", "/storage/runs/stale", ""},
	} {
		out := storageRequest(s, tc.method, tc.path, tc.body, "application/json", "")
		if out.Code != 401 {
			t.Fatalf("auth %s: %d", tc.path, out.Code)
		}
	}
	req := func(method, path, body string) *httptest.ResponseRecorder {
		return storageRequest(s, method, path, body, "application/json", "secret-test-token")
	}
	out := req("GET", "/storage/runs", "")
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	var usage struct {
		store.Usage
		Policy store.Policy `json:"policy"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.TotalRuns != 4 || usage.TotalBytes <= 0 || usage.Policy != store.DefaultPolicy() || !usage.Hosts[1].Stale {
		t.Fatalf("usage %+v", usage)
	}
	for _, ct := range []string{"", "text/plain"} {
		out = storageRequest(s, "POST", "/storage/runs/prune", `{"dryRun":false}`, ct, "secret-test-token")
		if out.Code != 415 {
			t.Fatalf("content type=%s status=%d", ct, out.Code)
		}
	}
	for _, body := range []string{`{}`, `{"dryRun":false,"unknown":1}`, `{"dryRun":true} {}`, `{"dryRun":"true"}`} {
		if out = req("POST", "/storage/runs/prune", body); out.Code != 400 {
			t.Fatalf("bad input %s status=%d", body, out.Code)
		}
	}
	before, _ := s.store.Usage(now)
	out = req("POST", "/storage/runs/prune", `{"dryRun":true}`)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	var plan store.PruneResult
	json.Unmarshal(out.Body.Bytes(), &plan)
	after, _ := s.store.Usage(now)
	if !reflect.DeepEqual(before, after) || plan.FilesDeleted != 1 || plan.FilesCompressed != 2 {
		t.Fatal("dry plan", plan)
	}
	var state pruneState
	s.store.ReadState("run-prune", &state)
	if state.Last != nil {
		t.Fatal("preview became last prune")
	}
	out = req("POST", "/storage/runs/prune", `{"dryRun":false}`)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	out = req("GET", "/storage/runs", "")
	json.Unmarshal(out.Body.Bytes(), &state)
	if state.Last == nil || state.Last.FilesDeleted != 1 || state.Last.Actions != nil {
		t.Fatalf("last result %+v", state)
	}
	if out = req("DELETE", "/storage/runs/active", ""); out.Code != 409 {
		t.Fatalf("forgot fresh host: %d", out.Code)
	}
	if out = req("DELETE", "/storage/runs/stale", ""); out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	if out = req("DELETE", "/storage/runs/missing", ""); out.Code != 404 {
		t.Fatal("missing status", out.Code)
	}
	if out = req("PUT", "/settings", `{"run_retention":{"keepAllDays":0,"hourlyDays":0,"dailyDays":0,"compressAfterDays":0}}`); out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	if s.effective().RunRetention != (store.Policy{}) {
		t.Fatal("zero settings lost")
	}
	if out = req("PUT", "/settings", `{"run_retention":{"dailyDays":-1}}`); out.Code != 400 {
		t.Fatal("negative accepted")
	}
}
func TestGzippedAPIHistoryComposeDriftAndTrend(t *testing.T) {
	s := testServer(t, "")
	now := time.Now().UTC()
	for i, hash := range []string{"a", "b"} {
		inv := &inventory.Inventory{SchemaVersion: inventory.SchemaVersion, Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now.AddDate(0, 0, -10+i), Docker: &inventory.Docker{Available: true, ComposeProjects: []inventory.ComposeProject{{Name: "app", ConfigFiles: []inventory.ComposeFile{{Path: "/compose.yml", Exists: true, SHA256: hash, Content: "services: " + hash}}}}}}
		if _, err := s.store.Save(inv); err != nil {
			t.Fatal(err)
		}
	}
	hist, _ := s.store.History("host-a")
	paths := []string{"/hosts", "/hosts/host-a", "/hosts/host-a?at=" + hist[1].ID, "/hosts/host-a/compose", "/hosts/host-a/compose/file?path=/compose.yml&project=app&run=" + hist[1].ID, "/hosts/host-a/drift?from=" + hist[1].ID + "&to=" + hist[0].ID, "/hosts/host-a/trend"}
	before := make(map[string][]byte)
	for _, path := range paths {
		r := storageRequest(s, "GET", path, "", "", "")
		if r.Code != 200 {
			t.Fatalf("%s: %d %s", path, r.Code, r.Body)
		}
		before[path] = r.Body.Bytes()
	}
	if _, err := s.store.Prune(store.DefaultPolicy(), now, false); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		r := storageRequest(s, "GET", path, "", "", "")
		if r.Code != 200 {
			t.Fatalf("gzip %s: %s", path, r.Body)
		}
		// Hosts/trend carry current time or freshness values. Compare the exact
		// historical payload for compose, drift, and At/Latest endpoints.
		if path != "/hosts" && path != "/hosts/host-a/trend" && !bytes.Equal(before[path], r.Body.Bytes()) {
			t.Fatalf("gzip changed %s", path)
		}
	}
	r := storageRequest(s, "GET", "/hosts/host-a/history", "", "", "")
	if r.Code != 200 || !bytes.Contains(r.Body.Bytes(), []byte(`"compressed": true`)) {
		t.Fatal("history missing compression", r.Body.String())
	}
}
func TestRetentionEnvironment(t *testing.T) {
	t.Setenv("RUN_KEEP_ALL_DAYS", "3")
	t.Setenv("RUN_HOURLY_DAYS", "10")
	t.Setenv("RUN_DAILY_DAYS", "0")
	t.Setenv("RUN_COMPRESS_AFTER_DAYS", "1")
	p, err := retentionEnv()
	if err != nil || p != (store.Policy{KeepAllDays: 3, HourlyDays: 10, DailyDays: 0, CompressAfterDays: 1}) {
		t.Fatal(p, err)
	}
	t.Setenv("RUN_DAILY_DAYS", "2")
	if _, err := retentionEnv(); err == nil {
		t.Fatal("invalid env accepted")
	}
	t.Setenv("RUN_DAILY_DAYS", "bad")
	if _, err := retentionEnv(); err == nil {
		t.Fatal("invalid integer accepted")
	}
}
