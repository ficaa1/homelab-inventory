package setup

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClaimPersistenceAndEnrollment(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	open := func(env Tokens) *Manager {
		t.Helper()
		m, err := Open(dir, env, now, Window, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := open(Tokens{})
	if state, left := m.Status(); state != "unclaimed" || left != 600 {
		t.Fatal("initial setup state")
	}
	var winners atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := m.Claim()
			if err == nil {
				winners.Add(1)
			} else if !errors.Is(err, ErrClaimed) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("claim must have exactly one winner")
	}
	tokens := m.Tokens()
	if len(tokens.API) != 64 || len(tokens.Agent) != 64 || tokens.API == tokens.Agent {
		t.Fatal("invalid token shape/separation")
	}
	if info, err := os.Stat(filepath.Join(dir, "state-auth.json")); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credentials must be private")
	}
	code, _, expiry, err := m.Enroll()
	if err != nil || expiry.Sub(now) != 15*time.Minute || !regexp.MustCompile(`^[2-9A-HJKMNP-Z]{3}(-[2-9A-HJKMNP-Z]{3}){2}$`).MatchString(code) {
		t.Fatal("invalid enrollment")
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "state-auth.json"))
	if strings.Contains(string(raw), code) {
		t.Fatal("plaintext code persisted")
	}
	m = open(Tokens{})
	if m.Tokens() != tokens {
		t.Fatal("credentials lost on restart")
	}
	if got, err := m.Redeem(code, "host-a"); err != nil || got.Agent != tokens.Agent {
		t.Fatal("code lost on restart")
	}
	if _, err := open(Tokens{}).Redeem(code, "host-b"); !errors.Is(err, ErrCode) {
		t.Fatal("code replay accepted")
	}
	if got := *m.List()[0].UsedBy; got != "host-a" {
		t.Fatal("missing redemption host")
	}
	override := Tokens{API: strings.Repeat("a", 32), Agent: strings.Repeat("g", 32), Operator: strings.Repeat("o", 32)}
	if open(override).Tokens() != override {
		t.Fatal("environment must override saved credentials")
	}
	// At most twenty unused codes, independently of recently redeemed entries.
	unused := ""
	for range 20 {
		var err error
		unused, _, _, err = m.Enroll()
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, _, _, err := m.Enroll(); !errors.Is(err, ErrLimit) {
		t.Fatal("outstanding limit not enforced")
	}
	now = now.Add(15 * time.Minute)
	if _, err := m.Redeem(unused, "host-a"); !errors.Is(err, ErrCode) {
		t.Fatal("expired redemption accepted")
	}
	if len(m.List()) != 0 {
		t.Fatal("expired entries retained in listing")
	}
	if _, _, _, err := m.Enroll(); err != nil {
		t.Fatal("expired codes still count against limit")
	}
}
func TestWindowAndRateLimit(t *testing.T) {
	now := time.Now()
	m, err := Open(t.TempDir(), Tokens{}, now, Window, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(Window)
	if state, _ := m.Status(); state != "expired" {
		t.Fatal("window not expired")
	}
	if _, err := m.Claim(); !errors.Is(err, ErrExpired) {
		t.Fatal("expired claim accepted")
	}
	for range 10 {
		if !m.AllowAttempt("192.0.2.10") {
			t.Fatal("early rate limit")
		}
	}
	if m.AllowAttempt("192.0.2.10") || !m.AllowAttempt("192.0.2.11") {
		t.Fatal("per-peer rate limit failed")
	}
	now = now.Add(time.Minute)
	if !m.AllowAttempt("192.0.2.10") {
		t.Fatal("limit did not reset")
	}
}

func TestEnvironmentClaimAndAgentProvisioning(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	env := Tokens{API: strings.Repeat("a", 32)}
	m, err := Open(dir, env, now.Add(-time.Hour), Window, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := m.Status(); state != "claimed" {
		t.Fatal("environment token must bypass window")
	}
	if _, err := m.Claim(); !errors.Is(err, ErrClaimed) {
		t.Fatal("environment token allowed claim")
	}
	code, _, _, err := m.Enroll()
	if err != nil {
		t.Fatal(err)
	}
	token := m.Tokens().Agent
	if len(token) != 64 || token == env.API {
		t.Fatal("API-only enrollment must provision separate agent authority")
	}
	m, err = Open(dir, env, now, Window, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := m.Redeem(code, "host-a"); err != nil || got.Agent != token {
		t.Fatal("provisioned agent token did not persist")
	}
}
