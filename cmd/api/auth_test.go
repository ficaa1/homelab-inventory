package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/settings"
	"github.com/ficaa1/homelab-inventory/internal/setup"
)

func authServer(t *testing.T) *server {
	t.Helper()
	s := testServer(t, "fixture-api-token")
	env := map[string]string{"OIDC_ENABLED": "true", "OIDC_ISSUER": "https://id.example.org", "OIDC_CLIENT_ID": "client", "OIDC_CLIENT_SECRET": "fixture-secret", "OIDC_ALLOWED": "@example.org", "OIDC_PUBLIC_URL": "https://dash.example.org"}
	s.defaults.Auth = settings.AuthEnv(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	s.agentToken = "fixture-agent-token"
	s.operatorToken = "fixture-operator-token"
	return s
}
func TestSessionAuthorityAndCSRF(t *testing.T) {
	s := authServer(t)
	h := s.routes()
	session := &http.Cookie{Name: auth.SessionCookie, Value: s.browserAuth.Issue("user@example.org", time.Now())}
	for _, tc := range []struct {
		method, path, bearer, csrf, origin string
		want                               int
	}{
		{"GET", "/hosts", "", "", "", 200},
		{"POST", "/inventory", "", "homelab-inventory", "", 401},
		{"GET", "/agent/manifest", "", "", "", 401},
		{"GET", "/agent/artifacts/1/file", "", "", "", 401},
		{"POST", "/agent/offers", "", "homelab-inventory", "", 403},
		{"POST", "/actions/execute", "", "homelab-inventory", "", 403},
		// Sessions may enroll hosts (503 here: the fixture has no setup manager), with CSRF.
		{"POST", "/setup/enroll", "", "homelab-inventory", "", 503},
		{"POST", "/setup/enroll", "", "", "", 403},
		{"GET", "/hosts", "wrong", "", "", 401},
		{"GET", "/hosts", "fixture-agent-token", "", "", 401},
		{"GET", "/hosts", "fixture-operator-token", "", "", 401},
		{"PUT", "/settings/auth", "", "", "", 403},
		{"PUT", "/settings/auth", "", "homelab-inventory", "https://evil.example", 403},
		{"PUT", "/settings/auth", "fixture-api-token", "", "https://evil.example", 200},
		{"PUT", "/settings/auth", "", "homelab-inventory", "https://dash.example.org", 200},
	} {
		// A save rotates generation, so issue the fixture against current policy.
		session.Value = s.browserAuth.Issue("user@example.org", time.Now())
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		r.AddCookie(session)
		if tc.bearer != "" {
			r.Header.Set("Authorization", "Bearer "+tc.bearer)
		}
		r.Header.Set("X-Requested-By", tc.csrf)
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s (bearer=%t, csrf=%q): %d %s", tc.method, tc.path, tc.bearer != "", tc.csrf, w.Code, w.Body)
		}
	}
	for _, bearer := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/audit", nil)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: s.browserAuth.Issue("user@example.org", time.Now())})
		want := "user@example.org"
		if bearer {
			r.Header.Set("Authorization", "Bearer fixture-api-token")
			want = "bearer"
		}
		s.auth(func(w http.ResponseWriter, r *http.Request) {
			if requestActor(r) != want {
				t.Error("wrong audit actor")
			}
			w.WriteHeader(204)
		})(httptest.NewRecorder(), r)
	}
	for _, csrf := range []bool{false, true} {
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		r.AddCookie(session)
		want := 403
		if csrf {
			r.Header.Set("X-Requested-By", "homelab-inventory")
			want = 204
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("logout: %d", w.Code)
		}
		if csrf && (len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge >= 0) {
			t.Error("logout did not clear session")
		}
	}
	// A configured provider remains unavailable while the instance is unclaimed.
	var err error
	s.setup, err = setup.Open(t.TempDir(), setup.Tokens{}, time.Now(), setup.Window, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	w := doRequest(t, h, "GET", "/auth/status", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled": false`) {
		t.Fatal("setup status exposed sign-in")
	}
	if w := doRequest(t, h, "GET", "/auth/login", nil); w.Code != 404 {
		t.Fatal("setup login exposed")
	}
	if w := doRequest(t, h, "GET", "/auth/callback", nil); w.Header().Get("Location") != "/#/login?error=disabled" {
		t.Fatal("setup callback exposed")
	}
}

func TestAuthSettingsAtomicSave(t *testing.T) {
	s := testServer(t, "fixture-api-token")
	h := s.routes()
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/settings/auth", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-api-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := call("PUT", `{"enabled":true,"provider_name":"should not save"}`)
	var failed struct {
		Fields map[string]string `json:"fields"`
	}
	if w.Code != 422 || json.Unmarshal(w.Body.Bytes(), &failed) != nil || failed.Fields["allowed"] != "add at least one address or @domain" || len(failed.Fields) != 5 {
		t.Fatalf("missing-field result: %d %s", w.Code, w.Body)
	}
	if st := s.stored(); st.AuthGeneration != 0 || st.Auth.ProviderName != nil {
		t.Fatal("rejected update saved partial settings")
	}
	body := `{"enabled":true,"issuer":"https://id.example.org","client_id":"client","client_secret":"fixture-secret","allowed":["@example.org"],"public_url":"https://dash.example.org"}`
	w = call("PUT", body)
	if w.Code != 200 || strings.Contains(w.Body.String(), "fixture-secret") || !strings.Contains(w.Body.String(), `"set": true`) {
		t.Fatalf("save or secret mask: %d %s", w.Code, w.Body)
	}
	before := s.browserAuth.Issue("user@example.org", time.Now())
	w = call("GET", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"issuer": "stored"`) {
		t.Fatal("missing provenance")
	}
	w = call("PUT", `{"provider_name":"Example"}`)
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if _, ok := s.browserAuth.Verify(before, time.Now()); ok {
		t.Fatal("save did not invalidate sessions")
	}
	// Notification reset preserves independent sign-in settings and generation.
	r := httptest.NewRequest("POST", "/settings/reset", nil)
	r.Header.Set("Authorization", "Bearer fixture-api-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if s.stored().AuthGeneration != 2 || !s.authConfig().Ready() {
		t.Fatal("notification reset changed sign-in policy")
	}
}

func TestAuthDiscoveryTestIsTemporaryAndSingleFlight(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var issuer string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "userinfo_endpoint": issuer + "/userinfo"})
	}))
	defer fake.Close()
	issuer = fake.URL
	s := authServer(t)
	h := s.routes()
	call := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-api-token")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- call("/settings/auth/test", `{"settings":{"issuer":"`+issuer+`"}}`) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("test did not reach discovery")
	}
	w := call("/settings/auth/test", "")
	close(release)
	if w.Code != 429 {
		t.Errorf("concurrent discovery test: %d", w.Code)
	}
	w = <-done
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok": true`) || calls.Load() != 1 {
		t.Fatalf("discovery: %d %s", w.Code, w.Body)
	}
	if s.stored().AuthGeneration != 0 || s.authConfig().Issuer == issuer {
		t.Fatal("test saved settings")
	}
}
