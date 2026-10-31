package activity

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// ActivityRecord is one download-queue row with the identifiers needed to place
// it. Only the aggregator reads it; the derived fields are never serialised.
type ActivityRecord struct {
	ID                    int    `json:"id"`
	SeriesID              int    `json:"seriesId"`
	EpisodeID             int    `json:"episodeId"`
	MovieID               int    `json:"movieId"`
	DownloadID            string `json:"downloadId"`
	Status                string `json:"status"`
	TrackedDownloadStatus string `json:"trackedDownloadStatus"`
	TrackedDownloadState  string `json:"trackedDownloadState"`
	ErrorMessage          string `json:"errorMessage"`
	Size                  int64  `json:"size"`
	SizeLeft              int64  `json:"sizeleft"`
	TimeLeft              string `json:"timeleft"`
	// Series and Movie are embedded by includeSeries / includeMovie; only the
	// title is decoded, so the Library strip can name the download.
	Series *ActivityName `json:"series"`
	Movie  *ActivityName `json:"movie"`

	DownloadState string `json:"-"`
	ImportState   string `json:"-"`
	Problem       bool   `json:"-"`
}

// ActivityName is the part of an embedded series or movie the aggregator reads.
type ActivityName struct {
	Title string `json:"title"`
}

// ImportRecord is one downloadFolderImported event for a queued download.
type ImportRecord struct {
	DownloadID string
	SeriesID   int
	EpisodeID  int
	MovieID    int
}

// ArrActivity is one Arr app's import picture: the queue, every import the
// queued downloads have already made, and the season of each episode named.
type ArrActivity struct {
	Queue   []ActivityRecord
	Imports []ImportRecord
	Seasons map[int]int // Sonarr: episodeId to seasonNumber
}

// pageAll reads every page of an Arr paged endpoint. Like fetchQueue, it
// refuses an incomplete, changing or oversized answer rather than count a
// partial list.
func pageAll[T any](ctx context.Context, a *activityReader, path string, q url.Values, maxPages int, id func(T) int) ([]T, error) {
	out := []T{}
	total := 0
	seen := map[int]bool{}
	for n := 1; n <= maxPages; n++ {
		q.Set("page", strconv.Itoa(n))
		var page struct {
			TotalRecords *int `json:"totalRecords"`
			Records      []T  `json:"records"`
		}
		if err := a.get(ctx, path, q, &page); err != nil {
			return nil, err
		}
		if page.TotalRecords == nil || *page.TotalRecords < 0 || page.Records == nil {
			return nil, fmt.Errorf("%s: incomplete %s response", a.service, path)
		}
		if n == 1 {
			total = *page.TotalRecords
		}
		if total != *page.TotalRecords || len(out)+len(page.Records) > total {
			return nil, fmt.Errorf("%s: %s changed during collection", a.service, path)
		}
		for _, record := range page.Records {
			key := id(record)
			if key <= 0 || seen[key] {
				return nil, fmt.Errorf("%s: invalid or repeated %s record", a.service, path)
			}
			seen[key] = true
		}
		out = append(out, page.Records...)
		if len(out) == total {
			return out, nil
		}
		if len(page.Records) == 0 {
			break
		}
	}
	return nil, fmt.Errorf("%s: incomplete %s pagination", a.service, path)
}

type historyRecord struct {
	ID         int    `json:"id"`
	EpisodeID  int    `json:"episodeId"`
	SeriesID   int    `json:"seriesId"`
	MovieID    int    `json:"movieId"`
	DownloadID string `json:"downloadId"`
	EventType  string `json:"eventType"`
}

type episodeRef struct {
	ID           int  `json:"id"`
	SeasonNumber *int `json:"seasonNumber"`
}

// A refresh has a shared request budget as well as a deadline. Paging a large
// history or queue cannot multiply the number of reads without a hard limit.
const activityRequestLimit = 128

type activityReader struct {
	*arr
	requests int
}

func (a *activityReader) get(ctx context.Context, path string, q url.Values, out any) error {
	if a.requests >= activityRequestLimit {
		return fmt.Errorf("%s: activity request limit", a.service)
	}
	a.requests++
	return a.arr.get(ctx, path, q, out)
}

func (source *arr) activity(ctx context.Context, episodes bool) (*ArrActivity, error) {
	a := &activityReader{arr: source}
	q := url.Values{"pageSize": {"100"}}
	if episodes {
		q.Set("includeUnknownSeriesItems", "true")
		q.Set("includeSeries", "true")
	} else {
		q.Set("includeUnknownMovieItems", "true")
		q.Set("includeMovie", "true")
	}
	queue, err := pageAll[ActivityRecord](ctx, a, "/queue", q, 20, func(r ActivityRecord) int { return r.ID })
	if err != nil {
		return nil, err
	}
	out := &ArrActivity{Queue: []ActivityRecord{}, Seasons: map[int]int{}}
	fetched := map[string]bool{}
	for _, rec := range queue {
		// Arr includes unidentified downloads and pack placeholders. They cannot
		// be placed on a Library card and must not hide identified downloads.
		if (episodes && (rec.SeriesID <= 0 || rec.EpisodeID <= 0)) || (!episodes && rec.MovieID <= 0) {
			continue
		}
		a.finishActivity(&rec)
		out.Queue = append(out.Queue, rec)
		key := strings.ToUpper(rec.DownloadID)
		if key == "" || fetched[key] {
			continue
		}
		fetched[key] = true
		imports, err := a.imports(ctx, rec.DownloadID, episodes)
		if err != nil {
			return nil, err
		}
		out.Imports = append(out.Imports, imports...)
	}
	if !episodes {
		return out, nil
	}
	series := map[int]bool{}
	for _, rec := range out.Queue {
		series[rec.SeriesID] = true
	}
	for _, im := range out.Imports {
		series[im.SeriesID] = true
	}
	for id := range series {
		var eps []episodeRef
		if err := a.get(ctx, "/episode", url.Values{"seriesId": {strconv.Itoa(id)}}, &eps); err != nil {
			return nil, err
		}
		for _, ep := range eps {
			if ep.SeasonNumber == nil {
				return nil, fmt.Errorf("%s: episode without season number", a.service)
			}
			out.Seasons[ep.ID] = *ep.SeasonNumber
		}
	}
	for _, rec := range out.Queue {
		if _, ok := out.Seasons[rec.EpisodeID]; !ok {
			return nil, fmt.Errorf("%s: queue episode has no season", a.service)
		}
	}
	for _, im := range out.Imports {
		if _, ok := out.Seasons[im.EpisodeID]; !ok {
			return nil, fmt.Errorf("%s: imported episode has no season", a.service)
		}
	}
	return out, nil
}

// imports reads the downloadFolderImported events for one download. The
// exact queue downloadId is preserved for the upstream filter; matching is
// also checked locally before any event contributes to a count.
func (a *activityReader) imports(ctx context.Context, key string, episodes bool) ([]ImportRecord, error) {
	q := url.Values{
		"downloadId":    {key},
		"eventType":     {"3"},
		"pageSize":      {"250"},
		"sortKey":       {"date"},
		"sortDirection": {"descending"},
	}
	records, err := pageAll[historyRecord](ctx, a, "/history", q, 20, func(r historyRecord) int { return r.ID })
	if err != nil {
		return nil, err
	}
	out := []ImportRecord{}
	for _, h := range records {
		if h.EventType != "downloadFolderImported" || !strings.EqualFold(h.DownloadID, key) {
			continue
		}
		if episodes && (h.SeriesID == 0 || h.EpisodeID == 0) {
			return nil, fmt.Errorf("%s: import history without series or episode id", a.service)
		}
		if !episodes && h.MovieID == 0 {
			return nil, fmt.Errorf("%s: import history without movie id", a.service)
		}
		out = append(out, ImportRecord{DownloadID: key, SeriesID: h.SeriesID, EpisodeID: h.EpisodeID, MovieID: h.MovieID})
	}
	return out, nil
}

func (a *arr) finishActivity(r *ActivityRecord) {
	r.Status, r.TrackedDownloadStatus = a.scrub(r.Status), a.scrub(r.TrackedDownloadStatus)
	r.TrackedDownloadState, r.ErrorMessage = a.scrub(r.TrackedDownloadState), a.scrub(r.ErrorMessage)
	r.TimeLeft = a.scrub(r.TimeLeft)
	it := queueItem{Status: r.Status, TrackedDownloadStatus: r.TrackedDownloadStatus, TrackedDownloadState: r.TrackedDownloadState, ErrorMessage: r.ErrorMessage}
	r.Problem = classifyQueueItem(&it)
	r.DownloadState, r.ImportState = it.DownloadState, it.ImportState
}

type arr struct {
	service string
	fetch   func(context.Context, string, url.Values, any) error
	scrub   func(string) string
}

func (a *arr) get(ctx context.Context, path string, q url.Values, out any) error {
	return a.fetch(ctx, path, q, out)
}
func Read(ctx context.Context, service string, episodes bool, fetch func(context.Context, string, url.Values, any) error, scrub func(string) string) (*ArrActivity, error) {
	return (&arr{service: service, fetch: fetch, scrub: scrub}).activity(ctx, episodes)
}

type queueItem struct{ Status, TrackedDownloadState, TrackedDownloadStatus, ErrorMessage, DownloadState, ImportState string }

func classifyQueueItem(it *queueItem) bool {
	status, state := strings.ToLower(it.Status), strings.ToLower(it.TrackedDownloadState)
	it.ImportState = "pending" // not yet imported; does not imply manual intervention
	switch state {
	case "importblocked":
		it.ImportState = "blocked"
	case "imported":
		it.ImportState = "imported"
	}
	switch {
	case status == "failed" || status == "error" || state == "failed" || state == "failedpending" || state == "downloadfailed":
		it.DownloadState = "failed"
	case status == "stalled" || state == "stalled" || strings.Contains(strings.ToLower(it.ErrorMessage), "stalled"):
		it.DownloadState = "stalled"
	case status == "paused":
		it.DownloadState = "paused"
	case status == "completed" || state == "importpending" || state == "importblocked" || state == "imported":
		it.DownloadState = "completed"
	case status == "downloading" || status == "queued" || state == "downloading":
		it.DownloadState = "downloading"
	default:
		it.DownloadState = "unknown"
	}
	tracked := strings.ToLower(it.TrackedDownloadStatus)
	return tracked == "warning" || tracked == "error" || it.DownloadState == "stalled" || it.DownloadState == "failed" || it.ImportState == "blocked" || strings.TrimSpace(it.ErrorMessage) != ""
}
