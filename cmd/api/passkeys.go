package main

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/settings"
)

// Passkeys are registered by someone the API token or a session already
// authorizes, on the page origin they will later sign in from. That origin is
// the request's Origin header: the browser sets it, and for a session the CSRF
// check has already matched it. Its host becomes the relying-party ID.

func passkeyList(list []auth.Passkey) map[string]any {
	out := make([]map[string]any, 0, len(list))
	for _, p := range list {
		item := map[string]any{"id": p.ID, "name": p.Name, "origin": p.Origin, "created_at": p.CreatedAt.UTC().Format(time.RFC3339)}
		if p.LastUsed != nil {
			item["last_used_at"] = p.LastUsed.UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return map[string]any{"passkeys": out, "max": auth.MaxPasskeys}
}

// passkeyOrigin returns the page origin of a browser request, when a passkey can exist for it.
func passkeyOrigin(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	origin := r.Header.Get("Origin")
	rpID, ok := auth.PasskeyRPID(origin)
	if !ok || len(r.Header.Values("Origin")) != 1 {
		writeError(w, 400, "passkeys need the dashboard opened over HTTPS on a host name (or on http://localhost), not an IP address")
		return "", "", false
	}
	return origin, rpID, true
}

func (s *server) handleListPasskeys(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading passkeys")
		return
	}
	writeJSON(w, 200, passkeyList(st.Passkeys))
}

// handlePasskeyOptions returns the options for navigator.credentials.create().
func (s *server) handlePasskeyOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.browserAuth == nil {
		writeError(w, 503, "browser sign-in unavailable")
		return
	}
	origin, rpID, ok := passkeyOrigin(w, r)
	if !ok {
		return
	}
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading passkeys")
		return
	}
	if len(st.Passkeys) >= auth.MaxPasskeys {
		writeError(w, 409, "the passkey list is full; remove one first")
		return
	}
	challenge, err := s.browserAuth.RegistrationChallenge(time.Now())
	if err != nil {
		writeError(w, 503, "too many pending passkey requests; try again in a few minutes")
		return
	}
	// Each credential gets its own opaque user handle. The exclude list stops
	// one authenticator from holding two for this dashboard.
	handle := make([]byte, 16)
	if _, err := rand.Read(handle); err != nil {
		writeError(w, 500, "creating passkey request")
		return
	}
	exclude := []map[string]string{}
	for _, p := range st.Passkeys {
		if p.Origin == origin {
			exclude = append(exclude, map[string]string{"type": "public-key", "id": p.ID})
		}
	}
	params := []map[string]any{}
	for _, alg := range auth.PasskeyAlgorithms {
		params = append(params, map[string]any{"type": "public-key", "alg": alg})
	}
	name := s.effective().DashboardName
	if name == "" {
		name = "homelab-inventory"
	}
	writeJSON(w, 200, map[string]any{
		"challenge":              challenge,
		"rp":                     map[string]string{"id": rpID, "name": name},
		"user":                   map[string]string{"id": base64.RawURLEncoding.EncodeToString(handle), "name": "admin", "displayName": name + " admin"},
		"pubKeyCredParams":       params,
		"excludeCredentials":     exclude,
		"authenticatorSelection": map[string]any{"residentKey": "required", "requireResidentKey": true, "userVerification": "preferred"},
		"attestation":            "none",
		"timeout":                300000,
	})
}

func (s *server) handleAddPasskey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.browserAuth == nil {
		writeError(w, 503, "browser sign-in unavailable")
		return
	}
	origin, _, ok := passkeyOrigin(w, r)
	if !ok {
		return
	}
	var in struct {
		Name              string `json:"name"`
		ClientDataJSON    string `json:"clientDataJSON"`
		AttestationObject string `json:"attestationObject"`
	}
	if !pluginBody(w, r, &in, false) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !auth.ValidPasskeyName(in.Name) {
		writeJSON(w, 422, map[string]any{"error": "invalid passkey", "fields": map[string]string{"name": "use 1 to 64 characters on one line"}})
		return
	}
	clientData, err1 := base64.RawURLEncoding.DecodeString(in.ClientDataJSON)
	attestation, err2 := base64.RawURLEncoding.DecodeString(in.AttestationObject)
	if err1 != nil || err2 != nil {
		writeError(w, 400, "invalid passkey request")
		return
	}
	p, err := s.browserAuth.Register(origin, clientData, attestation, time.Now())
	if err != nil {
		// The error is a fixed category, never request data.
		s.log.Printf("passkey registration failed: %s", err)
		writeError(w, 400, "the passkey could not be verified; try adding it again")
		return
	}
	p.Name = in.Name
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading passkeys")
		return
	}
	if len(st.Passkeys) >= auth.MaxPasskeys {
		writeError(w, 409, "the passkey list is full; remove one first")
		return
	}
	if slices.ContainsFunc(st.Passkeys, func(o auth.Passkey) bool { return o.ID == p.ID }) {
		writeError(w, 409, "that passkey is already registered")
		return
	}
	st.Passkeys = append(st.Passkeys, p)
	st.UpdatedAt = time.Now()
	if s.store.WriteState(settings.StateName, st) != nil {
		writeError(w, 500, "saving passkey")
		return
	}
	s.log.Printf("passkey added by %s", requestActor(r))
	writeJSON(w, 201, passkeyList(st.Passkeys))
}

// handleDeletePasskey removes a credential; sessions it issued stop at once.
func (s *server) handleDeletePasskey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.readSettings()
	if err != nil {
		writeError(w, 500, "reading passkeys")
		return
	}
	id := r.PathValue("id")
	i := slices.IndexFunc(st.Passkeys, func(p auth.Passkey) bool { return p.ID == id })
	if i < 0 {
		writeError(w, 404, "no such passkey")
		return
	}
	st.Passkeys = slices.Delete(st.Passkeys, i, i+1)
	st.UpdatedAt = time.Now()
	if s.store.WriteState(settings.StateName, st) != nil {
		writeError(w, 500, "removing passkey")
		return
	}
	s.log.Printf("passkey removed by %s", requestActor(r))
	writeJSON(w, 200, passkeyList(st.Passkeys))
}

// passkeyUsed records a sign-in under the settings lock, where the counter is
// compared again so two concurrent uses of one value cannot both pass.
func (s *server) passkeyUsed(id string, signCount uint32, at time.Time) error {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st, err := s.readSettings()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(st.Passkeys, func(p auth.Passkey) bool { return p.ID == id })
	if i < 0 {
		return errors.New("passkey removed")
	}
	if p := st.Passkeys[i]; (signCount != 0 || p.SignCount != 0) && signCount <= p.SignCount {
		return errors.New("passkey counter did not advance")
	}
	at = at.UTC().Truncate(time.Second)
	st.Passkeys[i].SignCount, st.Passkeys[i].LastUsed = signCount, &at
	return s.store.WriteState(settings.StateName, st)
}
