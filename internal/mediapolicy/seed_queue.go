package mediapolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"
)

// SeedQueueMax bounds one Link all request and the stored results.
const SeedQueueMax = 50

// defaultSeedQueueRetry spaces the waits on Plex, busy titles and qBittorrent.
const defaultSeedQueueRetry = 30 * time.Second

// SeedQueueEntry is one confirmed linkable torrent waiting for its turn. The
// plan hash is the one the operator reviewed; a different fresh plan skips it.
type SeedQueueEntry struct {
	AllowMove          bool      `json:"allowMove,omitempty"`
	Mode               string    `json:"mode,omitempty"`
	NASBytesCost       int64     `json:"nasBytesCost,omitempty"`
	Hash               string    `json:"hash"`
	PlanHash           string    `json:"planHash"`
	Name               string    `json:"name"`
	Size               int64     `json:"size"`
	DownloadBytesFreed int64     `json:"nvmeBytesFreed"` // Wire name retained for compatibility.
	Queued             time.Time `json:"queued"`         // Shared creation time identifies the queue run.
}

// SeedQueueResult is what happened to one entry: done, skipped or failed.
type SeedQueueResult struct {
	Mode   string    `json:"mode,omitempty"`
	Hash   string    `json:"hash"`
	Name   string    `json:"name"`
	Result string    `json:"result"`
	Reason string    `json:"reason,omitempty"`
	At     time.Time `json:"at"`
}

type SeedQueueItem struct {
	Hash     string `json:"hash"`
	PlanHash string `json:"planHash"`
}
type SeedQueueRequest struct {
	AllowMove bool            `json:"allowMove,omitempty"`
	Items     []SeedQueueItem `json:"items,omitempty"`
	Apply     bool            `json:"apply"`
}

// SeedLinkable is the Link all rule: a fresh plan that only hardlinks
// existing library files, so it costs no NAS space.
func SeedLinkable(p SeedPlan) bool {
	return p.Refusal == "" && p.PlanHash != "" && p.Mode == "link" && p.NASBytesCost == 0
}

// UpdateSeeding applies one change to the seeding job, queue and results in a
// single durable write. A change that returns an error leaves state untouched.
func (s *Store) UpdateSeeding(change func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := clone(s.state)
	if err := change(&s.state); err != nil {
		s.state = old
		return err
	}
	for key, l := range s.state.Leases {
		if l.Owner == "seeding" && s.refs[key] == 0 {
			delete(s.state.Leases, key)
		}
	}
	s.retainPolicies()
	err := s.saveLocked()
	if err != nil {
		s.state = old
	}
	return err
}

func addSeedResults(st *State, rs ...SeedQueueResult) {
	st.SeedQueueResults = append(st.SeedQueueResults, rs...)
	if n := len(st.SeedQueueResults); n > SeedQueueMax {
		st.SeedQueueResults = append([]SeedQueueResult(nil), st.SeedQueueResults[n-SeedQueueMax:]...)
	}
}

// beginSeed takes a confirmed plan's title leases and persists its queued
// job. With fromQueue, the queue's head must still be this torrent and is
// removed in the same write. The caller holds s.tick and runs the job.
func (s *Service) beginSeed(p SeedPlan, fromQueue bool) (*SeedJob, func(), error) {
	releases := []func(){}
	release := func() {
		for _, f := range releases {
			f()
		}
	}
	for _, title := range p.Titles {
		r, err := s.Store.Acquire(title, "seeding")
		if err != nil {
			release()
			return nil, nil, err
		}
		releases = append(releases, r)
	}
	j := &SeedJob{Plan: p, Phase: "queued", Started: time.Now().UTC()}
	// Audit first: a failed audit must never leave a saved "queued" job.
	err := s.Store.AuditDetail("seeding_queued", nil, j)
	if err == nil {
		err = s.Store.UpdateSeeding(func(st *State) error {
			if st.ArchivePaused {
				return fmt.Errorf("Paused by owner")
			}
			if fromQueue {
				if len(st.SeedQueue) == 0 || st.SeedQueue[0].Hash != p.Hash || st.SeedQueue[0].PlanHash != p.PlanHash {
					return fmt.Errorf("Queue changed")
				}
				st.SeedQueue = st.SeedQueue[1:]
			}
			st.Seeding = clone(j)
			return nil
		})
	}
	if err != nil {
		release()
		return nil, nil, err
	}
	return j, release, nil
}

func (s *Service) handleSeedQueue(w http.ResponseWriter, r *http.Request) {
	if s.Seeding == nil || !s.Config.Seeding.Enabled {
		writeJSON(w, 503, map[string]string{"error": "seeding mover is not configured"})
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return
	}
	var in SeedQueueRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 16<<10))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "invalid queue request"})
		return
	}
	if r.URL.Path == "/seeding/queue/cancel" {
		s.cancelSeedQueue(w, in)
		return
	}
	if len(in.Items) == 0 || len(in.Items) > SeedQueueMax {
		writeJSON(w, 400, map[string]string{"error": fmt.Sprintf("1 to %d items", SeedQueueMax)})
		return
	}
	seen := map[string]bool{}
	for _, it := range in.Items {
		if !ValidSeedHash(it.Hash) || it.PlanHash == "" || seen[it.Hash] {
			writeJSON(w, 400, map[string]string{"error": "invalid or repeated item"})
			return
		}
		seen[it.Hash] = true
	}
	if !in.Apply || s.DryRun {
		writeJSON(w, 409, map[string]string{"error": "confirmation and applied service required"})
		return
	}
	// Plan outside s.tick; the drainer re-plans each head before moving it.
	queue := make([]SeedQueueEntry, 0, len(in.Items))
	hashes := make([]string, 0, len(in.Items))
	for _, it := range in.Items {
		p, e := s.seedPlan(r.Context(), it.Hash)
		name := p.Name
		if name == "" {
			name = it.Hash[:8]
		}
		switch {
		case e != nil:
			writeJSON(w, 409, map[string]string{"error": name + ": " + e.Error(), "hash": it.Hash})
			return
		case !seedQueueEligible(p, in.AllowMove):
			writeJSON(w, 409, map[string]string{"error": name + ": Not linkable", "hash": it.Hash})
			return
		case p.PlanHash != it.PlanHash:
			writeJSON(w, 409, map[string]string{"error": name + ": Plan changed", "hash": it.Hash})
			return
		}
		queue = append(queue, SeedQueueEntry{AllowMove: in.AllowMove, Mode: p.Mode, NASBytesCost: p.NASBytesCost, Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name, Size: p.Size, DownloadBytesFreed: p.DownloadBytesFreed})
		hashes = append(hashes, p.Hash)
	}
	if !s.tick.TryLock() {
		writeJSON(w, 503, map[string]string{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	st := s.Store.Snapshot()
	if len(st.SeedQueue) > 0 {
		writeJSON(w, 409, map[string]string{"error": "Queue already running"})
		return
	}
	if st.Seeding.holds() {
		msg := "Previous move needs attention"
		if seedPhaseActive(st.Seeding.Phase) {
			msg = "Another move in progress"
		}
		writeJSON(w, 409, map[string]string{"error": msg})
		return
	}
	e := s.Store.AuditDetail("seeding_queue_queued", nil, map[string]any{"hashes": hashes})
	if e == nil {
		e = s.Store.UpdateSeeding(func(st *State) error {
			now := time.Now().UTC()
			for i := range queue {
				queue[i].Queued = now
			}
			st.SeedQueue, st.SeedQueueResults = queue, nil
			return nil
		})
	}
	if e != nil {
		writeJSON(w, 409, map[string]string{"error": e.Error()})
		return
	}
	s.startSeedQueue()
	writeJSON(w, 202, map[string]any{"queued": len(queue)})
}

// cancelSeedQueue drops every waiting entry. A move already running is never
// touched: it finishes, rolls back or halts on its own.
func (s *Service) cancelSeedQueue(w http.ResponseWriter, in SeedQueueRequest) {
	if !in.Apply || len(in.Items) > 0 {
		writeJSON(w, 400, map[string]string{"error": "cancel accepts only apply"})
		return
	}
	var dropped []string
	e := s.Store.UpdateSeeding(func(st *State) error {
		if len(st.SeedQueue) == 0 {
			return fmt.Errorf("No queue")
		}
		for _, q := range st.SeedQueue {
			dropped = append(dropped, q.Hash)
		}
		st.SeedQueue = nil
		return nil
	})
	if e == nil {
		e = s.Store.AuditDetail("seeding_queue_cancelled", nil, map[string]any{"hashes": dropped})
	}
	if e != nil {
		writeJSON(w, 409, map[string]string{"error": e.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"cancelled": len(dropped)})
}

// ResumeSeedQueue restarts a persisted queue after a restart. A seeding job
// that still holds leases drops the rest. Verified rollbacks keep the queue.
func (s *Service) ResumeSeedQueue() bool {
	if s.DryRun || s.Seeding == nil || !s.Config.Seeding.Enabled {
		return false
	}
	st := s.Store.Snapshot()
	if st.ArchivePaused {
		return true
	}
	if len(st.SeedQueue) == 0 {
		return false
	}
	if st.Seeding.holds() {
		s.stopSeedQueue(SeedQueueResult{}, "Previous move needs attention")
		return false
	}
	if j := st.Seeding; j != nil && j.Phase == "rolled_back" && !st.SeedQueue[0].Queued.IsZero() && !j.Started.Before(st.SeedQueue[0].Queued) {
		recorded := false
		for _, result := range st.SeedQueueResults {
			if result.Hash == j.Plan.Hash && !result.At.Before(j.Started) {
				recorded = true
			}
		}
		if !recorded {
			s.recordSeedQueue(SeedQueueResult{Hash: j.Plan.Hash, Name: j.Plan.Name, Result: "failed", Reason: "Rolled back", At: time.Now().UTC()}, false)
		}
	}

	s.startSeedQueue()
	return true
}

func (s *Service) startSeedQueue() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seedQueueRunning {
		return
	}
	s.seedQueueRunning = true
	s.seedWG.Add(1)
	go func() {
		defer s.seedWG.Done()
		s.drainSeedQueue()
	}()
}

// seedQueuePending reports a waiting entry, or marks the drainer stopped in
// the same critical section startSeedQueue checks, so a queue saved just
// before can never be stranded.
func (s *Service) seedQueuePending(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() == nil && !s.DryRun && len(s.Store.Snapshot().SeedQueue) > 0 {
		return true
	}
	s.seedQueueRunning = false
	return false
}

func (m *SeedMover) queueRetry() time.Duration {
	if m.QueueRetry > 0 {
		return m.QueueRetry
	}
	return defaultSeedQueueRetry
}

func seedSleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// drainSeedQueue runs entries one at a time. Each takes s.tick for its own
// move only, so archive cycles run between moves.
func (s *Service) drainSeedQueue() {
	ctx := s.Seeding.Context
	if ctx == nil {
		ctx = context.Background()
	}
	for s.seedQueuePending(ctx) {
		if !s.tick.TryLock() {
			seedSleep(ctx, s.Seeding.interval())
			continue
		}
		wait := s.seedQueueStep(ctx)
		s.tick.Unlock()
		if wait {
			seedSleep(ctx, s.Seeding.queueRetry())
		} else {
			// Let a waiting archive cycle take the lock first.
			seedSleep(ctx, s.Seeding.interval())
		}
	}
}

// seedQueueTransient refusals clear on their own; the entry waits.
func seedQueueTransient(err error) bool {
	msg := err.Error()
	for _, t := range []string{"Paused by owner", "Plex is in use", "Could not check Plex", "Busy, try again shortly", "Archive job in progress", "Another move in progress", "Could not save progress"} {
		if msg == t {
			return true
		}
	}
	return strings.HasPrefix(msg, "qBittorrent ") && !strings.Contains(msg, "outcome unknown")
}

// seedQueueStep handles the queue's head with s.tick held. It reports
// whether the head must wait for a transient refusal to clear.
func (s *Service) seedQueueStep(ctx context.Context) bool {
	st := s.Store.Snapshot()
	if st.ArchivePaused {
		return true
	}
	if len(st.SeedQueue) == 0 {
		return false
	}
	head := st.SeedQueue[0]
	if st.Seeding.holds() {
		if seedPhaseActive(st.Seeding.Phase) {
			return true
		}
		s.stopSeedQueue(SeedQueueResult{}, "Previous move needs attention")
		return false
	}
	if s.Seeding.Active == nil {
		return true
	}
	if active, err := s.Seeding.Active(ctx); err != nil || active {
		return true
	}
	p, err := s.seedPlan(ctx, head.Hash)
	reason := ""
	switch {
	case err != nil && (ctx.Err() != nil || seedQueueTransient(err)):
		// A shutdown mid-plan is not the entry's fault.
		return true
	case err != nil:
		reason = err.Error()
	case !seedQueueEligible(p, head.AllowMove):
		reason = "Not linkable"
	case p.PlanHash != head.PlanHash:
		reason = "Plan changed"
	}
	if reason == "" {
		j, release, e := s.beginSeed(p, true)
		if e != nil {
			if seedQueueTransient(e) {
				return true
			}
			reason = e.Error()
		} else {
			s.runSeed(j)
			release()
			if j.Phase == "stopped" {
				s.recordSeedQueue(SeedQueueResult{Hash: head.Hash, Name: head.Name, Result: "failed", Reason: "Stopped by owner", At: time.Now().UTC()}, false)
				return false
			}
			if j.Phase == "rolled_back" {
				s.recordSeedQueue(SeedQueueResult{Hash: head.Hash, Name: head.Name, Result: "failed", Reason: "Rolled back", At: time.Now().UTC()}, false)
				return false
			}
			if ctx.Err() != nil && j.Phase != "complete" {
				// Shutdown halted the move. The rest waits for the operator.
				s.recordSeedQueue(SeedQueueResult{Hash: head.Hash, Name: head.Name, Result: "failed", Reason: "Stopped", At: time.Now().UTC()}, false)
				return false
			}

			if j.Phase == "complete" {
				s.recordSeedQueue(SeedQueueResult{Mode: p.Mode, Hash: head.Hash, Name: head.Name, Result: "done", At: time.Now().UTC()}, false)
				return false
			}
			why := seedQueueOutcome[j.Phase]
			if why == "" {
				why = "Stopped"
			}
			s.stopSeedQueue(SeedQueueResult{Hash: head.Hash, Name: head.Name, Result: "failed", Reason: why, At: time.Now().UTC()}, why)
			return false
		}
	}
	if reason == "Queue changed" {
		return false
	}
	s.recordSeedQueue(SeedQueueResult{Hash: head.Hash, Name: head.Name, Result: "skipped", Reason: reason, At: time.Now().UTC()}, true)
	return false
}

var seedQueueOutcome = map[string]string{"rolled_back": "Rolled back", "halted": "Stopped", "failed": "Failed"}

// recordSeedQueue stores one result, dropping the head when it is still
// the entry the result names.
func (s *Service) recordSeedQueue(r SeedQueueResult, pop bool) {
	s.Store.UpdateSeeding(func(st *State) error {
		if pop && len(st.SeedQueue) > 0 && st.SeedQueue[0].Hash == r.Hash {
			st.SeedQueue = st.SeedQueue[1:]
		}
		addSeedResults(st, r)
		return nil
	})
	action := "seeding_queue_" + r.Result
	s.Store.AuditDetail(action, nil, r)
}

// stopSeedQueue ends the queue after a move that did not complete, so a
// systematic fault cannot repeat across the remaining torrents.
func (s *Service) stopSeedQueue(failed SeedQueueResult, why string) {
	var dropped []string
	s.Store.UpdateSeeding(func(st *State) error {
		at := time.Now().UTC()
		rs := []SeedQueueResult{}
		if failed.Hash != "" {
			rs = append(rs, failed)
		}
		for _, q := range st.SeedQueue {
			dropped = append(dropped, q.Hash)
			rs = append(rs, SeedQueueResult{Hash: q.Hash, Name: q.Name, Result: "skipped", Reason: "Queue stopped", At: at})
		}
		st.SeedQueue = nil
		addSeedResults(st, rs...)
		return nil
	})
	s.Store.AuditDetail("seeding_queue_stopped", nil, map[string]any{"reason": why, "failed": failed.Hash, "dropped": dropped})
}

// seedQueueView never returns nil, so the page reads an empty queue as [].
func seedQueueView(st State) ([]SeedQueueEntry, []SeedQueueResult) {
	q, r := st.SeedQueue, st.SeedQueueResults
	if q == nil {
		q = []SeedQueueEntry{}
	}
	if r == nil {
		r = []SeedQueueResult{}
	}
	return q, r
}

func seedQueueEligible(p SeedPlan, allowMove bool) bool {
	if !allowMove {
		return SeedLinkable(p)
	}
	return p.Refusal == "" && p.PlanHash != "" && (p.Mode == "link" || p.Mode == "move")
}
