package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth/passkeytest"
)

const passkeyOrigin = "https://dash.example.org"

// passkeyRig is a Manager whose config holds the passkeys registered so far.
type passkeyRig struct {
	t   *testing.T
	m   *Manager
	c   Config
	log string // the last line logged, which names why a sign-in failed
}

func newPasskeyRig(t *testing.T) *passkeyRig {
	t.Helper()
	rig := &passkeyRig{t: t, c: Config{SessionHours: 1, Generation: 3}}
	m, err := Open(t.TempDir(), func() Config { return rig.c }, func(format string, args ...any) { rig.log = fmt.Sprintf(format, args...) })
	if err != nil {
		t.Fatal(err)
	}
	rig.m = m
	return rig
}
func (rig *passkeyRig) register(a *passkeytest.Authenticator, name string) Passkey {
	rig.t.Helper()
	challenge, err := rig.m.RegistrationChallenge(time.Now())
	if err != nil {
		rig.t.Fatal(err)
	}
	clientData, attestation := a.Create(challenge)
	p, err := rig.m.Register(a.Origin, clientData, attestation, time.Now())
	if err != nil {
		rig.t.Fatalf("register: %v", err)
	}
	p.Name = name
	rig.c.Passkeys = append(rig.c.Passkeys, p)
	return p
}
func (rig *passkeyRig) used(id string, count uint32, at time.Time) error {
	for i := range rig.c.Passkeys {
		if rig.c.Passkeys[i].ID == id {
			rig.c.Passkeys[i].SignCount, rig.c.Passkeys[i].LastUsed = count, &at
			return nil
		}
	}
	return errors.New("removed")
}

var peers int

func passkeyPost(origin string, body any) *http.Request {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", "/auth/passkey/login", bytes.NewReader(b))
	// Each request comes from its own peer, so only the limiter test meets the limit.
	peers++
	r.RemoteAddr = fmt.Sprintf("10.%d.%d.%d:4000", peers>>16&255, peers>>8&255, peers&255)
	r.Header.Set("X-Requested-By", "homelab-inventory")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}
func (rig *passkeyRig) challenge() string {
	rig.t.Helper()
	w := httptest.NewRecorder()
	rig.m.PasskeyOptions(w, passkeyPost(passkeyOrigin, nil))
	var out struct {
		Challenge string `json:"challenge"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Challenge == "" {
		rig.t.Fatalf("options: %d %s", w.Code, w.Body)
	}
	return out.Challenge
}
func assertion(a *passkeytest.Authenticator, challenge string) map[string]string {
	e := base64.RawURLEncoding.EncodeToString
	clientData, authData, signature := a.Get(challenge)
	return map[string]string{"id": a.CredentialID(), "clientDataJSON": e(clientData), "authenticatorData": e(authData), "signature": e(signature)}
}
func (rig *passkeyRig) login(origin string, body map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	rig.m.PasskeyLogin(w, passkeyPost(origin, body), rig.used)
	return w
}

func TestPasskeySignIn(t *testing.T) {
	for _, alg := range []int{passkeytest.ES256, passkeytest.EdDSA, passkeytest.RS256} {
		rig := newPasskeyRig(t)
		a := passkeytest.New(t, passkeyOrigin, alg)
		p := rig.register(a, "Bitwarden")
		if p.Algorithm != alg || p.ID != a.CredentialID() || p.Origin != passkeyOrigin {
			t.Fatalf("alg %d: stored %+v", alg, p)
		}
		w := rig.login(passkeyOrigin, assertion(a, rig.challenge()))
		if w.Code != 204 {
			t.Fatalf("alg %d: login %d %s", alg, w.Code, w.Body)
		}
		ck := w.Result().Cookies()
		if len(ck) != 1 || ck[0].Name != SessionCookie || !ck[0].HttpOnly || !ck[0].Secure || ck[0].MaxAge != 3600 {
			t.Fatalf("alg %d: cookie %+v", alg, ck)
		}
		s, ok := rig.m.Verify(ck[0].Value, time.Now())
		if !ok || s.Passkey != p.ID || s.Email != "" || rig.c.Actor(s) != "passkey:Bitwarden" {
			t.Fatalf("alg %d: session %+v %v", alg, s, ok)
		}
		if rig.c.Passkeys[0].LastUsed == nil {
			t.Fatal("use was not recorded")
		}
		// The session belongs to the passkey's origin, with no public URL set.
		for origin, want := range map[string]bool{passkeyOrigin: true, "https://evil.example": false, "": true} {
			r := passkeyPost(origin, nil)
			if CSRF(r, rig.c, s) != want {
				t.Errorf("CSRF from %q", origin)
			}
		}
		if _, ok := rig.m.Verify(ck[0].Value, time.Now().Add(2*time.Hour)); ok {
			t.Error("expired session accepted")
		}
		rig.c.Generation++
		if _, ok := rig.m.Verify(ck[0].Value, time.Now()); ok {
			t.Error("session survived a settings save")
		}
		rig.c.Generation--
		rig.c.Passkeys = nil
		if _, ok := rig.m.Verify(ck[0].Value, time.Now()); ok {
			t.Error("session survived its passkey's removal")
		}
	}
}

func TestPasskeySignInRefusals(t *testing.T) {
	rig := newPasskeyRig(t)
	a := passkeytest.New(t, passkeyOrigin, passkeytest.ES256)
	rig.register(a, "one")
	other := passkeytest.New(t, passkeyOrigin, passkeytest.ES256)
	flip := func(field string) func(map[string]string) {
		return func(b map[string]string) {
			raw, _ := base64.RawURLEncoding.DecodeString(b[field])
			raw[len(raw)-1] ^= 1
			b[field] = base64.RawURLEncoding.EncodeToString(raw)
		}
	}
	for _, tc := range []struct {
		name   string
		forge  func(*passkeytest.Authenticator)
		mangle func(map[string]string)
		origin string
		want   int
		reason string
	}{
		{name: "phished origin", forge: func(a *passkeytest.Authenticator) { a.Origin = "https://dash.example.org.evil.example" }, want: 401, reason: "origin"},
		{name: "other relying party", forge: func(a *passkeytest.Authenticator) { a.RPID = "evil.example" }, want: 401, reason: "relying_party"},
		{name: "no user presence", forge: func(a *passkeytest.Authenticator) { a.Flags = 0x04 }, want: 401, reason: "user_presence"},
		{name: "tampered signature", mangle: flip("signature"), want: 401, reason: "signature"},
		{name: "tampered authenticator data", mangle: flip("authenticatorData"), want: 401, reason: "signature"},
		{name: "unregistered credential", mangle: func(b map[string]string) { b["id"] = other.CredentialID() }, want: 401, reason: "unknown"},
		{name: "another key's signature", mangle: func(b map[string]string) {
			for k, v := range assertion(other, "unused") {
				if k != "id" {
					b[k] = v
				}
			}
		}, want: 401, reason: "signature"},
		{name: "empty body", mangle: func(b map[string]string) { clear(b) }, want: 401, reason: "request"},
		{name: "address without a passkey", origin: "https://other.example.org", want: 404},
	} {
		rig.log = ""
		forged := *a
		if tc.forge != nil {
			tc.forge(&forged)
		}
		body := assertion(&forged, rig.challenge())
		if tc.mangle != nil {
			tc.mangle(body)
		}
		origin := tc.origin
		if origin == "" {
			origin = passkeyOrigin
		}
		w := rig.login(origin, body)
		if w.Code != tc.want || len(w.Result().Cookies()) != 0 || rig.log != "" && rig.log != "passkey sign-in failed: "+tc.reason {
			t.Errorf("%s: %d %s (%s)", tc.name, w.Code, w.Body, rig.log)
		}
	}

	// A registration answer is not a sign-in, and a challenge is spent once.
	challenge := rig.challenge()
	clientData, _ := a.Create(challenge)
	body := assertion(a, challenge)
	body["clientDataJSON"] = base64.RawURLEncoding.EncodeToString(clientData)
	if w := rig.login(passkeyOrigin, body); w.Code != 401 || !strings.HasSuffix(rig.log, ": client_data") {
		t.Errorf("create client data: %d (%s)", w.Code, rig.log)
	}
	body = assertion(a, challenge)
	if w := rig.login(passkeyOrigin, body); w.Code != 204 {
		t.Fatalf("valid sign-in: %d %s", w.Code, w.Body)
	}
	if w := rig.login(passkeyOrigin, body); w.Code != 401 || !strings.HasSuffix(rig.log, ": challenge") {
		t.Errorf("replayed assertion: %d (%s)", w.Code, rig.log)
	}
	if w := rig.login(passkeyOrigin, assertion(a, "never-issued")); w.Code != 401 || !strings.HasSuffix(rig.log, ": challenge") {
		t.Errorf("unissued challenge: %d (%s)", w.Code, rig.log)
	}
	create, err := rig.m.RegistrationChallenge(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if w := rig.login(passkeyOrigin, assertion(a, create)); w.Code != 401 {
		t.Errorf("registration challenge used to sign in: %d", w.Code)
	}

	// A counting authenticator must advance; a copy replaying an old count fails.
	a.Counter = 5
	if w := rig.login(passkeyOrigin, assertion(a, rig.challenge())); w.Code != 204 {
		t.Fatalf("counter 5: %d", w.Code)
	}
	for _, n := range []uint32{5, 4, 0} {
		a.Counter = n
		if w := rig.login(passkeyOrigin, assertion(a, rig.challenge())); w.Code != 401 || !strings.HasSuffix(rig.log, ": counter") {
			t.Errorf("counter %d after 5: %d (%s)", n, w.Code, rig.log)
		}
	}

	// The page's header and an Origin are required; a settings save or removal
	// during the ceremony leaves no session.
	r := passkeyPost(passkeyOrigin, nil)
	r.Header.Del("X-Requested-By")
	w := httptest.NewRecorder()
	rig.m.PasskeyOptions(w, r)
	if w.Code != 403 {
		t.Errorf("options without header: %d", w.Code)
	}
	w = httptest.NewRecorder()
	rig.m.PasskeyOptions(w, passkeyPost("", nil))
	if w.Code != 403 {
		t.Errorf("options without origin: %d", w.Code)
	}
	a.Counter = 9
	w = httptest.NewRecorder()
	rig.m.PasskeyLogin(w, passkeyPost(passkeyOrigin, assertion(a, rig.challenge())), func(string, uint32, time.Time) error {
		rig.c.Generation++
		return nil
	})
	if w.Code != 401 || len(w.Result().Cookies()) != 0 {
		t.Errorf("save during sign-in: %d", w.Code)
	}
	// Both steps share the per-peer limit with the provider callback.
	for i := 0; i <= 20; i++ {
		r := passkeyPost(passkeyOrigin, nil)
		r.RemoteAddr = "192.0.2.7:1"
		w = httptest.NewRecorder()
		rig.m.PasskeyOptions(w, r)
		if want := map[bool]int{true: 200, false: 429}[i < 20]; w.Code != want {
			t.Fatalf("request %d from one peer: %d", i, w.Code)
		}
	}
	rig.c.Passkeys = nil
	w = httptest.NewRecorder()
	rig.m.PasskeyOptions(w, passkeyPost(passkeyOrigin, nil))
	if w.Code != 404 {
		t.Errorf("options with no passkeys: %d", w.Code)
	}
}

func TestPasskeyRegistrationRefusals(t *testing.T) {
	rig := newPasskeyRig(t)
	for _, tc := range []struct {
		name   string
		forge  func(*passkeytest.Authenticator)
		origin string
	}{
		{name: "reported origin differs", forge: func(a *passkeytest.Authenticator) { a.Origin = "https://evil.example" }},
		{name: "other relying party", forge: func(a *passkeytest.Authenticator) { a.RPID = "evil.example" }},
		{name: "no user presence", forge: func(a *passkeytest.Authenticator) { a.Flags = 0 }},
		{name: "plain http", origin: "http://dash.example.org"},
		{name: "ip address", origin: "https://192.0.2.1"},
	} {
		origin := tc.origin
		if origin == "" {
			origin = passkeyOrigin
		}
		a := passkeytest.New(t, origin, passkeytest.ES256)
		if tc.forge != nil {
			tc.forge(a)
		}
		challenge, _ := rig.m.RegistrationChallenge(time.Now())
		clientData, attestation := a.Create(challenge)
		if _, err := rig.m.Register(origin, clientData, attestation, time.Now()); err == nil {
			t.Errorf("%s: registered", tc.name)
		}
	}
	a := passkeytest.New(t, passkeyOrigin, passkeytest.ES256)
	challenge, _ := rig.m.RegistrationChallenge(time.Now())
	clientData, attestation := a.Create(challenge)
	if _, err := rig.m.Register(passkeyOrigin, clientData, attestation[:len(attestation)-1], time.Now()); err == nil {
		t.Error("truncated attestation registered")
	}
	// An assertion is not a registration; a sign-in challenge is not one either.
	getData, _, _ := a.Get(challenge)
	if _, err := rig.m.Register(passkeyOrigin, getData, attestation, time.Now()); err == nil {
		t.Error("get client data registered")
	}
	if _, err := rig.m.Register(passkeyOrigin, clientData, attestation, time.Now().Add(6*time.Minute)); err == nil {
		t.Error("expired challenge registered")
	}
	if _, err := rig.m.Register(passkeyOrigin, clientData, attestation, time.Now()); err == nil {
		t.Error("challenge survived a failed redemption")
	}
	clientData, attestation = a.Create("never-issued")
	if _, err := rig.m.Register(passkeyOrigin, clientData, attestation, time.Now()); err == nil {
		t.Error("unissued challenge registered")
	}
	// Pending challenges are bounded, and expired ones make room again.
	now := time.Now()
	for len(rig.m.challenges) < maxChallenges {
		if _, err := rig.m.challenge("get", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rig.m.challenge("get", now); !errors.Is(err, ErrChallengesFull) {
		t.Errorf("challenge beyond the bound: %v", err)
	}
	if _, err := rig.m.challenge("get", now.Add(challengeTTL)); err != nil || len(rig.m.challenges) != 1 {
		t.Errorf("expired challenges kept: %v, %d", err, len(rig.m.challenges))
	}
}

func TestPasskeyPolicy(t *testing.T) {
	for origin, want := range map[string]string{
		"https://dash.example.org":      "dash.example.org",
		"https://dash.example.org:8443": "dash.example.org",
		"http://localhost:8080":         "localhost",
		"http://dash.example.org":       "",
		"https://192.0.2.1":             "",
		"https://[2001:db8::1]":         "",
		"http://127.0.0.1:8080":         "",
		"https://dash.example.org/":     "",
		"https://DASH.example.org":      "",
		"https://user@dash.example.org": "",
		"null":                          "",
		"":                              "",
	} {
		if got, ok := PasskeyRPID(origin); got != want || ok != (want != "") {
			t.Errorf("PasskeyRPID(%q) = %q, %v", origin, got, ok)
		}
	}
	for name, want := range map[string]bool{"Bitwarden": true, "Filip's phone": true, "": false, " padded": false, "two\nlines": false, strings.Repeat("x", 65): false} {
		if ValidPasskeyName(name) != want {
			t.Errorf("ValidPasskeyName(%q)", name)
		}
	}
	for _, raw := range [][]byte{
		{}, {0x18}, {0x5f, 0x41, 0x00, 0xff}, {0x9f, 0xff}, {0xbf, 0xff}, // truncated, indefinite lengths
		{0x5a, 0xff, 0xff, 0xff, 0xff}, {0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // lengths past the input
		{0xa1, 0x80, 0x00}, {0xa2, 0x01, 0x00, 0x01, 0x00}, // array as a key, duplicate key
		{0xfb, 0, 0, 0, 0, 0, 0, 0, 0}, {0x62, 0xff, 0xfe}, // float, invalid text
		bytes.Repeat([]byte{0x81}, 64), // nesting
	} {
		if _, _, err := cborItem(raw, 0); err == nil {
			t.Errorf("CBOR % x accepted", raw)
		}
	}
	v, rest, err := cborItem([]byte{0xa2, 0x01, 0x26, 0x63, 'f', 'm', 't', 0x82, 0x41, 0x07, 0xf5, 0xaa}, 0)
	m, _ := v.(map[any]any)
	list, _ := m["fmt"].([]any)
	if err != nil || len(rest) != 1 || m[int64(1)] != int64(-7) || len(list) != 2 || !bytes.Equal(list[0].([]byte), []byte{7}) || list[1] != true {
		t.Errorf("CBOR decode: %v %v %v", v, rest, err)
	}
}
