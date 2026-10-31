package deviceauth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func setup(t *testing.T) (*Store, Profile) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SaveProfile(Profile{Name: "Household"})
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}
func TestPairingPersistenceAndRevocation(t *testing.T) {
	s, p := setup(t)
	c, err := s.CreateCode(p.ID, []string{"media", "search", "media"})
	if err != nil {
		t.Fatal(err)
	}
	d, token, err := s.Redeem(strings.ToLower(c.Code), "TV", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	if d.ProfileID != p.ID || len(d.Scopes) != 2 {
		t.Fatalf("wrong binding: %+v", d)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), token) || strings.Contains(string(b), normalize(c.Code)) {
		t.Fatal("plaintext credential on disk")
	}
	info, _ := os.Stat(s.path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	s, err = New(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(token); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(strings.Repeat("A", 52)); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err = s.Redeem(c.Code, "Other", "192.0.2.2"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	d.Scopes[0] = "requests"
	got, _ := s.Authenticate(token)
	if got.Scopes[0] == "requests" {
		t.Fatal("mutable scope alias")
	}
	if err = s.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	s, err = New(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(token); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestExpiryAndRateLimit(t *testing.T) {
	s, p := setup(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	c, err := s.CreateCode(p.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(codeLifetime)
	if _, _, err = s.Redeem(c.Code, "TV", "ip"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for i := 1; i < 10; i++ {
		if _, _, err = s.Redeem("bad", "TV", "ip"); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, _, err = s.Redeem("bad", "TV", "ip"); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	reopened, err := New(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	reopened.now = s.now
	if _, _, err = reopened.Redeem("bad", "TV", "ip"); !errors.Is(err, ErrRateLimited) {
		t.Fatal("rate limit lost on restart", err)
	}
	now = now.Add(time.Minute)
	if _, _, err = reopened.Redeem("bad", "TV", "ip"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestConcurrentRedemption(t *testing.T) {
	s, p := setup(t)
	c, _ := s.CreateCode(p.ID, []string{"media"})
	var wg sync.WaitGroup
	success := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, token, err := s.Redeem(c.Code, "TV", "ip")
			if err == nil {
				success <- token
			}
		}()
	}
	wg.Wait()
	close(success)
	if len(success) != 1 || len(s.Devices()) != 1 {
		t.Fatalf("redemptions %d devices %d", len(success), len(s.Devices()))
	}
}
func TestProfileLifecycle(t *testing.T) {
	s, p := setup(t)
	p.Name = "Renamed"
	if _, err := s.SaveProfile(p); err != nil {
		t.Fatal(err)
	}
	if s.Profiles()[0].Name != "Renamed" {
		t.Fatal("rename failed")
	}
	if _, err := s.SaveProfile(Profile{ID: "unknown", Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.SaveProfile(Profile{Name: " "}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.CreateCode("unknown", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.CreateCode(p.ID, []string{"admin"}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	c, _ := s.CreateCode(p.ID, []string{"media"})
	_, token, _ := s.Redeem(c.Code, "TV", "ip")
	unused, _ := s.CreateCode(p.ID, nil)
	if err := s.DeleteProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	if len(s.Profiles()) != 0 || len(s.Devices()) != 0 {
		t.Fatal("orphaned data")
	}
	if _, err := s.Authenticate(token); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err := s.Redeem(unused.Code, "TV", "ip"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestBoundedCodesAndAddressAttempts(t *testing.T) {
	s, p := setup(t)
	now := time.Now()
	s.now = func() time.Time { return now }
	for i := 0; i < maxCodes; i++ {
		if _, err := s.CreateCode(p.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateCode(p.ID, nil); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	now = now.Add(codeLifetime)
	if _, err := s.CreateCode(p.ID, nil); err != nil {
		t.Fatal(err)
	}
	s.data.Attempts = map[string]attempts{}
	for i := 0; i < maxAddresses; i++ {
		s.data.Attempts[string(rune(i))] = attempts{now, 1}
	}
	if _, _, err := s.Redeem("bad", "TV", "new"); !errors.Is(err, ErrRateLimited) {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, _, err := s.Redeem("bad", "TV", "new"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}
func TestFailedWriteDoesNotConsumeCode(t *testing.T) {
	s, p := setup(t)
	c, _ := s.CreateCode(p.ID, nil)
	original := s.path
	s.path = filepath.Join(t.TempDir(), "missing", "devices.json")
	if _, _, err := s.Redeem(c.Code, "TV", "ip"); err == nil {
		t.Fatal("expected failure")
	}
	s.path = original
	if _, _, err := s.Redeem(c.Code, "TV", "ip"); err != nil {
		t.Fatal(err)
	}
}
