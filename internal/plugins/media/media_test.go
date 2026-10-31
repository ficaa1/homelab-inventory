package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// Synthetic fixtures pin Arr v3's paged queue/history and embedded titles,
// Tautulli v2's envelope/string numbers and Overseerr v1's request/detail join.
type fixture struct {
	t          *testing.T
	now        time.Time
	mu         sync.Mutex
	calls      map[string]int
	bad        atomic.Bool
	recovered  atomic.Bool
	pageTwo    atomic.Bool
	incomplete atomic.Bool
}

func (f *fixture) count(path string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[path] }
func (f *fixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.URL.Path]++
	f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.t.Error("upstream mutation attempted")
		w.WriteHeader(405)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	svc, path := parts[0], "/"+parts[1]
	if svc == "tautulli" {
		if r.URL.Query().Get("apikey") != "fixture-key" || r.Header.Get("X-Api-Key") != "" {
			f.t.Error("incorrect Tautulli authentication")
		}
	} else if r.Header.Get("X-Api-Key") != "fixture-key" || r.URL.Query().Has("apikey") {
		f.t.Error("incorrect header authentication")
	}
	if svc == "radarr" && f.bad.Load() {
		w.WriteHeader(503)
		fmt.Fprint(w, "sensitive upstream diagnostic fixture-key")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch path {
	case "/api/v3/system/status", "/api/v1/status":
		fmt.Fprint(w, `{"version":"1.2.3"}`)
	case "/api/v2":
		var data any
		switch r.URL.Query().Get("cmd") {
		case "get_tautulli_info":
			data = map[string]any{"tautulli_version": "v2.15.0"}
		case "get_activity":
			data = map[string]any{"stream_count": "1", "stream_count_transcode": "1", "total_bandwidth": "8000", "sessions": []any{map[string]any{"title": "Sample episode", "grandparent_title": "Example series", "friendly_name": "private-viewer", "ip_address": "192.0.2.90", "player": "private-device", "progress": "90", "view_offset": "250", "duration": 1000}}}
		case "get_libraries":
			data = []any{map[string]any{"section_type": "movie", "count": "8"}, map[string]any{"section_type": "show", "count": 3}}
		default:
			f.t.Error("unexpected Tautulli command")
		}
		json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "success", "data": data}})
	case "/api/v3/queue":
		if r.URL.Query().Get(include(service{id: svc})) != "true" {
			f.t.Error("missing queue title expansion")
		}
		if f.incomplete.Load() {
			fmt.Fprint(w, `{"records":[]}`)
			return
		}
		name := map[string]any{"id": 11, "title": "Example series"}
		if svc == "radarr" {
			name = map[string]any{"id": 12, "title": "Example movie"}
		}
		state := "warning"
		if f.recovered.Load() {
			state = "ok"
		}
		rows := []any{}
		start, end := 1, 2
		total := 2
		if f.pageTwo.Load() {
			total = 101
			start, end = 1, 100
			if r.URL.Query().Get("page") == "2" {
				start, end = 101, 101
			}
		}
		for i := start; i <= end; i++ {
			row := map[string]any{"id": i, "seriesId": 11, "movieId": 12, "size": 1000, "sizeleft": 500, "timeleft": "01:00:00", "downloadId": "same-pack", "status": "downloading", "trackedDownloadStatus": state, "trackedDownloadState": "downloading", "errorMessage": "private-indexer https://example.org/secret fixture-key"}
			if svc == "sonarr" {
				row["series"] = name
			} else {
				row["movie"] = name
			}
			rows = append(rows, row)
		}
		json.NewEncoder(w).Encode(map[string]any{"totalRecords": total, "records": rows})
	case "/api/v3/calendar":
		at := f.now.Add(24 * time.Hour).Format(time.RFC3339)
		if r.URL.Query().Get("start") == "" || r.URL.Query().Get("end") == "" {
			f.t.Error("unbounded calendar")
		}
		if svc == "sonarr" {
			json.NewEncoder(w).Encode([]any{map[string]any{"id": 101, "seriesId": 11, "title": "Next episode", "monitored": true, "seasonNumber": 1, "episodeNumber": 2, "airDateUtc": at, "series": map[string]any{"id": 11, "title": "Example series", "monitored": true}}})
		} else {
			json.NewEncoder(w).Encode([]any{map[string]any{"id": 12, "title": "Example movie", "monitored": true, "digitalRelease": at}, map[string]any{"id": 13, "title": "Outside window", "monitored": true, "digitalRelease": f.now.AddDate(0, 0, 40).Format(time.RFC3339)}})
		}
	case "/api/v3/history":
		if r.URL.Query().Get("eventType") != "3" || r.URL.Query().Get("pageSize") != "100" {
			f.t.Error("unbounded or unfiltered import history")
		}
		rows := []any{}
		for i := 0; i < 50; i++ {
			at := f.now.Add(-time.Duration(i) * time.Hour)
			if svc == "radarr" {
				at = at.Add(-30 * time.Minute)
			}
			row := map[string]any{"eventType": "downloadFolderImported", "date": at.Format(time.RFC3339), "quality": map[string]any{"quality": map[string]any{"name": "HD"}}, "data": map[string]any{"importedSize": "1024", "droppedPath": "private-path"}}
			if svc == "sonarr" {
				row["series"] = map[string]any{"title": "Example series"}
				row["episode"] = map[string]any{"title": fmt.Sprintf("Episode %d", i), "seasonNumber": i / 2, "episodeNumber": i}
			} else {
				row["movie"] = map[string]any{"title": "Example movie"}
			}
			rows = append(rows, row)
		}
		json.NewEncoder(w).Encode(map[string]any{"totalRecords": 100, "records": rows})
	case "/api/v3/series":
		fmt.Fprint(w, `[{"id":11,"title":"Example series","monitored":true,"statistics":{"episodeCount":10,"episodeFileCount":8,"sizeOnDisk":2048}}]`)
	case "/api/v3/movie":
		fmt.Fprint(w, `[{"id":12,"title":"Example movie","monitored":true,"hasFile":false,"sizeOnDisk":0}]`)
	case "/api/v3/health":
		fmt.Fprint(w, `[{"source":"DownloadClientCheck","type":"warning","message":"private-path fixture-key"},{"source":"IndexerCheck","type":"error"},{"source":"HealthyCheck","type":"notice"}]`)
	case "/api/v1/request/count":
		fmt.Fprint(w, `{"pending":1,"movie":4,"tv":2}`)
	case "/api/v1/request":
		if r.URL.Query().Get("filter") != "pending" {
			f.t.Error("request filter missing")
		}
		json.NewEncoder(w).Encode(map[string]any{"pageInfo": map[string]any{"results": 1}, "results": []any{map[string]any{"type": "movie", "status": 1, "createdAt": f.now.Format(time.RFC3339), "requestedBy": map[string]any{"displayName": "private-requester"}, "media": map[string]any{"tmdbId": 42}}}})
	case "/api/v1/movie/42":
		fmt.Fprint(w, `{"id":42,"title":"Requested movie","posterPath":"/unused.jpg"}`)
	case "/MediaCover/11/poster.jpg", "/MediaCover/12/poster.jpg":
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	default:
		f.t.Error("unexpected upstream endpoint: " + path)
		http.NotFound(w, r)
	}
}
func newFixture(t *testing.T) (*fixture, *httptest.Server) {
	t.Helper()
	f := &fixture{t: t, now: time.Now().UTC().Truncate(time.Second), calls: map[string]int{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server
}
func registry(t *testing.T, server *httptest.Server, logs *bytes.Buffer) (*plugin.Registry, *Media) {
	t.Helper()
	var instance *Media
	r, err := plugin.Open(t.TempDir(), []plugin.Builtin{{ID: "media", New: func(h plugin.Host) plugin.Plugin { instance = New(h).(*Media); return instance }}}, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	cfg := plugin.Config{}
	for _, s := range services {
		cfg[s.id+"_url"] = server.URL + "/" + s.id
		cfg[s.id+"_key"] = "fixture-key"
	}
	on := true
	if _, err = r.Update("media", plugin.Update{Enabled: &on, Settings: cfg}); err != nil {
		t.Fatal("enable failed:", err)
	}
	return r, instance
}
func view(t *testing.T, r *plugin.Registry, name string) plugin.View {
	t.Helper()
	raw, err := r.View(context.Background(), "media", name, nil)
	if err != nil {
		t.Fatal("view failed:", err)
	}
	for _, forbidden := range []string{"fixture-key", "private-viewer", "private-device", "192.0.2.90", "private-requester", "private-indexer", "private-path", "http://", "https://"} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Fatal("private upstream data escaped into view")
		}
	}
	var v plugin.View
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("invalid view JSON")
	}
	return v
}
func block(t *testing.T, v plugin.View, title string) plugin.Block {
	t.Helper()
	for _, b := range v.Blocks {
		if b.Title == title {
			return b
		}
	}
	t.Fatal("missing block", title)
	return plugin.Block{}
}
func TestViewsChecksCacheAndImages(t *testing.T) {
	f, server := newFixture(t)
	var logs bytes.Buffer
	r, m := registry(t, server, &logs)
	now := view(t, r, "now")
	playing := block(t, now, "Now playing")
	if len(playing.Items) != 1 || playing.Items[0].Subtitle != "a user" || playing.Items[0].Progress.Value != 0.25 {
		t.Fatal("playback privacy/progress projection failed")
	}
	downloads := block(t, now, "Downloading")
	if len(downloads.Items) != 2 || downloads.Items[0].Progress.Value != 0.5 || downloads.Items[0].Tone != "warn" || downloads.Items[0].Meta != "1h0m0s left" {
		t.Fatal("queue merge/progress failed")
	}
	if len(block(t, now, "Upcoming").Items) != 2 {
		t.Fatal("calendar window failed")
	}
	first := f.count("/sonarr/api/v3/queue")
	view(t, r, "now")
	if f.count("/sonarr/api/v3/queue") != first {
		t.Fatal("refresh bypassed cache")
	}
	library := view(t, r, "library")
	imports := block(t, library, "Recently imported")
	if len(imports.Rows) != 50 || imports.Rows[0]["size"].(float64) != 2048 || imports.Rows[0]["title"] != "Example series · S00 · 2 episodes" {
		t.Fatal("recent import projection/sort/cap failed")
	}
	if len(library.Blocks) != 5 {
		t.Fatal("missing service sections")
	}
	if got := block(t, library, "Overseerr").Blocks[1].Items; len(got) != 1 || got[0].Title != "Requested movie" || got[0].Meta == "" {
		t.Fatal("pending request title join failed")
	}
	checks, err := r.Test(context.Background(), "media", nil)
	if err != nil || !checks.OK || len(checks.Checks) != 4 {
		t.Fatal("configured service checks failed")
	}
	// The same configuration keeps refs valid across a connection check.
	now = view(t, r, "now")
	ref := block(t, now, "Downloading").Items[0].Image
	ct, body, err := r.Image(context.Background(), "media", ref)
	if err != nil || ct != "image/jpeg" || len(body) != 4 {
		t.Fatal("mapped image failed")
	}
	count := f.count("/sonarr/MediaCover/11/poster.jpg")
	if _, _, err = r.Image(context.Background(), "media", "unknown-ref"); err == nil {
		t.Fatal("unknown ref accepted")
	}
	if _, _, err = m.Image(context.Background(), "/MediaCover/11/poster.jpg"); err == nil {
		t.Fatal("raw image path accepted")
	}
	if f.count("/sonarr/MediaCover/11/poster.jpg") != count {
		t.Fatal("unknown image ref caused a request")
	}
	s := m.services[0]
	for i := 1; i <= maxImages+1; i++ {
		m.poster(s, 1000+i)
	}
	if len(m.images) != maxImages {
		t.Fatal("image table is not bounded")
	}
	if _, _, err = m.Image(context.Background(), ref); err == nil {
		t.Fatal("evicted ref accepted")
	}
	if logs.Len() != 0 {
		t.Fatal("unexpected plugin logs")
	}
}

type testHost struct {
	dir    string
	client *http.Client
	now    time.Time
	logs   bytes.Buffer
}

func (h *testHost) HTTP() *http.Client              { return h.client }
func (h *testHost) CacheDir() string                { return h.dir }
func (h *testHost) Logf(format string, args ...any) { fmt.Fprintf(&h.logs, format, args...) }
func (h *testHost) Now() time.Time                  { return h.now }

func TestDegradationPaginationAndAlertTransitions(t *testing.T) {
	f, server := newFixture(t)
	var logs bytes.Buffer
	r, _ := registry(t, server, &logs)
	f.bad.Store(true)
	now := view(t, r, "now")
	if len(block(t, now, "Downloading").Items) != 1 {
		t.Fatal("failed service hid working downloads")
	}
	notices := 0
	for _, b := range now.Blocks {
		if b.Type == "notice" && strings.HasPrefix(b.Text, "Radarr:") {
			notices++
		}
	}
	if notices != 2 {
		t.Fatal("missing per-service failure notices")
	}
	checks, err := r.Test(context.Background(), "media", nil)
	if err != nil || checks.OK || len(checks.Checks) != 4 || checks.Checks[1].OK || !checks.Checks[2].OK {
		t.Fatal("degraded checks did not remain independent")
	}
	h := &testHost{client: server.Client(), now: f.now, dir: t.TempDir()}
	m := New(h).(*Media)
	if err = m.Configure(plugin.Config{"sonarr_url": server.URL + "/sonarr", "sonarr_key": "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	f.pageTwo.Store(true)
	initial, err := m.Alerts(context.Background())
	if err != nil || len(initial) != 2 || initial[0].Rule != "sonarr.health" || initial[0].Subject != "DownloadClientCheck" || initial[0].Severity != "info" || initial[1].Severity != "warning" || strings.Contains(initial[0].Summary, "fixture-key") {
		t.Fatal("health rule projection failed")
	}
	if f.count("/sonarr/api/v3/queue") < 2 {
		t.Fatal("queue did not read all pages")
	}
	h.now = h.now.Add(time.Hour)
	atHour, err := m.Alerts(context.Background())
	if err != nil || len(atHour) != 2 {
		t.Fatal("queue warned before more than one hour")
	}
	h.now = h.now.Add(21 * time.Second)
	later, err := m.Alerts(context.Background())
	if err != nil || len(later) != 3 || later[2].Rule != "queue.stuck" || later[2].Subject != "Example series" || later[2].Severity != "warning" {
		t.Fatal("stuck rule threshold/title dedup failed")
	}
	reopened := New(h).(*Media)
	if err = reopened.Configure(plugin.Config{"sonarr_url": server.URL + "/sonarr", "sonarr_key": "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	preserved, err := reopened.Alerts(context.Background())
	if err != nil || len(preserved) != 3 || preserved[2].Rule != "queue.stuck" {
		t.Fatal("runtime reopen reset stuck observation")
	}
	m = reopened
	f.recovered.Store(true)
	h.now = h.now.Add(21 * time.Second)
	recovered, err := m.Alerts(context.Background())
	if err != nil || len(recovered) != 2 || len(m.stuck) != 0 {
		t.Fatal("recovery did not clear stuck observations")
	}
	f.incomplete.Store(true)
	h.now = h.now.Add(21 * time.Second)
	if _, err = m.Alerts(context.Background()); err == nil {
		t.Fatal("partial queue silently cleared alerts")
	}
	// A slow first service must not consume the later service's time budget.
	f.bad.Store(false)
	f.incomplete.Store(false)
	f.pageTwo.Store(false)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	if err = m.Configure(plugin.Config{"sonarr_url": slow.URL, "sonarr_key": "fixture-key", "radarr_url": server.URL + "/radarr", "radarr_key": "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	partial, err := m.View(context.Background(), "now", nil)
	if err != nil || len(block(t, partial, "Downloading").Items) != 1 {
		t.Fatal("slow service hid working service")
	}
	// Rule names must remain specific to each Arr service.
	if err = m.Configure(plugin.Config{"radarr_url": server.URL + "/radarr", "radarr_key": "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	radial, err := m.Alerts(context.Background())
	if err != nil || len(radial) != 2 || radial[0].Rule != "radarr.health" {
		t.Fatal("Radarr health rule failed")
	}
	// All upstreams can fail while a useful notice-only view is still valid.
	allDown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer allDown.Close()
	if err = m.Configure(plugin.Config{"sonarr_url": allDown.URL, "sonarr_key": "fixture-key"}); err != nil {
		t.Fatal(err)
	}
	v, err := m.View(context.Background(), "now", nil)
	if err != nil {
		t.Fatal("outage should render notices")
	}
	if _, err = plugin.ValidateView(v, m.Manifest()); err != nil {
		t.Fatal("invalid degraded view")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestConfigurationAndSecretSafeFailures(t *testing.T) {
	var requests atomic.Int32
	key := "synthetic-secret/with?symbols"
	h := &testHost{now: time.Now(), client: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("upstream transport echoed " + r.URL.String() + " " + key)
	})}}
	m := New(h).(*Media)
	invalid := []struct {
		cfg   plugin.Config
		field string
	}{
		{plugin.Config{}, "sonarr_url"},
		{plugin.Config{"radarr_url": "https://example.org"}, "radarr_key"},
		{plugin.Config{"tautulli_key": "synthetic"}, "tautulli_url"},
		{plugin.Config{"sonarr_url": "https://user@example.org/?token=synthetic", "sonarr_key": key}, "sonarr_url"},
	}
	for _, c := range invalid {
		var fe *plugin.FieldError
		if err := m.Configure(c.cfg); !errors.As(err, &fe) || fe.Key != c.field {
			t.Fatal("expected field error for", c.field)
		}
	}
	cfg := plugin.Config{"tautulli_url": "https://example.org/prefix", "tautulli_key": key}
	for _, days := range []any{0, 31, 2.5, "NaN"} {
		cfg["upcoming_days"] = days
		if err := m.Configure(cfg); err == nil {
			t.Fatal("invalid upcoming range accepted")
		}
	}
	cfg["upcoming_days"] = 30
	if err := m.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 {
		t.Fatal("Configure performed network I/O")
	}
	test := m.Test(context.Background())
	v, err := m.View(context.Background(), "now", url.Values{"apikey": {key}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct {
		Test plugin.TestResult
		View plugin.View
	}{test, v})
	for _, bad := range []string{key, url.QueryEscape(key), "example.org", "upstream transport echoed"} {
		if bytes.Contains(raw, []byte(bad)) || strings.Contains(h.logs.String(), bad) {
			t.Fatal("secret or transport detail escaped")
		}
	}
	if test.OK || len(test.Checks) != 1 || len(v.Blocks) == 0 {
		t.Fatal("missing safe failure information")
	}
	// Repeated failures are cached, but explicit Test always makes a live call.
	count := requests.Load()
	m.View(context.Background(), "now", nil)
	if requests.Load() != count {
		t.Fatal("failed refresh was not cached")
	}
	m.Test(context.Background())
	if requests.Load() != count+1 {
		t.Fatal("Test reused a cached check")
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	if err = m.Configure(cfg); err != nil {
		t.Fatal("cannot re-enable after Close")
	}
}
