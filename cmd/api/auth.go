package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/settings"
)

type actorContextKey struct{}

// requestActor is the audit identity selected by the authentication middleware.
func requestActor(r *http.Request) string {
	if actor, ok := r.Context().Value(actorContextKey{}).(string); ok {
		return actor
	}
	return "bearer"
}
func withActor(r *http.Request, actor string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor))
}

func (s *server) authConfig() auth.Config {
	st, err := s.readSettings()
	if err != nil {
		return auth.Config{}
	}
	c, _ := settings.ResolveAuth(st.Auth, s.defaults.Auth, st.AuthGeneration)
	if s.setup != nil {
		if state, _ := s.setup.Status(); state != "claimed" {
			c.Enabled = false
		}
	}
	return c
}
func (s *server) oidc() (*auth.Manager, error) {
	s.authOnce.Do(func() { s.browserAuth, s.authErr = auth.Open(s.store.Dir(), s.authConfig, s.log.Printf) })
	return s.browserAuth, s.authErr
}
func (s *server) authRoutes(mux *http.ServeMux) {
	m, err := s.oidc()
	if err != nil {
		s.log.Printf("browser sign-in unavailable: could not open signing key")
	}
	mux.HandleFunc("GET /auth/status", s.handleAuthStatus)
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			http.NotFound(w, r)
			return
		}
		m.Login(w, r)
	})
	mux.HandleFunc("GET /auth/callback", func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			http.Redirect(w, r, "/#/login?error=disabled", 302)
			return
		}
		m.Callback(w, r)
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if m == nil {
			writeError(w, 503, "browser sign-in unavailable")
			return
		}
		m.Logout(w, r)
	})
	mux.HandleFunc("GET /settings/auth", s.auth(s.handleGetAuthSettings))
	mux.HandleFunc("PUT /settings/auth", s.auth(s.handlePutAuthSettings))
	mux.HandleFunc("POST /settings/auth/test", s.auth(s.handleTestAuthSettings))
}
func (s *server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c := s.authConfig()
	out := map[string]any{"enabled": s.browserAuth != nil && c.Ready(), "provider": c.Provider(), "signed_in": false}
	if s.browserAuth != nil {
		if session, ok := s.browserAuth.Session(r); ok {
			out["signed_in"] = true
			out["email"] = session.Email
			out["expires_at"] = time.Unix(session.Exp, 0).UTC().Format(time.RFC3339)
		}
	}
	writeJSON(w, 200, out)
}

func (s *server) handleGetAuthSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading sign-in settings")
		return
	}
	c, sources := settings.ResolveAuth(st.Auth, s.defaults.Auth, st.AuthGeneration)
	state, message := "disabled", ""
	if len(c.Validate(true)) > 0 {
		state = "unconfigured"
	} else if c.Enabled {
		state = "ok"
	}
	if c.Enabled && state == "ok" {
		s.authTestMu.Lock()
		if s.authTestGeneration == st.AuthGeneration && s.authTestMessage != "" {
			state, message = "error", s.authTestMessage
		}
		s.authTestMu.Unlock()
		if s.authErr != nil {
			state, message = "error", "could not open session signing key"
		}
	}
	writeJSON(w, 200, map[string]any{
		"enabled": c.Enabled, "state": state, "message": message,
		"issuer": c.Issuer, "client_id": c.ClientID, "client_secret": map[string]bool{"set": c.ClientSecret != ""},
		"allowed": c.Allowed, "public_url": c.PublicURL, "provider_name": c.Provider(),
		"session_hours": c.SessionHours, "require_verified": c.RequireVerified, "redirect_uri": c.RedirectURI(), "sources": sources,
	})
}
func (s *server) handlePutAuthSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var patch map[string]json.RawMessage
	if !pluginBody(w, r, &patch, false) {
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading sign-in settings")
		return
	}
	next, err := settings.MergeAuth(st.Auth, s.defaults.Auth, patch, false)
	if err != nil {
		writeJSON(w, 422, err)
		return
	}
	st.Auth = next
	st.AuthGeneration++
	st.UpdatedAt = time.Now()
	if s.store.WriteState(settings.StateName, st) != nil {
		writeError(w, 500, "saving sign-in settings")
		return
	}
	s.authTestMu.Lock()
	s.authTestMessage = ""
	s.authTestMu.Unlock()
	s.log.Printf("browser sign-in settings updated; sessions invalidated")
	s.handleGetAuthSettings(w, r)
}
func (s *server) handleTestAuthSettings(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.authTesting.CompareAndSwap(false, true) {
		writeError(w, 429, "a sign-in test is already running")
		return
	}
	defer s.authTesting.Store(false)
	var in struct {
		Settings map[string]json.RawMessage `json:"settings"`
	}
	if !pluginBody(w, r, &in, true) {
		return
	}
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading sign-in settings")
		return
	}
	next, err := settings.MergeAuth(st.Auth, s.defaults.Auth, in.Settings, false)
	if err != nil {
		writeJSON(w, 422, err)
		return
	}
	c, _ := settings.ResolveAuth(next, s.defaults.Auth, st.AuthGeneration)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	message, ok := "Discovery and endpoints are valid", true
	if s.browserAuth == nil {
		message, ok = "could not open session signing key", false
	} else if _, err := s.browserAuth.Discover(ctx, c); err != nil {
		message, ok = err.Error(), false
	}
	// A test of unsaved inputs must not change the live settings' status.
	if len(in.Settings) == 0 {
		s.authTestMu.Lock()
		s.authTestGeneration = st.AuthGeneration
		s.authTestMessage = ""
		if !ok {
			s.authTestMessage = message
		}
		s.authTestMu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"ok": ok, "message": message, "checks": []any{map[string]any{"name": "Discovery", "ok": ok, "message": message}}})
}
