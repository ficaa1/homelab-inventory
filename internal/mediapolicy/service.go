package mediapolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

type Config struct {
	Seeding        SeedConfig         `json:"seeding"`
	Archive        archivetier.Config `json:"archive"`
	ArchiveEnabled bool               `json:"archiveEnabled"`
}

func DefaultConfig() Config {
	return Config{Seeding: DefaultSeedConfig(), Archive: archivetier.DefaultConfig()}
}
func (c Config) Validate() error {
	if err := c.Seeding.Validate(); err != nil {
		return err
	}
	if c.ArchiveEnabled {
		return c.Archive.Validate()
	}
	return nil
}

type ArchiveAPI interface {
	CheckAdmission(context.Context, time.Time, archivetier.Admission) (archivetier.State, error)
}

type Status struct {
	ArchivePaused      bool              `json:"archivePaused"`
	ArchiveSearches    []ArchiveSearch   `json:"archiveSearches,omitempty"`
	SelectionWarning   string            `json:"selectionWarning,omitempty"`
	Seeding            *SeedJob          `json:"seeding,omitempty"`
	SeedingQueue       []SeedQueueEntry  `json:"seedingQueue"`
	SeedingResults     []SeedQueueResult `json:"seedingQueueResults"`
	Service            string            `json:"service"`
	ArchiveEnabled     bool              `json:"archiveEnabled"`
	DryRun             bool              `json:"dryRun"`
	Healthy            bool              `json:"healthy"`
	ReadHealthy        bool              `json:"readHealthy"`
	EnforcementHealthy bool              `json:"enforcementHealthy"`
	Checked            time.Time         `json:"checked"`
	Error              string            `json:"error,omitempty"`
	Archive            archivetier.State `json:"archive"`
	Leases             map[string]Lease  `json:"leases"`
	// ArchiveInfo is the archive's schedule, budgets and disk headroom as
	// of the request, for the dashboard.
	ArchiveInfo *ArchiveInfo `json:"archiveInfo,omitempty"`
}

// Service serializes archive policy passes; title leases extend exclusion from
// a selected archive release through download, import, verification and failure.
type Service struct {
	seedWG sync.WaitGroup
	// seedQueueRunning is guarded by mu: at most one queue drainer.
	seedQueueRunning bool
	seedLive         seedObservation
	Seeding          *SeedMover
	LibraryClients   map[string]*archivetier.Client
	libraryMu        sync.Mutex
	libraryCache     Library
	Store            *Store
	Archive          ArchiveAPI
	Config           Config
	DryRun           bool
	Interval         time.Duration
	// DownloadFree reads the download disk's free bytes once per cycle.
	DownloadFree func() (int64, error)
	// ActionWait is how long an operator action waits for a running cycle
	// to finish before answering busy; default 20s.
	ActionWait time.Duration
	mu         sync.Mutex
	status     Status
	tick       sync.Mutex
	wake       chan struct{}
	runNow     RunNow
	disk       diskReading
}

// Tick reconciles archive work before processing optional searches.
func (s *Service) Tick(ctx context.Context, now time.Time) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	return s.tickLocked(ctx, now, false)
}

// RunNowTick is the operator's "Check now": discovery can run any time,
// while automatic grabs keep their nightly window. Ready imports run any time. It is a no-op
// unless a run was requested or an approval is pending.
func (s *Service) RunNowTick(ctx context.Context, now time.Time) error {
	s.tick.Lock()
	defer s.tick.Unlock()
	s.mu.Lock()
	if !s.runNow.Pending && !s.Store.Snapshot().Archive.HasRunnableApproval() {
		s.mu.Unlock()
		return nil
	}
	s.runNow.Pending, s.runNow.StartedAt = false, now
	s.mu.Unlock()
	err := s.tickLocked(ctx, now, true)
	st := s.Store.Snapshot().Archive
	result := "cycle finished"
	switch {
	case err != nil:
		result = err.Error()
	case st.Job != nil && st.Job.Phase != archivetier.Complete && st.Job.Phase != archivetier.Abandoned && st.Job.Phase != archivetier.Failed:
		result = "job " + string(st.Job.Phase)
		if st.Paused != "" {
			result += ": " + st.Paused
		}
	case st.Paused != "":
		result = st.Paused
	}
	s.mu.Lock()
	s.runNow.FinishedAt, s.runNow.Result = time.Now().UTC(), result
	s.mu.Unlock()
	s.Store.AuditDetail("run_now_finished", st.Job, map[string]any{"result": result, "dryRun": s.DryRun})
	return err
}

func (s *Service) tickLocked(ctx context.Context, now time.Time, runNow bool) error {
	selectionErr := s.syncSelection()
	var archiveErr error
	archive := s.Config.ArchiveEnabled && s.Archive != nil
	if archive {
		adm := archivetier.Admission{NewWrites: true}
		switch {
		case s.Store.Snapshot().ArchivePaused:
			adm = archivetier.Admission{Reason: "Paused by owner"}
		case selectionErr != nil:
			adm = archivetier.Admission{Reason: "invalid selection settings"}
		case s.Store.WriteAllowed() != nil:
			adm = archivetier.Admission{Reason: "state persistence failed"}
		}
		adm.CheckNow = runNow
		_, archiveErr = s.Archive.CheckAdmission(ctx, now, adm)
		s.readDisk(now)
	}
	// Reconcile existing work before optional searches.
	if selectionErr == nil {
		s.drainArchiveSearches(ctx)
	}
	persisted := s.Store.Snapshot().Archive
	if len(persisted.FailedJobs) > 0 && archiveErr == nil {
		archiveErr = fmt.Errorf("archive recovery pending: %d titles", len(persisted.FailedJobs))
	}
	if persisted.Job != nil && persisted.Job.Phase != archivetier.Failed && persisted.Job.Phase != archivetier.Complete && persisted.Job.Phase != archivetier.Abandoned && !archive && archiveErr == nil {
		archiveErr = fmt.Errorf("archive disabled with an unfinished %s job; it is neither verified nor timed out", persisted.Job.Phase)
	}
	err := errors.Join(archiveErr, selectionErr)
	if e := s.Store.Finish(now, err); e != nil {
		err = errors.Join(err, e)
	}
	st := s.Store.Snapshot()
	view := Status{Service: "mediapolicy", DryRun: s.DryRun, ArchiveEnabled: s.Config.ArchiveEnabled, Checked: now, Archive: st.Archive, Leases: st.Leases, Healthy: err == nil, ReadHealthy: archiveErr == nil, EnforcementHealthy: err == nil}
	if err != nil {
		view.Error = err.Error()
	}
	if !archivetier.Healthy(st.Archive) {
		view.Healthy = false
		view.EnforcementHealthy = false
	}
	s.mu.Lock()
	s.status = view
	s.mu.Unlock()
	return err
}
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	view := clone(s.status)
	snap := s.Store.Snapshot()
	view.ArchivePaused = snap.ArchivePaused
	view.ArchiveSearches = snap.ArchiveSearches
	settings, _ := s.selectionSettings()
	view.SelectionWarning = settings.Warning
	view.Seeding = s.withSeedLive(snap.Seeding)
	view.SeedingQueue, view.SeedingResults = seedQueueView(snap)
	view.Leases = snap.Leases
	view.Archive = snap.Archive
	if !archivetier.Healthy(snap.Archive) {
		view.Healthy = false
		view.EnforcementHealthy = false
	}
	if view.Seeding != nil && view.Seeding.Phase == "halted" {
		view.Healthy = false
		view.EnforcementHealthy = false
	}
	oldMessage := ""
	if view.Archive.Job != nil {
		oldMessage = view.Archive.Job.Message
	}
	view.Archive = archivetier.PublicState(view.Archive)
	if oldMessage != "" && view.Archive.Job != nil && !view.Archive.Job.HasImportIntent() {
		view.Error = strings.ReplaceAll(view.Error, oldMessage, view.Archive.Job.Message)
	}
	return view
}
func (s *Service) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	for _, path := range []string{"/archive/pause", "/archive/job/approve", "/archive/job/drop", "/archive/search/reorder", "/archive/approvals/reorder", "/seeding/stop", "/seeding/queue/reorder"} {
		mux.HandleFunc("POST "+path, s.handleOwnerControl)
	}
	mux.HandleFunc("GET /archive/selection", s.handleSelection)
	mux.HandleFunc("PUT /archive/selection", s.handleSelection)
	mux.HandleFunc("POST /archive/selection/preview", s.handleSelection)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /seeding", s.handleSeeding)
	mux.HandleFunc("POST /seeding/preview", s.handleSeeding)
	mux.HandleFunc("POST /seeding/move", s.handleSeeding)
	mux.HandleFunc("POST /seeding/queue", s.handleSeedQueue)
	mux.HandleFunc("POST /seeding/queue/cancel", s.handleSeedQueue)
	mux.HandleFunc("GET /library", s.handleLibrary)
	mux.HandleFunc("POST /archive/tag", s.handleTag)
	mux.HandleFunc("GET /healthz", s.handleStatus)
	mux.HandleFunc("POST /archive/run", s.handleRunNow)
	mux.HandleFunc("POST /archive/clear", s.handleClear)
	mux.HandleFunc("POST /archive/approve", s.handleApprove)
	mux.HandleFunc("POST /archive/search", s.handleSearch)
	mux.HandleFunc("POST /archive/restore", s.handleRestore)
	mux.HandleFunc("POST /archive/purge", s.handlePurge)
	mux.HandleFunc("POST /archive/purge-all", s.handlePurgeAll)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodPost && !(r.Method == http.MethodPut && r.URL.Path == "/archive/selection") {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !httpx.BearerOK(r, token, httpx.DenyWhenUnset) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if ct != "application/json" {
				writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type must be application/json"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/status" && r.URL.Query().Get("refresh") == "progress" {
		s.refreshProgress(r.Context())
	}
	view := s.Status()
	stale := view.Checked.IsZero() || time.Since(view.Checked) > max(3*s.Interval, 10*time.Minute)
	view.Healthy = view.Healthy && !stale && s.Store.WriteAllowed() == nil
	if s.Config.ArchiveEnabled {
		view.ArchiveInfo = s.archiveInfo(time.Now().UTC(), view.Archive)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && !view.Healthy {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(view)
}
