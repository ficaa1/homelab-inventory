package settings

import (
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

func defaults() Defaults {
	return Defaults{
		NotifyLevel:      alert.LevelCritical,
		NotifyInterval:   5 * time.Minute,
		DigestInterval:   168 * time.Hour,
		AppriseURL:       "https://example.org/notification-1",
		DigestAppriseURL: "",
	}
}

// The reason every field is a pointer: an empty stored blob must be
// indistinguishable from no stored blob at all, or a fresh deployment's
// environment gets silently overridden by a file nobody wrote.
func TestEmptyStoredFallsThroughToTheEnvironment(t *testing.T) {
	t.Parallel()
	e := Resolve(Stored{}, defaults())

	if e.NotifyLevel != alert.LevelCritical {
		t.Errorf("NotifyLevel = %q, want the env default", e.NotifyLevel)
	}
	if e.NotifyInterval != 5*time.Minute {
		t.Errorf("NotifyInterval = %s, want the env default", e.NotifyInterval)
	}
	if e.AppriseURL != defaults().AppriseURL {
		t.Errorf("AppriseURL = %q, want the env default", e.AppriseURL)
	}
	if !e.NotifyEnabled {
		t.Error("NotifyEnabled defaulted to off; an unset field must not mute the notifier")
	}
}

// Setting one field must not disturb the others.
func TestSettingOneFieldLeavesTheRestOnTheEnvironment(t *testing.T) {
	t.Parallel()
	warning := "warning"
	e := Resolve(Stored{NotifyLevel: &warning}, defaults())

	if e.NotifyLevel != alert.LevelWarning {
		t.Errorf("NotifyLevel = %q, want warning", e.NotifyLevel)
	}
	if e.NotifyInterval != 5*time.Minute {
		t.Errorf("NotifyInterval = %s; setting the level disturbed it", e.NotifyInterval)
	}
	if e.AppriseURL != defaults().AppriseURL {
		t.Error("setting the level disturbed the apprise URL")
	}
	if e.DigestAppriseURL != "" {
		t.Errorf("DigestAppriseURL = %q, want unset", e.DigestAppriseURL)
	}
}

// The digest endpoint layers over the environment like every other field, and
// an unset one falls back to the main channel at the caller's discretion.
func TestDigestEndpointLayersAndValidates(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	now := time.Now()

	digest := "https://example.org/notification-2"
	e := Resolve(Stored{DigestAppriseURL: &digest}, defaults())
	if e.DigestAppriseURL != digest {
		t.Errorf("DigestAppriseURL = %q, want the stored value", e.DigestAppriseURL)
	}

	got, err := Apply(Stored{}, Update{DigestAppriseURL: str(digest)}, now)
	if err != nil {
		t.Fatalf("valid digest url rejected: %v", err)
	}
	if got.DigestAppriseURL == nil || *got.DigestAppriseURL != digest {
		t.Errorf("DigestAppriseURL = %v", got.DigestAppriseURL)
	}

	// Empty clears it back to "no dedicated digest channel".
	cleared, err := Apply(got, Update{DigestAppriseURL: str("")}, now)
	if err != nil {
		t.Fatal(err)
	}
	if cleared.DigestAppriseURL == nil || *cleared.DigestAppriseURL != "" {
		t.Errorf("clearing DigestAppriseURL = %v, want empty", cleared.DigestAppriseURL)
	}

	// Same scheme/host validation as the apprise URL: a file:// digest
	// endpoint would turn the notifier into a local-file reader.
	if _, err := Apply(Stored{}, Update{DigestAppriseURL: str("file:///etc/passwd")}, now); err == nil {
		t.Error("a file:// digest endpoint was accepted")
	}
}

func TestStoredOverridesTheEnvironment(t *testing.T) {
	t.Parallel()
	lvl, every, digest := "info", "30m", "0s"
	url := "https://example.org/notification-3"
	off := false
	e := Resolve(Stored{
		NotifyLevel: &lvl, NotifyInterval: &every, DigestInterval: &digest,
		AppriseURL: &url, NotifyEnabled: &off,
	}, defaults())

	if e.NotifyLevel != alert.LevelInfo {
		t.Errorf("NotifyLevel = %q", e.NotifyLevel)
	}
	if e.NotifyInterval != 30*time.Minute {
		t.Errorf("NotifyInterval = %s", e.NotifyInterval)
	}
	if e.DigestInterval != 0 {
		t.Errorf("DigestInterval = %s, want 0 (disabled)", e.DigestInterval)
	}
	if e.AppriseURL != url {
		t.Errorf("AppriseURL = %q", e.AppriseURL)
	}
	if e.Sending() {
		t.Error("Sending() is true with notifications explicitly disabled")
	}
}

// A digest interval of zero disables the digest, so it has to survive Resolve
// rather than being treated as "unset" the way a zero notify interval is.
func TestZeroDigestIntervalDisablesRatherThanFallsThrough(t *testing.T) {
	t.Parallel()
	zero := "0s"
	e := Resolve(Stored{DigestInterval: &zero}, defaults())
	if e.DigestInterval != 0 {
		t.Errorf("DigestInterval = %s, want 0 — zero must disable, not fall through", e.DigestInterval)
	}
}

func TestApplyValidates(t *testing.T) {
	t.Parallel()
	now := time.Now()
	str := func(s string) *string { return &s }

	bad := []struct {
		name string
		u    Update
	}{
		{"unknown level", Update{NotifyLevel: str("catastrophic")}},
		{"unparseable interval", Update{NotifyInterval: str("soon")}},
		{"zero notify interval", Update{NotifyInterval: str("0s")}},
		{"interval far too short", Update{NotifyInterval: str("1s")}},
		{"interval far too long", Update{NotifyInterval: str("48h")}},
		{"digest under an hour", Update{DigestInterval: str("5m")}},
		{"negative digest", Update{DigestInterval: str("-1h")}},
		{"apprise not a url", Update{AppriseURL: str("://nope")}},
		{"apprise file scheme", Update{AppriseURL: str("file:///etc/passwd")}},
		{"apprise no host", Update{AppriseURL: str("http://")}},
	}
	for _, tc := range bad {
		if _, err := Apply(Stored{}, tc.u, now); err == nil {
			t.Errorf("%s: accepted, want an error", tc.name)
		}
	}

	good := Update{
		NotifyLevel: str("warning"), NotifyInterval: str("10m"),
		DigestInterval: str("0"), AppriseURL: str("https://example.org/notification-4"),
	}
	got, err := Apply(Stored{}, good, now)
	if err != nil {
		t.Fatalf("valid update rejected: %v", err)
	}
	if got.NotifyLevel == nil || *got.NotifyLevel != "warning" {
		t.Errorf("NotifyLevel = %v", got.NotifyLevel)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not stamped")
	}
}

// All-or-nothing: a bad interval must not leave the level changed.
func TestApplyIsAtomic(t *testing.T) {
	t.Parallel()
	str := func(s string) *string { return &s }
	before := Stored{NotifyLevel: str("critical")}

	after, err := Apply(before, Update{
		NotifyLevel: str("info"), NotifyInterval: str("nonsense"),
	}, time.Now())
	if err == nil {
		t.Fatal("expected an error")
	}
	if after.NotifyLevel == nil || *after.NotifyLevel != "critical" {
		t.Errorf("NotifyLevel = %v; a rejected update changed it anyway", after.NotifyLevel)
	}
}

// The path segment after /notify/ is the key that decides where a message
// goes, so it must never come back out.
func TestRedactedNeverRevealsTheKey(t *testing.T) {
	t.Parallel()
	raw := "https://example.org/notification-5"
	got := Redacted(raw)

	if got == raw {
		t.Fatal("Redacted returned the URL verbatim")
	}
	for _, leak := range []string{"super-secret-key-abc123", "/notify/"} {
		if contains(got, leak) {
			t.Errorf("Redacted() = %q, which leaks %q", got, leak)
		}
	}
	if !contains(got, "example.org") {
		t.Errorf("Redacted() = %q; the host is what tells one endpoint from another", got)
	}
	if Redacted("") != "" {
		t.Error("an unset URL should redact to empty, not to a placeholder")
	}
}

func contains(s, sub string) bool {
	return len(sub) <= len(s) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
