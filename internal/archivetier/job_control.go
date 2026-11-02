package archivetier

import (
	"context"
	"fmt"
	"time"
)

// DecideJob records an owner's decision for the displayed waiting job. Import
// executes on the next cycle; a decision never submits a command itself.
func (r *Runner) DecideJob(ctx context.Context, now time.Time, id string, approve bool) (State, error) {
	st, err := r.Store.Load()
	if err != nil {
		return st, err
	}
	if r.DryRun || r.NoSearch {
		return st, fmt.Errorf("Applied service required")
	}
	if err := r.Store.WriteAllowed(); err != nil {
		return st, err
	}
	j := st.Job
	if j == nil || JobID(*j) != id {
		return st, ErrUnknownJob
	}
	if j.Phase != NeedsApproval || j.HasImportIntent() {
		return st, fmt.Errorf("Job is not waiting")
	}
	action := "import_owner_approved"
	if !approve {
		action = "import_owner_dropped"
	}
	if err := r.Store.AuditDetail(action, j, map[string]string{"reason": j.ApprovalReason}); err != nil {
		return st, err
	}
	if approve {
		j.ForceImport, j.ManuallyApproved = true, true
		j.Message = "Owner approved import"
		return st, r.Store.Save(st)
	}
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return st, fmt.Errorf("Library inspection unavailable")
	}
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return st, err
	}
	if !SameFiles(j.Target, current) {
		return st, fmt.Errorf("Library changed, review recovery")
	}
	present, err := a.Present(ctx, current)
	if err != nil || !present {
		return st, fmt.Errorf("Library proof unavailable")
	}
	var own QueueItem
	// Optional metadata never substitutes for the unchanged-library proof.
	if qs, e := a.Queue(ctx); e == nil {
		for _, q := range qs {
			if q.DownloadID == j.DownloadID {
				own = q
				break
			}
		}
	}
	if err := r.abandon(&st, now, own, j.ApprovalReason); err != nil {
		return st, err
	}
	r.recordHistory(&st, now)
	return st, r.Store.Save(st)
}

// ownerImportItems discards unrelated files and episode mappings. It never
// supplies a target or path from the request: all files come from Manual's
// listing of the job's own saved selected download.
func ownerImportItems(items []ManualItem, t Target) []ManualItem {
	out := []ManualItem{}
	for _, it := range items {
		if t.Kind == "radarr" {
			if it.Movie.ID == t.ID {
				out = append(out, it)
			}
		} else if it.Series.ID == t.ID {
			eps := []Episode{}
			for _, ep := range it.Episodes {
				if ep.Season == t.Season && wantedEpisode(t, ep.ID) {
					eps = append(eps, ep)
				}
			}
			if len(eps) > 0 {
				it.Episodes = eps
				out = append(out, it)
			}
		}
	}
	return out
}

// forceImport bypasses policy, tags, other title downloads, rejections and
// probes. Target identity and safe paths, playback, preflight, a final
// fresh unchanged-library proof, and all post-write proofs remain mandatory.
func (r *Runner) forceImport(ctx context.Context, now time.Time, st *State, writes bool, blocked string) error {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return r.pause(st, pauseError("Library inspection unavailable"))
	}
	// Arr can finish the download independently after the owner decision.
	// Reconciliation must still run with Pause or supervisor admission closed.
	observed, err := a.Current(ctx, j.Target)
	if err != nil {
		return r.pause(st, pauseError("Library inspection unavailable"))
	}
	if !SameFiles(j.Target, observed) || j.HasImportIntent() {
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	if !writes {
		return r.pause(st, pauseError("Import admission closed"))
	}
	if j.DownloadID == "" {
		id, err := r.selectedDownload(ctx, j)
		if err != nil || id == "" {
			return r.pause(st, pauseError("Selected download unidentified"))
		}
		j.DownloadID = id
		if err := r.Store.Save(*st); err != nil {
			return err
		}
	}
	qs, err := a.Queue(ctx)
	if err != nil {
		return r.pause(st, pauseError("Download queue unavailable"))
	}
	own := QueueItem{DownloadID: j.DownloadID}
	ready := false
	for _, q := range qs {
		if q.DownloadID == j.DownloadID && !q.extraPackRow(j) {
			if q.SizeLeft > 0 {
				return r.pause(st, pauseError("Waiting for download completion"))
			}
			if !manualImportReady(q) {
				return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
			}
			ready = true
			own = q
		}
	}
	if !ready {
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	if err := a.Preflight(ctx); err != nil {
		return r.pause(st, pauseError("Pre-import safety unavailable"))
	}
	items, err := a.Manual(ctx, own)
	if err != nil {
		return r.pause(st, pauseError("Import listing unavailable"))
	}
	items = ownerImportItems(items, j.Target)
	files, err := validateImport(items, j.Target, r.Config, ModeAny, true)
	if err != nil {
		return r.pause(st, pauseError("Target files unavailable"))
	}
	var sum int64
	for _, it := range items {
		sum += it.Size
	}
	root, baseline, err := a.TargetPreflight(ctx, j.Target, items, sum)
	if err != nil || root != j.RecycleRoot || j.RecycleBaseline == nil {
		return r.pause(st, pauseError("Pre-import safety unavailable"))
	}
	merged, err := mergeBaseline(root, j.RecycleBaseline, baseline)
	if err != nil {
		return r.pause(st, pauseError("Recycle proof unavailable"))
	}
	if err := r.importAdmission(ctx, now, st); err != nil {
		return r.pause(st, err)
	}
	// This is the final read before publishing the write intent. A changed or
	// unreadable library cannot be overruled by the owner approval.
	current, err := a.Current(ctx, j.Target)
	if err != nil {
		return r.pause(st, pauseError("Library inspection unavailable"))
	}
	if !SameFiles(j.Target, current) || j.HasImportIntent() {
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	present, err := a.Present(ctx, current)
	if err != nil || !present {
		return r.pause(st, pauseError("Library proof unavailable"))
	}
	// Present may be slow. Recheck playback and owner admission again.
	if err := r.importAdmission(ctx, now, st); err != nil {
		return r.pause(st, err)
	}
	// Recheck the selected download after all slow preparation and admission reads.
	qs, err = a.Queue(ctx)
	if err != nil {
		return r.pause(st, pauseError("Download queue unavailable"))
	}
	ready = false
	for _, q := range qs {
		if q.DownloadID == j.DownloadID && !q.extraPackRow(j) {
			if !manualImportReady(q) {
				return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
			}
			ready = true
		}
	}
	current, err = a.Current(ctx, j.Target)
	if err != nil {
		return r.pause(st, pauseError("Library inspection unavailable"))
	}
	if !ready || !SameFiles(j.Target, current) {
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	return r.submitManualImport(ctx, now, st, files, merged)
}
