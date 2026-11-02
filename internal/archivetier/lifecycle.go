package archivetier

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// HistoryItem is one Arr history record. Sonarr 4.0.19.2979 and Radarr
// 6.3.0.10514 (HistoryService.cs) record a downloadFolderImported event with
// data.fileId (the imported episode/movie file ID) and data.importedPath.
type HistoryItem struct {
	ID          int               `json:"id"`
	SeriesID    int               `json:"seriesId"`
	EpisodeID   int               `json:"episodeId"`
	MovieID     int               `json:"movieId"`
	DownloadID  string            `json:"downloadId"`
	EventType   string            `json:"eventType"`
	SourceTitle string            `json:"sourceTitle"`
	Date        time.Time         `json:"date"`
	Data        map[string]string `json:"data"`
}

func (c *Client) titleHistory(ctx context.Context, t Target) ([]HistoryItem, error) {
	q := url.Values{"sortKey": {"date"}, "sortDirection": {"descending"}}
	if t.Kind == "sonarr" {
		q.Set("seriesIds", strconv.Itoa(t.ID))
	} else {
		q.Set("movieIds", strconv.Itoa(t.ID))
	}
	return paged(ctx, c, "/history", q, func(h HistoryItem) int { return h.ID })
}

// GrabbedDownload returns the download ID the application recorded when it
// grabbed the job's release: a grabbed event for the target, with the
// release's indexer title, at or after the job started. The queue names a
// download by its torrent name, which is usually not the indexer title, so
// the download ID is the only identity the two share. "" means no grab is
// recorded yet; more than one candidate is an error.
func (c *Client) GrabbedDownload(ctx context.Context, j *Job) (string, error) {
	history, e := c.titleHistory(ctx, j.Target)
	if e != nil {
		return "", e
	}
	ids := map[string]bool{}
	for _, h := range history {
		if h.EventType != "grabbed" || h.SourceTitle != j.Release.Title || h.Date.Before(j.Started.Add(-time.Minute)) {
			continue
		}
		if j.Target.Kind == "sonarr" && h.SeriesID != j.Target.ID || j.Target.Kind == "radarr" && h.MovieID != j.Target.ID {
			continue
		}
		if h.DownloadID == "" {
			return "", fmt.Errorf("grab history %d has no download ID", h.ID)
		}
		ids[h.DownloadID] = true
	}
	if len(ids) > 1 {
		return "", fmt.Errorf("selected release grabbed more than once")
	}
	for id := range ids {
		return id, nil
	}
	return "", nil
}

// Lifecycle requires GET evidence tying every current replacement file, by
// data.fileId and path, to an import of the selected download, every original
// episode to such an import, and no remaining queue rows for the target.
// Imports and selected-download queue rows for unrelated Sonarr episodes are
// ignored; any other replacement-file contradiction fails. An untracked copy
// may never produce this proof.
func (c *Client) Lifecycle(ctx context.Context, j *Job, current Target) error {
	at, err := c.importHistory(ctx, j, current)
	if err != nil {
		return err
	}
	qs, e := c.Queue(ctx)
	if e != nil {
		return fmt.Errorf("unresolved lifecycle: %v", e)
	}
	for _, q := range qs {
		if q.extraPackRow(j) {
			continue
		}
		if q.DownloadID == j.DownloadID || q.Matches(current) && !j.ignoredDownloads[q.DownloadID] {
			return &EvidenceConflict{"unresolved lifecycle: selected download/title remains in queue"}
		}
	}
	if j.CommandID == 0 {
		j.ImportedAt = at
	}
	return nil
}

func (c *Client) importHistory(ctx context.Context, j *Job, current Target) (time.Time, error) {
	if j.DownloadID == "" {
		return time.Time{}, &EvidenceConflict{"unresolved lifecycle: missing selected download ID"}
	}
	if len(current.Files) == 0 {
		return time.Time{}, &EvidenceConflict{"unresolved lifecycle: no current replacement files"}
	}
	history, e := c.titleHistory(ctx, current)
	if e != nil {
		return time.Time{}, e
	}
	// Use the wall clock after the GET, so slow reads cannot reject a valid date.
	observedAt := time.Now()
	dates := j.CommandID == 0
	byID := map[int]File{}
	for _, f := range current.Files {
		byID[f.ID] = f
	}
	episodeFile := map[int]int{}
	for _, ep := range current.Episodes {
		episodeFile[ep.ID] = ep.FileID
	}
	covered := map[int]bool{}
	files := map[int]bool{}
	var at time.Time
	for _, h := range history {
		if h.DownloadID != j.DownloadID || h.EventType != "downloadFolderImported" {
			continue
		}
		if current.Kind == "sonarr" && h.SeriesID != current.ID || current.Kind == "radarr" && h.MovieID != current.ID {
			return time.Time{}, &EvidenceConflict{"unresolved lifecycle: selected-download history for another title"}
		}
		if current.Kind == "sonarr" && h.EpisodeID > 0 && !wantedEpisode(j.Target, h.EpisodeID) {
			continue
		}
		if dates && (h.Date.IsZero() || j.Started.IsZero() || h.Date.Before(j.Started.Truncate(time.Second))) {
			return time.Time{}, &EvidenceConflict{"unresolved lifecycle: automatic import date predates the job or is missing"}
		}
		if dates && h.Date.After(observedAt.Add(time.Minute)) {
			return time.Time{}, &EvidenceConflict{"unresolved lifecycle: automatic import date is in the future"}
		}
		if h.Data == nil {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: import history %d has no data", h.ID)}
		}
		raw, ok := h.Data["fileId"]
		id, err := strconv.Atoi(raw)
		if !ok || err != nil || id <= 0 {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: import history %d lacks data.fileId", h.ID)}
		}
		f, ok := byID[id]
		if !ok {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: import history %d names file ID %d, which is not a current replacement file", h.ID, id)}
		}
		if h.Data["importedPath"] != f.Path {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: import history %d path differs from replacement file ID %d", h.ID, id)}
		}
		if current.Kind == "sonarr" {
			if episodeFile[h.EpisodeID] != id {
				return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: import history %d episode is not on replacement file ID %d", h.ID, id)}
			}
			covered[h.EpisodeID] = true
		}
		files[id] = true
		if at.IsZero() || h.Date.Before(at) {
			at = h.Date
		}
	}
	for _, f := range current.Files {
		if !files[f.ID] {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: no selected-download import history for replacement file ID %d", f.ID)}
		}
	}
	for _, ep := range j.Target.Episodes {
		if !covered[ep.ID] {
			return time.Time{}, &EvidenceConflict{fmt.Sprintf("unresolved lifecycle: no selected-download import history for episode ID %d", ep.ID)}
		}
	}
	return at, nil
}
