package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyAgentOnlySubmitsInventories(t *testing.T) {
	s := testServer(t, strings.Repeat("a", 32))
	s.agentToken = strings.Repeat("b", 32)
	s.operatorToken = strings.Repeat("c", 32)
	s.legacyAgentToken = "fixture-previous-shared-token"
	// Test the auth boundaries with a handler that cannot be confused with
	// payload validation, disabled features, or a missing release file.
	allowed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	for _, tc := range []struct {
		method, path string
		handler      http.HandlerFunc
		want         int
	}{
		{"POST", "/inventory", s.agentAuth(allowed), 204},
		{"GET", "/agent/manifest", s.agentAuth(allowed), 401},
		{"GET", "/agent/artifacts/1/file", s.agentAuth(allowed), 401},
		{"GET", "/settings", s.auth(allowed), 401},
		{"POST", "/admin/backup/export", s.auth(allowed), 401},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Authorization", "Bearer "+s.legacyAgentToken)
		rec := httptest.NewRecorder()
		tc.handler(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s %s=%d want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}
func TestLegacyAgentRejectsPrivilegeCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "state-migration"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state-migration", "legacy-agent.json")
	if err := os.WriteFile(path, []byte(`{"token":"fixture-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][2]string{{"fixture-secret", "other"}, {"other", "fixture-secret"}} {
		if _, err := loadLegacyAgent(dir, v[0], v[1]); err == nil {
			t.Fatal("accepted privileged collision")
		}
	}
	if v, err := loadLegacyAgent(dir, "other", "third"); err != nil || v != "fixture-secret" {
		t.Fatalf("load failed: %v", err)
	}
}
