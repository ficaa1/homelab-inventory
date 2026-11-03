package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/setup"
)

func bootstrapReleaseServer(t *testing.T) (*server, func(string)) {
	t.Helper()
	t.Setenv("AGENT_CANARY_HOSTS", "host-a")
	t.Setenv("AGENT_PLATFORMS", "linux/amd64")
	s := testServer(t, "test-token")
	s.agentToken = strings.Repeat("g", 32)
	s.operatorToken = strings.Repeat("o", 32)
	s.agentReleaseDir = t.TempDir()
	s.agentBinDir = testAgentBinDir(t)
	key := filepath.Join(t.TempDir(), "key")
	if _, err := agentrelease.Keygen(key); err != nil {
		t.Fatal(err)
	}
	input := t.TempDir()
	for _, role := range []string{"discover", "agentupdate"} {
		if err := os.WriteFile(filepath.Join(input, role+"-linux-amd64"), []byte(role+" fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	publish := func(version string) {
		t.Helper()
		if err := agentrelease.PublishRuntime(s.agentReleaseDir, key, input, version, inventory.AgentRevision); err != nil {
			t.Fatal(err)
		}
	}
	publish("baseline")
	return s, publish
}

func TestAgentBootstrapUsesSignerBaselineAndHonorsHolds(t *testing.T) {
	s, publish := bootstrapReleaseServer(t)
	request := func(host string, want int, sequence uint64) {
		t.Helper()
		w := agentRequest(s, "GET", "/agent/bootstrap?host="+host)
		if w.Code != want {
			t.Fatalf("bootstrap status %d, want %d", w.Code, want)
		}
		if want != 200 {
			return
		}
		public, err := os.ReadFile(filepath.Join(s.agentReleaseDir, "public.key"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := agentrelease.Verify(w.Body.Bytes(), public)
		if err != nil || m.Sequence != sequence || m.LauncherProtocol != 1 {
			t.Fatal("wrong signed bootstrap baseline")
		}
	}
	request("host-a", 200, 1)
	request("host-b", 409, 0)
	m, _, c, err := agentrelease.Read(s.agentReleaseDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"update", "promote"} {
		host := ""
		if action == "update" {
			host = "host-b"
		}
		if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, action, host, "operator@example.org"); err != nil {
			t.Fatal(err)
		}
		request("host-b", 409, 0)
	}
	if err := agentrelease.SetControl(s.agentReleaseDir, "promote"); err != nil {
		t.Fatal(err)
	}
	request("host-b", 200, 1)
	publish("candidate")
	request("host-a", 200, 2)
	request("host-b", 200, 1)
	// Browser promotion of the new release still cannot replace the signer baseline.
	m, _, c, err = agentrelease.Read(s.agentReleaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, "promote", "", "operator@example.org"); err != nil {
		t.Fatal(err)
	}
	request("host-b", 200, 1)
	if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, "hold", "", "operator@example.org"); err != nil {
		t.Fatal(err)
	}
	request("host-a", 409, 0)
	request("host-b", 409, 0)
	if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, "auto", "", "operator@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := agentrelease.SetControl(s.agentReleaseDir, "hold"); err != nil {
		t.Fatal(err)
	}
	request("host-a", 409, 0)
	request("host-b", 409, 0)
}

func TestAgentBootstrapCredentialBoundary(t *testing.T) {
	s, _ := bootstrapReleaseServer(t)
	for _, path := range []string{"/agent/bootstrap?host=host-a", "/agent/public-key"} {
		for _, token := range []string{"", s.operatorToken, s.agentToken, "test-token"} {
			r := httptest.NewRequest("GET", path, nil)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			w := httptest.NewRecorder()
			s.routes().ServeHTTP(w, r)
			want := 401
			if token == s.agentToken || token == "test-token" {
				want = 200
			}
			if w.Code != want {
				t.Fatalf("bootstrap credential boundary: status %d, want %d", w.Code, want)
			}
		}
	}
	if w := agentRequest(s, "GET", "/agent/bootstrap"); w.Code != 400 {
		t.Fatal("missing host accepted")
	}
	s.token, s.agentToken = "", ""
	for _, path := range []string{"/agent/bootstrap?host=host-a", "/agent/public-key"} {
		if w := doRequest(t, s.routes(), "GET", path, nil); w.Code != 401 {
			t.Fatal("bootstrap auth failed open without credentials")
		}
	}
}

func TestSignedEnrollmentCommandsBindingAndStatus(t *testing.T) {
	s, _ := bootstrapReleaseServer(t)
	now := time.Now()
	var err error
	s.setup, err = setup.Open(t.TempDir(), setup.Tokens{API: "test-token", Agent: s.agentToken}, now, setup.Window, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "https://inventory.example.org/setup/enroll", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	w := request(`{"hostname":"host-a","scheduler":"truenas-cron","state_dir":"/mnt/pool/agent","replace_schedule":"7"}`)
	if w.Code != 200 {
		t.Fatal("signed command unavailable", w.Code)
	}
	var out struct {
		Code     string            `json:"code"`
		Signed   bool              `json:"signed_updates"`
		Commands map[string]string `json:"commands"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--updates slots", "--hostname 'host-a'", "--scheduler truenas-cron", "--state-dir '/mnt/pool/agent'", "--replace-schedule '7'"} {
		if !strings.Contains(out.Commands["root"], flag) {
			t.Fatal("missing installer option", flag)
		}
	}
	if !out.Signed || strings.Contains(out.Commands["root"], "sudo") || !strings.Contains(out.Commands["linux"], "sudo") || strings.Contains(out.Commands["linux"], "--allow-http") {
		t.Fatal("root/sudo or HTTPS behavior changed")
	}
	if _, err := s.setup.RedeemNonce(out.Code, "host-b", strings.Repeat("n", 32)); err == nil {
		t.Fatal("code not host-bound")
	}
	// A lost HTTP response can be recovered with the same installation nonce.
	redeem := func(host, nonce string) int {
		t.Helper()
		body, _ := json.Marshal(map[string]string{"code": out.Code, "hostname": host, "nonce": nonce})
		r := httptest.NewRequest("POST", "https://inventory.example.org/agent/enroll", strings.NewReader(string(body)))
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w.Code
	}
	nonce := strings.Repeat("n", 32)
	if redeem("host-a", nonce) != 200 {
		t.Fatal("nonce enrollment refused")
	}
	now = now.Add(16 * time.Minute)
	if redeem("host-a", nonce) != 200 {
		t.Fatal("HTTP retry refused after enrollment-code expiry")
	}
	if redeem("host-a", strings.Repeat("x", 32)) != 403 || redeem("host-b", nonce) != 403 {
		t.Fatal("HTTP retry widened redemption authority")
	}

	// Invalid options and pre-promotion non-canaries must not mint codes.
	before := len(s.setup.List())
	for _, body := range []string{`{"hostname":"host-b"}`, `{"scheduler":"other"}`, `{"scheduler":"truenas-cron"}`, `{"scheduler":"truenas-cron","state_dir":"relative"}`, `{"scheduler":"truenas-cron","state_dir":"/mnt/pool/agent","replace_schedule":"-1"}`, `{"hostname":"../escape"}`, `{"scheduler":"truenas-cron","state_dir":"/mnt/tank/agent$"}`, `{"scheduler":"truenas-cron","state_dir":"/mnt/tank/agent;"}`} {
		if w := request(body); w.Code != 400 && w.Code != 409 {
			t.Fatal("invalid enrollment accepted")
		}
	}
	if len(s.setup.List()) != before {
		t.Fatal("invalid request minted a code")
	}
	w = agentRequest(s, "GET", "/setup/status?host=host-a")
	var status struct {
		Signed struct {
			Ready       bool   `json:"ready"`
			Fingerprint string `json:"key_fingerprint"`
		} `json:"signed_updates"`
	}
	if json.Unmarshal(w.Body.Bytes(), &status) != nil || !status.Signed.Ready || len(status.Signed.Fingerprint) != 64 {
		t.Fatal("missing authenticated enrollment readiness")
	}
	if w := doRequest(t, s.routes(), "GET", "/setup/status", nil); strings.Contains(w.Body.String(), "key_fingerprint") {
		t.Fatal("public setup status leaked release configuration")
	}
	if w := agentRequest(s, "GET", "/setup/enroll"); strings.Contains(w.Body.String(), "nonce_hash") || strings.Contains(w.Body.String(), out.Code) {
		t.Fatal("enrollment list leaked credentials")
	}
	s.agentReleaseDir = ""
	t.Setenv("AGENT_RELEASE_DIR", "")
	w = request(`{}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "--updates slots") {
		t.Fatal("unconfigured unsigned flow changed")
	}
}
