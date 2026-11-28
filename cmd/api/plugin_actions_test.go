package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveActionsRequireSeparateOperatorAndConfirmation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/archive/run" || r.Method != "POST" {
			t.Error("unexpected fixed route")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"accepted":true}`))
	}))
	defer upstream.Close()
	admin := strings.Repeat("a", 32)
	operator := strings.Repeat("o", 32)
	s := testServer(t, admin)
	s.operatorToken = operator
	registry, e := openPlugins(s.store.Dir(), nil)
	if e != nil {
		t.Fatal(e)
	}
	s.plugins = registry
	defer registry.Close()
	on := true
	if _, e = registry.Update("archive", plugin.Update{Enabled: &on, Settings: plugin.Config{"policy_url": upstream.URL}}); e != nil {
		t.Fatal(e)
	}
	handler := s.routes()
	for _, tc := range []struct {
		token, body string
		status      int
	}{{admin, `{"confirmed":true,"values":{}}`, 403}, {operator, `{"confirmed":false,"values":{}}`, 422}, {operator, `{"confirmed":true,"values":{"path":"/anything"}}`, 422}, {operator, `{"confirmed":true,"values":{}}`, 200}} {
		r := httptest.NewRequest("POST", "/plugins/archive/actions/run", strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("status %d want %d body %s", w.Code, tc.status, w.Body.String())
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized or unconfirmed upstream writes: %d", calls)
	}
}

type secretActionFixture struct{ plugin.Plugin }

func (secretActionFixture) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "example", Name: "Example", Actions: []plugin.ActionSpec{{ID: "finish", Label: "Finish", Operator: true, Fields: []plugin.Field{{Key: "callback", Type: plugin.Secret, Required: true}}}}}
}
func (secretActionFixture) Configure(plugin.Config) error { return nil }
func (secretActionFixture) Close() error                  { return nil }
func (secretActionFixture) Action(_ context.Context, _ string, in plugin.Config) (plugin.View, error) {
	if in["callback"] != "synthetic-private-callback" {
		return plugin.View{}, errors.New("missing callback")
	}
	return plugin.View{Title: "Finished"}, nil
}
func TestPluginActionSecretStaysOutOfLogs(t *testing.T) {
	s := testServer(t, strings.Repeat("a", 32))
	var logs bytes.Buffer
	s.log = log.New(&logs, "", 0)
	reg, err := plugin.Open(s.store.Dir(), []plugin.Builtin{{ID: "example", New: func(plugin.Host) plugin.Plugin { return secretActionFixture{} }}}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	s.plugins = reg
	s.operatorToken = strings.Repeat("o", 32)
	on := true
	if _, err = reg.Update("example", plugin.Update{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/plugins/example/actions/finish", strings.NewReader(`{"confirmed":true,"values":{"callback":"synthetic-private-callback"}}`))
	req.Header.Set("Authorization", "Bearer "+s.operatorToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	withLogging(s.log, s.routes()).ServeHTTP(rec, req)
	if rec.Code != 200 || logs.Len() == 0 {
		t.Fatal("action did not run through access logging")
	}
	if strings.Contains(logs.String(), "synthetic-private-callback") {
		t.Fatal("action value appeared in logs")
	}
	err = filepath.WalkDir(s.store.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if bytes.Contains(data, []byte("synthetic-private-callback")) {
			t.Fatal("action value persisted")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
