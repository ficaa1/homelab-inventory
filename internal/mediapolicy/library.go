package mediapolicy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// Library caches only Arr inventory; archive status is joined from durable
// state on every request, so an active job never waits for the cache expiry.
type Library struct {
	Items          []archivetier.LibraryItem `json:"items"`
	Fetched        time.Time                 `json:"fetched"`
	DryRun         bool                      `json:"dryRun"`
	ArchiveEnabled bool                      `json:"archiveEnabled"`
}

func (s *Service) handleLibrary(w http.ResponseWriter, r *http.Request) {
	s.libraryMu.Lock()
	defer s.libraryMu.Unlock()
	if s.libraryCache.Fetched.IsZero() || time.Since(s.libraryCache.Fetched) >= 5*time.Minute || r.URL.Query().Get("refresh") == "1" {
		items := []archivetier.LibraryItem{}
		for _, kind := range []string{"radarr", "sonarr"} {
			c := s.LibraryClients[kind]
			if c == nil {
				writeJSON(w, 503, map[string]string{"error": "library applications are not configured"})
				return
			}
			rows, err := c.Library(r.Context())
			if err != nil {
				writeJSON(w, 502, map[string]string{"error": err.Error()})
				return
			}
			items = append(items, rows...)
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].SizeBytes == items[j].SizeBytes {
				if items[i].Title == items[j].Title {
					return items[i].Kind < items[j].Kind
				}
				return items[i].Title < items[j].Title
			}
			return items[i].SizeBytes > items[j].SizeBytes
		})
		s.libraryCache = Library{Items: items, Fetched: time.Now().UTC()}
	}
	out := clone(s.libraryCache)
	out.DryRun, out.ArchiveEnabled = s.DryRun, s.Config.ArchiveEnabled
	st := s.Store.Snapshot().Archive
	for i := range out.Items {
		item := &out.Items[i]
		item.ArchiveStatus = libraryStatus(st, item.Kind, item.ID, -1)
		list, chosen := st.Seasons[item.Kind+":"+strconv.Itoa(item.ID)]
		for j := range item.Seasons {
			item.Seasons[j].ArchiveStatus = libraryStatus(st, item.Kind, item.ID, item.Seasons[j].Season)
			item.Seasons[j].Selected = item.Tags.Archive && !chosen && item.Seasons[j].Season > 0
			for _, n := range list {
				item.Seasons[j].Selected = item.Seasons[j].Selected || n == item.Seasons[j].Season
			}
		}
	}
	writeJSON(w, 200, out)
}

func libraryStatus(st archivetier.State, kind string, id, season int) archivetier.LibraryStatus {
	matches := func(t archivetier.Target) bool {
		return t.Kind == kind && t.ID == id && (season < 0 || t.Season == season)
	}
	out := archivetier.LibraryStatus{Status: "none"}
	// Count each season once; history can include several replacements of it.
	seen := map[int]bool{}
	for _, h := range st.History {
		if !matches(h.Job.Target) || seen[h.Job.Target.Season] {
			continue
		}
		seen[h.Job.Target.Season] = true
		if h.Job.Phase != archivetier.Complete || h.Status == archivetier.HistoryRestored {
			continue
		}
		out.Status = "shrunk"
		out.SavedBytes += max(int64(0), h.Job.Target.Bytes()-h.Job.ActualBytes)
		date := h.Job.Completed.UTC().Format(time.RFC3339)
		if date > out.Date {
			out.Date = date
		}
	}
	for _, a := range st.Approvals {
		if a.Kind == kind && a.ID == id && (season < 0 || a.Season == season) {
			out.Status = "pending_approval"
		}
	}
	if j := st.Job; j != nil && matches(j.Target) {
		if j.Phase != archivetier.Complete && j.Phase != archivetier.Abandoned {
			out.Status, out.Phase = "active", j.Phase
			if j.Phase == archivetier.Failed {
				out.Status = "failed"
			}
		}
		if j.Phase == archivetier.Complete && !seen[j.Target.Season] {
			out.Status = "shrunk"
			out.Date = j.Completed.UTC().Format(time.RFC3339)
			out.SavedBytes += max(int64(0), j.Target.Bytes()-j.ActualBytes)
		}
	}
	for _, j := range st.FailedJobs {
		if matches(j.Target) {
			out.Status, out.Phase = "failed", archivetier.Failed
		}
	}
	return out
}

// TagRequest is an explicit owner opt-in, with no profile/monitoring edits.
type TagRequest struct {
	Kind    string `json:"kind"`
	ID      int    `json:"id"`
	Archive *bool  `json:"archive"`
	FourK   *bool  `json:"fourK"`
	// Seasons, for a series, are the only seasons that may be archived.
	Seasons     []int  `json:"seasons,omitempty"`
	RequestedBy string `json:"requestedBy,omitempty"`
}

func (s *Service) handleTag(w http.ResponseWriter, r *http.Request) {
	var in TagRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || in.Archive == nil || in.FourK == nil || in.ID <= 0 || (in.Kind != "radarr" && in.Kind != "sonarr") || len(in.RequestedBy) > 200 || !validSeasons(in) {
		writeJSON(w, 400, map[string]string{"error": "Check your selection"})
		return
	}
	if s.DryRun || !s.Config.ArchiveEnabled {
		writeJSON(w, 409, map[string]string{"error": "Changes are paused"})
		return
	}
	c := s.LibraryClients[in.Kind]
	if c == nil {
		writeJSON(w, 503, map[string]string{"error": "Library unavailable"})
		return
	}
	if !s.lockTick(r.Context()) {
		writeJSON(w, 503, map[string]string{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	st := s.Store.Snapshot().Archive
	status := libraryStatus(st, in.Kind, in.ID, -1).Status
	if status == "active" || status == "failed" || status == "pending_approval" {
		writeJSON(w, 409, map[string]string{"error": "Title awaiting review"})
		return
	}
	if err := s.Store.WriteAllowed(); err != nil {
		writeJSON(w, 409, map[string]string{"error": "Changes unavailable"})
		return
	}
	if err := s.Store.AuditDetail("archive_tag_requested", nil, in); err != nil {
		writeJSON(w, 409, map[string]string{"error": "Changes unavailable"})
		return
	}
	_, queueSupported := s.Archive.(interface {
		SearchTitleCached(context.Context, time.Time, archivetier.ApprovalRequest) (archivetier.State, error)
	})
	var item archivetier.LibraryItem
	if queueSupported {
		items, err := c.Library(r.Context())
		if err != nil {
			writeJSON(w, 503, map[string]string{"error": "Library unavailable"})
			return
		}
		for _, row := range items {
			if row.ID == in.ID {
				item = row
				break
			}
		}
		if item.ID == 0 {
			writeJSON(w, 404, map[string]string{"error": "Title unavailable"})
			return
		}
	}
	// Keep result collection local to this request; library clients are shared.
	client := *c
	creates := []archivetier.TagCreateResult{}
	client.TagCreateResult = func(result archivetier.TagCreateResult) error {
		creates = append(creates, result)
		return s.Store.AuditDetail("archive_tag_create_result", nil, result)
	}
	err := client.SetArchiveTags(r.Context(), in.ID, *in.Archive, *in.FourK)
	// An editor call may have succeeded before the next failed. Invalidate even
	// on failure and audit the outcome; never automatically replay a write.
	s.libraryMu.Lock()
	s.libraryCache = Library{}
	s.libraryMu.Unlock()
	detail := map[string]any{"request": in, "applied": err == nil}
	if len(creates) > 0 {
		detail["creates"] = creates
	}
	if err != nil {
		detail["error"] = err.Error()
	}
	auditErr := s.Store.AuditDetail("archive_tag_result", nil, detail)
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "Tags need attention"})
		return
	}
	if auditErr != nil {
		writeJSON(w, 409, map[string]string{"error": "Changes unavailable"})
		return
	}
	if a, ok := s.Archive.(interface{ SetSeasons(string, []int) error }); ok && in.Kind == "sonarr" {
		var seasons []int
		if *in.Archive {
			seasons = in.Seasons
		}
		if err := a.SetSeasons(in.Kind+":"+strconv.Itoa(in.ID), seasons); err != nil {
			writeJSON(w, 409, map[string]string{"error": "Save seasons again"})
			return
		}
	}
	if queueSupported {
		if err := s.queueArchiveSearches(in, item, st); err != nil {
			writeJSON(w, 409, map[string]string{"error": "Search queue unavailable"})
			return
		}
	}
	s.publish()
	writeJSON(w, 200, map[string]any{"message": "Archive tags updated.", "creates": creates, "searchesQueued": queueSupported})
}

func validSeasons(in TagRequest) bool {
	if in.Seasons == nil {
		return true
	}
	seen := map[int]bool{}
	for _, n := range in.Seasons {
		if in.Kind != "sonarr" || n <= 0 || seen[n] {
			return false
		}
		seen[n] = true
	}
	return len(in.Seasons) > 0
}
