package archivetier

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HistoryLimit bounds State.History. UndoDays is the applications' recycle
// bin cleanup: after it the originals are gone and nothing can be undone.
const (
	HistoryLimit = 50
	UndoDays     = 7
)

// History statuses. Only HistoryAvailable entries can be restored or purged;
// a failed restore or purge stops there for the operator to inspect.
const (
	HistoryEnded         = "ended"
	HistoryAvailable     = "available"
	HistoryRestored      = "restored"
	HistoryPurged        = "purged"
	HistoryExpired       = "expired"
	HistoryRestoreFailed = "restore_failed"
	HistoryPurgeFailed   = "purge_failed"
)

// HistoryEntry is one finished job: completed, failed with recovery evidence,
// or ended cleanly without undo. Job omits the recycle baseline, which is the whole
// recycle inventory and is not needed once the evidence exists.
type HistoryEntry struct {
	ID        string    `json:"id"`
	Job       Job       `json:"job"`
	Status    string    `json:"status"`
	StatusAt  time.Time `json:"statusAt,omitempty"`
	UndoUntil time.Time `json:"undoUntil"`
	// InRecycle says whether every original is still in the recycle bin,
	// as last seen through the read-only view; nil is unknown.
	InRecycle *bool     `json:"inRecycle,omitempty"`
	Checked   time.Time `json:"recycleChecked,omitempty"`
	Note      string    `json:"note,omitempty"`
}

// Progress is where a job is, from GET reads only. Download figures come
// from the application queue entry of the job's own download; a season pack
// is listed once per episode with the same download, and is counted once.
type Progress struct {
	Phase          Phase      `json:"phase"`
	BytesTotal     int64      `json:"bytesTotal,omitempty"`
	BytesLeft      int64      `json:"bytesLeft,omitempty"`
	ETA            *time.Time `json:"eta,omitempty"`
	TimeLeft       string     `json:"timeLeft,omitempty"`
	DownloadStatus string     `json:"downloadStatus,omitempty"`
	TrackedState   string     `json:"trackedState,omitempty"`
	CommandStatus  string     `json:"commandStatus,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	Error          string     `json:"error,omitempty"`
}

// ProgressInterval is the shortest gap between two out-of-cycle progress
// reads, however often a dashboard asks.
const ProgressInterval = 15 * time.Second

// JobID names a job stably: its title and season, and when it started.
func JobID(j Job) string {
	return targetKey(j.Target) + "@" + j.Started.UTC().Format("20060102T150405Z")
}

// JobLabel is the title as people say it ("Example Series S01", "Example Movie (2025)").
func JobLabel(j Job) string { return targetLabel(j.Target) }

func undoUntil(j Job) time.Time {
	at := j.ImportedAt
	if at.IsZero() {
		at = j.Completed
	}
	if at.IsZero() {
		at = j.Started
	}
	return at.AddDate(0, 0, UndoDays)
}

// recordHistory files the current job once it is finished and restorable,
// and ages the entries: past the recycle cleanup they expire.
func (r *Runner) recordHistory(st *State, now time.Time) {
	jobs := append([]*Job{}, st.FailedJobs...)
	if st.Job != nil {
		jobs = append(jobs, st.Job)
	}
	for _, j := range jobs {
		if j.Phase == Abandoned || j.CleanFailure() {
			found := false
			for _, h := range st.History {
				if h.ID == JobID(*j) {
					found = true
				}
			}
			if !found {
				copy := *j
				copy.RecycleBaseline = nil
				copy.Progress = nil
				st.History = append([]HistoryEntry{{ID: JobID(*j), Job: copy, Status: HistoryEnded, StatusAt: j.Completed}}, st.History...)
			}
			continue
		}
		if j.Phase != Complete && j.Phase != Failed {
			continue
		}
		if _, err := RestorePlan(*j); err == nil {
			copy := *j
			copy.RecycleBaseline = nil
			copy.Progress = nil
			id := JobID(copy)
			found := false
			for i := range st.History {
				if st.History[i].ID == id {
					found = true
					if st.History[i].Status == HistoryAvailable {
						st.History[i].Job = copy
					}
				}
			}
			if !found {
				st.History = append([]HistoryEntry{{ID: id, Job: copy, Status: HistoryAvailable, UndoUntil: undoUntil(copy)}}, st.History...)
			}
		}
	}
	if len(st.History) > HistoryLimit {
		st.History = st.History[:HistoryLimit]
	}
	for i := range st.History {
		h := &st.History[i]
		if h.Status != HistoryAvailable {
			continue
		}
		if now.After(h.UndoUntil) {
			h.Status, h.StatusAt = HistoryExpired, now
			continue
		}
		if r.RecycleCheck == nil {
			continue
		}
		all, known := true, true
		for _, e := range h.Job.Recovery {
			ok, err := r.RecycleCheck(e)
			if err != nil {
				known = false
				break
			}
			all = all && ok
		}
		if known {
			h.InRecycle, h.Checked = &all, now
		}
	}
}

// downloadProgress records the selected download's queue position. It reads
// only; a failed read is noted and never fails the cycle.
func (r *Runner) downloadProgress(ctx context.Context, now time.Time, j *Job, a ArrAPI) {
	p := &Progress{Phase: Downloading, UpdatedAt: now}
	j.Progress = p
	qs, err := a.Queue(ctx)
	if err != nil {
		p.Error = "application queue unavailable"
		return
	}
	for _, q := range qs {
		if !q.Matches(j.Target) || j.DownloadID != "" && q.DownloadID != j.DownloadID {
			continue
		}
		// One download, however many episode entries list it.
		p.BytesTotal, p.BytesLeft = q.Size, q.SizeLeft
		p.ETA, p.TimeLeft = q.EstimatedCompletion, q.TimeLeft
		p.DownloadStatus, p.TrackedState = q.Status, q.State
		return
	}
	p.Error = "selected release not in the application queue yet"
}

// RefreshProgress updates the current job's progress outside the cycle, for
// a dashboard that is watching. GETs only, never a release search, and not
// more often than ProgressInterval. The supervisor must serialize it with
// cycles, exactly as it serializes cycles.
func (r *Runner) RefreshProgress(ctx context.Context, now time.Time) (State, error) {
	st, err := r.Store.Load()
	if err != nil {
		return st, err
	}
	j := st.Job
	if j == nil || j.Phase != Downloading && j.Phase != Verifying {
		return st, nil
	}
	if j.Progress != nil && now.Sub(j.Progress.UpdatedAt) < ProgressInterval {
		return st, nil
	}
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return st, nil
	}
	if j.Phase == Downloading {
		r.downloadProgress(ctx, now, j, a)
	} else {
		p := &Progress{Phase: Verifying, UpdatedAt: now}
		if j.CommandID == 0 {
			p.TrackedState = "importing"
		} else if status, err := a.Command(ctx, j.CommandID); err != nil {
			p.Error = "command status unavailable"
		} else {
			p.CommandStatus = status
		}
		j.Progress = p
	}
	return st, r.Store.Save(st)
}

// WindowBounds is the nightly window now open, or else the next one.
func (c Config) WindowBounds(now time.Time) (open bool, start, end time.Time) {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return false, time.Time{}, time.Time{}
	}
	a, errA := clockMinute(c.Start)
	b, errB := clockMinute(c.End)
	if errA != nil || errB != nil {
		return false, time.Time{}, time.Time{}
	}
	local := now.In(loc)
	for d := -1; d <= 1; d++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+d, 0, 0, 0, 0, loc)
		s := time.Date(day.Year(), day.Month(), day.Day(), a/60, a%60, 0, 0, loc)
		e := time.Date(day.Year(), day.Month(), day.Day(), b/60, b%60, 0, 0, loc)
		if b <= a {
			e = e.AddDate(0, 0, 1)
		}
		if !now.Before(s) && now.Before(e) {
			return true, s, e
		}
		if now.Before(s) {
			return false, s, e
		}
	}
	return false, time.Time{}, time.Time{}
}

// RecycleCheck reports whether one recorded recycle original is still in
// the recycle bin, through the read-only inspection mounts: the same path,
// a regular file of the recorded size and, when recorded, inode.
func (c Config) RecycleCheck(e Evidence) (bool, error) {
	for _, m := range c.Mounts {
		if !under(e.Path, m.AppRoot) {
			continue
		}
		local := filepath.Join(m.LocalRoot, strings.TrimPrefix(e.Path, m.AppRoot))
		if _, err := os.Lstat(local); errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		st, err := (LocalFilesystem{}).Stat(local)
		if err != nil {
			return false, err
		}
		return !st.Directory && st.Size == e.Size && (e.Inode == 0 || st.Inode == e.Inode), nil
	}
	return false, errors.New("recycle bin has no inspection mount")
}
