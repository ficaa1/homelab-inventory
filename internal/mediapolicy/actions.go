package mediapolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// RunNow is the state of the operator's last "run now" request.
type RunNow struct {
	Pending     bool      `json:"pending"`
	RequestedAt time.Time `json:"requestedAt,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
	Result      string    `json:"result,omitempty"`
}

type diskReading struct {
	free int64
	err  string
	at   time.Time
}

// ArchiveInfo is what the dashboard needs beyond the archive state: the
// window, the next cycle, the budgets and the download disk's headroom.
type ArchiveInfo struct {
	Timezone             string    `json:"timezone"`
	WindowOpen           bool      `json:"windowOpen"`
	WindowStart          time.Time `json:"windowStart"`
	WindowEnd            time.Time `json:"windowEnd"`
	NextCycle            time.Time `json:"nextCycle,omitempty"`
	IntervalSeconds      int       `json:"intervalSeconds"`
	DownloadFreeBytes    int64     `json:"downloadFreeBytes,omitempty"`
	DownloadFreeError    string    `json:"downloadFreeError,omitempty"`
	DownloadChecked      time.Time `json:"downloadChecked,omitempty"`
	DownloadMinFreeBytes int64     `json:"downloadMinFreeBytes"`
	SearchesUsed         int       `json:"searchesUsed"`
	SearchesPerMinute    int       `json:"searchesPerMinute"`
	SearchesLastMinute   int       `json:"searchesLastMinute"`
	MinScore             float64   `json:"minScore"`
	IdleDays             int       `json:"idleDays"`
	RestoreConfigured    bool      `json:"restoreConfigured"`
	QuarantineConfigured bool      `json:"quarantineConfigured"`
	UndoDays             int       `json:"undoDays"`
	ProgressSeconds      int       `json:"progressSeconds"`
	RunNow               RunNow    `json:"runNow"`
}

// ArchiveActions is what the archive runner offers the operator besides its
// cycle. *archivetier.Runner implements it.
type ArchiveActions interface {
	RestoreJob(ctx context.Context, now time.Time, id string, apply bool, hash string) (archivetier.ActionPlan, error)
	PurgeJob(ctx context.Context, now time.Time, id string, apply bool, hash, confirm string) (archivetier.ActionPlan, error)
	RefreshProgress(ctx context.Context, now time.Time) (archivetier.State, error)
}

func (s *Service) actions() ArchiveActions {
	if !s.Config.ArchiveEnabled {
		return nil
	}
	a, _ := s.Archive.(ArchiveActions)
	return a
}

// Wake signals the daemon loop that a "run now" is waiting.
func (s *Service) Wake() <-chan struct{} { return s.wakeChan() }

func (s *Service) wakeChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
	return s.wake
}

func (s *Service) readDisk(now time.Time) {
	if s.DownloadFree == nil {
		return
	}
	free, err := s.DownloadFree()
	d := diskReading{free: free, at: now}
	if err != nil {
		d = diskReading{err: "download disk free space unknown", at: now}
	}
	s.mu.Lock()
	s.disk = d
	s.mu.Unlock()
}

func (s *Service) archiveInfo(now time.Time, st archivetier.State) *ArchiveInfo {
	c := s.Config.Archive
	if settings, err := s.selectionSettings(); err == nil {
		c.Selection = settings.Effective
	}
	open, start, end := c.WindowBounds(now)
	_, night := c.Window(now)
	info := &ArchiveInfo{Timezone: c.Timezone, WindowOpen: open, WindowStart: start, WindowEnd: end, IntervalSeconds: int(s.Interval / time.Second),
		DownloadMinFreeBytes: int64(c.DownloadMinFreeGB * float64(archivetier.GB)), SearchesPerMinute: c.SearchesPerMinute, SearchesLastMinute: st.SearchesLastMinute(now),
		MinScore: c.Selection.MinScore, IdleDays: c.IdleDays, RestoreConfigured: len(c.RestoreMounts) > 0, QuarantineConfigured: c.QuarantineDir != "",
		UndoDays: archivetier.UndoDays, ProgressSeconds: int(archivetier.ProgressInterval / time.Second)}
	if st.SearchNight == night {
		info.SearchesUsed = st.SearchesUsed
	}
	if st.Night == night {
	}
	s.mu.Lock()
	if !s.status.Checked.IsZero() && s.Interval > 0 {
		info.NextCycle = s.status.Checked.Add(s.Interval)
	}
	info.DownloadFreeBytes, info.DownloadFreeError, info.DownloadChecked = s.disk.free, s.disk.err, s.disk.at
	info.RunNow = s.runNow
	s.mu.Unlock()
	return info
}

const archiveBusyMessage = "Busy, try again shortly"

// lockTick waits for a running cycle to finish, up to ActionWait.
func (s *Service) lockTick(ctx context.Context) bool {
	wait := s.ActionWait
	if wait <= 0 {
		wait = 20 * time.Second
	}
	deadline := time.Now().Add(wait)
	for !s.tick.TryLock() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}

// publish refreshes the served archive view after an action or refresh.
func (s *Service) publish() {
	st := s.Store.Snapshot()
	s.mu.Lock()
	s.status.Archive, s.status.Leases = st.Archive, st.Leases
	s.mu.Unlock()
}

// refreshProgress is the dashboard's light refresh: skipped while a cycle
// runs (the cycle refreshes anyway) and throttled by the runner.
func (s *Service) refreshProgress(ctx context.Context) {
	a := s.actions()
	if a == nil || !s.tick.TryLock() {
		return
	}
	defer s.tick.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := a.RefreshProgress(ctx, time.Now().UTC()); err == nil {
		s.publish()
	}
}

type actionRequest struct {
	Approval    *archivetier.ApprovalRequest `json:"approval,omitempty"`
	Title       *archivetier.ApprovalRequest `json:"title,omitempty"`
	ID          string                       `json:"id"`
	Apply       bool                         `json:"apply"`
	Hash        string                       `json:"hash"`
	Confirm     string                       `json:"confirm"`
	RequestedBy string                       `json:"requestedBy"`
}

func readAction(w http.ResponseWriter, r *http.Request) (actionRequest, bool) {
	var in actionRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return in, false
	}
	if len(in.RequestedBy) > 200 {
		in.RequestedBy = in.RequestedBy[:200]
	}
	return in, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// RequestRun queues one "run now" cycle for the daemon loop.
func (s *Service) RequestRun(by string) (RunNow, error) {
	if !s.Config.ArchiveEnabled || s.Archive == nil {
		return RunNow{}, fmt.Errorf("archive is disabled")
	}
	s.mu.Lock()
	already := s.runNow.Pending
	if !already {
		s.runNow = RunNow{Pending: true, RequestedAt: time.Now().UTC()}
	}
	out := s.runNow
	s.mu.Unlock()
	if !already {
		if err := s.Store.AuditDetail("run_now_requested", s.Store.Snapshot().Archive.Job, map[string]any{"by": by, "dryRun": s.DryRun}); err != nil {
			return out, err
		}
	}
	select {
	case s.wakeChan() <- struct{}{}:
	default:
	}
	return out, nil
}

func (s *Service) handleRunNow(w http.ResponseWriter, r *http.Request) {
	in, ok := readAction(w, r)
	if !ok {
		return
	}
	run, err := s.RequestRun(in.RequestedBy)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	msg := "queued: archive check; automatic downloads only inside the nightly window"
	if s.DryRun {
		msg = "queued as a dry run: mediapolicy runs without -apply, so it reports and grabs nothing"
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"runNow": run, "dryRun": s.DryRun, "message": msg})
}

func (s *Service) handleRestore(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, "restore", func(ctx context.Context, a ArchiveActions, in actionRequest) (any, error) {
		return a.RestoreJob(ctx, time.Now().UTC(), in.ID, in.Apply, in.Hash)
	})
}

func (s *Service) handlePurge(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, "purge", func(ctx context.Context, a ArchiveActions, in actionRequest) (any, error) {
		return a.PurgeJob(ctx, time.Now().UTC(), in.ID, in.Apply, in.Hash, in.Confirm)
	})
}

func (s *Service) handlePurgeAll(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, "purge-all", func(ctx context.Context, a ArchiveActions, in actionRequest) (any, error) {
		batch, ok := a.(interface {
			PurgeAll(context.Context, time.Time, bool, string, string) (archivetier.PurgeAllPlan, error)
		})
		if !ok {
			return nil, fmt.Errorf("Purge all unavailable")
		}
		return batch.PurgeAll(ctx, time.Now().UTC(), in.Apply, in.Hash, in.Confirm)
	})
}

// handleAction runs a restore or purge with the cycle lock held, so the
// archive runner cannot start or advance a job meanwhile.
func (s *Service) handleAction(w http.ResponseWriter, r *http.Request, name string, run func(context.Context, ArchiveActions, actionRequest) (any, error)) {
	in, ok := readAction(w, r)
	if !ok {
		return
	}
	a := s.actions()
	if a == nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "archive is disabled"})
		return
	}
	if in.ID == "" && name != "purge-all" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	if !s.lockTick(r.Context()) {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	if in.Apply {
		s.Store.AuditDetail(name+"_requested", nil, map[string]any{"id": in.ID, "by": in.RequestedBy})
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	plan, err := run(ctx, a, in)
	s.publish()
	switch {
	case errors.Is(err, archivetier.ErrUnknownJob):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "plan": plan})
	default:
		writeJSON(w, http.StatusOK, plan)
	}
}

func (s *Service) handleApprove(w http.ResponseWriter, r *http.Request) {
	in, ok := readAction(w, r)
	if !ok {
		return
	}
	a, ok := s.Archive.(interface {
		Approve(context.Context, time.Time, archivetier.ApprovalRequest) (archivetier.State, error)
	})
	if !ok || !s.Config.ArchiveEnabled {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "archive approval is unavailable"})
		return
	}
	if in.Approval == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "approval is required"})
		return
	}
	if !s.lockTick(r.Context()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	if err := s.Store.AuditDetail("approval_operator_requested", nil, map[string]any{"by": in.RequestedBy, "approval": in.Approval}); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	_, err := a.Approve(r.Context(), time.Now().UTC(), *in.Approval)
	s.publish()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if in.Approval.Clear {
		writeJSON(w, http.StatusOK, map[string]any{"message": "Pending approval cleared."})
		return
	}
	// Wake from the persisted approval itself. Clearing it cancels this work,
	// without cancelling an independently requested Run now.
	select {
	case s.wakeChan() <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": "Approval queued. It starts when no other replacement is running and nobody is streaming."})
}

// handleSearch runs one rate-limited release search for a tagged title now,
// so its releases can be reviewed and approved.
func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	in, ok := readAction(w, r)
	if !ok {
		return
	}
	a, ok := s.Archive.(interface {
		SearchTitle(context.Context, time.Time, archivetier.ApprovalRequest) (archivetier.State, error)
	})
	if !ok || !s.Config.ArchiveEnabled {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "archive search is unavailable"})
		return
	}
	if in.Title == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}
	if !s.lockTick(r.Context()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	if err := s.syncSelection(); err != nil {
		writeJSON(w, 409, map[string]string{"error": "Invalid value"})
		return
	}
	if s.Store.Snapshot().ArchivePaused {
		writeJSON(w, 409, map[string]string{"error": "Paused by owner"})
		return
	}
	st, err := a.SearchTitle(r.Context(), time.Now().UTC(), *in.Title)
	s.publish()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	n := 0
	for _, d := range st.Report {
		if d.Target.Kind == in.Title.Kind && d.Target.ID == in.Title.ID && d.Target.Season == in.Title.Season {
			n = len(d.Candidates)
		}
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"message": fmt.Sprintf("Search finished: %d releases.", n)})
}

func (s *Service) handleClear(w http.ResponseWriter, r *http.Request) {
	in, ok := readAction(w, r)
	if !ok {
		return
	}
	if in.ID == "" || !in.Apply || in.Hash != "" || in.Confirm != "" || in.Approval != nil || in.Title != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id and apply are required"})
		return
	}
	a, ok := s.Archive.(interface {
		ClearFailed(context.Context, time.Time, string) (archivetier.State, error)
	})
	if !ok || !s.Config.ArchiveEnabled {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "archive clear is unavailable"})
		return
	}
	if !s.lockTick(r.Context()) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	if err := s.Store.AuditDetail("clear_operator_requested", s.Store.Snapshot().Archive.FindJob(in.ID), map[string]string{"id": in.ID, "by": in.RequestedBy}); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	var err error
	if j := s.Store.Snapshot().Archive.FindJob(in.ID); j != nil {
		err = s.Store.ArchiveClearAllowed(j.Target.Key())
	}
	if err == nil {
		_, err = a.ClearFailed(r.Context(), time.Now().UTC(), in.ID)
	}
	if err != nil {
		if auditErr := s.Store.AuditDetail("clear_refused", s.Store.Snapshot().Archive.FindJob(in.ID), map[string]string{"reason": err.Error()}); auditErr != nil {
			err = auditErr
		}
		code := http.StatusConflict
		if errors.Is(err, archivetier.ErrUnknownJob) {
			code = http.StatusNotFound
		}
		writeJSON(w, code, map[string]any{"error": err.Error()})
		return
	}
	if err := s.Store.Finish(s.Store.Snapshot().Checked, nil); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	s.publish()
	stateError := s.Store.Snapshot().Error
	s.mu.Lock()
	s.status.Error = stateError
	s.status.Healthy = s.status.Error == "" && archivetier.Healthy(s.Store.Snapshot().Archive)
	s.status.EnforcementHealthy = s.status.Healthy
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"message": "Cleared", "applied": true})
}
