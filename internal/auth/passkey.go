package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	challengeTTL  = 5 * time.Minute
	maxChallenges = 1024
)

var ErrChallengesFull = errors.New("too many pending passkey requests")

// challenge issues a random value that can be redeemed once, for one purpose,
// within five minutes. They live in memory: a restart drops pending ceremonies.
func (m *Manager) challenge(purpose string, now time.Time) (string, error) {
	value, err := random()
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, exp := range m.challenges {
		if !now.Before(exp) {
			delete(m.challenges, k)
		}
	}
	if len(m.challenges) >= maxChallenges {
		return "", ErrChallengesFull
	}
	m.challenges[purpose+":"+value] = now.Add(challengeTTL)
	return value, nil
}

func (m *Manager) redeem(purpose, value string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.challenges[purpose+":"+value]
	delete(m.challenges, purpose+":"+value)
	return ok && now.Before(exp)
}

// RegistrationChallenge starts adding a passkey. The caller has already
// authorized the request.
func (m *Manager) RegistrationChallenge(now time.Time) (string, error) {
	return m.challenge("create", now)
}

// Register checks a navigator.credentials.create() answer made on origin and
// returns the credential to store. The error names a fixed category only.
func (m *Manager) Register(origin string, clientDataJSON, attestationObject []byte, now time.Time) (Passkey, error) {
	p, challenge, err := parseRegistration(clientDataJSON, attestationObject, origin, now)
	if err != nil {
		return Passkey{}, err
	}
	if !m.redeem("create", challenge, now) {
		return Passkey{}, errors.New("challenge")
	}
	return p, nil
}

func passkeyError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]string{"error": message})
	w.Write(append(b, '\n'))
}

// passkeyRequest applies what both public sign-in steps share: the page's own
// header, the rate limit, and at least one passkey registered for this origin.
func (m *Manager) passkeyRequest(w http.ResponseWriter, r *http.Request) (Config, string, bool) {
	w.Header().Set("Cache-Control", "no-store")
	c := m.config()
	if len(c.Passkeys) == 0 {
		passkeyError(w, http.StatusNotFound, "passkey sign-in is not set up")
		return c, "", false
	}
	if !m.allowCallback(r.RemoteAddr, time.Now()) {
		m.logf("passkey sign-in failed: rate_limit")
		w.Header().Set("Retry-After", "60")
		passkeyError(w, http.StatusTooManyRequests, "too many sign-in attempts; try again in a minute")
		return c, "", false
	}
	origin := r.Header.Get("Origin")
	if r.Header.Get("X-Requested-By") != "homelab-inventory" || len(r.Header.Values("Origin")) != 1 {
		passkeyError(w, http.StatusForbidden, "missing CSRF header")
		return c, "", false
	}
	for _, p := range c.Passkeys {
		if p.Origin == origin {
			return c, origin, true
		}
	}
	passkeyError(w, http.StatusNotFound, "no passkey is registered for this address")
	return c, "", false
}

// PasskeyOptions answers the first sign-in step with a challenge. It names no
// credentials: the passkey is discoverable, so the authenticator offers it.
func (m *Manager) PasskeyOptions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := m.passkeyRequest(w, r); !ok {
		return
	}
	challenge, err := m.challenge("get", time.Now())
	if err != nil {
		passkeyError(w, http.StatusServiceUnavailable, "too many pending sign-ins; try again in a few minutes")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"challenge": challenge, "timeout": int(challengeTTL / time.Millisecond), "userVerification": "preferred"})
}

// PasskeyLogin answers the second sign-in step: it verifies the assertion and
// sets the session cookie. used records the new signature counter and time;
// sign-in fails if it cannot.
func (m *Manager) PasskeyLogin(w http.ResponseWriter, r *http.Request, used func(id string, signCount uint32, at time.Time) error) {
	c, origin, ok := m.passkeyRequest(w, r)
	if !ok {
		return
	}
	fail := func(reason string) {
		// Reason is a fixed category, never request data.
		m.logf("passkey sign-in failed: %s", reason)
		passkeyError(w, http.StatusUnauthorized, "passkey not accepted")
	}
	var in struct {
		ID                string `json:"id"`
		ClientDataJSON    string `json:"clientDataJSON"`
		AuthenticatorData string `json:"authenticatorData"`
		Signature         string `json:"signature"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || json.Unmarshal(body, &in) != nil {
		fail("request")
		return
	}
	var parts [3][]byte
	for i, s := range []string{in.ClientDataJSON, in.AuthenticatorData, in.Signature} {
		if parts[i], err = base64.RawURLEncoding.DecodeString(s); err != nil || len(parts[i]) == 0 {
			fail("request")
			return
		}
	}
	p, found := c.Passkey(in.ID)
	if !found || p.Origin != origin {
		fail("unknown")
		return
	}
	now := time.Now()
	count, challenge, err := p.verifyAssertion(parts[0], parts[1], parts[2])
	if err != nil {
		fail(strings.ReplaceAll(err.Error(), " ", "_"))
		return
	}
	if !m.redeem("get", challenge, now) {
		fail("challenge")
		return
	}
	if err := used(p.ID, count, now); err != nil {
		m.logf("passkey sign-in failed: could not record use")
		passkeyError(w, http.StatusInternalServerError, "could not record the sign-in")
		return
	}
	// Removal or a settings save during the ceremony must not leave a session.
	latest := m.config()
	if _, still := latest.Passkey(p.ID); !still || latest.Generation != c.Generation {
		fail("removed")
		return
	}
	hours := c.sessionHours()
	session := Session{Passkey: p.ID, Origin: p.Origin, Exp: now.Add(time.Duration(hours) * time.Hour).Unix(), Generation: c.Generation}
	cookie(w, strings.HasPrefix(p.Origin, "https://"), SessionCookie, m.seal(session), "/", hours*3600)
	w.WriteHeader(http.StatusNoContent)
}
