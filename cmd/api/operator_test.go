package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
)

func TestTokenConfiguration(t *testing.T) {

	a, g, o := strings.Repeat("a", 32), strings.Repeat("g", 32), strings.Repeat("o", 32)
	for _, tc := range []struct {
		api, agent, operator string
		ok                   bool
	}{
		{"", "", "", true}, {"short", "", "", false},
		{a, "", "", true}, {a, g, o, true}, {a, "short", o, false},
		{a, g, "short", false}, {a, a, o, false}, {a, g, a, false}, {a, g, g, false},
		{a, "", o + "\n", false},
	} {
		if err := validateTokens(tc.api, tc.agent, tc.operator); (err == nil) != tc.ok {
			t.Fatalf("unexpected validation: %v", err)
		}
	}
	for _, tc := range []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8080", true}, {"[::1]:8080", true}, {":8080", false}, {"0.0.0.0:8080", false}, {"192.0.2.10:8080", false},
	} {
		if loopbackAddress(tc.addr) != tc.ok {
			t.Errorf("loopbackAddress(%s)", tc.addr)
		}
	}

}
func TestOperatorAuthorityAndCapabilities(t *testing.T) {
	s, m := releaseServer(t)
	s.operatorToken = strings.Repeat("o", 32)
	s.agentToken = strings.Repeat("g", 32)
	body := `{"action":"promote","sequence":1,"confirm":true}`
	for _, token := range []string{"", s.token, s.agentToken, "wrong", s.operatorToken} {
		req := httptest.NewRequest("POST", "/agent/offers", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		want := 403
		if token == s.operatorToken {
			want = 200
		}
		if w.Code != want {
			t.Fatalf("offers: got %d want %d", w.Code, want)
		}
	}
	// Ingested sequences never promote, and signer hold overrides operator state.
	if err := agentrelease.WriteJSON(s.agentDir()+"/control.json", agentrelease.Control{Sequence: m.Sequence, Mode: "hold"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/agent/offers", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.operatorToken)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, token := range []string{"", s.token, s.agentToken, s.operatorToken} {
		req := httptest.NewRequest("POST", "/actions/execute", strings.NewReader(`{"confirm":true}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		want := 403
		if token == s.operatorToken {
			want = 503
		}
		if w.Code != want {
			t.Fatalf("execute: got %d want %d", w.Code, want)
		}
	}
	req = httptest.NewRequest("GET", "/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	var caps struct {
		ActionsEnabled bool `json:"actions_enabled"`
		OffersEnabled  bool `json:"agent_offers_enabled"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &caps) != nil || caps.ActionsEnabled || !caps.OffersEnabled {
		t.Fatal(w.Code, w.Body.String())
	}
	// Operator credential is deliberately rejected by inventory/read endpoints.
	req = httptest.NewRequest("GET", "/hosts", nil)
	req.Header.Set("Authorization", "Bearer "+s.operatorToken)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestAgentTokenScope(t *testing.T) {
	s, _ := releaseServer(t)
	s.agentToken = strings.Repeat("g", 32)
	for _, tc := range []struct {
		method, path string
		agentOK      bool
	}{
		{"POST", "/inventory", true}, {"GET", "/agent/manifest?host=host-a", true},
		{"GET", "/agent/artifacts/1/discover-linux-amd64", true},
		{"GET", "/hosts", false}, {"PUT", "/settings", false}, {"GET", "/agent/status", false},
		{"POST", "/silences", false}, {"POST", "/notify/test", false}, {"POST", "/storage/runs/prune", false},
		{"DELETE", "/storage/runs/host-a", false}, {"POST", "/agent/offers", false}, {"POST", "/actions/execute", false},
	} {
		for _, token := range []string{s.token, s.agentToken} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			s.routes().ServeHTTP(rec, req)
			denied := rec.Code == 401 || rec.Code == 403
			wantDenied := !tc.agentOK && token == s.agentToken || tc.path == "/agent/offers" || tc.path == "/actions/execute"
			if denied != wantDenied {
				t.Errorf("%s %s agent=%v status=%d", tc.method, tc.path, token == s.agentToken, rec.Code)
			}
		}
	}
}
