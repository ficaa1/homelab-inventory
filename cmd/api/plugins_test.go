package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"github.com/ficaa1/homelab-inventory/internal/setup"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

type routePlugin struct {
	bad    bool
	failed bool
}

func (*routePlugin) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "sample", Name: "Sample", Settings: []plugin.Field{{Key: "endpoint", Type: plugin.URL, Required: true}, {Key: "key", Type: plugin.Secret}}, Tab: &plugin.Tab{Title: "Sample", Views: []plugin.ViewRef{{Name: "main", Title: "Main"}}}}
}
func (*routePlugin) Configure(c plugin.Config) error {
	if c["key"] == "rejected" {
		return &plugin.FieldError{Key: "key", Message: "rejected"}
	}
	return nil
}
func (*routePlugin) Test(context.Context) plugin.TestResult { return plugin.TestResult{OK: true} }
func (p *routePlugin) View(context.Context, string, url.Values) (plugin.View, error) {
	if p.bad {
		return plugin.View{Blocks: []plugin.Block{{Type: "html"}}}, nil
	}
	return plugin.View{Title: "Ready", Blocks: []plugin.Block{{Type: "notice", Text: "Ready"}}}, nil
}
func (*routePlugin) Close() error { return nil }
func (p *routePlugin) Alerts(context.Context) ([]plugin.Alert, error) {
	if p.failed {
		return nil, errors.New("unavailable")
	}
	return []plugin.Alert{{Rule: "health", Subject: "worker", Severity: "warning", Summary: "Needs attention"}}, nil
}
func (*routePlugin) Image(context.Context, string) (string, []byte, error) {
	return "image/png", []byte("sample"), nil
}
func pluginServer(t *testing.T) (*server, *routePlugin) {
	t.Helper()
	s := testServer(t, strings.Repeat("a", 32))
	s.agentToken = strings.Repeat("b", 32)
	s.operatorToken = strings.Repeat("c", 32)
	p := &routePlugin{}
	var err error
	s.plugins, err = plugin.Open(t.TempDir(), []plugin.Builtin{{ID: "sample", New: func(plugin.Host) plugin.Plugin { return p }}}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.plugins.Close)
	return s, p
}
func pluginRequest(s *server, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}
func TestPluginRoutesAuthenticationValidationAndViews(t *testing.T) {
	s, p := pluginServer(t)
	routes := [][3]string{{"GET", "/plugins", ""}, {"PUT", "/plugins/sample", "{}"}, {"POST", "/plugins/sample/test", "{}"}, {"GET", "/plugins/sample/views/main", ""}, {"GET", "/plugins/sample/images/poster-1", ""}}
	for _, route := range routes {
		for _, token := range []string{"", s.agentToken, s.operatorToken} {
			if rec := pluginRequest(s, route[0], route[1], route[2], token); rec.Code != 401 {
				t.Fatalf("%s must require admin auth", route[1])
			}
		}
	}
	rec := pluginRequest(s, "PUT", "/plugins/sample", `{"enabled":true}`, s.token)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"endpoint"`) {
		t.Fatal("missing required setting must be a field error")
	}
	rec = pluginRequest(s, "PUT", "/plugins/sample", `{"enabled":true,"settings":{"endpoint":"http://example.org","key":"initial-value"}}`, s.token)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "initial-value") || !strings.Contains(rec.Body.String(), `"set": true`) {
		t.Fatal("settings save or secret masking failed")
	}
	for _, body := range []string{`{"settings":{"endpoint":"file:///etc/example"}}`, `{"settings":{"key":"rejected"}}`} {
		rec = pluginRequest(s, "PUT", "/plugins/sample", body, s.token)
		if rec.Code != 422 || len(s.plugins.Capabilities()) != 1 {
			t.Fatal("invalid settings were accepted")
		}
	}
	rec = pluginRequest(s, "GET", "/capabilities", "", s.token)
	var capabilities struct {
		Plugins []plugin.Capability `json:"plugins"`
	}
	json.Unmarshal(rec.Body.Bytes(), &capabilities)
	if len(capabilities.Plugins) != 1 || capabilities.Plugins[0].ID != "sample" {
		t.Fatal("enabled plugin missing from capabilities")
	}
	if rec = pluginRequest(s, "POST", "/plugins/sample/test", `{"settings":{"key":"temporary"}}`, s.token); rec.Code != 200 {
		t.Fatal("test route failed")
	}
	if rec = pluginRequest(s, "GET", "/plugins/sample/views/main", "", s.token); rec.Code != 200 || rec.Body.Len() > plugin.MaxViewBytes {
		t.Fatal("valid view route failed")
	}
	p.bad = true
	if rec = pluginRequest(s, "GET", "/plugins/sample/views/main", "", s.token); rec.Code != 502 {
		t.Fatal("invalid plugin view passed through")
	}
	rec = pluginRequest(s, "GET", "/plugins/sample/images/poster-1", "", s.token)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "private, max-age=3600" {
		t.Fatal("image contract failed")
	}
	pluginRequest(s, "PUT", "/plugins/sample", `{"enabled":false}`, s.token)
	if rec = pluginRequest(s, "GET", "/plugins/sample/views/main", "", s.token); rec.Code != 404 {
		t.Fatal("disabled view was served")
	}
	mgr, err := setup.Open(t.TempDir(), setup.Tokens{}, time.Now(), setup.Window, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	s.setup = mgr
	for _, route := range routes {
		if rec = pluginRequest(s, route[0], route[1], route[2], s.token); rec.Code != 503 {
			t.Fatal("plugin route bypassed setup gate")
		}
	}
	// Plugin cache shares the data root with host inventories.
	if store.ValidateHost("plugins") == nil {
		t.Fatal("plugin cache namespace is not reserved")
	}
}
func TestPluginAlertsMergeBeforeAgeSilenceAndTransitions(t *testing.T) {
	s, p := pluginServer(t)
	ctx := context.Background()
	now := time.Now()
	on := true
	if _, err := s.plugins.Update("sample", plugin.Update{Enabled: &on, Settings: plugin.Config{"endpoint": "http://example.org"}}); err != nil {
		t.Fatal(err)
	}
	s.plugins.Poll(ctx)
	rep, err := s.evaluateAlerts(nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts.Warning != 1 || len(rep.Alerts) != 1 || rep.Alerts[0].Source != "plugin" {
		t.Fatal("plugin alert not merged")
	}
	ledger := &seen.Book{}
	ledger.Stamp(rep.Alerts, now)
	if rep.Alerts[0].FirstSeen == nil {
		t.Fatal("plugin alert did not receive age")
	}
	book := silence.Book{}
	_, err = book.Add(silence.Silence{Host: "sample", Rule: "sample.health", Subject: "worker"}, now)
	if err != nil {
		t.Fatal(err)
	}
	quiet := silence.Apply(rep, &book, now)
	if len(silence.Unsilenced(quiet.Alerts)) != 0 || quiet.Silenced != 1 {
		t.Fatal("plugin silence ignored")
	}
	prev, _ := notify.Diff(notify.State{}, rep.Alerts, alert.LevelWarning, now)
	p.failed = true
	for i := 0; i < 3; i++ {
		s.plugins.Poll(ctx)
	}
	rep, err = s.evaluateAlerts(nil, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Alerts) != 1 || rep.Alerts[0].Rule != "plugin.unreachable" {
		t.Fatal("unreachable condition not merged")
	}
	_, change := notify.Diff(prev, rep.Alerts, alert.LevelWarning, now)
	if len(change.New) != 1 || len(change.Resolved) != 1 {
		t.Fatal("normal transition machinery did not see failure")
	}
	off := false
	s.plugins.Update("sample", plugin.Update{Enabled: &off})
	rep, err = s.evaluateAlerts(nil, now.Add(2*time.Minute))
	if err != nil || rep.Counts.Total != 0 {
		t.Fatal("disable did not resolve plugin alerts")
	}
}
