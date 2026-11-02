package archivetier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// PurgeFile is one recycle original that a purge deletes (or deleted).
type PurgeFile struct {
	FileID int    `json:"fileId"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Inode  uint64 `json:"inode,omitempty"`
	Done   bool   `json:"done"`
}

// SidecarRestore is one original subtitle put back from its staged copy.
type SidecarRestore struct {
	Path   string `json:"path,omitempty"`
	Staged string `json:"staged"`
	Done   bool   `json:"done"`
	Note   string `json:"note,omitempty"`
}

// ActionPlan is what a restore or purge will do, or did. Hash identifies
// the plan that was shown: applying needs the same hash, so what runs is
// exactly what the operator confirmed. Confirm is the text a purge needs
// typed, because it cannot be undone.
type ActionPlan struct {
	Action   string           `json:"action"`
	ID       string           `json:"id"`
	Title    string           `json:"title"`
	Applied  bool             `json:"applied"`
	Hash     string           `json:"hash"`
	Confirm  string           `json:"confirm,omitempty"`
	Restore  *RestoreResult   `json:"restore,omitempty"`
	Sidecars []SidecarRestore `json:"sidecars,omitempty"`
	Purge    []PurgeFile      `json:"purge,omitempty"`
	Bytes    int64            `json:"bytes"`
	Error    string           `json:"error,omitempty"`
}

// ErrUnknownJob means no history entry has the requested ID.
var ErrUnknownJob = errors.New("unknown archive job")

func planHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

func (r *Runner) historyEntry(st *State, id string) (*HistoryEntry, error) {
	for i := range st.History {
		if st.History[i].ID == id {
			return &st.History[i], nil
		}
	}
	return nil, ErrUnknownJob
}

// actionable is what restore and purge both require of an entry.
func (r *Runner) actionable(st *State, h *HistoryEntry, now time.Time, apply bool) error {
	if h.Status != HistoryAvailable {
		return fmt.Errorf("this job is already %s", h.Status)
	}
	if now.After(h.UndoUntil) {
		return fmt.Errorf("past the %d-day recycle cleanup (%s); the originals are gone", UndoDays, h.UndoUntil.Format("2006-01-02"))
	}
	if j := st.Job; j != nil && j.Phase != Complete && j.Phase != Abandoned && j.Phase != Failed && j.Target.Key() == h.Job.Target.Key() {
		return fmt.Errorf("an archive job for this title is in progress")
	}
	if len(r.Config.RestoreMounts) == 0 {
		return fmt.Errorf("archive.restoreMounts is not configured; there is no writable view to restore or delete through")
	}
	if apply && (r.DryRun || r.NoSearch) {
		return fmt.Errorf("mediapolicy runs in dry-run (no -apply); this can be planned but not applied")
	}
	return nil
}

func (r *Runner) audit(action string, j *Job, detail any) {
	// An audit failure poisons the store, which stops every later write.
	r.Store.AuditDetail(action, j, detail)
}

// RestoreJob restores one history entry's originals in-process: the same
// Restorer as `mediapolicy -restore`, through archive.restoreMounts. Without
// apply it returns the plan and changes nothing. With apply it needs the
// plan's hash, holds the title's lease, moves the current replacement (and
// the subtitles carried next to it) into archive.quarantineDir, links each
// original back, restores the staged original subtitles, and asks the
// application to rescan. The caller must keep cycles from running meanwhile.
func (r *Runner) RestoreJob(ctx context.Context, now time.Time, id string, apply bool, hash string) (ActionPlan, error) {
	st, err := r.Store.Load()
	if err != nil {
		return ActionPlan{}, err
	}
	h, err := r.historyEntry(&st, id)
	if err != nil {
		return ActionPlan{}, err
	}
	plan := ActionPlan{Action: "restore", ID: id, Title: JobLabel(h.Job)}
	if err := r.actionable(&st, h, now, apply); err != nil {
		return plan, err
	}
	release, err := r.acquire(h.Job.Target)
	if err != nil {
		return plan, fmt.Errorf("title is busy: %v", err)
	}
	defer release()
	a := r.Arr[h.Job.Target.Kind]
	rescanner, ok := a.(Rescanner)
	if a == nil || !ok {
		return plan, fmt.Errorf("no %s client to read the title and rescan it", h.Job.Target.Kind)
	}
	current, err := a.Current(ctx, h.Job.Target)
	if err != nil {
		return plan, fmt.Errorf("cannot read the title's current files from %s", h.Job.Target.Kind)
	}
	originals := map[int]bool{}
	for _, f := range h.Job.Target.Files {
		originals[f.ID] = true
	}
	quarantine := []string{}
	for _, f := range current.Files {
		if originals[f.ID] {
			return plan, fmt.Errorf("original file %d is still in the library; there is nothing to swap back", f.ID)
		}
		quarantine = append(quarantine, f.Path)
	}
	for _, p := range carriedSidecars(h.Job, current) {
		if local, err := writableLocal(r.Config.RestoreMounts, p); err == nil {
			if _, err := os.Lstat(local); err == nil {
				quarantine = append(quarantine, p)
			}
		}
	}
	if len(quarantine) > 0 && r.Config.QuarantineDir == "" {
		return plan, fmt.Errorf("archive.quarantineDir is not configured; the replacement has nowhere to go")
	}
	restorer := &Restorer{Mounts: r.Config.RestoreMounts, LibraryRoots: r.Config.LibraryRoots, Quarantine: quarantine, QuarantineRoot: r.Config.QuarantineDir, Rescan: true}
	dry, err := restorer.Restore(ctx, h.Job)
	if err != nil {
		return plan, err
	}
	plan.Restore = &dry
	plan.Sidecars = RestoreSidecars(h.Job, r.Config.RestoreMounts, r.Config.LibraryRoots, false)
	for _, m := range dry.Restore {
		plan.Bytes += m.Size
	}
	plan.Hash = planHash([]any{dry, plan.Sidecars})
	if !apply {
		return plan, nil
	}
	if hash != plan.Hash {
		return plan, fmt.Errorf("the plan changed since it was shown; review it again")
	}
	r.audit("restore_intent", &h.Job, plan)
	if err := r.Store.WriteAllowed(); err != nil {
		return plan, err
	}
	restorer.Apply, restorer.Client = true, rescanner
	done, rerr := restorer.Restore(ctx, h.Job)
	plan.Restore, plan.Applied = &done, true
	all, some := len(done.Restore) > 0, false
	for _, m := range append(append([]Move{}, done.Quarantine...), done.Restore...) {
		some = some || m.Done
	}
	for _, m := range done.Restore {
		all = all && m.Done
	}
	if all {
		plan.Sidecars = RestoreSidecars(h.Job, r.Config.RestoreMounts, r.Config.LibraryRoots, true)
	}
	switch {
	case all:
		h.Status, h.StatusAt, h.Note = HistoryRestored, now, ""
		if rerr != nil {
			h.Note = rerr.Error()
		}
	case some:
		h.Status, h.StatusAt, h.Note = HistoryRestoreFailed, now, rerr.Error()
	default:
		h.Note = rerr.Error()
	}
	if rerr != nil {
		plan.Error = rerr.Error()
	}
	action := "restore_applied"
	if !all {
		action = "restore_failed"
	}
	saveErr := r.Store.Save(st)
	r.audit(action, &h.Job, plan)
	if saveErr != nil {
		return plan, saveErr
	}
	if !all {
		return plan, rerr
	}
	return plan, nil
}

// PurgeJob deletes one completed job's originals from the recycle bin before
// the cleanup would, which frees the space now and makes undo impossible.
// Applying needs the plan's hash and the title typed as confirm.
func (r *Runner) PurgeJob(ctx context.Context, now time.Time, id string, apply bool, hash, confirm string) (ActionPlan, error) {
	st, err := r.Store.Load()
	if err != nil {
		return ActionPlan{}, err
	}
	h, err := r.historyEntry(&st, id)
	if err != nil {
		return ActionPlan{}, err
	}
	return r.purgeJob(ctx, now, &st, h, apply, hash, confirm)
}

// purgeJob shares planning, leases, audit and persistence across purge actions.
func (r *Runner) purgeJob(ctx context.Context, now time.Time, st *State, h *HistoryEntry, apply bool, hash, confirm string) (ActionPlan, error) {
	id := h.ID
	plan := ActionPlan{Action: "purge", ID: id, Title: JobLabel(h.Job), Confirm: JobLabel(h.Job)}
	if err := r.actionable(st, h, now, apply); err != nil {
		return plan, err
	}
	release, err := r.acquire(h.Job.Target)
	if err != nil {
		return plan, fmt.Errorf("title is busy: %v", err)
	}
	defer release()
	files, err := PurgeOriginals(h.Job, r.Config.RestoreMounts, false)
	if err != nil {
		return plan, err
	}
	plan.Purge = files
	for _, f := range files {
		plan.Bytes += f.Size
	}
	plan.Hash = planHash(files)
	if !apply {
		return plan, nil
	}
	if confirm != plan.Confirm {
		return plan, fmt.Errorf("type the title exactly (%s) to confirm", plan.Confirm)
	}
	if hash != plan.Hash {
		return plan, fmt.Errorf("the plan changed since it was shown; review it again")
	}
	r.audit("purge_intent", &h.Job, plan)
	if err := r.Store.WriteAllowed(); err != nil {
		return plan, err
	}
	files, perr := PurgeOriginals(h.Job, r.Config.RestoreMounts, true)
	plan.Purge, plan.Applied = files, true
	all, some := len(files) > 0, false
	for _, f := range files {
		all, some = all && f.Done, some || f.Done
	}
	switch {
	case all && perr == nil:
		h.Status, h.StatusAt, h.Note = HistoryPurged, now, ""
		gone := false
		h.InRecycle = &gone
	case some:
		h.Status, h.StatusAt, h.Note = HistoryPurgeFailed, now, perr.Error()
	default:
		h.Note = perr.Error()
	}
	action := "purge_applied"
	if perr != nil {
		plan.Error = perr.Error()
		action = "purge_failed"
	}
	saveErr := r.Store.Save(*st)
	r.audit(action, &h.Job, plan)
	if saveErr != nil {
		return plan, saveErr
	}
	return plan, perr
}

// PurgeAllPlan identifies every actionable history entry in one confirmation.
type PurgeAllPlan struct {
	Action  string       `json:"action"`
	Entries []ActionPlan `json:"entries"`
	Bytes   int64        `json:"bytes"`
	Hash    string       `json:"hash"`
	Confirm string       `json:"confirm"`
	Applied bool         `json:"applied"`
	Purged  int          `json:"purged"`
}

// PurgeAll requires the caller to hold the cycle lock, just like PurgeJob.
// Refused entries are skipped during planning. Entry failures never stop the batch.
func (r *Runner) PurgeAll(ctx context.Context, now time.Time, apply bool, hash, confirm string) (PurgeAllPlan, error) {
	plan := PurgeAllPlan{Action: "purge-all", Confirm: "purge all", Entries: []ActionPlan{}}
	st, err := r.Store.Load()
	if err != nil {
		return plan, err
	}
	if apply && (r.DryRun || r.NoSearch) {
		return plan, fmt.Errorf("Apply is disabled")
	}
	for i := range st.History {
		h := &st.History[i]
		if r.actionable(&st, h, now, false) != nil {
			continue
		}
		entry, e := r.purgeJob(ctx, now, &st, h, false, "", "")
		if e != nil {
			continue
		}
		plan.Entries = append(plan.Entries, entry)
		plan.Bytes += entry.Bytes
	}
	plan.Hash = planHash(plan.Entries)
	if !apply {
		return plan, nil
	}
	if confirm != plan.Confirm {
		return plan, fmt.Errorf("Type purge all")
	}
	if hash != plan.Hash {
		return plan, fmt.Errorf("Plan changed")
	}
	plan.Applied = true
	for i, entry := range plan.Entries {
		h, e := r.historyEntry(&st, entry.ID)
		if e == nil && entry.Error == "" {
			plan.Entries[i], e = r.purgeJob(ctx, now, &st, h, true, entry.Hash, entry.Confirm)
		}
		if e != nil {
			plan.Entries[i].Error = e.Error()
		}
		if e == nil && plan.Entries[i].Error == "" {
			plan.Purged++
		}
	}
	return plan, nil
}
