package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/catalog"
	"github.com/ficaa1/homelab-inventory/internal/composelog"
	"github.com/ficaa1/homelab-inventory/internal/deps"
	"github.com/ficaa1/homelab-inventory/internal/drift"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/fwplan"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
	"github.com/ficaa1/homelab-inventory/internal/icons"
	"github.com/ficaa1/homelab-inventory/internal/images"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/overview"
	"github.com/ficaa1/homelab-inventory/internal/portmap"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"github.com/ficaa1/homelab-inventory/internal/settings"
	"github.com/ficaa1/homelab-inventory/internal/setup"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/softstore"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"github.com/ficaa1/homelab-inventory/internal/trend"
	"github.com/ficaa1/homelab-inventory/internal/triage"
	"github.com/ficaa1/homelab-inventory/internal/webui"
)

// maxInventoryBytes bounds a POST /inventory body. Compose file contents are
// embedded in the JSON, but even a large fleet's worth is a few MB; this
// leaves headroom without letting a misbehaving poster exhaust memory.
const maxInventoryBytes = 16 << 20

type server struct {
	setup                  *setup.Manager
	agentBinDir, publicURL string
	trustedProxy           bool
	// offerOperator is a test seam; production uses a separate operator token.
	operatorToken   string
	agentToken      string
	actionPolicy    execallow.Policy
	offerOperator   func(*http.Request) (string, bool)
	ingestMu        sync.Mutex
	softwareOnce    sync.Once
	software        *softstore.Store
	diskHealthMu    sync.Mutex
	agentReleaseDir string
	agentOffersMu   sync.Mutex
	overviewCache   overview.StorageCache
	overviewRuns    overview.RunMemo
	trends          trend.Cache
	settingsMu      sync.Mutex
	pruneMu         sync.Mutex
	scheduleMu      sync.Mutex
	nextPrune       time.Time
	store           *store.Store
	log             *log.Logger
	token           string
	notifier        *notify.Notifier
	icons           *icons.Cache
	exec            *execallow.Runner
	actions         *actions.Manager
	defaults        settings.Defaults
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	s.setupRoutes(mux)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /auth/status", s.handleAuthStatus)
	mux.HandleFunc("POST /inventory", s.agentAuth(s.handleIngest))
	mux.HandleFunc("GET /agent/manifest", s.agentAuth(s.handleAgentManifest))
	mux.HandleFunc("GET /agent/artifacts/{sequence}/{name}", s.agentAuth(s.handleAgentArtifact))
	mux.HandleFunc("GET /agent/status", s.auth(s.handleAgentStatus))
	mux.HandleFunc("POST /agent/offers", s.handleAgentOffers)
	mux.HandleFunc("GET /overview", s.auth(s.handleOverview))
	mux.HandleFunc("GET /hosts", s.auth(s.handleHosts))
	mux.HandleFunc("GET /hosts/{name}", s.auth(s.handleHost))
	mux.HandleFunc("GET /hosts/{name}/packages", s.auth(s.handleHostPackages))
	mux.HandleFunc("GET /hosts/{name}/packages/changes", s.auth(s.handlePackageChanges))
	mux.HandleFunc("GET /packages", s.auth(s.handlePackageSearch))
	mux.HandleFunc("GET /hosts/{name}/history", s.auth(s.handleHostHistory))
	mux.HandleFunc("GET /hosts/{name}/drift", s.auth(s.handleHostDrift))
	mux.HandleFunc("GET /hosts/{name}/trend", s.auth(s.handleHostTrend))
	mux.HandleFunc("GET /hosts/{name}/deps", s.auth(s.handleHostDeps))
	mux.HandleFunc("GET /hosts/{name}/compose", s.auth(s.handleHostCompose))
	mux.HandleFunc("GET /hosts/{name}/compose/diff", s.auth(s.handleHostComposeDiff))
	mux.HandleFunc("GET /hosts/{name}/compose/file", s.auth(s.handleHostComposeFile))
	mux.HandleFunc("GET /hosts/{name}/firewall-plan", s.auth(s.handleHostFirewallPlan))
	mux.HandleFunc("GET /images", s.auth(s.handleImages))
	mux.HandleFunc("GET /ports", s.auth(s.handlePorts))
	mux.HandleFunc("GET /alerts", s.auth(s.handleAlerts))
	mux.HandleFunc("GET /services", s.auth(s.handleServices))
	mux.HandleFunc("POST /services/accept", s.auth(s.handleAcceptServices))
	mux.HandleFunc("PUT /services/{key}", s.auth(s.handleCurateService))
	mux.HandleFunc("DELETE /services/{key}", s.auth(s.handleForgetService))
	mux.HandleFunc("GET /icons/{slug}", s.auth(s.handleIcon))
	mux.HandleFunc("GET /notify", s.auth(s.handleNotifyStatus))
	mux.HandleFunc("GET /settings", s.auth(s.handleGetSettings))
	mux.HandleFunc("PUT /settings", s.auth(s.handlePutSettings))
	mux.HandleFunc("POST /settings/reset", s.auth(s.handleResetSettings))
	mux.HandleFunc("POST /notify/test", s.auth(s.handleNotifyTest))
	mux.HandleFunc("GET /silences", s.auth(s.handleListSilences))
	mux.HandleFunc("POST /silences", s.auth(s.handleAddSilence))
	mux.HandleFunc("DELETE /silences/{id}", s.auth(s.handleRemoveSilence))
	mux.HandleFunc("GET /capabilities", s.auth(s.handleCapabilities))
	mux.HandleFunc("POST /actions/preview", s.auth(s.handleActionPreview))
	mux.HandleFunc("POST /actions/execute", s.handleActionExecute)
	mux.HandleFunc("GET /actions/log", s.auth(s.handleActionLog))
	mux.HandleFunc("GET /actions/stacks", s.auth(s.handleActionStacks))
	mux.HandleFunc("GET /storage", s.auth(s.handleStorage))
	mux.HandleFunc("GET /storage/runs", s.auth(s.handleRunsStorage))
	mux.HandleFunc("POST /storage/runs/prune", s.auth(s.handlePruneRuns))
	mux.HandleFunc("DELETE /storage/runs/{host}", s.auth(s.handleForgetHost))
	mux.Handle("GET /", webui.Handler())
	return s.setupGate(mux)
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if httpx.BearerOK(r, s.apiToken(), httpx.OpenWhenUnset) {
			next(w, r)
			return
		}
		writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
	}
}

// agentAuth permits only the narrowly scoped routes registered with it.
func (s *server) agentAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if httpx.BearerOK(r, s.agentCredential(), httpx.DenyWhenUnset) {
			next(w, r)
			return
		}
		s.auth(next)(w, r)
	}
}

// Browser login is disabled; the dashboard uses bearer authentication.
func (s *server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
}

func (s *server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"operator_enabled": s.operatorToken != "", "agent_offers_enabled": s.operatorToken != "", "actions_enabled": s.actions != nil && s.operatorToken != "", "actions_host": s.actionPolicy.Host})
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleIngest accepts exactly what `discover` emits and stores it under its
// hostname and collection time. Storage is append-only: a run never
// overwrites another, so drift between runs is just "diff two files".
// Software is ingested separately into softstore and removed before Save, even
// if that ingest fails: package-storage failures must never discard a run.
func (s *server) handleIngest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxInventoryBytes)

	var inv inventory.Inventory
	if err := json.NewDecoder(r.Body).Decode(&inv); err != nil {
		writeError(w, http.StatusBadRequest, "decoding inventory: "+err.Error())
		return
	}
	if err := store.ValidateHost(inv.Host.Hostname); err != nil {
		writeError(w, http.StatusBadRequest, "invalid or reserved inventory host.hostname")
		return
	}

	s.ingestMu.Lock()
	defer s.ingestMu.Unlock()

	var softwareError string
	if inv.Software != nil {
		if err := s.packageStore().Ingest(&inv); err != nil {
			s.log.Printf("ingest: software for %s: %v", inv.Host.Hostname, err)
			softwareError = "software storage failed; see server log"
		}
		inv.Software = nil
	}
	rec, err := s.store.Save(&inv)
	if err != nil {
		s.log.Printf("ingest: saving %s: %v", inv.Host.Hostname, err)
		writeError(w, http.StatusInternalServerError, "storing inventory")
		return
	}

	s.overviewCache.Invalidate()

	resp := map[string]any{
		"host":           rec.Host,
		"collected_at":   rec.CollectedAt,
		"size_bytes":     rec.SizeBytes,
		"schema_version": inv.SchemaVersion,
	}
	if softwareError != "" {
		resp["software_error"] = softwareError
	}
	if inv.SchemaVersion != inventory.SchemaVersion {
		resp["warning"] = fmt.Sprintf("stored as schema v%s but this server expects v%s",
			inv.SchemaVersion, inventory.SchemaVersion)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// hostSummary is a per-host digest for GET /hosts. It stays in cmd/api rather
// than internal/ because, unlike portmap, nothing else needs it yet — if a
// second consumer shows up, move it there.
type hostSummary struct {
	Name                 string                 `json:"name"`
	OS                   string                 `json:"os,omitempty"`
	CollectedAt          time.Time              `json:"collected_at"`
	SchemaVersion        string                 `json:"schema_version"`
	Elevated             bool                   `json:"elevated"`
	AgentVersion         string                 `json:"agent_version"`
	AgentRevision        int                    `json:"agent_revision"`
	AgentOutdated        bool                   `json:"agent_outdated"`
	AgentOfferedRevision int                    `json:"agent_offered_revision,omitempty"`
	AgentOfferedSequence uint64                 `json:"agent_offered_sequence,omitempty"`
	AgentUpdate          *inventory.AgentUpdate `json:"agent_update,omitempty"`
	Runs                 int                    `json:"runs"`
	Warnings             int                    `json:"warnings"`
	Errors               int                    `json:"errors"`
}

func (s *server) handleHosts(w http.ResponseWriter, r *http.Request) {
	names, err := s.store.Hosts()
	if err != nil {
		s.log.Printf("hosts: %v", err)
		writeError(w, http.StatusInternalServerError, "listing hosts")
		return
	}

	out := make([]hostSummary, 0, len(names))
	for _, name := range names {
		inv, err := s.store.Latest(name)
		if err != nil {
			s.log.Printf("hosts: latest for %s: %v", name, err)
			continue
		}
		history, err := s.store.History(name)
		if err != nil {
			s.log.Printf("hosts: history for %s: %v", name, err)
		}
		sum := hostSummary{
			Name:          name,
			OS:            inv.Host.OS.Pretty,
			CollectedAt:   inv.CollectedAt,
			SchemaVersion: inv.SchemaVersion,
			Elevated:      inv.Tool.Elevated,
			AgentVersion:  inv.Tool.Version,
			AgentRevision: inv.Tool.Revision,
			AgentOutdated: inv.Tool.Revision < inventory.AgentRevision,
			Runs:          len(history),
		}
		sum.AgentUpdate = inv.AgentUpdate
		if m, _, c, e := agentrelease.Read(s.agentDir()); e == nil && s.agentOffered(name, m, c) {
			sum.AgentOfferedRevision = m.Revision
			sum.AgentOfferedSequence = m.Sequence
			sum.AgentOutdated = inv.Tool.Revision < m.Revision || inv.AgentUpdate == nil || inv.AgentUpdate.Sequence < m.Sequence
		}
		for _, d := range inv.Diagnostics {
			switch d.Severity {
			case inventory.SeverityWarn:
				sum.Warnings++
			case inventory.SeverityError:
				sum.Errors++
			}
		}
		out = append(out, sum)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleHost serves the latest inventory for a host, or a specific run when
// ?at=<id> is given — the escape hatch for "what did this look like before I
// made the change". id is a Record.ID as returned by GET /hosts/{name}/history,
// not a timestamp you construct yourself.
func (s *server) handleHost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	if id := r.URL.Query().Get("at"); id != "" {
		inv, err := s.store.At(name, id)
		if err != nil {
			s.respondLookup(w, name, err)
			return
		}
		writeJSON(w, http.StatusOK, inv)
		return
	}

	inv, err := s.store.Latest(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *server) handleHostHistory(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	records, err := s.store.History(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	writeJSON(w, http.StatusOK, records)
}

// handleHostDrift diffs two stored runs of one host.
//
// Defaults to "the previous run against the latest", because that is the
// question asked ninety percent of the time — something changed since this
// morning, what was it. ?from= and ?to= take run ids from the history endpoint
// for the other ten percent.
func (s *server) handleHostDrift(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	records, err := s.store.History(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	if len(records) < 2 {
		writeError(w, http.StatusBadRequest,
			"this host has only one stored run, so there is nothing to compare it with")
		return
	}

	// History is newest first.
	toID := records[0].ID
	fromID := records[1].ID
	if v := r.URL.Query().Get("to"); v != "" {
		toID = v
	}
	if v := r.URL.Query().Get("from"); v != "" {
		fromID = v
	}

	to, err := s.store.At(name, toID)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	from, err := s.store.At(name, fromID)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	// Comparing a run against something older than itself would report every
	// change backwards, which is worse than refusing.
	if from.CollectedAt.After(to.CollectedAt) {
		writeError(w, http.StatusBadRequest, "from is newer than to; pass them oldest first")
		return
	}
	writeJSON(w, http.StatusOK, drift.Compare(from, to, fromID, toID))
}

// handleHostFirewallPlan suggests an inbound ruleset from the ports the host
// is actually using.
//
// It generates text and nothing else. `discover` cannot mutate anything and
// this does not change that: no endpoint here applies a firewall, and the plan
// carries its own rollback and a timed-trial recipe precisely because applying
// one is a decision a human makes on a machine they might not be sitting next
// to.
func (s *server) handleHostFirewallPlan(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	inv, err := s.store.Latest(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}

	opts := fwplan.Options{AllowICMP: true}
	if v := r.URL.Query().Get("lan"); v != "" {
		if _, _, err := net.ParseCIDR(v); err != nil {
			writeError(w, http.StatusBadRequest, "lan must be a CIDR like 192.0.2.24/24")
			return
		}
		opts.LAN = v
	}
	if v := r.URL.Query().Get("ssh_port"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			writeError(w, http.StatusBadRequest, "ssh_port must be a port number")
			return
		}
		opts.SSHPort = n
	}

	plan, err := fwplan.Build(inv, opts)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

// handleHostDeps answers "what breaks if I stop this" for one host.
func (s *server) handleHostDeps(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	inv, err := s.store.Latest(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	writeJSON(w, http.StatusOK, deps.Build(inv))
}

// handleHostCompose lists every compose file this host has had, collapsed to
// distinct versions.
//
// The data has been on disk since the agents went live — every inventory
// carries the full text and a sha256 of every compose file it found — and
// nothing read it until now. This is version control for compose files that
// cost nothing to collect.
func (s *server) handleHostCompose(w http.ResponseWriter, r *http.Request) {
	rep, err := s.composeLog(r.PathValue("name"), false)
	if err != nil {
		s.respondLookup(w, r.PathValue("name"), err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// handleHostComposeFile returns one compose file's current content and its
// version list.
//
// Separate from the listing endpoint because content is large — seventeen
// files a run — and a listing that carried every version of every file would
// be megabytes to render a table of dates.
func (s *server) handleHostComposeFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	project := r.URL.Query().Get("project")
	if project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}
	rep, err := s.composeLog(name, true)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	f, ok := rep.Find(project, r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such compose file on this host")
		return
	}
	// Newest first, so [0] is what is on disk now.
	if sha := r.URL.Query().Get("at"); sha != "" {
		v, ok := versionBySHA(f, sha)
		if !ok {
			writeError(w, http.StatusNotFound, "no version with that sha256")
			return
		}
		f.Versions = append([]composelog.Version{v}, f.Versions...)
	}
	writeJSON(w, http.StatusOK, f)
}

// handleHostComposeDiff shows what changed between two versions of one file.
//
// Defaults to the two most recent versions, which is the question actually
// being asked: what did I change last.
func (s *server) handleHostComposeDiff(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	project := r.URL.Query().Get("project")
	if project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}

	rep, err := s.composeLog(name, true)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}
	f, ok := rep.Find(project, r.URL.Query().Get("path"))
	if !ok {
		writeError(w, http.StatusNotFound, "no such compose file on this host")
		return
	}
	if len(f.Versions) < 2 {
		writeError(w, http.StatusBadRequest,
			"this file has only ever had one version, so there is nothing to compare")
		return
	}

	// Versions are newest first, so the default comUTCon is [1] -> [0].
	to, from := f.Versions[0], f.Versions[1]
	if v := r.URL.Query().Get("from"); v != "" {
		from, ok = versionBySHA(f, v)
		if !ok {
			writeError(w, http.StatusNotFound, "no version with that sha256")
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		to, ok = versionBySHA(f, v)
		if !ok {
			writeError(w, http.StatusNotFound, "no version with that sha256")
			return
		}
	}

	unified, added, removed := composelog.Unified(from.Content, to.Content,
		shortLabel(f.Path, from), shortLabel(f.Path, to))
	writeJSON(w, http.StatusOK, composelog.Diff{
		Project: f.Project, Path: f.Path,
		From: stripContent(from), To: stripContent(to),
		Unified: unified, Added: added, Removed: removed,
	})
}

func versionBySHA(f composelog.File, sha string) (composelog.Version, bool) {
	for _, v := range f.Versions {
		if v.SHA256 == sha {
			return v, true
		}
	}
	return composelog.Version{}, false
}

// stripContent keeps a response small: the diff already carries the text that
// matters, and shipping both full versions alongside it doubles the payload
// for no benefit.
func stripContent(v composelog.Version) composelog.Version {
	v.Content = ""
	return v
}

func shortLabel(path string, v composelog.Version) string {
	return fmt.Sprintf("%s @ %s", path, v.LastSeen.Format("2006-01-02 15:04"))
}

// composeLog reads the host's history and groups its compose files.
//
// It samples for the same reason the trend does: the cost grows with history,
// and a file's version list does not get better for having read two thousand
// identical runs. Sampling can in principle miss a version that existed only
// between two samples — a compose file edited and reverted within minutes —
// which is an acceptable trade for an endpoint that would otherwise parse a
// gigabyte.
func (s *server) composeLog(name string, withContent bool) (*composelog.Report, error) {
	records, err := s.store.History(name)
	if err != nil {
		return nil, err
	}
	sampled := sampleRecords(records, maxComposeSamples)
	// Oldest first, so first-seen and last-seen read naturally.
	invs := make([]*inventory.Inventory, 0, len(sampled))
	ids := make([]string, 0, len(sampled))
	for i := len(sampled) - 1; i >= 0; i-- {
		inv, err := s.store.At(name, sampled[i].ID)
		if err != nil {
			s.log.Printf("compose: %s at %s: %v", name, sampled[i].ID, err)
			continue
		}
		invs = append(invs, inv)
		ids = append(ids, sampled[i].ID)
	}
	if len(invs) == 0 {
		return nil, store.ErrNotFound
	}
	return composelog.Build(invs, ids, withContent), nil
}

// maxComposeSamples bounds how many runs a compose history reads.
const maxComposeSamples = 300

// maxTrendSamples bounds how many stored runs a trend reads.
//
// This is the only endpoint whose cost grows with history, and it grows fast:
// the agents post every 15 minutes, so host-a accumulates ~96 runs a day at
// ~350 KiB each — a gigabyte a month, and every byte of it parsed. A capacity
// line does not get better past a couple of hundred points, so the history is
// sampled evenly instead of read whole. The first and last runs are always
// included, because they set the window the forecast is fitted over.
const maxTrendSamples = 200

// handleHostTrend is capacity over time for one host, with a forecast when the
// history can support one.
func (s *server) handleHostTrend(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	records, err := s.store.History(name)
	if err != nil {
		s.respondLookup(w, name, err)
		return
	}

	// One unreadable run is a gap in a line, not a failed request.
	onErr := func(id string, err error) {
		s.log.Printf("trend: %s at %s: %v", name, id, err)
	}
	rep, err := s.trends.Report(s.store, name, recordIDs(records), recordIDs(sampleRecords(records, maxTrendSamples)), time.Now(), onErr)
	if errors.Is(err, trend.ErrNoRuns) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("no readable runs for host %q", name))
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func recordIDs(records []store.Record) []string {
	ids := make([]string, len(records))
	for i, rec := range records {
		ids[i] = rec.ID
	}
	return ids
}

// sampleRecords picks at most n records spread evenly across the history,
// always keeping the oldest and newest.
func sampleRecords(records []store.Record, n int) []store.Record {
	if n <= 0 || len(records) <= n {
		return records
	}
	out := make([]store.Record, 0, n)
	// records is newest first; step through by a fractional stride so the
	// samples are evenly spaced in position rather than clustered.
	stride := float64(len(records)-1) / float64(n-1)
	for i := 0; i < n; i++ {
		out = append(out, records[int(float64(i)*stride+0.5)])
	}
	return out
}

// handleImages reports what every container is running and how firmly it is
// pinned there. It matters here because Watchtower is running: an image on
// :latest can change without anybody asking.
func (s *server) handleImages(w http.ResponseWriter, r *http.Request) {
	invs, err := s.store.LatestAll()
	if err != nil {
		s.log.Printf("images: %v", err)
		writeError(w, http.StatusInternalServerError, "loading inventories")
		return
	}
	rep := images.Build(invs)
	if host := r.URL.Query().Get("host"); host != "" {
		rep = rep.ForHost(host)
	}
	writeJSON(w, http.StatusOK, rep)
}

// handlePorts is portmap.Build over every host's latest inventory — the
// "prove the ingest pipeline is worth something" endpoint. It returns exactly
// the struct `portmap -json` prints, so a web UI and the CLI stay in sync for
// free.
func (s *server) handlePorts(w http.ResponseWriter, r *http.Request) {
	invs, err := s.store.LatestAll()
	if err != nil {
		s.log.Printf("ports: %v", err)
		writeError(w, http.StatusInternalServerError, "loading inventories")
		return
	}

	pm := portmap.Build(invs)
	if host := r.URL.Query().Get("host"); host != "" {
		pm = pm.FilterHost(host)
		if len(pm.Hosts) == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no inventory for host %q", host))
			return
		}
	}
	writeJSON(w, http.StatusOK, pm)
}

// handleAlerts evaluates every threshold rule over each host's latest run.
//
// Same shape as handlePorts, and for the same reason: the rules live in
// internal/alert so the CLI, this endpoint and a future dashboard share one
// definition of "what is wrong" rather than three that drift.
func (s *server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	invs, err := s.store.LatestAll()
	if err != nil {
		s.log.Printf("alerts: %v", err)
		writeError(w, http.StatusInternalServerError, "loading inventories")
		return
	}

	now := time.Now()
	rep, err := s.evaluateAlerts(invs, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "loading disk health evidence")
		return
	}
	// Stamped after evaluation, never inside a rule: a silence changes how
	// loudly something is reported, not whether it was found.
	book, err := s.silences()
	if err != nil {
		s.log.Printf("alerts: reading silences: %v", err)
	}
	silencesOK := err == nil
	rep = silence.Apply(rep, &book, now)

	// How long each condition has been true. Stamped after evaluation for the
	// same reason silences are: a rule says what is true, not how long for.
	if ledger, err := s.seenBook(); err == nil {
		if ledger.Stamp(rep.Alerts, now) {
			if err := s.store.WriteState(seen.StateName, ledger); err != nil {
				s.log.Printf("alerts: saving the age ledger: %v", err)
			}
		}
	}

	var triageBook *silence.Book
	if silencesOK {
		triageBook = &book
	}
	review := triage.Build(invs, rep.Alerts, triageBook, now)

	if host := r.URL.Query().Get("host"); host != "" {
		rep = rep.FilterHost(host)
		if len(rep.Hosts) == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("no inventory for host %q", host))
			return
		}
	}
	if lvl := r.URL.Query().Get("level"); lvl != "" {
		min, ok := alert.ParseLevel(lvl)
		if !ok {
			writeError(w, http.StatusBadRequest, "level must be one of: critical, warning, info")
			return
		}
		rep = rep.FilterLevel(min)
	}
	writeJSON(w, http.StatusOK, struct {
		*alert.Report
		Triage triage.Report `json:"triage"`
	}{rep, review})
}

// handleServices is the launcher: everything listening, reconciled against the
// curated Book in the store.
//
// It may write, which is unusual for a GET and is deliberate. First sight of a
// service is what records when it was first seen, and that has to happen on the
// read path because nothing else notices. catalog.Build reports whether
// anything actually changed, so a steady-state poll writes nothing.
func (s *server) handleServices(w http.ResponseWriter, r *http.Request) {
	cat, _, err := s.catalog()
	if err != nil {
		s.log.Printf("services: %v", err)
		writeError(w, http.StatusInternalServerError, "building the service catalog")
		return
	}
	writeJSON(w, http.StatusOK, cat)
}

// networkLookback is how many older runs withLastNetwork reads for a host whose
// latest run has no port list: about two hours at the 15-minute cadence.
const networkLookback = 8

// withLastNetwork fills in the port list of a host whose latest run failed to
// collect one (ss timed out, say) from its most recent run that has one. One
// failed scan must not mark every service on the host as gone.
func (s *server) withLastNetwork(invs []*inventory.Inventory) []*inventory.Inventory {
	out := make([]*inventory.Inventory, len(invs))
	for i, inv := range invs {
		out[i] = inv
		if inv == nil || (inv.Network != nil && !inv.NetworkFailed()) {
			continue
		}
		records, err := s.store.History(inv.Host.Hostname)
		if err != nil || len(records) < 2 {
			continue
		}
		for _, r := range records[1:min(len(records), networkLookback+1)] {
			old, err := s.store.At(r.Host, r.ID)
			if err != nil || old.Network == nil || old.NetworkFailed() {
				continue
			}
			patched := *inv
			patched.Network = old.Network
			out[i] = &patched
			break
		}
	}
	return out
}

// catalog builds the launcher and persists the Book when the build changed it.
func (s *server) catalog() (*catalog.Catalog, catalog.Book, error) {
	invs, err := s.store.LatestAll()
	if err != nil {
		return nil, catalog.Book{}, err
	}
	invs = s.withLastNetwork(invs)
	var book catalog.Book
	if err := s.store.ReadState(catalogStateName, &book); err != nil {
		// A corrupt Book must not take the home page down with it: fall back to
		// an empty one, which renders everything as new rather than as nothing.
		s.log.Printf("services: reading the catalog book: %v", err)
		book = catalog.Book{}
	}
	cat, next, changed := catalog.Build(invs, book, time.Now())
	if changed {
		if err := s.store.WriteState(catalogStateName, next); err != nil {
			// A line in the log, but not worth failing the page for: everything
			// still renders, it just gets offered as new again next time.
			s.log.Printf("services: saving the catalog book: %v", err)
		}
	}
	return cat, next, nil
}

// handleCurateService records one human decision about one service: its name,
// its category, where it really lives, or that it does not belong on the page.
//
// Being able to set a URL from here is the difference between working and not.
// The derived link is a LAN address, which is exactly wrong when the dashboard
// is read from a phone off the network, and the public hostname a reverse proxy
// serves a service on is not discoverable from any inventory — nothing on the
// machine knows it. So a human has to say, from the device they are holding.
func (s *server) handleCurateService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	r.Body = http.MaxBytesReader(w, r.Body, maxCurationBytes)

	var in catalog.Stored
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "decoding: "+err.Error())
		return
	}

	book, err := s.readBook()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading the catalog book")
		return
	}
	if err := book.Curate(key, in, time.Now()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.WriteState(catalogStateName, book); err != nil {
		s.log.Printf("services: %v", err)
		writeError(w, http.StatusInternalServerError, "saving the catalog book")
		return
	}
	cat, _, _ := s.catalog()
	writeJSON(w, http.StatusOK, cat)
}

// handleForgetService drops an entry entirely, and is the only thing that
// removes one. See the package comment in internal/catalog for why a service
// vanishing from the inventory is never enough on its own.
func (s *server) handleForgetService(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if _, _, ok := catalog.SplitKey(key); !ok {
		writeError(w, http.StatusBadRequest, "not a valid service key (want host:port)")
		return
	}
	book, err := s.readBook()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading the catalog book")
		return
	}
	book.Forget(key, time.Now())
	if err := s.store.WriteState(catalogStateName, book); err != nil {
		s.log.Printf("services: %v", err)
		writeError(w, http.StatusInternalServerError, "saving the catalog book")
		return
	}
	cat, _, _ := s.catalog()
	writeJSON(w, http.StatusOK, cat)
}

// handleAcceptServices confirms every new service at once, with whatever the
// seed table guessed. The first run has thirty-odd of them, and naming each by
// hand before the page is usable would be absurd.
func (s *server) handleAcceptServices(w http.ResponseWriter, r *http.Request) {
	cat, book, err := s.catalog()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "building the service catalog")
		return
	}
	n := book.AcceptAll(cat, time.Now())
	if n > 0 {
		if err := s.store.WriteState(catalogStateName, book); err != nil {
			s.log.Printf("services: %v", err)
			writeError(w, http.StatusInternalServerError, "saving the catalog book")
			return
		}
	}
	fresh, _, _ := s.catalog()
	writeJSON(w, http.StatusOK, map[string]any{"accepted": n, "catalog": fresh})
}

func (s *server) readBook() (catalog.Book, error) {
	var book catalog.Book
	if err := s.store.ReadState(catalogStateName, &book); err != nil {
		s.log.Printf("services: reading the catalog book: %v", err)
		return catalog.Book{}, err
	}
	return book, nil
}

// handleIcon serves a launcher icon from the server's own cache.
//
// The dashboard loads icons from here rather than from a CDN so that the page
// keeps loading nothing external: it works offline once an icon is cached, and
// no browser ever tells an icon repository which services this homelab runs.
//
// A 404 is an ordinary answer, not an error — the UI falls back to initials,
// and most fleets will have several services no repository has a logo for.
func (s *server) handleIcon(w http.ResponseWriter, r *http.Request) {
	if s.icons == nil {
		http.NotFound(w, r)
		return
	}
	slug := r.PathValue("slug")

	// Only fetch a slug some service actually asks for. Slug normalisation
	// already makes the outbound URL harmless — it can only ever be
	// [a-z0-9-] interpolated into a fixed template on a known host — but
	// without this check any caller could walk the alphabet and have the
	// server make an outbound request, and cache a miss, for every string it
	// tried. Serving something already cached needs no such check.
	if !s.icons.Cached(slug) && !s.iconIsWanted(slug) {
		http.NotFound(w, r)
		return
	}

	ct, data, ok := s.icons.Get(r.Context(), slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	// An SVG can carry script. The dashboard renders these in an <img>, which
	// disables scripting outright, but these headers mean the same file is
	// still inert if it is opened directly in a tab.
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; sandbox")
	// Icons never change under a slug, so let the browser keep them.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// iconIsWanted reports whether any service in the catalog references this
// slug. It is what bounds icon fetching to the services this fleet runs.
func (s *server) iconIsWanted(slug string) bool {
	slug = icons.Normalise(slug)
	if slug == "" {
		return false
	}
	cat, _, err := s.catalog()
	if err != nil {
		return false
	}
	lists := [][]catalog.Service{cat.New, cat.Missing, cat.Suppressed}
	for _, g := range cat.Groups {
		lists = append(lists, g.Services)
	}
	for _, l := range lists {
		for _, svc := range l {
			if svc.IconSlug == slug {
				return true
			}
		}
	}
	return false
}

// maxCurationBytes bounds one curation. It is a name, a category and a URL.
const maxCurationBytes = 16 << 10

// catalogStateName is where the curated launcher catalog is stored.
const catalogStateName = "catalog"

// seenBook reads the age ledger.
func (s *server) seenBook() (*seen.Book, error) {
	var b seen.Book
	if err := s.store.ReadState(seen.StateName, &b); err != nil {
		s.log.Printf("alerts: reading the age ledger: %v", err)
		return &seen.Book{}, err
	}
	return &b, nil
}

// digestLoop sends a periodic summary. It checks often and sends rarely: the
// interval is a week, and a process restarted twice a day must not miss it.
func (s *server) digestLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		eff := s.effective()
		// Re-read every tick rather than captured once: the interval is a
		// setting now, and zero means the digest is off.
		every := eff.DigestInterval
		if every <= 0 || !eff.Sending() {
			continue
		}
		now := time.Now()
		due, err := s.notifier.DigestDue(every, now)
		if err != nil {
			s.log.Printf("digest: %v", err)
			continue
		}
		if !due {
			continue
		}
		d, err := s.buildDigest(now, every)
		if err != nil {
			s.log.Printf("digest: %v", err)
			continue
		}
		err = s.notifier.SendDigest(ctx, d)
		s.notifier.Recorder.Record(notify.Delivery{
			At: now, Title: "homelab: weekly digest", Type: "info", Kind: "digest",
			OK: err == nil, Error: errText(err),
		})
	}
}

// buildDigest assembles the summary from the same views the dashboard uses, so
// it cannot disagree with what the page says.
func (s *server) buildDigest(now time.Time, every time.Duration) (notify.Digest, error) {
	invs, err := s.store.LatestAll()
	if err != nil {
		return notify.Digest{}, err
	}
	rep, err := s.evaluateAlerts(invs, now)
	if err != nil {
		return notify.Digest{}, err
	}
	if book, err := s.silences(); err == nil {
		rep = silence.Apply(rep, &book, now)
	}
	if ledger, err := s.seenBook(); err == nil {
		ledger.Stamp(rep.Alerts, now)
	}

	d := notify.Digest{
		Since: now.Add(-every), Now: now,
		Counts: rep.Counts, Silenced: rep.Silenced, Hosts: rep.Hosts,
		// A day is the threshold for "still true after a while": anything
		// younger was probably already notified as a transition this week.
		Longstanding: notify.LongstandingFrom(rep.Alerts, 24*time.Hour, now, 8),
	}

	// Capacity, and only where it moved enough to be worth a line.
	for _, inv := range invs {
		if inv.ZFS == nil || !inv.ZFS.Available {
			continue
		}
		for _, p := range inv.ZFS.Pools {
			if p.CapacityPct >= 80 {
				d.Notes = append(d.Notes, fmt.Sprintf("pool %s on %s is at %d%%",
					p.Name, inv.Host.Hostname, p.CapacityPct))
			}
		}
	}

	// Images on a moving tag, which is the thing most likely to have changed
	// under you between digests.
	img := images.Build(invs)
	if img.Counts.Floating > 0 {
		d.Notes = append(d.Notes, fmt.Sprintf("%d of %d containers still run a moving image tag",
			img.Counts.Floating, img.Counts.Total))
	}

	// Services the launcher has noticed and nobody has confirmed or dismissed.
	if cat, _, err := s.catalog(); err == nil {
		if n := len(cat.New); n > 0 {
			d.Notes = append(d.Notes, fmt.Sprintf("%d new service(s) waiting to be named on the dashboard", n))
		}
		if n := len(cat.Missing); n > 0 {
			d.Notes = append(d.Notes, fmt.Sprintf("%d service(s) have stopped listening", n))
		}
	}
	return d, nil
}

// silences reads the silence book, dropping anything that has lapsed.
//
// Pruning on read is safe here in a way that pruning the service catalog is
// not: an expiry is an instruction a human gave, so acting on it is carrying
// out that instruction, whereas a service missing from an inventory might just
// be a failed collection.
func (s *server) silences() (silence.Book, error) {
	var book silence.Book
	if err := s.store.ReadState(silence.StateName, &book); err != nil {
		return silence.Book{}, err
	}
	if book.Prune(time.Now()) {
		if err := s.store.WriteState(silence.StateName, book); err != nil {
			s.log.Printf("silences: pruning: %v", err)
		}
	}
	return book, nil
}

func (s *server) handleListSilences(w http.ResponseWriter, r *http.Request) {
	book, err := s.silences()
	if err != nil {
		s.log.Printf("silences: %v", err)
		writeError(w, http.StatusInternalServerError, "reading silences")
		return
	}
	writeJSON(w, http.StatusOK, book.List(time.Now()))
}

// handleAddSilence acknowledges one condition. The body carries the same
// Host + Rule + Subject triple that identifies an alert everywhere else, plus
// an optional reason and an optional duration.
func (s *server) handleAddSilence(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCurationBytes)

	var in struct {
		Host    string `json:"host"`
		Rule    string `json:"rule"`
		Subject string `json:"subject"`
		Reason  string `json:"reason"`
		// For is a Go duration ("24h", "168h"). Empty means indefinite, which
		// is legitimate but is the answer the UI should ask twice about.
		For string `json:"for"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "decoding: "+err.Error())
		return
	}

	now := time.Now()
	sil := silence.Silence{Host: in.Host, Rule: in.Rule, Subject: in.Subject, Reason: in.Reason}
	if in.For != "" {
		d, err := time.ParseDuration(in.For)
		if err != nil || d <= 0 {
			writeError(w, http.StatusBadRequest, `"for" must be a positive duration like "24h"`)
			return
		}
		until := now.Add(d)
		sil.Until = &until
	}

	book, err := s.silences()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading silences")
		return
	}
	added, err := book.Add(sil, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.WriteState(silence.StateName, book); err != nil {
		s.log.Printf("silences: %v", err)
		writeError(w, http.StatusInternalServerError, "saving silences")
		return
	}
	s.log.Printf("silenced: %s", silence.Describe(added))
	writeJSON(w, http.StatusCreated, added)
}

func (s *server) handleRemoveSilence(w http.ResponseWriter, r *http.Request) {
	book, err := s.silences()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reading silences")
		return
	}
	if !book.Remove(r.PathValue("id"), time.Now()) {
		writeError(w, http.StatusNotFound, "no such silence")
		return
	}
	if err := s.store.WriteState(silence.StateName, book); err != nil {
		s.log.Printf("silences: %v", err)
		writeError(w, http.StatusInternalServerError, "saving silences")
		return
	}
	writeJSON(w, http.StatusOK, book.List(time.Now()))
}

// handleNotifyStatus reports whether notifications are configured and what
// they have done lately. It never returns the apprise URL: the key in its path
// is a credential, and an endpoint that echoes it back would undo keeping it
// out of git.
func (s *server) handleNotifyStatus(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeJSON(w, http.StatusOK, notify.Status{Enabled: false})
		return
	}
	writeJSON(w, http.StatusOK, s.notifier.Status())
}

// handleNotifyTest sends one notification so a deployment can be verified
// end-to-end without waiting for something to break. It deliberately does not
// touch the stored state — a test must not be able to mark a real alert as
// already reported.
func (s *server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	eff := s.effective()
	if s.notifier == nil || eff.AppriseURL == "" {
		writeError(w, http.StatusServiceUnavailable,
			"no apprise endpoint is set — add one in Admin, or set APPRISE_URL")
		return
	}
	// A test is sent even when notifications are muted. Muting says "do not
	// tell me about alerts"; pressing Test is an explicit request, and a test
	// that silently did nothing would be indistinguishable from a broken one,
	// which is the confusion this whole endpoint exists to remove.
	msg := notify.Message{
		Title: "homelab: test notification",
		Body: "Sent from the Admin tab. If you are reading this, the API can " +
			"reach apprise and the key is right.",
		Type: "info",
	}
	now := time.Now()
	if err := s.notifier.Sender.Send(r.Context(), msg); err != nil {
		s.log.Printf("notify test: %v", err)
		s.notifier.Recorder.Record(notify.Delivery{
			At: now, Title: msg.Title, Type: msg.Type, Kind: "test",
			OK: false, Error: err.Error(),
		})
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.notifier.Recorder.Record(notify.Delivery{
		At: now, Title: msg.Title, Type: msg.Type, Kind: "test", OK: true,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

func (s *server) respondLookup(w http.ResponseWriter, host string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, fmt.Sprintf("no inventory for host %q", host))
	case errors.Is(err, store.ErrInvalidID):
		writeError(w, http.StatusBadRequest, "at must be a run id from GET /hosts/{name}/history")
	default:
		s.log.Printf("lookup %s: %v", host, err)
		writeError(w, http.StatusInternalServerError, "loading inventory")
	}
}

// errText renders an error for the delivery log, empty when there was none.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// withLogging logs one line per request: method, path, status, duration.
func withLogging(logger *log.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		logger.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

// Let ResponseController reach the HTTP server's deadline controls.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}
