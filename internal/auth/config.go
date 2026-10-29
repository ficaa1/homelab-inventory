// Package auth implements browser sign-in using discovery, authorization codes
// and PKCE. Identity is read from the provider's userinfo endpoint over TLS.
// It deliberately does not parse ID tokens: this stdlib-only implementation
// trusts the authenticated TLS response rather than implementing JOSE/JWKS.
// A nonce is sent with authorization; userinfo does not return it, so state and
// PKCE bind the callback to the browser and code, rather than an ID-token nonce.
package auth

import (
	"net"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
)

// Config is a resolved snapshot. Generation changes whenever settings are saved.
type Config struct {
	Enabled         bool
	Issuer          string
	ClientID        string
	ClientSecret    string
	PublicURL       string
	ProviderName    string
	Allowed         []string
	SessionHours    int
	RequireVerified bool
	Generation      uint64
}

func (c Config) RedirectURI() string {
	if c.PublicURL == "" {
		return ""
	}
	return strings.TrimSuffix(c.PublicURL, "/") + "/auth/callback"
}
func (c Config) Provider() string {
	if c.ProviderName != "" {
		return c.ProviderName
	}
	u, _ := url.Parse(c.Issuer)
	if u != nil {
		return u.Hostname()
	}
	return ""
}
func (c Config) Ready() bool { return c.Enabled && len(c.Validate(true)) == 0 }

// ValidURL permits TLS everywhere and HTTP only to a literal loopback or localhost.
func ValidURL(raw string) bool {
	if !validHTTPURL(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (strings.EqualFold(u.Hostname(), "localhost") || ip != nil && ip.IsLoopback())
}

// Public dashboard origins may use HTTP; provider credentials require TLS.
func validHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.Contains(raw, "#") || u.Opaque != "" {
		return false
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

func validAddress(email string) bool {
	a, err := mail.ParseAddress(email)
	return err == nil && a.Address == email && strings.Count(email, "@") == 1 && !strings.ContainsAny(email, " \t\r\n<>")
}
func ValidAllowed(entry string) bool {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if strings.HasPrefix(entry, "@") {
		return validAddress("user" + entry)
	}
	return validAddress(entry)
}

// Allowed matches complete addresses or complete domains, never domain suffixes.
func Allowed(email string, entries []string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	if !validAddress(email) {
		return false
	}
	_, domain, _ := strings.Cut(email, "@")
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == email || entry == "@"+domain {
			return true
		}
	}
	return false
}

// Validate does no network I/O. Required fields apply when enabling sign-in.
func (c Config) Validate(required bool) map[string]string {
	fields := map[string]string{}
	for key, raw := range map[string]string{"issuer": c.Issuer, "public_url": c.PublicURL} {
		if raw != "" && (key == "issuer" && !ValidURL(raw) || key == "public_url" && !validHTTPURL(raw)) {
			fields[key] = "must be an http(s) URL without credentials or a fragment"
			if key == "issuer" {
				fields[key] = "must be an https URL (http is allowed only on loopback)"
			}
			continue
		}
		if raw == "" {
			continue
		}
		u, _ := url.Parse(raw)
		if u.RawQuery != "" || u.ForceQuery || key == "public_url" && u.Path != "" && u.Path != "/" {
			fields[key] = "must have no query or fragment; public_url must have no path"
		}
	}
	if c.SessionHours < 1 || c.SessionHours > 720 {
		fields["session_hours"] = "must be between 1 and 720"
	}
	for _, entry := range c.Allowed {
		if !ValidAllowed(entry) {
			fields["allowed"] = "use email addresses or @domain entries"
		}
	}
	if required {
		for key, value := range map[string]string{"issuer": c.Issuer, "client_id": c.ClientID, "client_secret": c.ClientSecret, "public_url": c.PublicURL} {
			if value == "" {
				fields[key] = "is required"
			}
		}
		if len(c.Allowed) == 0 {
			fields["allowed"] = "add at least one address or @domain"
		}
	}
	return fields
}

// SafeNext restricts redirects to a path on this origin, including hash routes.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || unsafePath(next) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(u.Path, "//") || unsafePath(u.Path) {
		return "/"
	}
	return next
}

func unsafePath(s string) bool {
	for _, r := range s {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
