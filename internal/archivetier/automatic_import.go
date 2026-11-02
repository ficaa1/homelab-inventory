package archivetier

import (
	"context"
	"errors"
	"time"
)

type downloadSnapshot struct {
	current Target
	queue   []QueueItem
}

// checkDownloadResult waits for Arr to drain the completed download, then uses
// the same replacement proof as ManualImport. A changed library retains recovery
// evidence even when history cannot attribute it to the selected download.
func (r *Runner) checkDownloadResult(ctx context.Context, now time.Time, st *State, snapshot *downloadSnapshot, writes bool, blocked string) error {
	j := st.Job
	// An observed replacement must be resolved before recycle cleanup, even
	// during owner approval or playback waits. Cap evidence waiting at one day.
	if j.HasImportIntent() && now.Sub(j.ImportedAt) > min(time.Duration(r.Config.TimeoutHours)*time.Hour, 24*time.Hour) {
		return r.fail(ctx, st, "observed library replacement not verified within timeout")
	}
	if j.Phase != NeedsApproval && now.Sub(j.Started) > time.Duration(r.Config.TimeoutHours)*time.Hour {
		return r.requestImportApproval(ctx, now, st, "replacement timeout", writes, blocked)
	}
	if j.HasImportIntent() && len(j.Recovery) == 0 {
		j.Message = r.collectRecovery(ctx, j, j.Message)
		if err := r.Store.Save(*st); err != nil {
			return err
		}
	}
	a := r.Arr[j.Target.Kind]
	var err error
	if snapshot == nil {
		snapshot = &downloadSnapshot{}
		snapshot.current, err = a.Current(ctx, j.Target)
		if err != nil {
			return r.pause(st, pauseError("replacement inspection unavailable"))
		}
		if err := r.observeLibraryChange(ctx, now, st, snapshot.current); err != nil {
			return err
		}
		snapshot.queue, err = a.Queue(ctx)
		if err != nil {
			return r.pause(st, pauseError("download queue unavailable"))
		}
	} else if err := r.observeLibraryChange(ctx, now, st, snapshot.current); err != nil {
		return err
	}
	current, qs := snapshot.current, snapshot.queue
	for _, q := range qs {
		if q.extraPackRow(j) || st.abandoned(j.Target.Kind, q.DownloadID) {
			continue
		}
		if q.DownloadID == j.DownloadID || q.Matches(j.Target) {
			st.Paused = "waiting for application import to finish"
			if j.DownloadID == "" {
				st.Paused = "waiting for grab history of selected release"
			}
			return nil
		}
	}
	if SameFiles(j.Target, current) {
		if j.HasImportIntent() {
			return r.fail(ctx, st, "observed library replacement could not be verified")
		}
		st.Paused = "waiting for selected download import evidence"
		return nil
	}
	id, err := r.selectedDownload(ctx, j)
	if err != nil {
		var conflict *EvidenceConflict
		if errors.As(err, &conflict) {
			return r.fail(ctx, st, "selected download unproven: "+err.Error())
		}
		return r.pause(st, pauseError("selected download history unavailable"))
	}
	if id == "" || j.DownloadID != "" && id != j.DownloadID {
		return r.fail(ctx, st, "library changed without matching selected-download grab history")
	}
	j.DownloadID = id
	if j.Phase == Downloading || j.Phase == NeedsApproval {
		if err := Transition(j, Verifying); err != nil {
			return err
		}
		j.PhaseStarted = now
		if err := r.Store.Save(*st); err != nil {
			return err
		}
		if err := r.Store.Audit("automatic_import_observed", j); err != nil {
			return err
		}
	}
	if !j.SidecarsStaged && j.SidecarMessage == "" {
		j.SidecarMessage = "subtitles not carried over: not staged before import; inspect recycle bin"
	}
	return r.verifyReplacement(ctx, now, st, current, writes, blocked, "application import")
}

func (r *Runner) observeLibraryChange(ctx context.Context, now time.Time, st *State, current Target) error {
	if SameFiles(st.Job.Target, current) || st.Job.HasImportIntent() {
		return nil
	}
	st.Job.ImportedAt = now
	st.Job.Message = r.collectRecovery(ctx, st.Job, st.Job.Message)
	if err := r.Store.Save(*st); err != nil {
		return err
	}
	return r.Store.Audit("library_change_observed", st.Job)
}

func manualImportReady(q QueueItem) bool {
	return q.SizeLeft == 0 && (q.State == "importPending" || q.State == "importBlocked")
}

// One final check prevents a duplicate import after slow probes/preflight.
func (r *Runner) manualImportAllowed(ctx context.Context, now time.Time, st *State, writes bool, blocked string) (bool, error) {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	qs, err := a.Queue(ctx)
	if err != nil {
		return false, err
	}
	matches, ready := 0, true
	for _, q := range qs {
		if q.extraPackRow(j) || !q.Matches(j.Target) || st.abandoned(j.Target.Kind, q.DownloadID) {
			continue
		}
		if q.DownloadID != j.DownloadID {
			return false, r.requestImportApproval(ctx, now, st, "queue changed before import", writes, blocked)
		}
		matches++
		ready = ready && manualImportReady(q)
	}
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return false, err
	}
	if j.HasImportIntent() || !SameFiles(j.Target, current) || matches == 0 || !ready {
		return false, r.checkDownloadResult(ctx, now, st, &downloadSnapshot{current, qs}, writes, blocked)
	}
	return true, nil
}
