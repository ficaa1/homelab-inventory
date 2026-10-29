package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fakePlugin struct {
	host                     Host
	config                   Config
	fail                     bool
	closed                   int
	entered, release         chan struct{}
	pollEntered, pollRelease chan struct{}
}

func (p *fakePlugin) Manifest() Manifest {
	return Manifest{ID: "sample", Name: "Sample", Version: "1", Settings: []Field{
		{Key: "endpoint", Type: URL, Required: true}, {Key: "key", Type: Secret}, {Key: "count", Type: Number}, {Key: "active", Type: Bool}, {Key: "mode", Type: Select, Options: []Option{{Value: "one", Label: "One"}}},
	}, Tab: &Tab{Title: "Sample", Views: []ViewRef{{Name: "main", Title: "Main"}}}}
}
func (p *fakePlugin) Configure(c Config) error {
	p.config = c
	if c["key"] == "reject-value" {
		return &FieldError{Key: "key", Message: "invalid reject-value"}
	}
	return nil
}
func (p *fakePlugin) Test(ctx context.Context) TestResult {
	if p.entered != nil {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
	return TestResult{OK: true, Message: "connected", Checks: []Check{{Name: "Sample", OK: true}}}
}
func (p *fakePlugin) View(context.Context, string, url.Values) (View, error) {
	return View{Title: "Main", Blocks: []Block{{Type: "notice", Text: "Ready", Tone: "ok"}}}, nil
}
func (p *fakePlugin) Close() error { p.closed++; return nil }
func (p *fakePlugin) Alerts(ctx context.Context) ([]Alert, error) {
	if p.pollEntered != nil {
		close(p.pollEntered)
		select {
		case <-p.pollRelease:
		case <-ctx.Done():
		}
	}

	if p.fail {
		return nil, errors.New("unavailable")
	}
	return []Alert{{Rule: "health", Subject: "worker", Severity: "warning", Summary: "Needs attention"}}, nil
}
func newFakeRegistry(t *testing.T) (*Registry, *fakePlugin, string) {
	t.Helper()
	dir := t.TempDir()
	p := &fakePlugin{}
	r, err := Open(dir, []Builtin{{ID: "sample", New: func(h Host) Plugin { p.host = h; return p }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, p, dir
}
func enabledUpdate() Update {
	on := true
	return Update{Enabled: &on, Settings: Config{"endpoint": "http://example.org", "key": "initial-value"}}
}
func TestSettingsMergeMaskValidationAndPersistence(t *testing.T) {
	t.Setenv("PLUGIN_SAMPLE_ENDPOINT", "http://example.com")
	r, p, dir := newFakeRegistry(t)
	status, err := r.Update("sample", enabledUpdate())
	if err != nil {
		t.Fatal(err)
	}
	if status.Settings["key"].(map[string]bool)["set"] != true {
		t.Fatal("secret must be masked as set")
	}
	path := filepath.Join(dir, "state-plugins.json")
	before, _ := os.ReadFile(path)
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state file must be private")
	}
	cache, _ := os.Stat(p.host.CacheDir())
	if cache.Mode().Perm() != 0700 {
		t.Fatal("cache must be private")
	}
	for _, patch := range []Config{
		{"endpoint": "file:///etc/example"}, {"endpoint": "https:///missing"}, {"endpoint": nil}, {"count": "oops"}, {"active": "true"}, {"mode": "other"}, {"unknown": true}, {"key": "reject-value"},
	} {
		_, err := r.Update("sample", Update{Settings: patch})
		var validation *ValidationError
		if !errors.As(err, &validation) {
			t.Fatal("invalid settings must produce validation error")
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) {
			t.Fatal("rejected update changed state")
		}
		if p.config["key"] != "initial-value" {
			t.Fatal("failed configure did not restore settings")
		}
		if strings.Contains(err.Error(), "reject-value") {
			t.Fatal("configure error disclosed secret")
		}
	}
	_, err = r.Update("sample", Update{Settings: Config{"count": "2.5"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.config["key"] != "initial-value" || p.config["count"] != 2.5 {
		t.Fatal("absent secret or number parsing failed")
	}
	_, err = r.Update("sample", Update{Settings: Config{"key": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.config["key"]; ok {
		t.Fatal("null did not clear secret")
	}
	off := false
	_, err = r.Update("sample", Update{Enabled: &off, Settings: Config{"endpoint": nil}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.List()[0].Settings["endpoint"]; ok {
		t.Fatal("cleared URL revived environment seed")
	}
	if len(r.Capabilities()) != 0 || p.closed == 0 {
		t.Fatal("disable did not close or remove capability")
	}
	// Unknown state is retained verbatim as an object across saves, never echoed.
	raw, _ := os.ReadFile(path)
	var file stateFile
	json.Unmarshal(raw, &file)
	file.Plugins["missing"] = json.RawMessage(`{"enabled":true,"settings":{"private":"hidden"},"future":42}`)
	data, _ := json.Marshal(file)
	os.WriteFile(path, data, 0600)
	reopened, err := Open(dir, []Builtin{{ID: "sample", New: func(Host) Plugin { return &fakePlugin{} }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err = reopened.Update("sample", Update{}); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(reopened.List())
	if bytes.Contains(out, []byte("hidden")) {
		t.Fatal("unavailable plugin settings disclosed")
	}
	raw, _ = os.ReadFile(path)
	var after stateFile
	json.Unmarshal(raw, &after)
	if !bytes.Equal(after.Plugins["missing"], file.Plugins["missing"]) {
		t.Fatal("unknown plugin state was changed")
	}
}
func TestTemporaryTestConfigurationIsExclusiveAndNotSaved(t *testing.T) {
	r, p, dir := newFakeRegistry(t)
	if _, err := r.Update("sample", enabledUpdate()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	p.entered = make(chan struct{})
	p.release = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := r.Test(context.Background(), "sample", Config{"key": "temporary-value"})
		done <- err
	}()
	<-p.entered
	if _, err := r.Test(context.Background(), "sample", nil); !errors.Is(err, ErrBusy) {
		t.Fatal("concurrent test was allowed")
	}
	close(p.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p.config["key"] != "initial-value" {
		t.Fatal("test configuration was not restored")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("test saved settings")
	}
}
func TestViewsRejectUnsafeAndUnboundedData(t *testing.T) {
	m := (&fakePlugin{}).Manifest()
	valid := View{Title: "Now", Blocks: []Block{{Type: "list", Items: []Item{{Title: "Task", Image: "poster-1", Link: &Link{View: "main", Params: map[string]string{"q": "hello"}}, Progress: &Progress{Value: 0.5}, Badges: []Badge{{Text: "New", Tone: "info"}}}}}}}
	if _, err := ValidateView(valid, m); err != nil {
		t.Fatal(err)
	}
	invalid := []View{
		{Title: "<b>HTML</b>"}, {Blocks: []Block{{Type: "html"}}}, {Blocks: []Block{{Type: "notice", Tone: "bad"}}},
		{Blocks: []Block{{Type: "list", Items: []Item{{Title: "<img src=x>"}}}}},
		{Blocks: []Block{{Type: "list", Items: []Item{{Image: "https://example.org/img"}}}}},
		{Blocks: []Block{{Type: "list", Items: []Item{{Link: &Link{Href: "javascript:alert(1)"}}}}}},
		{Blocks: []Block{{Type: "list", Items: []Item{{Link: &Link{View: "unknown"}}}}}},
		{Blocks: []Block{{Type: "section", Blocks: []Block{{Type: "section", Blocks: []Block{{Type: "notice"}}}}}}},
		{Blocks: []Block{{Type: "stats", Items: make([]Item, 501)}}},
		{Blocks: make([]Block, 51)}, {Title: strings.Repeat("x", MaxViewBytes)},
		{Blocks: []Block{{Type: "table", Columns: []Column{{Key: "name"}}, Rows: []map[string]any{{"name": map[string]any{"html": "content"}}}}}},
	}
	for i, v := range invalid {
		if _, err := ValidateView(v, m); err == nil {
			t.Fatalf("invalid view %d accepted", i)
		}
	}
	if _, _, err := validateImage("text/html", nil); err == nil {
		t.Fatal("non-image accepted")
	}
	if _, _, err := validateImage("image/png", make([]byte, MaxImageBytes+1)); err == nil {
		t.Fatal("oversize image accepted")
	}
}
func TestHostEnforcesCurrentAllowlistRedirectsBodyCapAndSafeErrors(t *testing.T) {
	var hits atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer forbidden.Close()
	allowed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, forbidden.URL+"?private=hidden", 302)
		case "/localredirect":
			http.Redirect(w, r, "/", 302)
		case "/badredirect":
			w.Header().Set("Location", "http://%invalid?private=hidden")
			w.WriteHeader(302)
		case "/large":
			io.Copy(w, io.LimitReader(zeroReader{}, MaxHTTPBody+1))
		default:
			w.Write([]byte("ok"))
		}
	}))
	defer allowed.Close()
	var logs bytes.Buffer
	h := newHost(t.TempDir(), "sample", log.New(&logs, "", 0))
	m := (&fakePlugin{}).Manifest()
	h.configure(m, Config{"endpoint": allowed.URL, "key": "private-value"})
	for _, target := range []string{forbidden.URL + "?key=private-value", allowed.URL + "/redirect?key=private-value", allowed.URL + "/badredirect?key=private-value"} {
		resp, err := h.HTTP().Get(target)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatal("forbidden request succeeded")
		}
		if strings.Contains(err.Error(), "?") || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "hidden") {
			t.Fatal("HTTP error contains a query credential")
		}
	}
	if conn, err := h.transport.DialContext(context.Background(), "tcp", strings.TrimPrefix(forbidden.URL, "http://")); err == nil {
		conn.Close()
		t.Fatal("dial guard allowed an unconfigured host")
	}
	resp, err := h.HTTP().Get(allowed.URL + "/localredirect")
	if err != nil {
		t.Fatal("same-host redirect was refused")
	}
	resp.Body.Close()
	if hits.Load() != 0 {
		t.Fatal("forbidden host was contacted")
	}
	resp, err = h.HTTP().Get(allowed.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.configure(m, nil)
	if _, err := h.HTTP().Get(allowed.URL); err == nil {
		t.Fatal("pooled connection bypassed updated allowlist")
	}
	h.configure(m, Config{"endpoint": allowed.URL})
	resp, err = h.HTTP().Get(allowed.URL + "/large")
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil || len(data) > MaxHTTPBody {
		t.Fatal("body cap was not enforced")
	}
	h.Logf("request %s %s", "http://example.org/?key=hidden", "private-value")
	if strings.Contains(logs.String(), "hidden") || strings.Contains(logs.String(), "private-value") {
		t.Fatal("log disclosed credentials")
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestPollRetainsLastGoodThenUnreachableAndRecovery(t *testing.T) {
	r, p, _ := newFakeRegistry(t)
	if _, err := r.Update("sample", enabledUpdate()); err != nil {
		t.Fatal(err)
	}
	r.Poll(context.Background())
	got := r.Alerts()
	if len(got) != 1 || got[0].Host != "sample" || got[0].Rule != "sample.health" || got[0].Source != "plugin" {
		t.Fatal("incorrect alert namespace")
	}
	p.fail = true
	for i := 1; i <= 3; i++ {
		r.Poll(context.Background())
		got = r.Alerts()
		want := "sample.health"
		if i == 3 {
			want = "plugin.unreachable"
		}
		if len(got) != 1 || got[0].Rule != want {
			t.Fatalf("incorrect alert after failure %d", i)
		}
	}
	p.fail = false
	r.Poll(context.Background())
	if r.Alerts()[0].Rule != "sample.health" {
		t.Fatal("poll did not recover")
	}
	if r.List()[0].CheckedAt == nil || r.List()[0].State != "ok" {
		t.Fatal("poll status did not recover")
	}
	off := false
	r.Update("sample", Update{Enabled: &off})
	if len(r.Alerts()) != 0 {
		t.Fatal("disabled alerts not cleared")
	}
	if PollInterval != 5*time.Minute || p.host.HTTP().Timeout != 10*time.Second {
		t.Fatal("incorrect contract timeouts")
	}
}

func TestCachedReadsRemainAvailableDuringAlertPoll(t *testing.T) {
	r, p, _ := newFakeRegistry(t)
	if _, err := r.Update("sample", enabledUpdate()); err != nil {
		t.Fatal(err)
	}
	r.Poll(context.Background())
	p.pollEntered = make(chan struct{})
	p.pollRelease = make(chan struct{})
	done := make(chan struct{})
	go func() { r.Poll(context.Background()); close(done) }()
	<-p.pollEntered
	read := make(chan struct{})
	go func() { r.List(); r.Capabilities(); r.Alerts(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		close(p.pollRelease)
		<-done
		t.Fatal("cached reads blocked on live poll")
	}
	close(p.pollRelease)
	<-done
}
func TestOpenRejectsBadManifestWithoutPanic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state-plugins.json"), []byte(`{"plugins":{"sample":{"enabled":true,"settings":{}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir, []Builtin{{ID: "sample", New: func(Host) Plugin { return &fakePlugin{} }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.List()[0].State != "unconfigured" {
		t.Fatal("missing startup settings must be unconfigured")
	}
	if _, err := r.View(context.Background(), "sample", "main", nil); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatal("enabled configuration failure must be a gateway error")
	}

	if _, err := Open(t.TempDir(), []Builtin{{ID: "sample", New: func(Host) Plugin { return &fakePlugin{} }}, {ID: "sample", New: func(Host) Plugin { return &fakePlugin{} }}}, nil); err == nil {
		t.Fatal("duplicate registration accepted")
	}
}
