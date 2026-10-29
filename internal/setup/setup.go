// Package setup owns durable first-run credentials and single-use enrollment.
package setup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

var (
	ErrClaimed = errors.New("already claimed")
	ErrExpired = errors.New("setup window expired")
	ErrCode    = errors.New("invalid enrollment code")
	ErrLimit   = errors.New("too many outstanding enrollment codes")
)

const Window = 10 * time.Minute
const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

type Tokens struct {
	API      string `json:"api_token"`
	Agent    string `json:"agent_token"`
	Operator string `json:"-"`
}
type Enrollment struct {
	ID      string    `json:"id"`
	Hash    string    `json:"hash"`
	Hint    string    `json:"code_hint"`
	Expires time.Time `json:"expires_at"`
	UsedBy  *string   `json:"used_by"`
}
type diskState struct {
	Tokens
	Codes []Enrollment `json:"codes,omitempty"`
}
type attempt struct {
	Start time.Time
	Count int
}
type Manager struct {
	mu       sync.Mutex
	path     string
	state    diskState
	env      Tokens
	deadline time.Time
	window   time.Duration
	now      func() time.Time
	attempts map[string]attempt
}

// Open starts the claim window at process start. now and window are injectable
// for tests; production always uses Window. Environment values override disk.
func Open(dir string, env Tokens, started time.Time, window time.Duration, now func() time.Time) (*Manager, error) {
	m := &Manager{path: filepath.Join(dir, "state-auth.json"), env: env, deadline: started.Add(window), window: window, now: now, attempts: make(map[string]attempt)}
	info, err := os.Lstat(m.path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("state-auth.json must be a private regular file (0600)")
		}
		b, err := os.ReadFile(m.path)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &m.state); err != nil {
			return nil, errors.New("invalid state-auth.json")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}
func (m *Manager) tokens() Tokens {
	t := m.state.Tokens
	if m.env.API != "" {
		t.API = m.env.API
	}
	if m.env.Agent != "" {
		t.Agent = m.env.Agent
	}
	if m.env.Operator != "" {
		t.Operator = m.env.Operator
	}
	return t
}

// Window is the full claim window, for the wizard's progress bar.
func (m *Manager) Window() time.Duration { return m.window }
func (m *Manager) Tokens() Tokens        { m.mu.Lock(); defer m.mu.Unlock(); return m.tokens() }
func (m *Manager) Status() (string, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens().API != "" {
		return "claimed", 0
	}
	remaining := m.deadline.Sub(m.now())
	if remaining <= 0 {
		return "expired", 0
	}
	return "unclaimed", int((remaining + time.Second - 1) / time.Second)
}
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
func (m *Manager) save(next diskState) error {
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	// Install in memory even when directory fsync fails after the rename. The
	// credentials must never be claimed twice in this process after publication.
	if err = fsx.WriteAtomic(m.path, append(b, '\n'), 0600); err != nil {
		if got, e := os.ReadFile(m.path); e == nil && string(got) == string(append(b, '\n')) {
			m.state = next
		}
		return err
	}
	m.state = next
	return nil
}
func (m *Manager) Claim() (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens().API != "" {
		return Tokens{}, ErrClaimed
	}
	if !m.now().Before(m.deadline) {
		return Tokens{}, ErrExpired
	}
	t := m.tokens()
	var err error
	t.API, err = randomToken()
	if err != nil {
		return Tokens{}, err
	}
	if t.Agent == "" {
		t.Agent, err = randomToken()
		if err != nil {
			return Tokens{}, err
		}
	}
	next := m.state
	next.Tokens = t
	if err = m.save(next); err != nil {
		return Tokens{}, err
	}
	return t, nil
}
func hash(code string) string { h := sha256.Sum256([]byte(code)); return hex.EncodeToString(h[:]) }
func (m *Manager) live() []Enrollment {
	out := make([]Enrollment, 0)
	for _, c := range m.state.Codes {
		if m.now().Before(c.Expires) {
			out = append(out, c)
		}
	}
	// Bound redeemed history as well as outstanding credentials.
	used := 0
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].UsedBy != nil {
			used++
			if used > 100 {
				out = append(out[:i], out[i+1:]...)
			}
		}
	}
	return out
}

// Enroll returns a new code and its opaque id. The id is random, not derived
// from the code, so listing it never narrows a brute-force search.
func (m *Manager) Enroll() (code, id string, expires time.Time, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tokens().API == "" {
		return "", "", time.Time{}, errors.New("enrollment requires a claimed instance")
	}
	codes := m.live()
	n := 0
	for _, c := range codes {
		if c.UsedBy == nil {
			n++
		}
	}
	if n >= 20 {
		return "", "", time.Time{}, ErrLimit
	}
	// Rejection sampling avoids modulo bias.
	raw := make([]byte, 0, 9)
	for len(raw) < 9 {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return "", "", time.Time{}, err
		}
		for _, v := range b {
			if int(v) < 256-256%len(alphabet) {
				raw = append(raw, alphabet[int(v)%len(alphabet)])
				if len(raw) == 9 {
					break
				}
			}
		}
	}
	code = string(raw[:3]) + "-" + string(raw[3:6]) + "-" + string(raw[6:])
	expires = m.now().UTC().Add(15 * time.Minute)
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", time.Time{}, err
	}
	id = hex.EncodeToString(idBytes)
	codes = append(codes, Enrollment{ID: id, Hash: hash(code), Hint: code[:3] + "-…", Expires: expires})
	next := m.state
	next.Codes = codes
	// A manual API-only configuration can still add hosts without distributing
	// admin authority. Persist a separate agent credential on first enrollment.
	if m.tokens().Agent == "" {
		token, err := randomToken()
		if err != nil {
			return "", "", time.Time{}, err
		}
		next.Agent = token
	}
	if err := m.save(next); err != nil {
		return "", "", time.Time{}, err
	}
	return code, id, expires, nil
}
func (m *Manager) List() []Enrollment { m.mu.Lock(); defer m.mu.Unlock(); return m.live() }
func (m *Manager) Redeem(code, host string) (Tokens, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	codes := m.live()
	for i, c := range codes {
		if c.Hash == hash(code) && c.UsedBy == nil {
			codes[i].UsedBy = &host
			next := m.state
			next.Codes = codes
			if err := m.save(next); err != nil {
				return Tokens{}, err
			}
			return m.tokens(), nil
		}
	}
	return Tokens{}, ErrCode
}

// AllowAttempt counts all redemptions, using only the socket peer, never XFF.
func (m *Manager) AllowAttempt(remote string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	for k, v := range m.attempts {
		if now.Sub(v.Start) >= time.Minute {
			delete(m.attempts, k)
		}
	}
	a, ok := m.attempts[remote]
	if !ok {
		if len(m.attempts) >= 4096 {
			return false
		}
		a.Start = now
	}
	if a.Count >= 10 {
		return false
	}
	a.Count++
	m.attempts[remote] = a
	return true
}
