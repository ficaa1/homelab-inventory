package archivetier

import (
	"context"
	"time"
)

// observePlayback persists the last observed stream and the beginning of a
// continuous quiet interval. Unknown activity, restart and observation gaps
// break that interval.
func (r *Runner) observePlayback(ctx context.Context, now time.Time, st *State) (bool, error) {
	active, err := r.Plex.Active(ctx)
	interval := r.Interval
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	// Compare wall time explicitly: time.Time.Sub can hide wall-clock jumps
	// when both timestamps contain monotonic readings.
	gap := now.UTC().Sub(r.playbackObservedAt.UTC())
	reset := r.playbackObservedAt.IsZero() || gap < 0 || gap > 2*interval || now.Before(st.StreamQuietSince)
	r.playbackObservedAt = now
	if active {
		st.LastStreamSeen = now
	}
	if active || err != nil {
		st.StreamQuietSince = time.Time{}
	} else if reset || st.StreamQuietSince.IsZero() {
		st.StreamQuietSince = now
	}
	if j := st.Job; j != nil && !j.PlaybackWaitStarted.IsZero() && err == nil && !active && now.Sub(st.StreamQuietSince) >= 5*time.Minute {
		j.PlaybackWaitDuration += now.Sub(j.PlaybackWaitStarted)
		j.PlaybackWaitStarted = time.Time{}
	}
	return active, err
}

// importAdmission requires five quiet minutes with no upper playback cap.
func (r *Runner) importAdmission(ctx context.Context, now time.Time, st *State) error {
	if r.Now != nil {
		if at := r.Now(); at.After(now) {
			now = at
		}
	}
	active, err := r.observePlayback(ctx, now, st)
	if j := st.Job; j != nil && (active || err != nil || now.Sub(st.StreamQuietSince) < 5*time.Minute) && j.PlaybackWaitStarted.IsZero() {
		j.PlaybackWaitStarted = now
	}
	if saveErr := r.Store.Save(*st); saveErr != nil {
		return saveErr
	}
	if err != nil {
		return err
	}
	if active {
		return pauseError("Plex sessions active")
	}
	if now.Sub(st.StreamQuietSince) < 5*time.Minute {
		return pauseError("Waiting for quiet playback")
	}
	if r.OwnerPaused != nil && r.OwnerPaused() {
		return pauseError("Paused by owner")
	}
	n, err := r.DState()
	if err != nil {
		return err
	}
	if n >= r.Config.MaxDState {
		return pauseError("host D-state threshold reached")
	}
	return nil
}

// requestImportApproval proves unchanged originals before waiting for an owner.
// A changed library follows automatic-import reconciliation instead.
func (r *Runner) requestImportApproval(ctx context.Context, now time.Time, st *State, why string, writes bool, blocked string) error {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return r.pause(st, pauseError("library inspection unavailable"))
	}
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return r.pause(st, pauseError("library inspection unavailable"))
	}
	if err := r.observeLibraryChange(ctx, now, st, current); err != nil {
		return err
	}
	if j.HasImportIntent() {
		if j.Phase == ImportIntent || j.CommandID > 0 || now.Sub(j.Started) > time.Duration(r.Config.TimeoutHours)*time.Hour {
			return r.fail(ctx, st, why)
		}
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	if j.Phase != NeedsApproval {
		if err := Transition(j, NeedsApproval); err != nil {
			return err
		}
		j.PhaseStarted = now
		j.Message, j.ApprovalReason = why, why
		st.Error, st.Paused = "", "Needs owner approval"
		if err := r.Store.Save(*st); err != nil {
			return err
		}
		return r.Store.Audit("import_approval_needed", j)
	}
	st.Paused = "Needs owner approval"
	return nil
}
