package main

import (
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/http"
	"net/http/httptest"
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
