// Package deviceauth owns household profiles and narrowly scoped device credentials.
package deviceauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

var ErrInvalid = errors.New("invalid device credential or pairing input")
var ErrRateLimited = errors.New("too many pairing attempts")

const codeLifetime = 15 * time.Minute
const maxCodes = 20
const maxDevices = 1000
const maxProfiles = 100
const maxAddresses = 1024

// Profile is a server-owned household identity.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Device contains no credential; scopes are independently authorized by callers.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	ProfileID string   `json:"profile_id"`
	Scopes    []string `json:"scopes"`
}

// Code is returned only when an administrator creates a pairing code.
type Code struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}
type credential struct {
	Device Device `json:"device"`
	Hash   string `json:"hash"`
}
type pairing struct {
	Hash      string    `json:"hash"`
	ProfileID string    `json:"profile_id"`
	Scopes    []string  `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
}
type attempts struct {
	Start time.Time `json:"start"`
	Count int       `json:"count"`
}
type state struct {
	Profiles []Profile           `json:"profiles"`
	Devices  []credential        `json:"devices"`
	Codes    []pairing           `json:"codes"`
	Attempts map[string]attempts `json:"attempts"`
}
type Store struct {
	mu   sync.Mutex
	path string
	data state
	now  func() time.Time
}

// New opens private persistent state. The directory must be dedicated to this store.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "devices.json"), now: time.Now}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err = json.Unmarshal(b, &s.data); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.data.Attempts == nil {
		s.data.Attempts = map[string]attempts{}
	}
	return s, nil
}
func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func normalize(v string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(v), "-", ""))
}
func validName(v string) bool {
	return len(v) > 0 && len(v) <= 100 && !strings.ContainsAny(v, "\r\n\x00")
}
func scopes(v []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, x := range v {
		switch x {
		case "media", "upcoming", "discover", "search", "requests", "preferences", "refresh", "taste":
		default:
			return nil, ErrInvalid
		}
		if !seen[x] {
			out = append(out, x)
			seen[x] = true
		}
	}
	sort.Strings(out)
	return out, nil
}
func copyDevice(d Device) Device { d.Scopes = append([]string{}, d.Scopes...); return d }
func (s *Store) clone() state {
	b, _ := json.Marshal(s.data)
	var d state
	_ = json.Unmarshal(b, &d)
	return d
}

// commit publishes memory only after an atomic private-file replacement succeeds.
func (s *Store) commit(d state) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err = fsx.WriteAtomic(s.path, b, 0600); err != nil {
		return err
	}
	s.data = d
	return nil
}
func (s *Store) Profiles() []Profile {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Profile{}, s.data.Profiles...)
}
func (s *Store) SaveProfile(p Profile) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.Name = strings.TrimSpace(p.Name)
	if !validName(p.Name) {
		return Profile{}, ErrInvalid
	}
	d := s.clone()
	if p.ID == "" {
		if len(d.Profiles) >= maxProfiles {
			return Profile{}, ErrInvalid
		}
		id, err := random(16)
		if err != nil {
			return Profile{}, err
		}
		p.ID = id
		d.Profiles = append(d.Profiles, p)
	} else {
		found := false
		for i := range d.Profiles {
			if d.Profiles[i].ID == p.ID {
				d.Profiles[i] = p
				found = true
				break
			}
		}
		if !found {
			return Profile{}, ErrInvalid
		}
	}
	if err := s.commit(d); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// DeleteProfile atomically removes its devices and outstanding codes too.
func (s *Store) DeleteProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.clone()
	found := false
	for i, p := range d.Profiles {
		if p.ID == id {
			d.Profiles = append(d.Profiles[:i], d.Profiles[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		return ErrInvalid
	}
	devices := d.Devices[:0]
	for _, v := range d.Devices {
		if v.Device.ProfileID != id {
			devices = append(devices, v)
		}
	}
	d.Devices = devices
	codes := d.Codes[:0]
	for _, v := range d.Codes {
		if v.ProfileID != id {
			codes = append(codes, v)
		}
	}
	d.Codes = codes
	return s.commit(d)
}
func (s *Store) CreateCode(profileID string, permissions []string) (Code, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, err := scopes(permissions)
	if err != nil {
		return Code{}, err
	}
	found := false
	for _, p := range s.data.Profiles {
		if p.ID == profileID {
			found = true
		}
	}
	if !found {
		return Code{}, ErrInvalid
	}
	d := s.clone()
	now := s.now()
	active := d.Codes[:0]
	for _, c := range d.Codes {
		if now.Before(c.ExpiresAt) {
			active = append(active, c)
		}
	}
	d.Codes = active
	if len(d.Codes) >= maxCodes {
		return Code{}, ErrRateLimited
	}
	raw, err := random(5)
	if err != nil {
		return Code{}, err
	}
	expires := now.Add(codeLifetime)
	d.Codes = append(d.Codes, pairing{digest(raw), profileID, allowed, expires})
	if err = s.commit(d); err != nil {
		return Code{}, err
	}
	return Code{raw[:4] + "-" + raw[4:], expires}, nil
}
func (s *Store) Redeem(code, name, ip string) (Device, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.clone()
	now := s.now()
	for k, v := range d.Attempts {
		if !now.Before(v.Start.Add(time.Minute)) {
			delete(d.Attempts, k)
		}
	}
	key := digest(ip)
	a, exists := d.Attempts[key]
	if !exists {
		if len(d.Attempts) >= maxAddresses {
			return Device{}, "", ErrRateLimited
		}
		a.Start = now
	}
	if a.Count >= 10 {
		return Device{}, "", ErrRateLimited
	}
	a.Count++
	d.Attempts[key] = a
	invalid := func() (Device, string, error) {
		if err := s.commit(d); err != nil {
			return Device{}, "", err
		}
		return Device{}, "", ErrInvalid
	}
	name = strings.TrimSpace(name)
	raw := normalize(code)
	if len(raw) != 8 || len(ip) > 256 || !validName(name) {
		return invalid()
	}
	hash := digest(raw)
	idx := -1
	for i, c := range d.Codes {
		if equal(c.Hash, hash) && now.Before(c.ExpiresAt) {
			idx = i
		}
	}
	if idx < 0 || len(d.Devices) >= maxDevices {
		return invalid()
	}
	c := d.Codes[idx]
	id, err := random(16)
	if err != nil {
		return Device{}, "", err
	}
	token, err := random(32)
	if err != nil {
		return Device{}, "", err
	}
	device := Device{id, name, c.ProfileID, append([]string{}, c.Scopes...)}
	d.Devices = append(d.Devices, credential{device, digest(token)})
	d.Codes = append(d.Codes[:idx], d.Codes[idx+1:]...)
	if err = s.commit(d); err != nil {
		return Device{}, "", err
	}
	return copyDevice(device), token, nil
}
func (s *Store) Authenticate(token string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(token) != 52 {
		return Device{}, ErrInvalid
	}
	hash := digest(token)
	for _, d := range s.data.Devices {
		if equal(hash, d.Hash) {
			return copyDevice(d.Device), nil
		}
	}
	return Device{}, ErrInvalid
}
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Device, 0, len(s.data.Devices))
	for _, v := range s.data.Devices {
		out = append(out, copyDevice(v.Device))
	}
	return out
}
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.clone()
	for i, v := range d.Devices {
		if v.Device.ID == id {
			d.Devices = append(d.Devices[:i], d.Devices[i+1:]...)
			return s.commit(d)
		}
	}
	return ErrInvalid
}
