package settings

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/auth"
)

// AuthStored keeps environment fallbacks distinct from explicit zero/empty values.
// A null patch stores the field's empty/default value, suppressing its env seed.
type AuthStored struct {
	Enabled         *bool     `json:"enabled,omitempty"`
	Issuer          *string   `json:"issuer,omitempty"`
	ClientID        *string   `json:"client_id,omitempty"`
	ClientSecret    *string   `json:"client_secret,omitempty"`
	Allowed         *[]string `json:"allowed,omitempty"`
	PublicURL       *string   `json:"public_url,omitempty"`
	ProviderName    *string   `json:"provider_name,omitempty"`
	SessionHours    *int      `json:"session_hours,omitempty"`
	RequireVerified *bool     `json:"require_verified,omitempty"`
}

// AuthEnv reads only the documented OIDC seeds. Invalid typed seeds fail closed
// through validation, without logging their values.
func AuthEnv(lookup func(string) (string, bool)) AuthStored {
	var out AuthStored
	for key, ptr := range map[string]**string{"ISSUER": &out.Issuer, "CLIENT_ID": &out.ClientID, "CLIENT_SECRET": &out.ClientSecret, "PUBLIC_URL": &out.PublicURL, "PROVIDER_NAME": &out.ProviderName} {
		if v, ok := lookup("OIDC_" + key); ok {
			*ptr = &v
		}
	}
	if v, ok := lookup("OIDC_ALLOWED"); ok {
		entries := []string{}
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				entries = append(entries, e)
			}
		}
		out.Allowed = &entries
	}
	if v, ok := lookup("OIDC_SESSION_HOURS"); ok {
		n, _ := strconv.Atoi(v)
		out.SessionHours = &n
	}
	if v, ok := lookup("OIDC_ENABLED"); ok {
		b, _ := strconv.ParseBool(v)
		out.Enabled = &b
	}
	return out
}

// ResolveAuth reports provenance alongside the complete effective configuration.
func ResolveAuth(st, env AuthStored, generation uint64) (auth.Config, map[string]string) {
	c := auth.Config{SessionHours: 168, RequireVerified: true, Allowed: []string{}, Generation: generation}
	sources := map[string]string{}
	text := func(key string, s, e *string, dst *string) {
		sources[key] = "default"
		if e != nil {
			*dst = *e
			sources[key] = "env"
		}
		if s != nil {
			*dst = *s
			sources[key] = "stored"
		}
	}
	boolean := func(key string, s, e *bool, dst *bool) {
		sources[key] = "default"
		if e != nil {
			*dst = *e
			sources[key] = "env"
		}
		if s != nil {
			*dst = *s
			sources[key] = "stored"
		}
	}
	text("issuer", st.Issuer, env.Issuer, &c.Issuer)
	text("client_id", st.ClientID, env.ClientID, &c.ClientID)
	text("client_secret", st.ClientSecret, env.ClientSecret, &c.ClientSecret)
	text("public_url", st.PublicURL, env.PublicURL, &c.PublicURL)
	text("provider_name", st.ProviderName, env.ProviderName, &c.ProviderName)
	boolean("enabled", st.Enabled, env.Enabled, &c.Enabled)
	boolean("require_verified", st.RequireVerified, env.RequireVerified, &c.RequireVerified)
	sources["session_hours"] = "default"
	if env.SessionHours != nil {
		c.SessionHours = *env.SessionHours
		sources["session_hours"] = "env"
	}
	if st.SessionHours != nil {
		c.SessionHours = *st.SessionHours
		sources["session_hours"] = "stored"
	}
	sources["allowed"] = "default"
	if env.Allowed != nil {
		c.Allowed = append([]string{}, (*env.Allowed)...)
		sources["allowed"] = "env"
	}
	if st.Allowed != nil {
		c.Allowed = append([]string{}, (*st.Allowed)...)
		sources["allowed"] = "stored"
	}
	return c, sources
}

type AuthValidationError struct {
	Message string            `json:"error"`
	Fields  map[string]string `json:"fields"`
}

func (e *AuthValidationError) Error() string { return e.Message }

// MergeAuth uses value/set, null/clear, absent/keep semantics, including secrets.
// It returns the original value on any failure, and never mutates pointer targets.
func MergeAuth(st, env AuthStored, patch map[string]json.RawMessage, required bool) (AuthStored, error) {
	out := st
	fields := map[string]string{}
	for key, raw := range patch {
		var target any
		// Fresh pointers ensure a rejected candidate cannot change the original.
		switch key {
		case "enabled":
			v := false
			out.Enabled = &v
			target = out.Enabled
		case "require_verified":
			v := true
			out.RequireVerified = &v
			target = out.RequireVerified
		case "session_hours":
			v := 168
			out.SessionHours = &v
			target = out.SessionHours
		case "issuer":
			v := ""
			out.Issuer = &v
			target = out.Issuer
		case "client_id":
			v := ""
			out.ClientID = &v
			target = out.ClientID
		case "client_secret":
			v := ""
			out.ClientSecret = &v
			target = out.ClientSecret
		case "public_url":
			v := ""
			out.PublicURL = &v
			target = out.PublicURL
		case "provider_name":
			v := ""
			out.ProviderName = &v
			target = out.ProviderName
		case "allowed":
			v := []string{}
			out.Allowed = &v
			target = out.Allowed
		default:
			fields[key] = "unknown setting"
			continue
		}
		if string(raw) != "null" && json.Unmarshal(raw, target) != nil {
			fields[key] = "invalid type"
		}
	}
	c, _ := ResolveAuth(out, env, 0)
	for key, msg := range c.Validate(required || c.Enabled) {
		fields[key] = msg
	}
	if len(fields) != 0 {
		return st, &AuthValidationError{Message: "invalid sign-in settings", Fields: fields}
	}
	return out, nil
}
