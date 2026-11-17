package cve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type testHost struct {
	dir    string
	now    time.Time
	client *http.Client
}

func (h *testHost) HTTP() *http.Client  { return h.client }
func (h *testHost) CacheDir() string    { return h.dir }
func (h *testHost) Now() time.Time      { return h.now }
func (h *testHost) Logf(string, ...any) {}

const primary = "CVE-2024-5535"
const secondary = "CVE-2025-10001"
const third = "CVE-2025-10002"

func fixtureHosts(now time.Time) []plugin.PackageHost {
	deb := plugin.PackageSet{Scope: "host", Manager: "dpkg", Status: "ok", Containers: []string{}, Distro: plugin.PackageDistro{ID: "debian", VersionID: "12", Codename: "bookworm"}, Packages: []plugin.Package{
		{Name: "openssl", Version: "3.0.11-1~deb12u2"}, {Name: "libssl3", Version: "3.0.11-1~deb12u2+b1", Source: "openssl", SourceVersion: "3.0.11-1~deb12u2"},
	}}
	alp := plugin.PackageSet{Scope: "image:sha256:alpine", ImageRef: "example/alpine:3.19", Containers: []string{"web", "worker"}, Manager: "apk", Status: "ok", Distro: plugin.PackageDistro{ID: "alpine", VersionID: "3.19.1"}, Packages: []plugin.Package{{Name: "libssl3", Version: "3.1.4-r0", Source: "openssl"}}}
	ub := plugin.PackageSet{Scope: "host", Manager: "dpkg", Status: "ok", Containers: []string{}, Distro: plugin.PackageDistro{ID: "ubuntu", VersionID: "22.04"}, Packages: []plugin.Package{{Name: "openssl", Version: "3.0.2-0ubuntu1.10"}}}
	debImage := deb
	debImage.Scope = "image:sha256:debian"
	debImage.ImageRef = "example/debian:12"
	debImage.Containers = []string{"db"}
	return []plugin.PackageHost{{Host: "host-a", CollectedAt: now, Sets: []plugin.PackageSet{deb, alp}}, {Host: "host-b", CollectedAt: now, Sets: []plugin.PackageSet{ub, debImage}}, {Host: "host-c", CollectedAt: now, Sets: []plugin.PackageSet{{Scope: "host", Status: "ok", Distro: plugin.PackageDistro{ID: "fedora", VersionID: "40"}}}}}
}

type fixtureServer struct {
	mu             sync.Mutex
	counts         map[string]int
	queries        []query
	modified       string
	failDetails    bool
	failEnrichment bool
	server         *httptest.Server
}

func newFixture(t *testing.T) (*Integration, *testHost, *fixtureServer) {
	t.Helper()
	f := &fixtureServer{counts: map[string]int{}, modified: "2026-11-01T00:00:00Z"}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	h := &testHost{dir: t.TempDir(), now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC), client: f.server.Client()}
	p := New(h).(*Integration)
	if err := p.Configure(plugin.Config{"osv_url": f.server.URL, "kev_url": f.server.URL + "/kev", "epss_url": f.server.URL + "/epss"}); err != nil {
		t.Fatal(err)
	}
	p.SetPackageSource(func() ([]plugin.PackageHost, error) { return fixtureHosts(h.now), nil })
	t.Cleanup(func() { _ = p.Close() })
	return p, h, f
}
func (f *fixtureServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[r.URL.Path]++
	w.Header().Set("Content-Type", "application/json")
	send := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.URL.Path {
	case "/v1/querybatch":
		var in struct {
			Queries []query `json:"queries"`
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			w.WriteHeader(400)
			return
		}
		out := batchResponse{Results: []batchResult{}}
		for _, q := range in.Queries {
			f.queries = append(f.queries, q)
			res := batchResult{}
			switch q.Package.Ecosystem {
			case "Debian:12":
				if q.PageToken == "" {
					res.Vulns = []match{{ID: "DEBIAN-" + primary, Modified: f.modified}}
					res.NextPageToken = "page2"
				} else {
					res.Vulns = []match{{ID: "DEBIAN-" + secondary, Modified: f.modified}}
				}
			case "Alpine:v3.19":
				res.Vulns = []match{{ID: "ALPINE-" + primary, Modified: f.modified}, {ID: "ALPINE-" + third, Modified: f.modified}}
			case "Ubuntu:22.04:LTS":
				res.Vulns = []match{{ID: "UBUNTU-" + primary, Modified: f.modified}}
			}
			out.Results = append(out.Results, res)
		}
		send(out)
	case "/kev":
		if f.failEnrichment {
			w.WriteHeader(503)
			return
		}
		send(map[string]any{"vulnerabilities": []any{map[string]any{"cveID": primary, "vulnerabilityName": "Example vulnerability", "dateAdded": "2026-11-01", "dueDate": "2026-11-20", "knownRansomwareCampaignUse": "Known"}}})
	case "/epss":
		if f.failEnrichment {
			w.WriteHeader(503)
			return
		}
		if len(strings.Split(r.URL.Query().Get("cve"), ",")) > 100 {
			w.WriteHeader(400)
			return
		}
		send(map[string]any{"data": []any{map[string]string{"cve": primary, "epss": "0.061", "percentile": "0.935"}}})
	default:
		if !strings.HasPrefix(r.URL.Path, "/v1/vulns/") {
			w.WriteHeader(404)
			return
		}
		if f.failDetails {
			w.WriteHeader(503)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/v1/vulns/")
		eco, fixed := "Debian:12", "3.0.15-1~deb12u1"
		if strings.HasPrefix(id, "ALPINE-") {
			eco = "Alpine:v3.19"
			fixed = "3.1.4-r1"
		}
		if strings.HasPrefix(id, "UBUNTU-") {
			eco = "Ubuntu:22.04:LTS"
			fixed = ""
		}
		canonical := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(id, "DEBIAN-"), "ALPINE-"), "UBUNTU-")
		a := advisory{ID: id, Modified: f.modified, Upstream: []string{canonical}, Summary: "Synthetic OpenSSL vulnerability", Details: strings.Repeat("detail ", 1000), Published: ptr(time.Date(2024, 6, 27, 0, 0, 0, 0, time.UTC)), References: []Reference{{Type: "ADVISORY", URL: "https://example.invalid/advisory"}}, Affected: []osvAffected{{Package: osvPackage{Name: "openssl", Ecosystem: eco}, Ranges: []osvRange{{Type: "ECOSYSTEM", Events: []osvEvent{{Introduced: "0"}, {Fixed: fixed}}}}}}, Severity: []severity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:H"}}}
		a.DatabaseSpecific.CWEIDs = []string{"CWE-125"}
		if canonical == secondary {
			a.Severity = nil
			a.Upstream = nil
			a.Aliases = []string{secondary}
			a.Affected[0].EcosystemSpecific.Urgency = "high"
			a.Affected[0].Ranges = nil
		}
		if canonical == third {
			a.Severity = []severity{{Type: "CVSS_V4", Score: "CVSS:4.0/AV:N/AC:L/AT:N/PR:N/UI:N/VC:H/VI:H/VA:H/SC:N/SI:N/SA:N"}}
			a.Affected[0].Ranges = nil
		}
		send(a)
	}
}
func scanFixture(t *testing.T, p *Integration) {
	t.Helper()
	if err := p.scan(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func getAPI(t *testing.T, p *Integration, path string) plugin.APIResponse {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.API(context.Background(), plugin.APIRequest{Method: "GET", Path: u.Path, Query: u.Query()})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func decodeAPI[T any](t *testing.T, out plugin.APIResponse) T {
	t.Helper()
	if out.Status != 200 {
		t.Fatalf("HTTP %d: %s", out.Status, out.Body)
	}
	var v T
	if err := json.Unmarshal(out.Body, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestScanGroupingFixesAndCache(t *testing.T) {
	p, h, f := newFixture(t)
	scanFixture(t, p)
	v := decodeAPI[Vulnerability](t, getAPI(t, p, "vulns/"+primary))
	if v.Severity != "critical" || v.SeveritySource != "cvss_v3" || v.CVSS.Score != 9.1 || v.Hosts != 2 || v.Sets != 4 || v.Fix != "some" || len(v.OSVIDs) != 3 || len(v.Affected) != 4 || v.Priority != 1543 {
		t.Fatalf("grouped row: %+v", v)
	}
	if v.KEV == nil || !v.KEV.Ransomware || v.EPSS == nil || v.EPSS.Score != .061 || len(v.Details) != 4000 || !reflect.DeepEqual(v.CWEIDs, []string{"CWE-125"}) {
		t.Fatalf("trimmed/enriched: %+v", v)
	}
	for _, a := range v.Affected {
		if a.Ecosystem == "Debian:12" {
			if len(a.Binaries) != 2 || a.Installed != "3.0.11-1~deb12u2" || !a.Fixable || a.Fixed == nil {
				t.Fatalf("source/binary projection: %+v", a)
			}
		}
	}
	second := decodeAPI[Vulnerability](t, getAPI(t, p, "vulns/"+secondary))
	if second.Severity != "high" || second.SeveritySource != "debian" || second.Fix != "none" {
		t.Fatalf("vendor fallback: %+v", second)
	}
	v4 := decodeAPI[Vulnerability](t, getAPI(t, p, "vulns/"+third))
	if v4.Severity != "unknown" || v4.SeveritySource != "none" || v4.CVSS == nil || v4.CVSS.Version != "4.0" {
		t.Fatalf("v4 handling: %+v", v4)
	}
	o := decodeAPI[Overview](t, getAPI(t, p, "overview"))
	if o.Totals.Vulns != 3 || o.Totals.KEV != 1 || o.Totals.Fixable != 1 || o.Totals.PackagesAffected != 6 || len(o.TopFixes) != 2 || len(o.EOL) != 1 {
		t.Fatalf("overview %+v", o)
	}
	f.mu.Lock()
	if len(f.queries) != 4 {
		t.Errorf("dedupe/pagination queries = %+v", f.queries)
	}
	before := cloneCounts(f.counts)
	f.mu.Unlock()
	scanFixture(t, p)
	f.mu.Lock()
	for path, n := range f.counts {
		if path != "/v1/querybatch" && n != before[path] {
			t.Errorf("cached path fetched again %s: %d vs %d", path, n, before[path])
		}
	}
	f.mu.Unlock()
	// A new process reloads the independent advisory cache and skips GET details.
	p2 := New(h).(*Integration)
	_ = p2.Configure(plugin.Config{"osv_url": f.server.URL, "kev_url": f.server.URL + "/kev", "epss_url": f.server.URL + "/epss"})
	p2.SetPackageSource(p.source)
	scanFixture(t, p2)
	f.mu.Lock()
	for path, n := range f.counts {
		if strings.HasPrefix(path, "/v1/vulns/") && n != before[path] {
			t.Errorf("restart refetched unchanged %s", path)
		}
	}
	f.modified = "2026-11-17T12:01:00Z"
	f.mu.Unlock()
	scanFixture(t, p2)
	f.mu.Lock()
	for path, n := range f.counts {
		if strings.HasPrefix(path, "/v1/vulns/") && n != before[path]+1 {
			t.Errorf("modified detail not refreshed %s", path)
		}
	}
	f.mu.Unlock()
}
func cloneCounts(in map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func TestFiltersFacetsSortAndPackages(t *testing.T) {
	p, _, _ := newFixture(t)
	scanFixture(t, p)
	type list struct {
		Total  int       `json:"total"`
		Offset int       `json:"offset"`
		Limit  int       `json:"limit"`
		Facets facets    `json:"facets"`
		Items  []VulnRow `json:"items"`
	}
	got := decodeAPI[list](t, getAPI(t, p, "vulns?severity=high&sort=id&order=asc"))
	if got.Total != 1 || got.Items[0].ID != secondary || got.Facets.Severity.Critical != 1 || got.Facets.Severity.Unknown != 1 || got.Facets.KEV != 1 {
		t.Fatalf("severity facets: %+v", got)
	}
	for _, tt := range []struct {
		path  string
		total int
		id    string
	}{
		{"vulns?kev=1&fix=1", 1, primary}, {"vulns?host=host-b&set=image:sha256:alpine", 0, ""}, {"vulns?host=host-b&ecosystem=Ubuntu:22.04:LTS", 1, primary}, {"vulns?q=libssl3&sort=severity&order=asc", 3, third}, {"vulns?package=libssl3&sort=epss", 3, primary}, {"vulns?sort=id&order=asc&offset=1&limit=1", 3, secondary}, {"vulns?offset=999999&limit=999", 3, ""}, {"vulns?ecosystem=Alpine:v3.19", 2, primary},
	} {
		out := decodeAPI[list](t, getAPI(t, p, tt.path))
		if out.Total != tt.total || tt.id != "" && (len(out.Items) == 0 || out.Items[0].ID != tt.id) {
			t.Errorf("%s: %+v", tt.path, out)
		}
		if out.Limit > 200 {
			t.Fatal("limit unbounded")
		}
	}
	for _, path := range []string{"vulns?severity=bogus", "vulns?limit=0", "vulns?offset=-1", "vulns?sort=wat", "packages?sort=wat", "packages?order=wat", "vulns?suppressed=wat"} {
		if out := getAPI(t, p, path); out.Status != 400 {
			t.Errorf("%s: %d", path, out.Status)
		}
	}
	type packages struct {
		Total int          `json:"total"`
		Items []PackageRow `json:"items"`
	}
	pkgs := decodeAPI[packages](t, getAPI(t, p, "packages?host=host-a&sort=priority"))
	if pkgs.Total != 2 {
		t.Fatalf("packages: %+v", pkgs)
	}
	for _, row := range pkgs.Items {
		if row.Name != "openssl" || row.Hosts != 1 || row.Sets != 1 || row.FixVersion == nil || row.Fixes != 1 || row.Fixable != 1 || row.KEV != 1 || row.Top[0] != primary {
			t.Errorf("package row: %+v", row)
		}
	}
	if out := getAPI(t, p, "vulns/CVE-1900-0000"); out.Status != 404 {
		t.Fatal(out)
	}
	type hosts struct {
		Items []HostRow `json:"items"`
	}
	hh := decodeAPI[hosts](t, getAPI(t, p, "hosts"))
	if len(hh.Items) != 5 || hh.Items[0].Scope != "host" || hh.Items[1].EOL == nil || hh.Items[4].Status != "unsupported" || hh.Items[4].Reason == nil {
		t.Fatalf("hosts: %+v", hh)
	}
}
func TestSuppressionsExpiryPersistenceAndAlerts(t *testing.T) {
	p, h, _ := newFixture(t)
	scanFixture(t, p)
	api := func(method, body string) plugin.APIResponse {
		out, _ := p.API(context.Background(), plugin.APIRequest{Method: method, Path: "suppressions/" + primary, Actor: "admin:test", Body: []byte(body)})
		return out
	}
	for _, body := range []string{`{"state":"other","reason":"x"}`, `{"state":"accepted","reason":" "}`, `{"state":"accepted","reason":"x","until":"tomorrow"}`, `{"state":"accepted","reason":"x","extra":true}`, fmt.Sprintf(`{"state":"accepted","reason":%q}`, strings.Repeat("x", 501))} {
		if out := api("PUT", body); out.Status != 400 {
			t.Fatal(string(out.Body))
		}
	}
	row := decodeAPI[VulnRow](t, api("PUT", `{"state":"accepted","reason":"Reviewed configuration","until":"2026-11-17"}`))
	if row.Suppressed == nil || row.Suppressed.By != "admin:test" || row.Suppressed.Until == nil {
		t.Fatal(row)
	}
	o := decodeAPI[Overview](t, getAPI(t, p, "overview"))
	if o.Totals.Vulns != 2 || o.Totals.Suppressed != 1 || o.Totals.KEV != 0 || o.Totals.Fixable != 0 {
		t.Fatal(o)
	}
	var list struct {
		Total int       `json:"total"`
		Items []VulnRow `json:"items"`
	}
	list = decodeAPI[struct {
		Total int       `json:"total"`
		Items []VulnRow `json:"items"`
	}](t, getAPI(t, p, "vulns?suppressed=1"))
	if list.Total != 1 || list.Items[0].ID != primary {
		t.Fatal(list)
	}
	alerts, err := p.Alerts(context.Background())
	if err != nil || len(alerts) != 0 {
		t.Fatal(alerts, err)
	}
	p2 := New(h).(*Integration)
	if p2.suppressions[primary].By != "admin:test" {
		t.Fatal("suppression not persisted")
	}
	h.now = h.now.Add(24 * time.Hour)
	alerts, err = p.Alerts(context.Background())
	if err != nil || len(alerts) == 0 {
		t.Fatal(alerts, err)
	}
	for _, a := range alerts {
		if a.Rule != "kev" || a.Severity != "critical" || !strings.HasPrefix(a.Summary, a.Subject+": 1 CVE known exploited, 1 fixable ("+primary+")") || a.Detail != primary {
			t.Fatalf("%+v", a)
		}
	}
	row = decodeAPI[VulnRow](t, api("DELETE", ""))
	if row.Suppressed != nil {
		t.Fatal(row)
	}
	info, err := os.Stat(filepath.Join(h.dir, "suppressions.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}
func TestEnrichmentFailuresRetainPriorAndDailyLimit(t *testing.T) {
	p, h, f := newFixture(t)
	scanFixture(t, p)
	h.now = h.now.Add(25 * time.Hour)
	f.mu.Lock()
	f.failEnrichment = true
	f.mu.Unlock()
	scanFixture(t, p)
	v := decodeAPI[Vulnerability](t, getAPI(t, p, "vulns/"+primary))
	if v.KEV == nil || v.EPSS == nil {
		t.Fatal("lost prior enrichment")
	}
	status := decodeAPI[Status](t, getAPI(t, p, "status"))
	if status.Sources.KEV.OK || status.Sources.EPSS.OK || status.Sources.KEV.Error == nil || status.Sources.OSV.Error != nil {
		t.Fatal(status.Sources)
	}
	f.mu.Lock()
	before := cloneCounts(f.counts)
	f.mu.Unlock()
	scanFixture(t, p)
	f.mu.Lock()
	if f.counts["/kev"] != before["/kev"] || f.counts["/epss"] != before["/epss"] {
		t.Fatal("failed feeds retried within daily window")
	}
	f.mu.Unlock()
	// The same limit survives a process restart.
	p2 := New(h).(*Integration)
	_ = p2.Configure(p.configFixture(f))
	p2.SetPackageSource(p.source)
	scanFixture(t, p2)
	f.mu.Lock()
	if f.counts["/kev"] != before["/kev"] || f.counts["/epss"] != before["/epss"] {
		t.Fatal("failed feeds retried on restart")
	}
	f.mu.Unlock()
}
func (p *Integration) configFixture(f *fixtureServer) plugin.Config {
	return plugin.Config{"osv_url": f.server.URL, "kev_url": f.server.URL + "/kev", "epss_url": f.server.URL + "/epss"}
}
func TestWorkerScanAndPersistedResult(t *testing.T) {
	p, h, f := newFixture(t)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, p)
	status := decodeAPI[Status](t, getAPI(t, p, "status"))
	if status.State != "idle" || status.LastScan.FinishedAt == nil || status.NextScanAt == nil || status.Coverage.SetsScanned != 4 {
		t.Fatalf("status: %+v", status)
	}
	_ = p.Close()
	p2 := New(h).(*Integration)
	o := decodeAPI[Overview](t, getAPI(t, p2, "overview"))
	if o.GeneratedAt == nil || o.Totals.Vulns != 3 {
		t.Fatalf("reload: %+v", o)
	}
	f.mu.Lock()
	before := cloneCounts(f.counts)
	f.mu.Unlock()
	for _, path := range []string{"status", "overview", "hosts", "packages", "vulns", "vulns/" + primary} {
		getAPI(t, p2, path)
	}
	_, _ = p2.Alerts(context.Background())
	f.mu.Lock()
	if !reflect.DeepEqual(before, f.counts) {
		t.Fatal("API or Alerts performed network IO")
	}
	f.mu.Unlock()
	if err := p.Start(); err != nil {
		t.Fatal("restart", err)
	}
	awaitIdle(t, p)
	out, _ := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "scan"})
	if out.Status != 202 {
		t.Fatal(out)
	}
	out, _ = p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "scan"})
	if out.Status != 409 {
		t.Fatal(out)
	}
	awaitIdle(t, p)
}
func awaitIdle(t *testing.T, p *Integration) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		p.mu.RLock()
		s := p.status
		p.mu.RUnlock()
		if s.State == "idle" {
			return
		}
		if s.State == "error" {
			t.Fatalf("scan failed: %+v", s.LastScan)
		}
		select {
		case <-deadline.C:
			t.Fatal("scan did not finish")
		case <-tick.C:
		}
	}
}
func TestCloseCancelsAndJoinsWorker(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(canceled)
	}))
	defer server.Close()
	h := &testHost{dir: t.TempDir(), now: time.Now(), client: server.Client()}
	p := New(h).(*Integration)
	_ = p.Configure(plugin.Config{"osv_url": server.URL, "kev_url": "", "epss_url": ""})
	p.SetPackageSource(func() ([]plugin.PackageHost, error) { return fixtureHosts(h.now), nil })
	_ = p.Start()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no scan")
	}
	done := make(chan struct{})
	go func() { _ = p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close failed to join")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("HTTP request not canceled")
	}
	p.mu.RLock()
	running := p.running
	p.mu.RUnlock()
	if running {
		t.Fatal("worker still running")
	}
}

func TestNeverStateNullsConfigureAndFallback(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	h := &testHost{dir: t.TempDir(), now: time.Now(), client: server.Client()}
	p := New(h).(*Integration)
	for _, n := range []any{0, 169, "invalid"} {
		if p.Configure(plugin.Config{"interval_hours": n}) == nil {
			t.Fatalf("accepted interval %v", n)
		}
	}
	if err := p.Configure(plugin.Config{"osv_url": server.URL, "kev_url": "", "epss_url": "", "interval_hours": 1}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("Configure did IO")
	}
	status := getAPI(t, p, "status")
	var shape map[string]any
	if json.Unmarshal(status.Body, &shape) != nil {
		t.Fatal("invalid JSON")
	}
	for _, key := range []string{"phase", "progress", "last_scan", "next_scan_at"} {
		if value, exists := shape[key]; !exists || value != nil {
			t.Errorf("%s must be explicit null: %s", key, status.Body)
		}
	}
	if shape["state"] != "never" {
		t.Fatal(shape)
	}
	overview := getAPI(t, p, "overview")
	if !strings.Contains(string(overview.Body), `"generated_at":null`) {
		t.Fatal(string(overview.Body))
	}
	for _, view := range []string{"vulns", "packages", "hosts"} {
		v, err := p.View(context.Background(), view, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plugin.ValidateView(v, p.Manifest()); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("cache reads did IO")
	}
	result := p.Test(context.Background())
	if !result.OK || calls != 1 {
		t.Fatal(result, calls)
	}
}
func TestBatchBoundsAndUnsupportedCoverage(t *testing.T) {
	sizes := []int{}
	var sizesMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Queries []query `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		sizesMu.Lock()
		sizes = append(sizes, len(in.Queries))
		sizesMu.Unlock()
		_ = json.NewEncoder(w).Encode(batchResponse{Results: make([]batchResult, len(in.Queries))})
	}))
	defer server.Close()
	h := &testHost{dir: t.TempDir(), now: time.Now(), client: server.Client()}
	p := New(h).(*Integration)
	works := []*queryWork{}
	for i := range 501 {
		works = append(works, &queryWork{Query: query{Package: osvPackage{Name: fmt.Sprintf("pkg%d", i), Ecosystem: "Debian:12"}, Version: "1"}})
	}
	if skipped, err := p.match(context.Background(), server.URL, works); err != nil || len(skipped) != 0 {
		t.Fatal(err, skipped)
	}
	if len(sizes) != 6 {
		t.Fatal(sizes)
	}
	for _, n := range sizes {
		if n > 500 {
			t.Fatal("unbounded batch", sizes)
		}
	}
	hosts := fixtureHosts(h.now)
	hosts = append(hosts, plugin.PackageHost{Host: "uncollected"}, plugin.PackageHost{Host: "partial", Sets: []plugin.PackageSet{{Scope: "host", Status: "partial", Distro: plugin.PackageDistro{ID: "debian", VersionID: "12"}}}})
	queries, r := inventoryQueries(hosts, false, h.now)
	if len(queries) != 2 || r.Coverage.SetsScanned != 2 || len(r.Coverage.Unsupported) != 5 {
		t.Fatalf("coverage %+v, queries %d", r.Coverage, len(queries))
	}
}
func TestFailedScanRetainsCompleteResultAndDetailsResume(t *testing.T) {
	p, h, f := newFixture(t)
	scanFixture(t, p)
	old := decodeAPI[Overview](t, getAPI(t, p, "overview"))
	// A missing/changed detail causes a failed scan, but never removes the last
	// complete CVE result. Already persisted detail records remain reusable.
	f.mu.Lock()
	f.modified = "2026-11-17T13:00:00Z"
	f.failDetails = true
	f.mu.Unlock()
	if err := p.scan(context.Background()); err == nil {
		t.Fatal("expected detail failure")
	}
	got := decodeAPI[Overview](t, getAPI(t, p, "overview"))
	if !reflect.DeepEqual(got, old) {
		t.Fatal("failed scan replaced complete result")
	}
	f.mu.Lock()
	f.modified = "2026-11-01T00:00:00Z"
	before := cloneCounts(f.counts)
	f.mu.Unlock()
	p2 := New(h).(*Integration)
	_ = p2.Configure(p.configFixture(f))
	p2.SetPackageSource(p.source)
	scanFixture(t, p2)
	f.mu.Lock()
	defer f.mu.Unlock()
	for path, n := range f.counts {
		if strings.HasPrefix(path, "/v1/vulns/") && n != before[path] {
			t.Errorf("unchanged detail was not resumed from disk: %s", path)
		}
	}
}

// A query OSV cannot answer (the Ubuntu kernel outruns the client timeout) is
// split out of its batch and skipped; the rest of the batch still matches.
func TestMatchSplitsFailingBatchAndSkipsOneQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Queries []query `json:"queries"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		out := batchResponse{Results: make([]batchResult, len(in.Queries))}
		for i, q := range in.Queries {
			if q.Package.Name == "linux" {
				http.Error(w, "slow", http.StatusBadGateway)
				return
			}
			out.Results[i].Vulns = []match{{ID: "DEBIAN-CVE-2024-0001", Modified: "2026-01-01T00:00:00Z"}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	h := &testHost{dir: t.TempDir(), now: time.Now(), client: server.Client()}
	p := New(h).(*Integration)
	works := []*queryWork{}
	for _, name := range []string{"bash", "linux", "openssl", "zlib"} {
		works = append(works, &queryWork{Query: query{Package: osvPackage{Name: name, Ecosystem: "Debian:12"}, Version: "1"}, Occurrences: []Affected{{Host: "a", Scope: "host"}, {Host: "a", Scope: "host"}}})
	}
	skipped, err := p.match(context.Background(), server.URL, works)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0].Package != "linux" || skipped[0].Sets != 1 {
		t.Fatal(skipped)
	}
	for _, w := range works {
		if want := w.Query.Package.Name != "linux"; (len(w.Matches) == 1) != want {
			t.Fatal(w.Query.Package.Name, w.Matches)
		}
	}
}
func TestRestartKeepsScheduleAndSettingsChangeRescans(t *testing.T) {
	p, h, f := newFixture(t)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	awaitIdle(t, p)
	_ = p.Close()

	p2 := New(h).(*Integration)
	t.Cleanup(func() { _ = p2.Close() })
	cfg := p.configFixture(f)
	if err := p2.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	p2.SetPackageSource(func() ([]plugin.PackageHost, error) { return fixtureHosts(h.now), nil })
	if err := p2.Start(); err != nil {
		t.Fatal(err)
	}
	p2.mu.RLock()
	state, next := p2.status.State, p2.status.NextScanAt
	p2.mu.RUnlock()
	if state != "idle" || next == nil || !next.After(h.now) {
		t.Fatalf("restart after a fresh scan rescanned: state %s next %v", state, next)
	}

	// The same settings again (a no-op save) keep the schedule.
	if err := p2.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	_ = p2.Start()
	p2.mu.RLock()
	state = p2.status.State
	p2.mu.RUnlock()
	if state != "idle" {
		t.Fatalf("unchanged settings rescanned: %s", state)
	}

	cfg["include_images"] = false
	if err := p2.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	_ = p2.Start()
	p2.mu.RLock()
	state = p2.status.State
	p2.mu.RUnlock()
	if state != "scanning" {
		t.Fatalf("changed settings did not rescan: %s", state)
	}
	awaitIdle(t, p2)
}

func TestAlertsGroupByHost(t *testing.T) {
	h := &testHost{dir: t.TempDir(), now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}
	known := map[string]KEV{}
	for _, id := range []string{"CVE-2026-0001", "CVE-2026-0002", "CVE-2026-0003", "CVE-2026-0004"} {
		known[id] = KEV{}
	}
	p := &Integration{host: h, cfg: config{KEV: "https://kev.example", AlertKEV: true, AlertCritical: true}, suppressions: map[string]Suppression{}, enrich: enrichment{KEV: known, EPSS: map[string]EPSS{}}}
	affect := func(fixable bool, hosts ...string) []Affected {
		out := []Affected{}
		for _, h := range hosts {
			out = append(out, Affected{Host: h, Fixable: fixable})
		}
		return out
	}
	for _, id := range []string{"CVE-2026-0001", "CVE-2026-0002", "CVE-2026-0003"} {
		p.result.Vulns = append(p.result.Vulns, Vulnerability{VulnRow: VulnRow{ID: id, Severity: "high", Fix: "full"}, Affected: affect(true, "plex", "pve")})
	}
	p.result.Vulns = append(p.result.Vulns,
		Vulnerability{VulnRow: VulnRow{ID: "CVE-2026-0004", Severity: "high", Fix: "none"}, Affected: affect(false, "truenas")},
		Vulnerability{VulnRow: VulnRow{ID: "CVE-2026-0005", Severity: "critical", Fix: "full"}, Affected: affect(true, "plex")},
		Vulnerability{VulnRow: VulnRow{ID: "CVE-2026-0006", Severity: "critical", Fix: "none"}, Affected: affect(false, "plex")})
	got := map[string]plugin.Alert{}
	alerts, err := p.Alerts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		got[a.Rule+"/"+a.Subject] = a
	}
	if len(got) != 4 {
		t.Fatalf("%+v", alerts)
	}
	if a := got["kev/plex"]; a.Severity != "critical" || a.Summary != "plex: 3 CVEs known exploited, 3 fixable (CVE-2026-0001, CVE-2026-0002 +1)" {
		t.Fatalf("%+v", a)
	}
	if a := got["kev/truenas"]; a.Severity != "warning" || a.Summary != "truenas: 1 CVE known exploited, 0 fixable (CVE-2026-0004)" {
		t.Fatalf("%+v", a)
	}
	if a := got["critical/plex"]; a.Severity != "warning" || a.Summary != "plex: 1 critical CVE with a fix available (CVE-2026-0005)" {
		t.Fatalf("%+v", a)
	}
	if _, ok := got["kev/pve"]; !ok {
		t.Fatal("pve missing")
	}
}
