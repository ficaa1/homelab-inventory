package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

func config() Config {
	return Config{Enabled: true, Issuer: "https://id.example.org", ClientID: "client", ClientSecret: "synthetic-secret", PublicURL: "https://dash.example.org", Allowed: []string{"@example.org"}, SessionHours: 1, RequireVerified: true, Generation: 3}
}
func TestPolicyAndSession(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"https://id.example.org/realm", true}, {"http://127.0.0.1:8080", true}, {"http://[::1]:8080", true},
		{"http://localhost:8080", true}, {"http://id.example.org", false}, {"https://id.example.org:65536", false},
		{"https://user:password@id.example.org", false}, {"https://id.example.org/#", false}, {"file:///tmp/provider", false},
	} {
		if ValidURL(tc.raw) != tc.want {
			t.Errorf("URL policy for %q", tc.raw)
		}
	}
	for _, tc := range []struct {
		email   string
		entries []string
		want    bool
	}{
		{"User@Example.ORG", []string{"user@example.org"}, true},
		{"User@Example.ORG", []string{"@EXAMPLE.org"}, true},
		{"user@sub.example.org", []string{"@example.org"}, false},
		{"user@badexample.org", []string{"@example.org"}, false},
		{"user@example.org", nil, false},
		{"Display <user@example.org>", []string{"@example.org"}, false},
	} {
		if got := Allowed(tc.email, tc.entries); got != tc.want {
			t.Errorf("Allowed(%q): %v", tc.email, got)
		}
	}
	for _, next := range []string{"https://evil.example", "//evil.example", "/\\evil.example", "/%2fexample.org", "/%5cexample.org", "/\r\nHost:x", "elsewhere"} {
		if SafeNext(next) != "/" {
			t.Errorf("unsafe next accepted: %q", next)
		}
	}
	if SafeNext("/#/hosts?q=1") != "/#/hosts?q=1" {
		t.Fatal("hash route refused")
	}
	c := config()
	dir := t.TempDir()
	m, err := Open(dir, func() Config { return c }, nil)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "state-session-key"))
	if err != nil || info.Mode().Perm() != 0600 || info.Size() != 32 {
		t.Fatal("key permissions/length")
	}
	now := time.Now()
	value := m.Issue("User@Example.org", now)
	if s, ok := m.Verify(value, now); !ok || s.Email != "user@example.org" {
		t.Fatal("valid session refused")
	}
	reopened, err := Open(dir, func() Config { return c }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Verify(value, now); !ok {
		t.Fatal("restart lost signing key")
	}
	for _, v := range []string{value + "x", "x" + value, "invalid"} {
		if _, ok := m.Verify(v, now); ok {
			t.Fatal("tampered session accepted")
		}
	}
	if _, ok := m.Verify(value, now.Add(time.Hour)); ok {
		t.Fatal("expired session accepted")
	}
	c.Generation++
	if _, ok := m.Verify(value, now); ok {
		t.Fatal("old generation accepted")
	}
	c.Generation--
	c.Allowed = []string{"other@example.org"}
	if _, ok := m.Verify(value, now); ok {
		t.Fatal("removed account accepted")
	}
	c = config()
	c.Enabled = false
	if _, ok := m.Verify(value, now); ok {
		t.Fatal("disabled sign-in accepted")
	}
}

// One loopback fixture exercises the wire flow, including PKCE and both client
// authentication methods. No provider credentials or real services are used.
func TestProviderRoundTrip(t *testing.T) {
	var issuer, challenge, state, nonce, callback string
	var mode atomic.Int32
	var tokenCalls atomic.Int32
	var postOnly bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			methods := []string{"client_secret_basic", "client_secret_post"}
			if postOnly {
				methods = []string{"client_secret_post"}
			}
			p := Provider{Issuer: issuer, Authorization: issuer + "/authorize", Token: issuer + "/token", Userinfo: issuer + "/userinfo", AuthMethods: methods}
			if mode.Load() == 4 {
				p.Issuer = issuer + "/wrong"
			}
			if mode.Load() == 5 {
				p.Token = "http://id.example.org/token"
			}
			json.NewEncoder(w).Encode(p)
		case "/token":
			if mode.Load() == 6 {
				http.Redirect(w, r, issuer+"/userinfo", 302)
				return
			}
			if mode.Load() == 7 {
				w.WriteHeader(400)
				return
			}
			tokenCalls.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if base64.RawURLEncoding.EncodeToString(digest[:]) != challenge || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fixture-code" || r.Form.Get("redirect_uri") != callback {
				t.Error("wrong PKCE or exchange fields")
			}
			if postOnly {
				if r.Header.Get("Authorization") != "" || r.Form.Get("client_secret") != "synthetic-secret" || r.Form.Get("client_id") != "client" {
					t.Error("wrong post credentials")
				}
			} else {
				user, pass, ok := r.BasicAuth()
				if !ok || user != "client" || pass != "synthetic-secret" || r.Form.Get("client_secret") != "" {
					t.Error("wrong basic credentials")
				}
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "fixture-access"})
		case "/userinfo":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				t.Error("missing access token")
			}
			email, verified := "User@Example.org", true
			if mode.Load() == 1 {
				verified = false
			}
			if mode.Load() == 2 {
				email = "user@other.example"
			}
			if mode.Load() == 3 {
				json.NewEncoder(w).Encode(map[string]string{"email": email})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"email": email, "email_verified": verified})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	issuer = fake.URL
	c := config()
	c.Issuer = issuer
	c.PublicURL = "http://127.0.0.1:8080"
	callback = c.RedirectURI()
	m, err := Open(t.TempDir(), func() Config { return c }, nil)
	if err != nil {
		t.Fatal(err)
	}
	login := func(next string) *http.Cookie {
		rec := httptest.NewRecorder()
		m.Login(rec, httptest.NewRequest("GET", "/auth/login?next="+url.QueryEscape(next), nil))
		if rec.Code != 302 {
			t.Fatalf("login: %d", rec.Code)
		}
		u, _ := url.Parse(rec.Header().Get("Location"))
		q := u.Query()
		challenge = q.Get("code_challenge")
		state = q.Get("state")
		nonce = q.Get("nonce")
		if len(state) < 32 || len(nonce) < 32 || challenge == "" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid email profile" {
			t.Fatal("missing flow bindings")
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Name != stateCookie || cookies[0].Path != "/auth/" || cookies[0].MaxAge != 600 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
			t.Fatal("state cookie attributes")
		}
		return cookies[0]
	}
	for _, tc := range []struct {
		name       string
		mode       int32
		next, want string
		post       bool
	}{
		{"success", 0, "/#/hosts", "/#/hosts", false},
		{"open redirect", 0, "//evil.example", "/", false},
		{"post only", 0, "/", "/", true},
		{"unverified", 1, "/", "/#/login?error=unverified", false},
		{"not allowed", 2, "/", "/#/login?error=not_allowed", false},
		{"verified absent", 3, "/", "/#/login?error=unverified", false},
		{"redirect refused", 6, "/", "/#/login?error=provider", false},
		{"token refused", 7, "/", "/#/login?error=provider", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode.Store(tc.mode)
			postOnly = tc.post
			ck := login(tc.next)
			req := httptest.NewRequest("GET", "/auth/callback?code=fixture-code&state="+state, nil)
			req.AddCookie(ck)
			rec := httptest.NewRecorder()
			m.Callback(rec, req)
			if rec.Code != 302 || rec.Header().Get("Location") != tc.want {
				t.Fatalf("callback: %d %s", rec.Code, rec.Header().Get("Location"))
			}
			sessionFound, cleared := false, false
			for _, cookie := range rec.Result().Cookies() {
				if cookie.Name == stateCookie && cookie.MaxAge < 0 {
					cleared = true
				}
				if cookie.Name == SessionCookie {
					sessionFound = true
					if _, ok := m.Verify(cookie.Value, time.Now()); !ok || cookie.MaxAge != 3600 || cookie.Path != "/" || !cookie.HttpOnly {
						t.Error("invalid session cookie")
					}
				}
			}
			if !cleared || sessionFound != (tc.mode == 0) {
				t.Fatal("incorrect cookie result")
			}
		})
	}
	mode.Store(0)
	ck := login("/")
	for _, tc := range []struct {
		query, want string
		cookie      bool
	}{
		{"state=wrong&code=fixture-code", "state", true},
		{"state=" + state + "&error=access_denied", "denied", true},
		{"state=" + state + "&code=fixture-code", "state", false},
	} {
		before := tokenCalls.Load()
		req := httptest.NewRequest("GET", "/auth/callback?"+tc.query, nil)
		if tc.cookie {
			req.AddCookie(ck)
		}
		rec := httptest.NewRecorder()
		m.Callback(rec, req)
		if rec.Header().Get("Location") != "/#/login?error="+tc.want || tokenCalls.Load() != before {
			t.Fatal("failed callback reached exchange")
		}
	}
	for _, st := range []loginState{
		{State: state, Exp: time.Now().Add(-time.Second).Unix(), Generation: c.Generation},
		{State: state, Exp: time.Now().Add(time.Minute).Unix(), Generation: c.Generation + 1},
	} {
		req := httptest.NewRequest("GET", "/auth/callback?state="+state+"&code=fixture-code", nil)
		req.AddCookie(&http.Cookie{Name: stateCookie, Value: m.seal(st)})
		rec := httptest.NewRecorder()
		m.Callback(rec, req)
		if rec.Header().Get("Location") != "/#/login?error=state" {
			t.Fatal("stale login state accepted")
		}
	}
	for _, v := range []int32{4, 5} {
		mode.Store(v)
		if _, err := m.Discover(context.Background(), c); err == nil {
			t.Fatal("unsafe discovery accepted")
		}
	}
	c.Enabled = false
	rec := httptest.NewRecorder()
	m.Login(rec, httptest.NewRequest("GET", "/auth/login", nil))
	if rec.Code != 404 {
		t.Fatal("disabled login")
	}
	rec = httptest.NewRecorder()
	m.Callback(rec, httptest.NewRequest("GET", "/auth/callback", nil))
	if !strings.HasSuffix(rec.Header().Get("Location"), "disabled") {
		t.Fatal("disabled callback")
	}
	for i := 0; i < 20; i++ {
		if !m.allowCallback("127.0.0.2:1", time.Now()) {
			t.Fatal("early rate limit")
		}
	}
	if m.allowCallback("127.0.0.2:2", time.Now()) {
		t.Fatal("source port bypassed rate limit")
	}
}
