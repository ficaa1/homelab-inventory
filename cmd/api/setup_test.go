package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/setup"
)

func TestSetupRoutes(t *testing.T) {
	now := time.Now()
	s := testServer(t, "")
	var err error
	s.setup, err = setup.Open(t.TempDir(), setup.Tokens{}, now, setup.Window, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	s.agentBinDir = testAgentBinDir(t)
	h := s.routes()
	call := func(method, path, body, token string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://inventory.example.org"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/hosts", "/agent/enroll", "/settings", "/missing"} {
		if w := call("GET", path, "", ""); w.Code != 503 || !strings.Contains(w.Body.String(), "setup required") {
			t.Fatalf("setup gate: %s status %d", path, w.Code)
		}
	}
	for _, path := range []string{"/", "/healthz", "/setup/status", "/auth/status", "/icon.svg"} {
		if w := call("GET", path, "", ""); w.Code != 200 {
			t.Fatalf("public setup path %s: %d", path, w.Code)
		}
	}
	if w := call("POST", "/setup/claim", "null", ""); w.Code != 400 {
		t.Fatal("claim accepted null")
	}
	if w := call("POST", "/setup/claim", "{}", ""); w.Code != 200 {
		t.Fatalf("claim: %d", w.Code)
	}
	tokens := s.setup.Tokens()
	if w := call("POST", "/setup/claim", "{}", ""); w.Code != 409 {
		t.Fatal("repeat claim")
	}
	if w := call("GET", "/hosts", "", tokens.Agent); w.Code != 401 {
		t.Fatal("agent accessed admin")
	}
	if w := call("GET", "/hosts", "", tokens.API); w.Code != 200 {
		t.Fatal("new API credential not active")
	}
	if w := call("POST", "/setup/enroll", "{}", tokens.Agent); w.Code != 401 {
		t.Fatal("agent minted enrollment code")
	}
	w := call("POST", "/setup/enroll", "{}", tokens.API)
	if w.Code != 200 {
		t.Fatalf("enroll: %d", w.Code)
	}
	var out struct {
		ID       string            `json:"id"`
		Code     string            `json:"code"`
		HTTP     bool              `json:"http"`
		Commands map[string]string `json:"commands"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.Contains(out.Commands["linux"], "--allow-http") || !out.HTTP || out.ID == "" {
		t.Fatalf("enroll response missing HTTP opt-in or id: %s", w.Body)
	}
	if w := call("POST", "/agent/enroll", `{"code":"`+out.Code+`","hostname":"host-a"}`, ""); w.Code != 200 {
		t.Fatalf("redeem: %d", w.Code)
	}
	for range 9 {
		w = call("POST", "/agent/enroll", `{"code":"`+out.Code+`","hostname":"host-a"}`, "")
		if w.Code != 403 || !strings.Contains(w.Body.String(), "invalid enrollment code") {
			t.Fatal("replay revealed code status")
		}
	}
	if w := call("POST", "/agent/enroll", "{}", ""); w.Code != 429 {
		t.Fatal("redemption not rate limited")
	}
	if w := call("GET", "/setup/enroll", "", tokens.API); w.Code != 200 || !strings.Contains(w.Body.String(), "host-a") || strings.Contains(w.Body.String(), "hash") {
		t.Fatal("unsafe enrollment list")
	}
	s.agentBinDir = ""
	if w := call("POST", "/setup/enroll", "{}", tokens.API); w.Code != 200 || !strings.Contains(w.Body.String(), "Agent binaries unavailable") {
		t.Fatal("missing binaries must provide an explanatory wizard command")
	}
	// Inject the clock at the handler boundary, without a production window knob.
	s.setup, err = setup.Open(t.TempDir(), setup.Tokens{}, now, setup.Window, func() time.Time { return now.Add(setup.Window) })
	if err != nil {
		t.Fatal(err)
	}
	if w := call("POST", "/setup/claim", "{}", ""); w.Code != 410 {
		t.Fatal("expired claim must return 410")
	}
	if w := call("GET", "/setup/status", "", ""); !strings.Contains(w.Body.String(), "expired") {
		t.Fatal("missing expired state")
	}
}
func TestSetupOriginAndInstaller(t *testing.T) {
	s := testServer(t, "")
	for _, tc := range []struct {
		host, proto, forwarded string
		trusted, ok            bool
		want                   string
	}{
		{"inventory.example.org:8080", "https", "proxy.example.org", false, true, "http://inventory.example.org:8080"},
		{"inventory.example.org", "https", "proxy.example.org", true, true, "https://proxy.example.org"},
		{"[::1]:8080", "", "", false, true, "http://[::1]:8080"},
		{"bad'host", "", "", false, false, ""}, {"good;id", "", "", false, false, ""}, {"good/evil", "", "", false, false, ""},
		{"good", "http;id", "", true, false, ""}, {"good", "https", "good,evil", true, false, ""},
		{"bad:65536", "", "", false, false, ""}, {"-bad", "", "", false, false, ""},
	} {
		r := httptest.NewRequest("GET", "http://example.org/install.sh", nil)
		r.Host = tc.host
		r.Header.Set("X-Forwarded-Proto", tc.proto)
		r.Header.Set("X-Forwarded-Host", tc.forwarded)
		s.trustedProxy = tc.trusted
		got, err := s.setupURL(r)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("origin host=%q: %q, %v", tc.host, got, err)
		}
	}
	s.trustedProxy = false
	s.publicURL = "https://public.example.org/"
	r := httptest.NewRequest("GET", "http://example.org/install.sh", nil)
	if got, err := s.setupURL(r); err != nil || got != "https://public.example.org" {
		t.Fatal("PUBLIC_URL not applied")
	}
	w := httptest.NewRecorder()
	s.handleInstaller(w, r)
	if w.Code != 404 || !strings.Contains(w.Body.String(), "AGENT_BIN_DIR") {
		t.Fatal("missing binaries must explain manual setup")
	}
	s.agentBinDir = testAgentBinDir(t)
	w = httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "INSTALL_API_URL='https://public.example.org'") {
		t.Fatal("installer origin not prefilled/quoted")
	}
	for _, path := range []string{"/agent/bin/SHA256SUMS", "/agent/bin/linux-amd64/discover", "/agent/bin/linux-arm64/agentupdate"} {
		if w := doRequest(t, s.routes(), "GET", path, nil); w.Code != 200 {
			t.Fatalf("download %s: %d", path, w.Code)
		}
	}
	if w := doRequest(t, s.routes(), "GET", "/agent/bin/freebsd-amd64/discover", nil); w.Code != 404 {
		t.Fatal("unknown binary exposed")
	}
}

func testAgentBinDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64"} {
		if err := os.Mkdir(filepath.Join(dir, p), 0755); err != nil {
			t.Fatal(err)
		}
		for _, b := range []string{"discover", "agentupdate"} {
			if err := os.WriteFile(filepath.Join(dir, p, b), []byte("test"), 0755); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}
