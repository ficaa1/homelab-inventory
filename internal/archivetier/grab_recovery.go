package archivetier

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const grabReconcileWindow = 15 * time.Minute

// EvidenceConflict means successful GETs disagree about the selected download.
// Read failures are retried; they never establish absence.
type EvidenceConflict struct{ Reason string }

func (e *EvidenceConflict) Error() string { return e.Reason }

// ReconcileDownload requires indexer evidence as well as target/title/date.
// Queue titles may differ from release titles; history joins those by download ID.
func (c *Client) ReconcileDownload(ctx context.Context, j *Job) (string, error) {
	history, err := c.titleHistory(ctx, j.Target)
	if err != nil {
		return "", err
	}
	indexer := j.Release.Indexer
	var ids = map[string]bool{}
	for _, h := range history {
		sameTarget := j.Target.Kind == "sonarr" && h.SeriesID == j.Target.ID || j.Target.Kind == "radarr" && h.MovieID == j.Target.ID
		if sameTarget && h.EventType == "grabbed" && (h.Date.IsZero() || h.SourceTitle == "") {
			return "", fmt.Errorf("grab history lacks release identity or date")
		}

		if h.EventType != "grabbed" || h.SourceTitle != j.Release.Title {
			continue
		}
		if j.Target.Kind == "sonarr" && h.SeriesID != j.Target.ID || j.Target.Kind == "radarr" && h.MovieID != j.Target.ID {
			continue
		}
		if raw := h.Data["indexerId"]; raw != "" {
			if raw != strconv.Itoa(j.Release.IndexerID) {
				return "", &EvidenceConflict{"grab history indexer differs from selected release"}
			}
		} else {
			if indexer == "" {
				var ix struct {
					ID   int    `json:"id"`
					Name string `json:"name"`
				}
				if err := c.get(ctx, "/indexer/"+strconv.Itoa(j.Release.IndexerID), nil, &ix); err != nil {
					return "", err
				}
				if ix.ID != j.Release.IndexerID || ix.Name == "" {
					return "", fmt.Errorf("indexer identity unavailable")
				}
				indexer = ix.Name
			}
			if h.Data["indexer"] == "" {
				return "", fmt.Errorf("grab history lacks indexer evidence")
			}
			if h.Data["indexer"] != indexer {
				return "", &EvidenceConflict{"grab history indexer differs from selected release"}
			}
		}
		if h.Date.Before(j.Started.Add(-time.Minute)) {
			return "", &EvidenceConflict{"selected release has grab history before the intent; clock or prior download must be reviewed"}
		}
		if h.DownloadID == "" {
			return "", fmt.Errorf("grab history lacks download identity")
		}
		ids[h.DownloadID] = true
	}
	if len(ids) > 1 {
		return "", &EvidenceConflict{"selected release grabbed more than once"}
	}
	for id := range ids {
		return id, nil
	}
	return "", nil
}

func (r *Runner) selectedDownload(ctx context.Context, j *Job) (string, error) {
	a := r.Arr[j.Target.Kind]
	if reader, ok := a.(interface {
		ReconcileDownload(context.Context, *Job) (string, error)
	}); ok {
		return reader.ReconcileDownload(ctx, j)
	}
	return a.GrabbedDownload(ctx, j)
}

// grabEvidence reads both complete collections before concluding anything.
func (r *Runner) grabEvidence(ctx context.Context, st State, j *Job) (string, bool, error) {
	if j.Started.IsZero() || j.Target.ID <= 0 || j.Release.Title == "" || j.Release.IndexerID <= 0 {
		return "", false, fmt.Errorf("grab identity or start time unavailable")
	}
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return "", false, fmt.Errorf("application unavailable")
	}
	qs, err := a.Queue(ctx)
	if err != nil {
		return "", false, err
	}
	queued := false
	for _, q := range qs {
		if q.Matches(j.Target) && !st.abandoned(j.Target.Kind, q.DownloadID) {
			queued = true
		}
	}
	id, err := r.selectedDownload(ctx, j)
	if err != nil {
		return "", queued, err
	}
	if j.DownloadID != "" {
		if id != "" && id != j.DownloadID {
			return "", false, &EvidenceConflict{"grab history download differs from job"}
		}
		id = j.DownloadID
	}
	unresolved := false
	candidates := map[string]bool{}
	for _, q := range qs {
		if st.abandoned(j.Target.Kind, q.DownloadID) {
			continue
		}
		if id != "" && q.DownloadID == id && !q.Matches(j.Target) {
			return "", false, &EvidenceConflict{"selected download belongs to another title"}
		}
		if !q.Matches(j.Target) {
			continue
		}
		queued = true
		if q.DownloadID == "" {
			return "", true, fmt.Errorf("title queue lacks download identity")
		}
		if id != "" {
			if q.DownloadID != id {
				return "", true, &EvidenceConflict{"another download exists for title"}
			}
			candidates[id] = true
		} else if q.Title == j.Release.Title && (q.IndexerID > 0 && q.IndexerID == j.Release.IndexerID || q.Indexer != "" && q.Indexer == j.Release.Indexer) {
			candidates[q.DownloadID] = true
		} else {
			// Wait for history to join a torrent name to its release title.
			unresolved = true
		}
	}
	if unresolved && len(candidates) > 0 {
		return "", true, &EvidenceConflict{"unmatched download also exists for title"}
	}
	if unresolved {
		return "", true, nil
	}
	if len(candidates) > 1 {
		return "", queued, &EvidenceConflict{"selected release has multiple queued downloads"}
	}
	for candidate := range candidates {
		id = candidate
	}
	return id, queued, nil
}

func (r *Runner) reconcileGrab(ctx context.Context, now time.Time, st *State) error {
	j := st.Job
	id, queued, err := r.grabEvidence(ctx, *st, j)
	j.GrabQueueSeen = j.GrabQueueSeen || queued
	if err != nil {
		var conflict *EvidenceConflict
		if errors.As(err, &conflict) {
			return r.fail(ctx, st, "Grab evidence conflicts: "+conflict.Error())
		}
		st.Paused = "Download needs attention"
		return nil
	}
	if id != "" {
		j.DownloadID, j.Message, j.PhaseStarted = id, "", now
		if err := Transition(j, Downloading); err != nil {
			return err
		}
		if err := r.Store.Save(*st); err != nil {
			return err
		}
		return r.Store.Audit("grab_adopted", j)
	}
	// Count separated successful observations, never dashboard refreshes or failed GETs.
	if j.GrabCheckedAt.IsZero() || now.Sub(j.GrabCheckedAt) >= time.Minute {
		j.GrabChecks++
		j.GrabCheckedAt = now
	}
	if now.Sub(j.Started) >= grabReconcileWindow && j.GrabChecks >= 3 {
		if j.GrabQueueSeen {
			return r.fail(ctx, st, "Queued download cannot be matched to selected release")
		}
		return r.finishUngrabbed(ctx, st, now, "Grab not found in queue or history after 15 minutes", "grab_not_found")
	}
	j.Message = "Grab response uncertain; checking queue and history"
	st.Paused = "Waiting for grab evidence"
	return r.Store.Save(*st)
}

// finishUngrabbed is terminal but does not halt later titles or retain a lease.
func (r *Runner) finishUngrabbed(ctx context.Context, st *State, now time.Time, message, action string) error {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return r.pause(st, pauseError("library inspection unavailable before clean grab failure"))
	}
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return r.pause(st, pauseError("library inspection unavailable before clean grab failure"))
	}
	if err := r.observeLibraryChange(ctx, now, st, current); err != nil {
		return err
	}
	if j.HasImportIntent() {
		return r.fail(ctx, st, "library change or import intent prevents clean grab failure: "+message)
	}
	if err := Transition(j, Failed); err != nil {
		return err
	}
	j.NonHalting, j.Message, j.Completed, j.PhaseStarted = true, message, now, now
	st.Error, st.Paused = "", ""
	if err := r.Store.Save(*st); err != nil {
		return err
	}
	return r.Store.Audit(action, j)
}

// ClearFailed clears only a failed, pre-import job with fresh negative GET evidence.
// The caller serializes this with policy cycles and supplies the displayed job ID.
func (r *Runner) ClearFailed(ctx context.Context, now time.Time, id string) (State, error) {
	st, err := r.Store.Load()
	if err != nil {
		return st, err
	}
	if err := r.Store.WriteAllowed(); err != nil {
		return st, err
	}
	j := st.FindJob(id)
	if j == nil {
		return st, ErrUnknownJob
	}
	if j.Phase != Failed {
		return st, fmt.Errorf("job is not failed")
	}
	if j.GrabQueueSeen {
		return st, fmt.Errorf("unresolved earlier queue evidence; review the download")
	}
	if j.HasImportIntent() {
		return st, fmt.Errorf("import intent recorded; review recovery")
	}
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return st, fmt.Errorf("application unavailable")
	}
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return st, err
	}
	if !SameFiles(j.Target, current) {
		return st, fmt.Errorf("library changed; review recovery")
	}
	found, queued, err := r.grabEvidence(ctx, st, j)
	if err != nil {
		return st, err
	}
	if queued {
		return st, fmt.Errorf("title has a queued download")
	}
	if found != "" {
		return st, fmt.Errorf("grab history records a download; review it")
	}
	// Old incidents and human-review failures cannot bypass the ambiguity window.
	if !j.NonHalting && (now.Sub(j.Started) < grabReconcileWindow || j.Started.IsZero()) {
		return st, fmt.Errorf("Waiting for download confirmation")
	}
	if err := r.Store.Audit("clear_intent", j); err != nil {
		return st, err
	}
	if st.Job == j {
		st.Job = nil
	}
	for i, failed := range st.FailedJobs {
		if JobID(*failed) == id {
			st.FailedJobs = append(st.FailedJobs[:i], st.FailedJobs[i+1:]...)
			break
		}
	}
	st.Error, st.Paused = "", ""
	if err := r.Store.Save(st); err != nil {
		return st, err
	}
	return st, r.Store.Audit("cleared", j)
}

// Older workers added import recovery guidance to pre-import failures.
func preImportMessage(message string) string {
	if i := strings.Index(message, "; recovery evidence unavailable:"); i >= 0 {
		message = message[:i]
	}
	return strings.ReplaceAll(message, "ambiguous write intent after restart; operator reconciliation required", "Grab outcome unresolved; review queue evidence")
}
