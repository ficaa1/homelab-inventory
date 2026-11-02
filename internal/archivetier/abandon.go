package archivetier

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const abandonedRetention = 9 * 24 * time.Hour

type AbandonedDownload struct {
	ReleaseID   string    `json:"releaseId"`
	Kind        string    `json:"kind"`
	DownloadID  string    `json:"downloadId"`
	Title       string    `json:"title"`
	Release     string    `json:"release"`
	CompletedAt time.Time `json:"completedAt"`
	RemoveAfter time.Time `json:"removeAfter"`
	Bytes       int64     `json:"bytes"`
}

func (st State) abandoned(kind, id string) bool {
	if id == "" {
		return false
	}
	for _, d := range st.AbandonedDownloads {
		if d.Kind == kind && d.DownloadID == id {
			return true
		}
	}
	return false
}
func (st State) activeQueue(kind string, qs []QueueItem) []QueueItem {
	out := []QueueItem{}
	for _, q := range qs {
		if !st.abandoned(kind, q.DownloadID) {
			out = append(out, q)
		}
	}
	return out
}
func without(ss []string, key string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != key {
			out = append(out, s)
		}
	}
	return out
}
func (r *Runner) abandon(st *State, now time.Time, q QueueItem, why string) error {
	j := st.Job
	if j.ReleaseID == "" {
		for _, rel := range st.Lookups[targetKey(j.Target)].Releases {
			if rel.Title == j.Release.Title && rel.IndexerID == j.Release.IndexerID && rel.Size == j.Release.Size {
				j.ReleaseID = releaseID(rel)
				break
			}
		}
		if j.ReleaseID == "" {
			j.ReleaseID = releaseNameID(j.Release)
		}
	}
	if err := Transition(j, Abandoned); err != nil {
		return err
	}
	j.Message, j.Completed, j.PhaseStarted = why, now, now
	j.Summary = targetLabel(j.Target) + " abandoned: " + why + ". Original library files retained; download retained for seeding."
	key := targetKey(j.Target)
	if st.BadReleases == nil {
		st.BadReleases = map[string]map[string]string{}
	}
	if st.BadReleases[key] == nil {
		st.BadReleases[key] = map[string]string{}
	}
	st.BadReleases[key][j.ReleaseID] = why
	bytes := q.Size
	if bytes <= 0 {
		bytes = j.Release.Size
	}
	if j.DownloadID != "" && !st.abandoned(j.Target.Kind, j.DownloadID) {
		removeAfter := time.Time{}
		if !j.DownloadCompletedAt.IsZero() {
			removeAfter = j.DownloadCompletedAt.Add(abandonedRetention)
		}
		st.AbandonedDownloads = append(st.AbandonedDownloads, AbandonedDownload{ReleaseID: j.ReleaseID, Kind: j.Target.Kind, DownloadID: j.DownloadID, Title: targetLabel(j.Target), Release: j.Release.Title, CompletedAt: j.DownloadCompletedAt, RemoveAfter: removeAfter, Bytes: bytes})
	}
	st.Error, st.Paused = "", ""
	st.Attempted = without(st.Attempted, key)
	delete(st.Skipped, key)
	// Recompute the cached list so the rejected release is visibly marked and
	// the next cycle can choose another without spending a tracker lookup.
	if l, ok := st.Lookups[key]; ok {
		l.Decision = sanitized(selectReleases(j.Target, l.Releases, r.Config, st.BadReleases[key]))
		l.Decision.SearchedAt = l.At
		st.Lookups[key] = l
		for i := range st.Report {
			if targetKey(st.Report[i].Target) == key {
				st.Report[i] = l.Decision
			}
		}
	}
	if err := r.Store.Save(*st); err != nil {
		return err
	}
	if r.LogSummary != nil {
		r.LogSummary(j.Summary)
	}
	return r.Store.Audit("abandoned", j)
}

// QueueDeleteAllowed is shared by both write barriers. No bulk endpoint,
// extra parameters, duplicate values, or noncanonical numeric IDs qualify.
func QueueDeleteAllowed(p string, q url.Values) bool {
	const prefix = "/queue/"
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	id := strings.TrimPrefix(p, prefix)
	n, err := strconv.Atoi(id)
	return err == nil && n > 0 && strconv.Itoa(n) == id && len(q) == 3 && len(q["removeFromClient"]) == 1 && q.Get("removeFromClient") == "true" && len(q["blocklist"]) == 1 && q.Get("blocklist") == "true" && len(q["skipRedownload"]) == 1 && q.Get("skipRedownload") == "true"
}

// RemoveQueue rechecks the live ID immediately before DELETE. IDs are not
// persisted: Sonarr may have one row per episode for the same download.
func (c *Client) RemoveQueue(ctx context.Context, selected QueueItem) error {
	if selected.ID <= 0 || selected.DownloadID == "" {
		return fmt.Errorf("cleanup lacks queue identity")
	}
	if err := c.Preflight(ctx); err != nil {
		return err
	}
	qs, err := c.Queue(ctx)
	if err != nil {
		return err
	}
	for _, q := range qs {
		if q.ID != selected.ID {
			continue
		}
		if q.DownloadID != selected.DownloadID || q.MovieID != selected.MovieID || q.SeriesID != selected.SeriesID || q.SizeLeft != 0 || (q.State != "importPending" && q.State != "importBlocked") {
			return fmt.Errorf("cleanup queue id/downloadId mismatch or download no longer blocked and complete")
		}
		return c.call(ctx, http.MethodDelete, "/queue/"+strconv.Itoa(q.ID), url.Values{"removeFromClient": {"true"}, "blocklist": {"true"}, "skipRedownload": {"true"}}, nil, nil)
	}
	return fmt.Errorf("cleanup queue id disappeared; re-resolve next cycle")
}

func (r *Runner) cleanupAbandoned(ctx context.Context, now time.Time, st *State, adm Admission) {
	st.CleanupError = ""
	if r.DryRun || r.NoSearch || !adm.NewWrites || r.Config.Validate() != nil {
		return
	}
	if err := r.Store.WriteAllowed(); err != nil {
		return
	}
	for i := 0; i < len(st.AbandonedDownloads); {
		d := st.AbandonedDownloads[i]
		// A failed title keeps its download and recovery sources. Other titles
		// can still finish retention cleanup while recovery is pending.
		jobs := append([]*Job{}, st.FailedJobs...)
		if st.Job != nil {
			jobs = append(jobs, st.Job)
		}
		held := false
		for _, j := range jobs {
			if j.HoldsLease() && j.Target.Kind == d.Kind && (j.DownloadID != "" && j.DownloadID == d.DownloadID || d.Title != "" && targetLabel(j.Target) == d.Title) {
				held = true
			}
		}
		if held {
			i++
			continue
		}
		if d.CompletedAt.IsZero() || now.Before(d.CompletedAt.Add(abandonedRetention)) {
			i++
			continue
		}
		a := r.Arr[d.Kind]
		remover, ok := a.(interface {
			RemoveQueue(context.Context, QueueItem) error
		})
		if !ok || d.DownloadID == "" {
			st.CleanupError = "abandoned download cleanup unavailable"
			return
		}
		qs, err := a.Queue(ctx)
		if err != nil {
			st.CleanupError = err.Error()
			return
		}
		var own *QueueItem
		for _, q := range qs {
			if q.DownloadID == d.DownloadID {
				copy := q
				own = &copy
				break
			}
		}
		if own != nil {
			if err = r.Store.AuditDetail("abandoned_cleanup_intent", nil, d); err == nil {
				err = remover.RemoveQueue(ctx, *own)
			}
			if err != nil {
				st.CleanupError = err.Error()
				return
			}
		}
		// Already absent is reconciled without replaying a delete. A response
		// lost after DELETE is safe to retry only after another live lookup.
		st.AbandonedDownloads = append(st.AbandonedDownloads[:i], st.AbandonedDownloads[i+1:]...)
		if err = r.Store.Save(*st); err != nil {
			st.CleanupError = err.Error()
			return
		}
		if err = r.Store.AuditDetail("abandoned_cleanup_complete", nil, d); err != nil {
			st.CleanupError = err.Error()
			return
		}
	}
}
