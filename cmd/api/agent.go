package main

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

// The release tree stays read-only; browser decisions live in the API data dir.
func (s *server) agentDir() string {
	if s.agentReleaseDir != "" {
		return s.agentReleaseDir
	}
	return os.Getenv("AGENT_RELEASE_DIR")
}
func (s *server) handleAgentManifest(w http.ResponseWriter, r *http.Request) {
	m, data, c, e := agentrelease.Read(s.agentDir())
	if e != nil {
		writeError(w, 503, "agent release unavailable")
		return
	}
	if !s.agentOffered(r.URL.Query().Get("host"), m, c) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}
func (s *server) handleAgentArtifact(w http.ResponseWriter, r *http.Request) {
	seq, e := strconv.ParseUint(r.PathValue("sequence"), 10, 64)
	if e != nil || strconv.FormatUint(seq, 10) != r.PathValue("sequence") {
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("name")
	// Immutable artifact access is limited to names from the current signed manifest.
	m, _, _, e := agentrelease.Read(s.agentDir())
	if e != nil || seq != m.Sequence {
		http.NotFound(w, r)
		return
	}
	for _, a := range m.Artifacts {
		if name == a.Name && !strings.ContainsAny(name, "/\\") {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFile(w, r, filepath.Join(s.agentDir(), strconv.FormatUint(seq, 10), name))
			return
		}
	}
	http.NotFound(w, r)
}
func (s *server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	m, _, c, e := agentrelease.Read(s.agentDir())
	if e != nil {
		writeError(w, 503, "agent release unavailable")
		return
	}
	s.agentOffersMu.Lock()
	state, _, err := agentoffers.Read(s.store.AgentOffersPath())
	s.agentOffersMu.Unlock()
	if err != nil {
		writeError(w, 503, "agent offer state unavailable")
		return
	}
	mode := agentoffers.Effective(m, c, state)
	writeJSON(w, 200, map[string]any{"sequence": m.Sequence, "revision": m.Revision, "version": m.Version, "control": c.Mode, "mode": mode, "ssh_hold": c.Mode == "hold", "promoted": mode == "promote", "canary_hosts": agentoffers.CanaryHosts()})
}

// Operator credentials authorize only offers and restart execution.
func (s *server) agentSession(r *http.Request) (string, bool) {
	if s.offerOperator != nil {
		return s.offerOperator(r)
	}
	if s.operatorToken != "" && s.operatorToken != s.apiToken() && len(s.operatorToken) >= 32 && httpx.BearerOK(r, s.operatorToken, httpx.DenyWhenUnset) {
		return "operator", true
	}
	return "", false
}
func (s *server) agentOffered(host string, m agentrelease.Manifest, c agentrelease.Control) bool {
	s.agentOffersMu.Lock()
	defer s.agentOffersMu.Unlock()
	state, _, err := agentoffers.Read(s.store.AgentOffersPath())
	if err != nil {
		// A broken dashboard journal must not disable SSH recovery controls.
		state = agentoffers.State{}
	}
	// The caller may have read the release before waiting for this lock.
	// Recheck after reading dashboard state: a new release or SSH hold must
	// invalidate that older snapshot rather than revive its dashboard grants.
	current, _, control, err := agentrelease.Read(s.agentDir())
	if err != nil || current.Sequence != m.Sequence {
		return false
	}
	c = control
	// Shared-token inventories are display data, never offer authority.
	return agentoffers.Offered(host, m, c, state)
}
func (s *server) handleAgentOffers(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.agentSession(r)
	if !ok {
		writeError(w, 403, "operator token required or route disabled")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, 415, "application/json required")
		return
	}
	var in struct {
		Action   string `json:"action"`
		Host     string `json:"host"`
		Sequence uint64 `json:"sequence"`
		Confirm  bool   `json:"confirm"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&in); err != nil {
		writeError(w, 400, "invalid offer JSON")
		return
	}
	if dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "one JSON object required")
		return
	}
	if !in.Confirm {
		writeError(w, 400, "confirmation required")
		return
	}
	if in.Action != "update" && in.Action != "promote" && in.Action != "hold" && in.Action != "auto" {
		writeError(w, 400, "invalid action")
		return
	}
	if (in.Action == "update") != (in.Host != "") {
		writeError(w, 400, "host required only for update")
		return
	}
	s.agentOffersMu.Lock()
	defer s.agentOffersMu.Unlock()
	m, _, c, err := agentrelease.Read(s.agentDir())
	if err != nil {
		writeError(w, 503, "agent release unavailable")
		return
	}
	if in.Sequence != m.Sequence {
		writeError(w, 409, "release changed; refresh")
		return
	}
	if c.Mode == "hold" || c.Sequence != m.Sequence || (c.Mode != "auto" && c.Mode != "promote") {
		writeError(w, 409, "SSH hold; release over SSH first")
		return
	}
	if in.Action == "update" {
		state, _, err := agentoffers.Read(s.store.AgentOffersPath())
		if err != nil {
			writeError(w, 503, "agent offer state unavailable")
			return
		}
		if agentoffers.Effective(m, c, state) == "hold" {
			writeError(w, 409, "dashboard hold; select auto or promote first")
			return
		}
	}
	if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, in.Action, in.Host, actor); err != nil {
		writeError(w, 503, "saving agent offers failed")
		return
	}
	writeJSON(w, 200, map[string]any{"sequence": m.Sequence, "action": in.Action})
}
