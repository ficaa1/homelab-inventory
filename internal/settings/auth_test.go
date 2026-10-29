package settings

import (
	"encoding/json"
	"testing"
)

func TestAuthMergeAndProvenance(t *testing.T) {
	values := map[string]string{"OIDC_ISSUER": "https://id.example.org", "OIDC_CLIENT_ID": "client", "OIDC_CLIENT_SECRET": "fixture-secret", "OIDC_ALLOWED": "User@Example.org, @other.example", "OIDC_PUBLIC_URL": "https://dash.example.org", "OIDC_ENABLED": "true"}
	env := AuthEnv(func(key string) (string, bool) { v, ok := values[key]; return v, ok })
	c, sources := ResolveAuth(AuthStored{}, env, 7)
	if !c.Ready() || c.Generation != 7 || c.SessionHours != 168 || !c.RequireVerified || sources["client_secret"] != "env" || sources["require_verified"] != "default" {
		t.Fatal("environment layering")
	}
	patch := func(raw string) map[string]json.RawMessage {
		var p map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	st, err := MergeAuth(AuthStored{}, env, patch(`{"provider_name":"Example","session_hours":24}`), false)
	if err != nil {
		t.Fatal(err)
	}
	next, err := MergeAuth(st, env, patch(`{"enabled":false,"client_secret":null,"allowed":null}`), false)
	if err != nil {
		t.Fatal(err)
	}
	c, sources = ResolveAuth(next, env, 8)
	if c.ClientSecret != "" || len(c.Allowed) != 0 || c.Enabled || c.ProviderName != "Example" || sources["client_secret"] != "stored" {
		t.Fatal("clear or absent semantics")
	}
	if c, _ := ResolveAuth(st, env, 7); !c.Ready() {
		t.Fatal("merge mutated original")
	}
	for _, raw := range []string{`{"enabled":true}`, `{"session_hours":0}`, `{"session_hours":721}`, `{"session_hours":1.5}`, `{"require_verified":"true"}`, `{"public_url":"https://dash.example.org/path"}`, `{"issuer":"http://id.example.org"}`, `{"allowed":["invalid"]}`} {
		if _, err := MergeAuth(next, env, patch(raw), false); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
