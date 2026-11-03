package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agenthealth"
	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentreceipt"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/agentstatus"
	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
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
	if p := r.URL.Query().Get("protocol"); p != "" {
		if p != "1" {
			writeError(w, 400, "unsupported launcher protocol")
			return
		}
		runtime, runtimeData, err := agentrelease.ReadSequence(s.agentDir(), m.Sequence, true)
		if err != nil || runtime.Version != m.Version || runtime.Revision != m.Revision {
			writeError(w, 503, "signed runtime unavailable")
			return
		}
		for _, platform := range m.Platforms {
			a := runtime.Artifacts[platform+"/discover"]
			legacy := m.Artifacts[platform+"/discover"]
			if a.Name != legacy.Name || a.SHA256 != legacy.SHA256 || a.Size != legacy.Size {
				writeError(w, 503, "runtime identity conflict")
				return
			}
		}
		data = runtimeData
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
	// Retained release envelopes authorize exact immutable artifact names.
	m, _, e := agentrelease.ReadSequence(s.agentDir(), seq, true)
	if e != nil {
		m, _, e = agentrelease.ReadSequence(s.agentDir(), seq, false)
	}
	if e != nil {
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
func (s *server) agentHealth(now time.Time) (agenthealth.Snapshot, error) {
	invs, err := s.store.LatestAll()
	if err != nil {
		return agenthealth.Snapshot{}, err
	}
	return agenthealth.Evaluate(s.store.Dir(), s.agentDir(), invs, now)
}
func (s *server) handleAgentStatus(w http.ResponseWriter, r *http.Request) {
	snap, err := s.agentHealth(time.Now())
	if err != nil {
		writeError(w, 503, "agent health unavailable")
		return
	}
	// Configuration absence is a normal, useful status, not an HTTP error.
	writeJSON(w, 200, map[string]any{"sequence": snap.Sequence, "revision": snap.Revision, "version": snap.Version, "mode": snap.Mode, "control": snap.Control, "ssh_hold": snap.SSHHold, "promoted": snap.Mode == "promote", "configured": snap.Configured, "unavailable": snap.Unavailable, "canary_hosts": agentoffers.CanaryHosts(), "hosts": snap.Hosts})
}
func (s *server) handlePostAgentStatus(w http.ResponseWriter, r *http.Request) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, 415, "application/json required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, agentstatus.MaxSize)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var d agentstatus.Document
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF || (!agentstatus.Valid(d) || store.ValidateHost(d.Host) != nil) {
		writeError(w, 400, "invalid agent status")
		return
	}
	if err = agentstatus.Save(s.store.Dir(), d, "status", time.Now()); err != nil {
		writeError(w, 500, "saving agent status")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Operator credentials authorize only offers and restart execution.
func (s *server) agentSession(r *http.Request) (string, bool) {
	if s.offerOperator != nil {
		return s.offerOperator(r)
	}
	if s.operatorToken != "" && s.operatorToken != s.apiToken() && len(s.operatorToken) >= 32 && httpx.BearerOK(r, s.operatorToken, httpx.DenyWhenUnset) {
		return "operator", true
	}
	// A signed-in browser session may change offers too; the API bearer never can.
	if s.browserAuth != nil {
		if session, ok := s.browserAuth.Session(r); ok && auth.CSRF(r, s.authConfig()) {
			return session.Email, true
		}
	}
	return "", false
}

// offersBySession reports whether a browser sign-in can change agent offers.
func (s *server) offersBySession() bool { return s.browserAuth != nil && s.authConfig().Enabled }

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

// The launcher owns this POST. A receipt binds the exact bytes, manifest and
// transaction, and is generated only after durable storage on this server.
func (s *server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		writeError(w, 415, "application/json required")
		return
	}
	data, e := io.ReadAll(http.MaxBytesReader(w, r.Body, agentproto.MaxReport))
	if e != nil {
		writeError(w, 413, "report too large")
		return
	}
	o, e := agentproto.Object(data)
	if e != nil {
		writeError(w, 400, "invalid report")
		return
	}
	var rt agentproto.Runtime
	var inv inventory.Inventory
	if json.Unmarshal(o["agent_runtime"], &rt) != nil || json.Unmarshal(data, &inv) != nil || store.ValidateHost(inv.Host.Hostname) != nil {
		writeError(w, 400, "invalid report")
		return
	}
	if _, e = agentproto.ValidateReport(data, inv.Host.Hostname, rt.Nonce, rt.Identity); e != nil {
		writeError(w, 400, "invalid report envelope")
		return
	}
	// Bind role identities to the retained signed runtime, never telemetry.
	m, envelope, e := agentrelease.ReadSequence(s.agentDir(), rt.Sequence, true)
	var signed agentrelease.Signed
	_ = json.Unmarshal(envelope, &signed)
	a, ok := m.Artifacts[strings.ReplaceAll(inv.Tool.Platform, "/", "-")+"/discover"]
	u, uok := m.Artifacts[strings.ReplaceAll(inv.Tool.Platform, "/", "-")+"/agentupdate"]
	// Older collectors may omit platform. Match only a complete signed pair.
	if inv.Tool.Platform == "" {
		for _, p := range m.Platforms {
			da := m.Artifacts[p+"/discover"]
			ua := m.Artifacts[p+"/agentupdate"]
			if da.SHA256 == rt.Discover && ua.SHA256 == rt.Updater {
				a = da
				u = ua
				ok = true
				uok = true
				break
			}
		}
	}
	if e != nil || !ok || !uok || rt.Digest != agentrelease.Digest(signed.Manifest) || rt.Version != m.Version || rt.Revision != m.Revision || rt.Discover != a.SHA256 || rt.Updater != u.SHA256 {
		writeError(w, 400, "report runtime mismatch")
		return
	}
	rec, e := agentreceipt.Accept(s.store.Dir(), inv.Host.Hostname, rt, data, func(rec agentproto.Receipt) error {
		s.ingestMu.Lock()
		defer s.ingestMu.Unlock()
		// Use the durable server receipt for the indexing ID: a backward host clock
		// or same-time fallback report cannot collide with a different transaction.
		if inv.Software != nil {
			if err := s.packageStore().Ingest(&inv); err != nil {
				return err
			}
			inv.Software = nil
		}
		if _, err := s.store.SaveReceived(&inv, rec.Received); err != nil {
			return err
		}
		s.overviewCache.Invalidate()
		return nil
	})
	if e != nil {
		code := 500
		if errors.Is(e, agentreceipt.ErrConflict) {
			code = 409
		}
		writeError(w, code, "accepting report")
		return
	}
	writeJSON(w, 201, rec)
}
