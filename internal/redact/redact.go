// Package redact removes secrets from collected data before it is stored.
//
// The rule this package exists to enforce: redaction happens at the point of
// collection, not at the point of output. A secret that reaches the Inventory
// struct in plaintext has already leaked, because the struct gets marshalled,
// written to disk, and later shipped to the API. Collectors therefore
// call into this package on every value they are not certain about.
//
// Redaction is deliberately over-eager. Losing a non-secret value to a false
// positive costs you one field in an inventory; missing a real secret puts a
// password in a JSON file you will scp around. The trade is not close.
//
// Redacted values keep a short fingerprint: [REDACTED:sha256:1a2b3c4d]. The
// fingerprint is stable, so diffing two runs still tells you "this credential
// changed" without ever storing the credential. Eight hex characters is far
// too little to brute-force a real secret from, but plenty to compare.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// See TestIsSensitiveKey for representative sensitive keys.

// Placeholder is used when a value is redacted without a fingerprint.
const Placeholder = "[REDACTED]"

// sensitiveKey matches assignment keys whose values should never be stored.
// Anchored on word-ish boundaries so that "keyboard_layout" does not match
// "key" but "SSH_KEY" and "api-key" do.
//
// The trailing alternation handles keys that run words together and so defeat
// the boundary: Proxmox cloud-init writes "cipassword" and "sshkeys" into
// guest configs, and neither has a separator before the sensitive word.
// Relaxing the boundary globally would have caught them, but it would also
// catch "bypass" and similar, so they are named explicitly instead.
var sensitiveKey = regexp.MustCompile(`(?i)(^|[^a-z])(pass(word|wd|phrase)?|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credentials?|auth|bearer|salt|signature|client[_-]?secret|encryption[_-]?key|claim|webhook|.*_key)([^a-z]|$)|^(cipassword|sshkeys|ciuser_password)$`)

// allowKey lists keys that look sensitive but are safe and useful to keep.
// Without this, compose files lose most of their meaning.
//
// Two families live here. First, *_FILE keys, which name a path rather than
// holding a secret. Second, configuration *about* authentication —
// PHOTOPRISM_AUTH_MODE ("password"), OAUTHLIB_RELAX_TOKEN_SCOPE ("1") — which
// the first pass fingerprinted purely because "auth" and "token" appear in the
// key. Losing those to a false positive costs real configuration detail, and
// the value space (mode names, booleans, URLs) contains no credentials.
var allowKey = regexp.MustCompile(`(?i)^(` + strings.Join([]string{
	`pass(word)?_?file`,
	`.*_(key|secret|password|token|credentials?)_file`,
	`token_file`,
	`authorized_keys_file`,
	`secret_name`,
	// Settings that describe an auth scheme rather than authenticate with one.
	`.*auth_?(mode|method|type|url|scheme|provider|enabled|required)`,
	`authentication`,
	`.*_token_scope`,
	`.*_token_expiry`,
	`.*_(key|token|secret)_(id|name|path|dir|alias)`,
}, "|") + `)$`)

// assignment matches KEY=value and "KEY: value" style lines, which covers
// env files, compose YAML, systemd unit Environment= lines, and cron.
var assignment = regexp.MustCompile(`^(\s*-?\s*)([A-Za-z_][A-Za-z0-9_.\-]*)(\s*[:=]\s*)(.*)$`)

// valuePatterns catch secrets that carry no key at all: URLs with inline
// credentials, PEM blocks, JWTs, and common vendor token prefixes.
var valuePatterns = []*regexp.Regexp{
	// scheme://user:password@host  -> only the password is replaced
	regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/@]+:)([^\s@/]+)(@)`),
	// JWTs
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\.[A-Za-z0-9_\-]{5,}\b`),
	// GitHub / Slack / OpenAI / AWS style prefixed tokens
	regexp.MustCompile(`\b(gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9\-]{10,}|sk-[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16})\b`),
	// Plex claim tokens may appear in compose environment entries.
	regexp.MustCompile(`\bclaim-[A-Za-z0-9_\-]{12,}\b`),
	// Telegram bot tokens, which notification stacks scatter everywhere.
	regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_\-]{30,}\b`),
	// Discord webhook URLs: the path segment is the credential.
	regexp.MustCompile(`(https://discord(?:app)?\.com/api/webhooks/\d+/)([A-Za-z0-9_\-]{20,})`),
}

// Deliberately absent: any "long random-looking string" rule. Container image
// digests (sha256:eaba...), git revisions, and volume IDs all look exactly
// like secrets to a generic entropy heuristic, and destroying them would gut
// the inventory's usefulness. Secrets are matched by key name or by a
// recognisable vendor shape, never by shape alone.

var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// Fingerprint returns the short stable marker used in place of a secret.
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "[REDACTED:sha256:" + hex.EncodeToString(sum[:])[:8] + "]"
}

// IsSensitiveKey reports whether a key name suggests its value is a secret.
func IsSensitiveKey(key string) bool {
	k := strings.TrimSpace(key)
	if k == "" {
		return false
	}
	if allowKey.MatchString(k) {
		return false
	}
	return sensitiveKey.MatchString(k)
}

// Value redacts val if key looks sensitive, and otherwise scrubs val for
// key-less secrets such as inline URL credentials.
func Value(key, val string) string {
	if IsSensitiveKey(key) {
		if strings.TrimSpace(val) == "" {
			return val
		}
		return Fingerprint(val)
	}
	return String(val)
}

// String scrubs a standalone value that has no associated key.
func String(s string) string {
	if s == "" {
		return s
	}
	for _, re := range valuePatterns {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatch(m)
			// Patterns that capture surrounding context keep it, so the host
			// and username in a URL stay readable while the credential does
			// not. The convention: group 1 is the prefix to keep, group 2 is
			// the secret, group 3 (if present) is the suffix to keep.
			switch len(sub) {
			case 4:
				return sub[1] + Fingerprint(sub[2]) + sub[3]
			case 3:
				return sub[1] + Fingerprint(sub[2])
			default:
				return Fingerprint(m)
			}
		})
	}
	return s
}

// Map returns a copy of m with sensitive values redacted. The input is not
// modified, which matters because collectors often hand us maps they parsed
// straight out of a tool's JSON output.
func Map(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = Value(k, v)
	}
	return out
}

// Text redacts a whole multi-line document: compose YAML, .env files, unit
// files, crontabs. It works line by line so structure and comments survive,
// which is the point of storing the file at all.
func Text(s string) string {
	if s == "" {
		return s
	}
	// Whole PEM blocks first: they span lines and must not be treated as
	// individual assignments.
	s = pemBlock.ReplaceAllStringFunc(s, Fingerprint)

	lines := strings.Split(s, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			// Comments still get value-level scrubbing; people paste
			// credentials into comments constantly.
			lines[i] = String(line)
			continue
		}
		if m := assignment.FindStringSubmatch(line); m != nil {
			prefix, key, sep, val := m[1], m[2], m[3], m[4]
			// Preserve an inline trailing comment rather than fingerprinting it
			// along with the value.
			val, comment := splitInlineComment(val)
			if IsSensitiveKey(key) && strings.TrimSpace(val) != "" {
				lines[i] = prefix + key + sep + quotePreserving(val) + comment
			} else {
				lines[i] = prefix + key + sep + String(val) + comment
			}
			continue
		}
		lines[i] = String(line)
	}
	return strings.Join(lines, "\n")
}

// quotePreserving replaces a value with its fingerprint while keeping any
// surrounding quotes, so redacted YAML and env files still parse.
func quotePreserving(val string) string {
	t := strings.TrimSpace(val)
	lead := val[:len(val)-len(strings.TrimLeft(val, " \t"))]
	trail := val[len(strings.TrimRight(val, " \t")):]
	if len(t) >= 2 {
		if q := t[0]; (q == '"' || q == '\'') && t[len(t)-1] == q {
			return lead + string(q) + Fingerprint(t[1:len(t)-1]) + string(q) + trail
		}
	}
	return lead + Fingerprint(t) + trail
}

// splitInlineComment separates a trailing " # comment" from a value. It is
// intentionally naive: a '#' inside a quoted string is rare in the files we
// read, and erring toward redacting more of the line is the safe direction.
func splitInlineComment(val string) (value, comment string) {
	idx := strings.Index(val, " #")
	if idx < 0 {
		return val, ""
	}
	return val[:idx], val[idx:]
}
