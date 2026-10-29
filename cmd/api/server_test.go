package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func testServer(t *testing.T, token string) *server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: st, log: log.New(io.Discard, "", 0), token: token}
}

func testInventoryJSON(host string) []byte {
	inv := inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host:          inventory.Host{Hostname: host, OS: inventory.OSRelease{Pretty: "Debian GNU/Linux 12"}},
		Network: &inventory.Network{
			EphemeralRange: [2]int{32768, 60999},
			Listeners: []inventory.Listener{
				{Protocol: "tcp", Port: 22, Address: "0.0.0.0", Exposed: true, Process: "sshd"},
			},
		},
	}
	data, _ := json.Marshal(inv)
	return data
}

func doRequest(t *testing.T, h http.Handler, method, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestIngestThenLatest(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	rec := doRequest(t, h, "POST", "/inventory", testInventoryJSON("node1"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /inventory: status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = doRequest(t, h, "GET", "/hosts/node1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hosts/node1: status = %d, body = %s", rec.Code, rec.Body)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(rec.Body.Bytes(), &inv); err != nil {
		t.Fatal(err)
	}
	if inv.Host.Hostname != "node1" {
		t.Errorf("hostname = %q, want node1", inv.Host.Hostname)
	}
}

func TestIngestRejectsMissingHostname(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	rec := doRequest(t, h, "POST", "/inventory", []byte(`{"schema_version":"5"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
}

func TestIngestRejectsGarbageBody(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	rec := doRequest(t, h, "POST", "/inventory", []byte(`not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
	}
}

func TestUnknownHostIs404(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	rec := doRequest(t, h, "GET", "/hosts/ghost", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestHostsListAndPorts(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	for _, host := range []string{"node1", "host-b"} {
		rec := doRequest(t, h, "POST", "/inventory", testInventoryJSON(host))
		if rec.Code != http.StatusCreated {
			t.Fatalf("seeding %s: status = %d, body = %s", host, rec.Code, rec.Body)
		}
	}

	rec := doRequest(t, h, "GET", "/hosts", nil)
	var hosts []hostSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 {
		t.Fatalf("len(hosts) = %d, want 2", len(hosts))
	}

	rec = doRequest(t, h, "GET", "/ports", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ports: status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = doRequest(t, h, "GET", "/ports?host=node1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ports?host=node1: status = %d", rec.Code)
	}

	rec = doRequest(t, h, "GET", "/ports?host=nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /ports?host=nope: status = %d, want 404", rec.Code)
	}
}

func TestHistoryAndAtLookup(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	doRequest(t, h, "POST", "/inventory", testInventoryJSON("host-a"))

	rec := doRequest(t, h, "GET", "/hosts/host-a/history", nil)
	var records []store.Record
	if err := json.Unmarshal(rec.Body.Bytes(), &records); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1", len(records))
	}

	rec = doRequest(t, h, "GET", "/hosts/host-a?at="+records[0].ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /hosts/host-a?at=...: status = %d, body = %s", rec.Code, rec.Body)
	}

	rec = doRequest(t, h, "GET", "/hosts/host-a?at=garbage", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET with malformed at: status = %d, want 400", rec.Code)
	}
}

func TestAuthRequiredWhenTokenSet(t *testing.T) {
	t.Parallel()
	s := testServer(t, "s3cret")
	h := s.routes()

	rec := doRequest(t, h, "GET", "/hosts", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest("GET", "/hosts", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest("GET", "/hosts", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200", rec.Code)
	}
}

func TestAlerts(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	// A host with one genuinely broken thing: the cancelled scrub this
	// homelab actually has on media and secure.
	inv := inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   time.Now(),
		Host:          inventory.Host{Hostname: "host-c"},
		ZFS: &inventory.ZFS{
			Available: true,
			Pools: []inventory.ZPool{{
				Name: "media", Health: "ONLINE", CapacityPct: 76,
				Scan:   "scrub canceled on Sun Jul 20 17:06:06 2025",
				Errors: "No known data errors",
			}},
		},
	}
	body, _ := json.Marshal(inv)
	if rec := doRequest(t, h, "POST", "/inventory", body); rec.Code != http.StatusCreated {
		t.Fatalf("seeding: status = %d, body = %s", rec.Code, rec.Body)
	}

	rec := doRequest(t, h, "GET", "/alerts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /alerts: status = %d, body = %s", rec.Code, rec.Body)
	}
	var rep alert.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Counts.Total == 0 {
		t.Fatal("expected at least the cancelled-scrub alert")
	}
	var found bool
	for _, a := range rep.Alerts {
		if a.Rule == "zfs.pool.scrub" && a.Host == "host-c" {
			found = true
		}
	}
	if !found {
		t.Errorf("no zfs.pool.scrub alert in %+v", rep.Alerts)
	}

	// Filters.
	if rec := doRequest(t, h, "GET", "/alerts?host=host-c", nil); rec.Code != http.StatusOK {
		t.Errorf("?host=host-c: status = %d", rec.Code)
	}
	if rec := doRequest(t, h, "GET", "/alerts?host=nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("?host=nope: status = %d, want 404", rec.Code)
	}
	if rec := doRequest(t, h, "GET", "/alerts?level=critical", nil); rec.Code != http.StatusOK {
		t.Errorf("?level=critical: status = %d", rec.Code)
	}
	if rec := doRequest(t, h, "GET", "/alerts?level=bogus", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("?level=bogus: status = %d, want 400", rec.Code)
	}
}

// The dashboard has to be reachable without a token: a browser cannot put an
// Authorization header on a plain navigation, so gating the HTML would leave
// no way to load it at all. It carries no inventory data — only the code that
// asks for it, which does send the token.
func TestDashboardIsServedAndCarriesNoData(t *testing.T) {
	t.Parallel()
	// Unauthenticated server, so the seed actually lands: otherwise the
	// "page contains no host data" assertion below would pass for the boring
	// reason that there was never any data to leak.
	s := testServer(t, "")
	h := s.routes()

	if rec := doRequest(t, h, "POST", "/inventory", testInventoryJSON("secret-host")); rec.Code != http.StatusCreated {
		t.Fatalf("seeding: status = %d, body = %s", rec.Code, rec.Body)
	}
	if rec := doRequest(t, h, "GET", "/hosts", nil); !strings.Contains(rec.Body.String(), "secret-host") {
		t.Fatal("seed did not land; the leak check below would be vacuous")
	}

	rec := doRequest(t, h, "GET", "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<title>homelab-inventory</title>") {
		t.Error("response does not look like the dashboard")
	}
	// No inventory should be inlined into the page.
	if strings.Contains(body, "secret-host") {
		t.Error("dashboard HTML contains host data; it must fetch, not embed")
	}
	// Self-contained: nothing may be loaded from another origin. See
	// webui.TestSelfContained for why this checks loads rather than schemes —
	// the launcher renders links to your own services at runtime.
	for _, bad := range []string{`<script src`, `<link rel="stylesheet"`, "@import", "//cdn"} {
		if strings.Contains(body, bad) {
			t.Errorf("dashboard loads something external (%q); it must be self-contained", bad)
		}
	}
}

func TestUnknownPathIs404(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	if rec := doRequest(t, h, "GET", "/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope: status = %d, want 404", rec.Code)
	}
}

func TestHealthzNeverRequiresAuth(t *testing.T) {
	t.Parallel()
	s := testServer(t, "s3cret")
	h := s.routes()

	rec := doRequest(t, h, "GET", "/healthz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// Notifications are optional. With APPRISE_URL unset the endpoints must say so
// plainly rather than 500 or pretend to work.
func TestNotifyEndpointsWhenUnconfigured(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()

	rec := doRequest(t, h, "GET", "/notify", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /notify: status = %d, want 200", rec.Code)
	}
	var st notify.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Enabled {
		t.Error("notifications report as enabled with no notifier configured")
	}

	rec = doRequest(t, h, "POST", "/notify/test", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /notify/test: status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "APPRISE_URL") {
		t.Errorf("the 503 does not say what to set: %s", rec.Body)
	}
}

func TestNotifyStatusNeverLeaksTheAppriseURL(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	const secret = "http://apprise.invalid:3010/notify/s3cretkey"
	s.notifier = &notify.Notifier{
		State:  s.store,
		Sender: notify.NewApprise(secret),
		Min:    alert.LevelCritical,
	}
	h := s.routes()

	rec := doRequest(t, h, "GET", "/notify", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The path segment after /notify/ is the credential — the whole URL is.
	for _, bad := range []string{"s3cretkey", "apprise.invalid", secret} {
		if strings.Contains(rec.Body.String(), bad) {
			t.Errorf("GET /notify leaked %q: %s", bad, rec.Body)
		}
	}
}

func TestNotifyEndpointsRequireAuth(t *testing.T) {
	t.Parallel()
	s := testServer(t, "s3cret")
	h := s.routes()

	for _, tc := range []struct{ method, path string }{
		{"GET", "/notify"},
		{"POST", "/notify/test"},
	} {
		rec := doRequest(t, h, tc.method, tc.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// One run whose port listing failed must not mark the host's services as gone:
// the catalog uses the host's last run that has a listing.
func TestCatalogKeepsLastNetworkAfterFailedScan(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	var good inventory.Inventory
	if err := json.Unmarshal(testInventoryJSON("host-a"), &good); err != nil {
		t.Fatal(err)
	}
	good.CollectedAt = time.Now().Add(-15 * time.Minute)
	failed := good
	failed.CollectedAt = time.Now()
	failed.Network = nil
	for _, inv := range []*inventory.Inventory{&good, &failed} {
		if _, err := s.store.Save(inv); err != nil {
			t.Fatal(err)
		}
	}
	invs, err := s.store.LatestAll()
	if err != nil || len(invs) != 1 || invs[0].Network != nil {
		t.Fatalf("fixture: latest run should have no network: %v", err)
	}
	patched := s.withLastNetwork(invs)
	if patched[0].Network == nil || len(patched[0].Network.Listeners) != 1 || !patched[0].CollectedAt.Equal(failed.CollectedAt) {
		t.Fatalf("last listing not carried over: %+v", patched[0].Network)
	}
	if invs[0].Network != nil {
		t.Fatal("withLastNetwork modified the stored inventory")
	}

	// Older agents stored an empty list plus an error diagnostic instead.
	old := failed
	old.CollectedAt = time.Now().Add(time.Minute)
	old.Network = &inventory.Network{Listeners: []inventory.Listener{}}
	old.Diagnostics = []inventory.Diagnostic{{Collector: "network", Severity: inventory.SeverityError, Message: "collector failed"}}
	if _, err := s.store.Save(&old); err != nil {
		t.Fatal(err)
	}
	invs, _ = s.store.LatestAll()
	if patched = s.withLastNetwork(invs); len(patched[0].Network.Listeners) != 1 {
		t.Fatalf("empty listing with a network error not replaced: %+v", patched[0].Network)
	}
}

func TestHostsAgentRevision(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		revision int
		outdated bool
	}{
		{"pre-revision", 0, true},
		{"lower", inventory.AgentRevision - 1, true},
		{"current", inventory.AgentRevision, false},
		{"newer", inventory.AgentRevision + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := testServer(t, "")
			h := s.routes()
			var inv inventory.Inventory
			if err := json.Unmarshal(testInventoryJSON("host-a"), &inv); err != nil {
				t.Fatal(err)
			}
			inv.Tool = inventory.Tool{Version: "a97d276", Revision: tt.revision}
			body, err := json.Marshal(inv)
			if err != nil {
				t.Fatal(err)
			}
			if rec := doRequest(t, h, "POST", "/inventory", body); rec.Code != http.StatusCreated {
				t.Fatalf("POST /inventory: status = %d, body = %s", rec.Code, rec.Body)
			}
			rec := doRequest(t, h, "GET", "/hosts", nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("GET /hosts: status = %d, body = %s", rec.Code, rec.Body)
			}
			var hosts []hostSummary
			if err := json.Unmarshal(rec.Body.Bytes(), &hosts); err != nil {
				t.Fatal(err)
			}
			if len(hosts) != 1 {
				t.Fatalf("got %d hosts, want one", len(hosts))
			}
			host := hosts[0]
			if host.AgentVersion != inv.Tool.Version || host.AgentRevision != tt.revision || host.AgentOutdated != tt.outdated {
				t.Errorf("host agent fields = %+v, want version %s, revision %d, outdated %t", host, inv.Tool.Version, tt.revision, tt.outdated)
			}
			// The detail endpoint already exposes Tool; keep its provenance intact.
			rec = doRequest(t, h, "GET", "/hosts/host-a", nil)
			var latest inventory.Inventory
			if err := json.Unmarshal(rec.Body.Bytes(), &latest); err != nil {
				t.Fatal(err)
			}
			if latest.Tool.Version != inv.Tool.Version || latest.Tool.Revision != tt.revision {
				t.Errorf("detail tool = %+v, want %+v", latest.Tool, inv.Tool)
			}
		})
	}
}

// Display filters must never make an accepted warning look cleared or remove
// another host's units from the fleet review. Notification counts stay unchanged.
func TestAlertsTriageUsesUnfilteredSnapshot(t *testing.T) {
	t.Parallel()
	s := testServer(t, "")
	h := s.routes()
	now := time.Now()
	for _, host := range []string{"host-d", "host-e"} {
		inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: now, Host: inventory.Host{Hostname: host}, Systemd: &inventory.Systemd{Available: true, Units: []inventory.Unit{{Name: "logrotate.service", Active: "failed", Sub: "failed", Description: "Rotate log files"}}, Timers: []inventory.Timer{}}}
		b, _ := json.Marshal(inv)
		if rec := doRequest(t, h, "POST", "/inventory", b); rec.Code != http.StatusCreated {
			t.Fatal(rec.Body.String())
		}
	}
	b := []byte(`{"host":"host-d","rule":"systemd.unit.failed","subject":"logrotate.service","reason":"Accepted for test"}`)
	if rec := doRequest(t, h, "POST", "/silences", b); rec.Code != http.StatusCreated {
		t.Fatal(rec.Body.String())
	}
	rec := doRequest(t, h, "GET", "/alerts?level=critical&host=host-e", nil)
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
	var rep struct {
		alert.Report
		Triage struct {
			Hosts []struct {
				Host        string
				FailedCount int `json:"failed_count"`
			}
			Silences []struct{ Condition string }
			Counts   struct {
				FailedUnits int `json:"failed_units"`
				StillTrue   int `json:"still_true"`
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Alerts) != 0 || rep.Counts.Total != 0 {
		t.Fatal("existing filtered alert report changed")
	}
	if len(rep.Triage.Hosts) != 2 || len(rep.Triage.Silences) != 1 || rep.Triage.Silences[0].Condition != "still_true" || rep.Triage.Counts.FailedUnits != 2 || rep.Triage.Counts.StillTrue != 1 {
		t.Fatalf("filtered review: %+v", rep.Triage)
	}
}

// Removed feature paths must never fall through to a callable handler or the UI.
func TestRemovedFeatureRoutesAreNotServed(t *testing.T) {
	s := testServer(t, "test-token")
	h := s.routes()
	for _, path := range []string{
		"/ext/tautulli/activity", "/tv/media", "/taste/picks", "/tdarr/status",
		"/archive", "/upload", "/network", "/seasongate",
		"/digest/send", "/exec",
	} {
		for _, method := range []string{"GET", "POST"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer test-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s still served: %d", method, path, rec.Code)
			}
		}
	}
}

func TestBrowserLoginStatusIsDisabled(t *testing.T) {
	s := testServer(t, "test-token")
	rec := doRequest(t, s.routes(), "GET", "/auth/status", nil)
	var status map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &status) != nil || status["enabled"] != false || status["signed_in"] != false {
		t.Fatalf("login status: %d %s", rec.Code, rec.Body)
	}
}
