package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/auth/passkeytest"
	"github.com/ficaa1/homelab-inventory/internal/settings"
	"github.com/ficaa1/homelab-inventory/internal/setup"
)

const testPasskeyOrigin = "https://dash.example.org"

type passkeyCall struct {
	method, path, origin, bearer string
	body                         any
	cookie                       *http.Cookie
}

func (c passkeyCall) do(h http.Handler) *httptest.ResponseRecorder {
	var body string
	if c.body != nil {
		b, _ := json.Marshal(c.body)
		body = string(b)
	}
	r := httptest.NewRequest(c.method, c.path, strings.NewReader(body))
	r.Header.Set("X-Requested-By", "homelab-inventory")
	if c.origin != "" {
		r.Header.Set("Origin", c.origin)
	}
	if c.bearer != "" {
		r.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.cookie != nil {
		r.AddCookie(c.cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// addPasskey runs the registration ceremony with the API token.
func addPasskey(t *testing.T, h http.Handler, a *passkeytest.Authenticator, name string) *httptest.ResponseRecorder {
	t.Helper()
	w := passkeyCall{method: "POST", path: "/settings/passkeys/options", origin: a.Origin, bearer: "fixture-api-token"}.do(h)
	var options struct {
		Challenge string `json:"challenge"`
		RP        struct {
			ID string `json:"id"`
		} `json:"rp"`
		Exclude []struct {
			ID string `json:"id"`
		} `json:"excludeCredentials"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &options) != nil || options.Challenge == "" {
		t.Fatalf("registration options: %d %s", w.Code, w.Body)
	}
	if options.RP.ID != a.RPID {
		t.Fatalf("relying party %q", options.RP.ID)
	}
	e := base64.RawURLEncoding.EncodeToString
	clientData, attestation := a.Create(options.Challenge)
	return passkeyCall{method: "POST", path: "/settings/passkeys", origin: a.Origin, bearer: "fixture-api-token",
		body: map[string]string{"name": name, "clientDataJSON": e(clientData), "attestationObject": e(attestation)}}.do(h)
}

// passkeySignIn runs the sign-in ceremony and returns the session cookie.
func passkeySignIn(t *testing.T, h http.Handler, a *passkeytest.Authenticator) (*http.Cookie, int) {
	t.Helper()
	w := passkeyCall{method: "POST", path: "/auth/passkey/options", origin: a.Origin}.do(h)
	var options struct {
		Challenge string `json:"challenge"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &options) != nil {
		return nil, w.Code
	}
	e := base64.RawURLEncoding.EncodeToString
	clientData, authData, signature := a.Get(options.Challenge)
	w = passkeyCall{method: "POST", path: "/auth/passkey/login", origin: a.Origin,
		body: map[string]string{"id": a.CredentialID(), "clientDataJSON": e(clientData), "authenticatorData": e(authData), "signature": e(signature)}}.do(h)
	for _, ck := range w.Result().Cookies() {
		return ck, w.Code
	}
	return nil, w.Code
}

func TestPasskeyRegistrationAndSignIn(t *testing.T) {
	s := testServer(t, "fixture-api-token")
	s.operatorToken = "fixture-operator-token"
	h := s.routes()
	status := func(ck *http.Cookie) map[string]any {
		var out map[string]any
		json.Unmarshal(passkeyCall{method: "GET", path: "/auth/status", cookie: ck}.do(h).Body.Bytes(), &out)
		return out
	}
	a := passkeytest.New(t, testPasskeyOrigin, passkeytest.ES256)

	// Nothing is offered, and nobody gets in, before a passkey exists.
	if st := status(nil); st["passkeys"] != false || st["enabled"] != false {
		t.Fatalf("fresh status: %v", st)
	}
	if _, code := passkeySignIn(t, h, a); code != 404 {
		t.Fatalf("sign-in with no passkeys: %d", code)
	}
	if s.offersBySession() {
		t.Fatal("sessions offered without any way to sign in")
	}

	// Registration needs the API token, and a page origin a passkey can exist for.
	for _, c := range []passkeyCall{
		{method: "GET", path: "/settings/passkeys"},
		{method: "POST", path: "/settings/passkeys/options", origin: testPasskeyOrigin},
		{method: "POST", path: "/settings/passkeys", origin: testPasskeyOrigin, body: map[string]string{}},
		{method: "DELETE", path: "/settings/passkeys/x", origin: testPasskeyOrigin},
	} {
		if w := c.do(h); w.Code != 401 {
			t.Errorf("%s %s without credentials: %d", c.method, c.path, w.Code)
		}
	}
	for _, origin := range []string{"", "http://192.0.2.10:8101", "http://dash.example.org", "https://192.0.2.10"} {
		if w := (passkeyCall{method: "POST", path: "/settings/passkeys/options", origin: origin, bearer: "fixture-api-token"}).do(h); w.Code != 400 {
			t.Errorf("registration options from %q: %d", origin, w.Code)
		}
	}
	if w := addPasskey(t, h, a, " "); w.Code != 422 {
		t.Errorf("blank name: %d %s", w.Code, w.Body)
	}
	w := addPasskey(t, h, a, "Bitwarden")
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"name": "Bitwarden"`) || strings.Contains(w.Body.String(), "public_key") {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := addPasskey(t, h, a, "again"); w.Code != 409 {
		t.Errorf("same credential twice: %d", w.Code)
	}
	w = passkeyCall{method: "POST", path: "/settings/passkeys/options", origin: testPasskeyOrigin, bearer: "fixture-api-token"}.do(h)
	if !strings.Contains(w.Body.String(), a.CredentialID()) {
		t.Error("registered credential is not excluded from a second registration")
	}
	if st := status(nil); st["passkeys"] != true || st["signed_in"] != false {
		t.Fatalf("status with a passkey: %v", st)
	}

	// Sign in. The session does what a provider session does, bound to its origin.
	ck, code := passkeySignIn(t, h, a)
	if code != 204 || ck == nil {
		t.Fatalf("sign-in: %d", code)
	}
	if st := status(ck); st["signed_in"] != true || st["passkey"] != "Bitwarden" || st["email"] != nil || st["expires_at"] == nil {
		t.Fatalf("signed-in status: %v", st)
	}
	if !s.offersBySession() {
		t.Error("a passkey session cannot change offers")
	}
	for _, tc := range []struct {
		method, path, origin string
		want                 int
	}{
		{"GET", "/hosts", "", 200},
		{"GET", "/settings/passkeys", "", 200},
		{"PUT", "/settings/auth", testPasskeyOrigin, 200},
		{"PUT", "/settings/auth", "https://evil.example", 403},
		{"POST", "/agent/offers", testPasskeyOrigin, 415},
		{"POST", "/inventory", testPasskeyOrigin, 401},
	} {
		// Saving sign-in settings signs everyone out, so sign in for each.
		ck, _ := passkeySignIn(t, h, a)
		if w := (passkeyCall{method: tc.method, path: tc.path, origin: tc.origin, cookie: ck, body: map[string]any{}}).do(h); w.Code != tc.want {
			t.Errorf("session %s %s from %q: %d %s", tc.method, tc.path, tc.origin, w.Code, w.Body)
		}
	}
	ck, _ = passkeySignIn(t, h, a)
	r := httptest.NewRequest("GET", "/audit", nil)
	r.AddCookie(ck)
	s.auth(func(w http.ResponseWriter, r *http.Request) {
		if requestActor(r) != "passkey:Bitwarden" {
			t.Errorf("audit actor %q", requestActor(r))
		}
	})(httptest.NewRecorder(), r)
	st, _ := s.readSettings()
	if len(st.Passkeys) != 1 || st.Passkeys[0].LastUsed == nil {
		t.Fatalf("stored passkeys: %+v", st.Passkeys)
	}

	// A notification-settings reset keeps passkeys; sign-out clears the cookie.
	if w := (passkeyCall{method: "POST", path: "/settings/reset", bearer: "fixture-api-token"}).do(h); w.Code != 200 {
		t.Fatalf("reset: %d", w.Code)
	}
	if st, _ := s.readSettings(); len(st.Passkeys) != 1 {
		t.Fatal("settings reset dropped passkeys")
	}
	if w := (passkeyCall{method: "POST", path: "/auth/logout", origin: "https://evil.example", cookie: ck}).do(h); w.Code != 403 {
		t.Errorf("logout from another origin: %d", w.Code)
	}
	w = passkeyCall{method: "POST", path: "/auth/logout", origin: testPasskeyOrigin, cookie: ck}.do(h)
	if w.Code != 204 || len(w.Result().Cookies()) != 1 || w.Result().Cookies()[0].MaxAge >= 0 || !w.Result().Cookies()[0].Secure {
		t.Errorf("logout: %d %v", w.Code, w.Result().Cookies())
	}

	// An unclaimed instance offers no passkey sign-in and honours no session.
	claimed := s.setup
	var err error
	if s.setup, err = setup.Open(t.TempDir(), setup.Tokens{}, time.Now(), setup.Window, time.Now); err != nil {
		t.Fatal(err)
	}
	if st := status(ck); st["passkeys"] != false || st["signed_in"] != false {
		t.Errorf("unclaimed status: %v", st)
	}
	if _, code := passkeySignIn(t, h, a); code != 503 {
		t.Errorf("unclaimed sign-in: %d", code)
	}
	s.setup = claimed

	// Removing the passkey ends its sessions and its sign-in.
	if w := (passkeyCall{method: "DELETE", path: "/settings/passkeys/unknown", bearer: "fixture-api-token"}).do(h); w.Code != 404 {
		t.Errorf("remove unknown: %d", w.Code)
	}
	w = passkeyCall{method: "DELETE", path: "/settings/passkeys/" + a.CredentialID(), bearer: "fixture-api-token"}.do(h)
	if w.Code != 200 || strings.Contains(w.Body.String(), "Bitwarden") {
		t.Fatalf("remove: %d %s", w.Code, w.Body)
	}
	if w := (passkeyCall{method: "GET", path: "/hosts", cookie: ck}).do(h); w.Code != 401 {
		t.Errorf("session after removal: %d", w.Code)
	}
	if _, code := passkeySignIn(t, h, a); code != 404 {
		t.Errorf("sign-in after removal: %d", code)
	}
}

func TestPasskeyListIsBounded(t *testing.T) {
	s := testServer(t, "fixture-api-token")
	h := s.routes()
	st, _ := s.readSettings()
	for len(st.Passkeys) < 20 {
		st.Passkeys = append(st.Passkeys, auth.Passkey{ID: fmt.Sprint("fixture-", len(st.Passkeys)), Name: "fixture", Origin: testPasskeyOrigin})
	}
	if err := s.store.WriteState(settings.StateName, st); err != nil {
		t.Fatal(err)
	}
	if w := (passkeyCall{method: "POST", path: "/settings/passkeys/options", origin: testPasskeyOrigin, bearer: "fixture-api-token"}).do(h); w.Code != 409 {
		t.Errorf("options with a full list: %d", w.Code)
	}
}

// A restore keeps the destination's passkeys, never the backup's: they are
// bound to the address this instance is opened at.
func TestRestoreKeepsDestinationPasskeys(t *testing.T) {
	s := testServer(t, "fixture-api-token")
	source := settings.Stored{Passkeys: []auth.Passkey{{ID: "from-backup", Name: "source", Origin: "https://old.example.org"}}}
	staged := func() settings.Stored {
		t.Helper()
		dir := t.TempDir()
		b, _ := json.Marshal(source)
		if err := os.WriteFile(filepath.Join(dir, "state-settings.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.preserveBackupCredentials(dir); err != nil {
			t.Fatal(err)
		}
		var out settings.Stored
		b, _ = os.ReadFile(filepath.Join(dir, "state-settings.json"))
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := staged(); len(out.Passkeys) != 0 {
		t.Errorf("backup passkeys installed: %+v", out.Passkeys)
	}
	if err := s.store.WriteState(settings.StateName, settings.Stored{Passkeys: []auth.Passkey{{ID: "here", Name: "destination", Origin: testPasskeyOrigin}}}); err != nil {
		t.Fatal(err)
	}
	if out := staged(); len(out.Passkeys) != 1 || out.Passkeys[0].ID != "here" {
		t.Errorf("destination passkeys not kept: %+v", out.Passkeys)
	}
}
