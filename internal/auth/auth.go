package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

const SessionCookie = "hi_session"
const stateCookie = "hi_oidc"

// Session is the signed cookie payload. A provider sign-in carries Email; a
// passkey sign-in carries the credential ID and the origin it is bound to.
type Session struct {
	Email      string `json:"email,omitempty"`
	Passkey    string `json:"pk,omitempty"`
	Origin     string `json:"origin,omitempty"`
	Exp        int64  `json:"exp"`
	Generation uint64 `json:"gen"`
}
type loginState struct {
	State      string `json:"state"`
	Nonce      string `json:"nonce"`
	Verifier   string `json:"verifier"`
	Next       string `json:"next"`
	Exp        int64  `json:"exp"`
	Generation uint64 `json:"gen"`
}
type attempt struct {
	start time.Time
	count int
}

// Manager owns the durable signing key, HTTP client, bounded callback limiter
// and pending passkey challenges.
// Config is read afresh for every operation so disable/save takes effect at once.
type Manager struct {
	config     func() Config
	key        []byte
	client     *http.Client
	logf       func(string, ...any)
	mu         sync.Mutex
	attempts   map[string]attempt
	challenges map[string]time.Time
}

func Open(dir string, config func() Config, logf func(string, ...any)) (*Manager, error) {
	path := filepath.Join(dir, "state-session-key")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := fsx.CreateAtomic(path, key, 0600); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("session key must be a regular file with mode 0600")
	}
	if info.Size() != 32 {
		return nil, errors.New("invalid session key length")
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid session key length")
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{config: config, key: key, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: httpx.RefuseRedirects()}, logf: logf, attempts: map[string]attempt{}, challenges: map[string]time.Time{}}, nil
}

func (m *Manager) seal(v any) string {
	b, _ := json.Marshal(v)
	payload := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (m *Manager) unseal(value string, v any) bool {
	if len(value) > 8192 {
		return false
	}
	payload, signature, ok := strings.Cut(value, ".")
	if !ok {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(payload))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	return err == nil && json.Unmarshal(b, v) == nil
}

// Issue is used after provider identity checks; Verify always rechecks policy.
func (m *Manager) Issue(email string, now time.Time) string {
	c := m.config()
	return m.seal(Session{Email: strings.ToLower(strings.TrimSpace(email)), Exp: now.Add(time.Duration(c.SessionHours) * time.Hour).Unix(), Generation: c.Generation})
}
func (m *Manager) Verify(value string, now time.Time) (Session, bool) {
	c := m.config()
	var s Session
	if !m.unseal(value, &s) || now.Unix() >= s.Exp || s.Generation != c.Generation {
		return Session{}, false
	}
	if s.Passkey != "" {
		// A passkey session lasts only while its credential stays registered.
		if p, ok := c.Passkey(s.Passkey); !ok || p.Origin != s.Origin || s.Email != "" {
			return Session{}, false
		}
		return s, true
	}
	if !c.Ready() || !Allowed(s.Email, c.Allowed) {
		return Session{}, false
	}
	return s, true
}

// Actor is the audit identity of a session: the email, or the passkey's name.
func (c Config) Actor(s Session) string {
	if s.Passkey == "" {
		return s.Email
	}
	p, _ := c.Passkey(s.Passkey)
	return "passkey:" + p.Name
}
func (m *Manager) Session(r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(SessionCookie)
	if err != nil {
		return Session{}, false
	}
	return m.Verify(cookie.Value, time.Now())
}

func cookie(w http.ResponseWriter, secure bool, name, value, path string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}
func (c Config) secure() bool { return strings.HasPrefix(c.PublicURL, "https://") }

// CSRF checks an unsafe request authorized by session s: the page's header,
// and, when the browser names an origin, the one the session belongs to. That
// is the configured public URL, or for a passkey the origin it is bound to.
func CSRF(r *http.Request, c Config, s Session) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	if r.Header.Get("X-Requested-By") != "homelab-inventory" {
		return false
	}
	if origins := r.Header.Values("Origin"); len(origins) != 0 {
		if len(origins) != 1 {
			return false
		}
		want := s.Origin
		if s.Passkey == "" {
			u, err := url.Parse(c.PublicURL)
			if err != nil || u == nil {
				return false
			}
			want = u.Scheme + "://" + u.Host
		}
		if origins[0] != want {
			return false
		}
	}
	return true
}
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	c := m.config()
	secure := c.secure()
	if ck, err := r.Cookie(SessionCookie); err == nil {
		// The signature is enough to trust which origin the cookie names.
		var s Session
		if !m.unseal(ck.Value, &s) {
			s = Session{}
		}
		if !CSRF(r, c, s) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "{\"error\":\"missing CSRF header\"}\n")
			return
		}
		// Clear the cookie with the flag it was set with.
		secure = secure || strings.HasPrefix(s.Origin, "https://")
	}
	cookie(w, secure, SessionCookie, "", "/", -1)
	w.WriteHeader(http.StatusNoContent)
}

func random() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b), err
}

func (m *Manager) Login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c := m.config()
	if !c.Ready() {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := m.Discover(ctx, c)
	if err != nil {
		m.fail(w, r, "provider")
		return
	}
	st := loginState{Next: SafeNext(r.URL.Query().Get("next")), Exp: time.Now().Add(10 * time.Minute).Unix(), Generation: c.Generation}
	for _, dst := range []*string{&st.State, &st.Nonce, &st.Verifier} {
		*dst, err = random()
		if err != nil {
			m.fail(w, r, "provider")
			return
		}
	}
	challenge := sha256.Sum256([]byte(st.Verifier))
	u, _ := url.Parse(p.Authorization)
	q := u.Query()
	q.Set("client_id", c.ClientID)
	q.Set("redirect_uri", c.RedirectURI())
	q.Set("response_type", "code")
	q.Set("scope", "openid email profile")
	q.Set("state", st.State)
	q.Set("nonce", st.Nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	cookie(w, c.secure(), stateCookie, m.seal(st), "/auth/", 600)
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (m *Manager) allowCallback(remote string, now time.Time) bool {
	peer, _, err := net.SplitHostPort(remote)
	if err != nil {
		peer = remote
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, a := range m.attempts {
		if now.Sub(a.start) >= time.Minute {
			delete(m.attempts, k)
		}
	}
	a, ok := m.attempts[peer]
	if !ok {
		if len(m.attempts) >= 4096 {
			return false
		}
		a.start = now
	}
	if a.count >= 20 {
		return false
	}
	a.count++
	m.attempts[peer] = a
	return true
}
func (m *Manager) fail(w http.ResponseWriter, r *http.Request, reason string) {
	// Reason is a fixed category, never provider body/error text or callback data.
	m.logf("browser sign-in failed: %s", reason)
	http.Redirect(w, r, "/#/login?error="+reason, http.StatusFound)
}
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c := m.config()
	cookie(w, c.secure(), stateCookie, "", "/auth/", -1)
	if !m.allowCallback(r.RemoteAddr, time.Now()) {
		m.logf("browser sign-in failed: rate_limit")
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if !c.Ready() {
		m.fail(w, r, "disabled")
		return
	}
	ck, err := r.Cookie(stateCookie)
	var st loginState
	if err != nil || !m.unseal(ck.Value, &st) || time.Now().Unix() >= st.Exp || st.Generation != c.Generation || st.State == "" || subtle.ConstantTimeCompare([]byte(st.State), []byte(r.URL.Query().Get("state"))) != 1 {
		m.fail(w, r, "state")
		return
	}
	if r.URL.Query().Get("error") != "" {
		m.fail(w, r, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		m.fail(w, r, "provider")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	p, err := m.Discover(ctx, c)
	if err != nil {
		m.fail(w, r, "provider")
		return
	}
	email, reason := m.exchange(ctx, c, p, code, st.Verifier)
	if reason != "" {
		m.fail(w, r, reason)
		return
	}
	latest := m.config()
	if !latest.Ready() || latest.Generation != c.Generation {
		m.fail(w, r, "disabled")
		return
	}
	session := Session{Email: email, Exp: time.Now().Add(time.Duration(c.SessionHours) * time.Hour).Unix(), Generation: c.Generation}
	cookie(w, c.secure(), SessionCookie, m.seal(session), "/", c.SessionHours*3600)
	http.Redirect(w, r, SafeNext(st.Next), http.StatusFound)
}
