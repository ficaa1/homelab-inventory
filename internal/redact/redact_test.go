package redact

import (
	"strings"
	"testing"
)

func TestIsSensitiveKey(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"MYSQL_ROOT_PASSWORD":   true,
		"password":              true,
		"api_key":               true,
		"API-KEY":               true,
		"AWS_SECRET_ACCESS_KEY": true,
		"jwt_token":             true,
		"private_key":           true,
		"BASIC_AUTH":            true,

		"POSTGRES_USER": false,
		"PUID":          false,
		"keyboard":      false,
		"author":        false,
		"TZ":            false,
		"image":         false,
		// *_file variants name a path, not a secret; keeping them is the
		// whole point of the allowlist.
		"POSTGRES_PASSWORD_FILE": false,

		// Settings that describe an auth scheme rather than authenticate with
		// one. These fields describe configuration rather than credentials.
		"PHOTOPRISM_AUTH_MODE":       false,
		"OAUTHLIB_RELAX_TOKEN_SCOPE": false,
		"AUTH_PROVIDER":              false,
		"API_KEY_NAME":               false,
		// ...but the credentials themselves stay redacted.
		"CLOUDFLARE_DNS_API_TOKEN":  true,
		"PHOTOPRISM_ADMIN_PASSWORD": true,
		"SPOTIFY_CLIENT_SECRET":     true,
		"DISCORD_TOKEN":             true,
	}
	for key, want := range cases {
		if got := IsSensitiveKey(key); got != want {
			t.Errorf("IsSensitiveKey(%q) = %v, want %v", key, got, want)
		}
	}
}

func TestValueRedactsSensitiveKeys(t *testing.T) {
	t.Parallel()
	got := Value("MYSQL_ROOT_PASSWORD", "fake-password")
	if !strings.HasPrefix(got, "[REDACTED:sha256:") {
		t.Fatalf("secret not redacted: %q", got)
	}
	if strings.Contains(got, "fake-password") {
		t.Fatalf("plaintext leaked: %q", got)
	}
	if keep := Value("POSTGRES_USER", "admin"); keep != "admin" {
		t.Errorf("non-secret altered: %q", keep)
	}
}

func TestFingerprintIsStableAndDistinct(t *testing.T) {
	t.Parallel()
	if Fingerprint("a") != Fingerprint("a") {
		t.Error("fingerprint not stable across calls")
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Error("distinct secrets share a fingerprint")
	}
}

func TestStringScrubsURLCredentials(t *testing.T) {
	t.Parallel()
	got := String("postgres://appuser:fake-password@db.example.org:5432/app")
	if strings.Contains(got, "fake-password") {
		t.Fatalf("password leaked: %q", got)
	}
	// Host and username must survive: they are the useful part.
	for _, want := range []string{"postgres://appuser:", "@db.example.org:5432/app"} {
		if !strings.Contains(got, want) {
			t.Errorf("context lost, %q missing from %q", want, got)
		}
	}
}

func TestStringScrubsPrefixedTokens(t *testing.T) {
	t.Parallel()
	in := "token is ghp_abcdefghijklmnopqrstuvwxyz0123456789 ok"
	got := String(in)
	if strings.Contains(got, "ghp_abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("token leaked: %q", got)
	}
}

// TestRegressionPlexClaimToken checks that claim tokens are redacted.
func TestRegressionPlexClaimToken(t *testing.T) {
	t.Parallel()
	in := `    environment:
      - TZ=UTC
      # Token from https://www.plex.tv/claim needed to automatically login
      - PLEX_CLAIM=claim-FAKE_TEST_VALUE_0000
      - ALLOWED_NETWORKS=192.0.2.24/24,192.0.2.29/24
`
	got := Text(in)
	if strings.Contains(got, "claim-FAKE_TEST_VALUE_0000") {
		t.Fatalf("claim token leaked:\n%s", got)
	}
	// The surrounding non-secrets must survive, including the comment that
	// merely mentions the word "claim".
	for _, want := range []string{"TZ=UTC", "www.plex.tv/claim", "ALLOWED_NETWORKS=192.0.2.24/24"} {
		if !strings.Contains(got, want) {
			t.Errorf("over-redacted, %q missing from:\n%s", want, got)
		}
	}
}

// TestImageDigestsSurvive guards the other direction. A generic
// high-entropy heuristic would shred these, and an inventory that cannot tell
// you which image digest is running is not worth much.
func TestImageDigestsSurvive(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"docker.io/redis:6.2-alpine@sha256:eaba718fc9c5b1c1e1b1b1a1d1e1f1a1b1c1d1e1f1a1b1c1d1e1f1a1b1c1d1e1",
		"git_commit=c14f7dd",
		"org.opencontainers.image.revision: 4f3d2a1b9c8e7d6f5a4b3c2d1e0f9a8b7c6d5e4f",
	} {
		if got := String(in); got != in {
			t.Errorf("non-secret altered:\n in: %s\nout: %s", in, got)
		}
	}
}

func TestVendorTokenShapes(t *testing.T) {
	t.Parallel()
	secrets := []string{
		"claim-abcdefghijklmnop",
		"000000000:AA_FAKE_TEST_abcdefghijklmnopqrstuvwxyz",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789",
	}
	for _, s := range secrets {
		if got := String("value " + s + " end"); strings.Contains(got, s) {
			t.Errorf("token not redacted: %q -> %q", s, got)
		}
	}
	// Discord webhooks keep the identifying prefix and lose the credential.
	hook := "https://discord.com/api/webhooks/123456789012345678/FAKE_WEBHOOK_abcdefghijklmnopqrstuvwxyz"
	got := String(hook)
	if strings.Contains(got, "FAKE_WEBHOOK_abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("webhook credential leaked: %q", got)
	}
	if !strings.Contains(got, "discord.com/api/webhooks/123456789012345678/") {
		t.Errorf("webhook context lost: %q", got)
	}
}

func TestTextRedactsComposeFile(t *testing.T) {
	t.Parallel()
	in := `version: "3.8"
services:
  db:
    image: postgres:15
    environment:
      - POSTGRES_USER=appuser
      - POSTGRES_PASSWORD=fake-password
      - TZ=Europe/Amsterdam
    # note: rotate POSTGRES_PASSWORD quarterly
`
	got := Text(in)

	if strings.Contains(got, "fake-password") {
		t.Fatalf("password leaked:\n%s", got)
	}
	for _, want := range []string{
		`version: "3.8"`,
		"image: postgres:15",
		"POSTGRES_USER=appuser",
		"TZ=Europe/Amsterdam",
		"  db:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("structure lost, %q missing from:\n%s", want, got)
		}
	}
}

func TestTextRedactsPEMBlock(t *testing.T) {
	t.Parallel()
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\nFAKE_BASE64...\nAAAA\n-----END RSA PRIVATE KEY-----\nafter"
	got := Text(in)
	if strings.Contains(got, "FAKE_BASE64") {
		t.Fatalf("key material leaked: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding content lost: %q", got)
	}
}

func TestTextPreservesQuotesAroundRedactedValues(t *testing.T) {
	t.Parallel()
	got := Text(`  password: "fake-password"`)
	if strings.Contains(got, "fake-password") {
		t.Fatalf("password leaked: %q", got)
	}
	if !strings.HasSuffix(got, `"`) {
		t.Errorf("quotes not preserved, YAML would no longer parse: %q", got)
	}
}

func TestMapDoesNotMutateInput(t *testing.T) {
	t.Parallel()
	in := map[string]string{"api_key": "abc123", "name": "web"}
	out := Map(in)
	if in["api_key"] != "abc123" {
		t.Error("input map was mutated")
	}
	if out["name"] != "web" {
		t.Errorf("non-secret altered: %q", out["name"])
	}
	if strings.Contains(out["api_key"], "abc123") {
		t.Errorf("secret leaked: %q", out["api_key"])
	}
}
