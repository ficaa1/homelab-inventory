package media

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type digestArr struct {
	media   *Media
	service service
}

func (a *digestArr) get(ctx context.Context, path string, q url.Values, out any) error {
	return a.media.get(ctx, a.service, "/api/v3"+path, q, out, true)
}

var arrToken = regexp.MustCompile(`\b[a-fA-F0-9]{32,64}\b`)

type wanted struct {
	TotalRecords int `json:"totalRecords"`
	Records      []struct {
		Title         string `json:"title"`
		AirDateUTC    string `json:"airDateUtc"`
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		ReleaseDate   string `json:"releaseDate"`
		Added         string `json:"added"`
		Year          int    `json:"year"`
	} `json:"records"`
}

type QueueStatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

type digestQueueItem struct {
	Title                 string               `json:"title"`
	DownloadState         string               `json:"download_state"`
	ImportState           string               `json:"import_state"`
	StatusMessages        []QueueStatusMessage `json:"statusMessages"`
	Status                string               `json:"status"`
	TrackedDownloadStatus string               `json:"trackedDownloadStatus"`
	TrackedDownloadState  string               `json:"trackedDownloadState"`
	Size                  int64                `json:"size"`
	SizeLeft              int64                `json:"sizeleft"`
	TimeLeft              string               `json:"timeleft"`
	ErrorMessage          string               `json:"errorMessage"`
	DownloadClient        string               `json:"downloadClient"`
	Indexer               string               `json:"indexer"`
}

type Queue struct {
	Total        int               `json:"total"`
	Downloading  int               `json:"downloading"`
	Queued       int               `json:"queued"`
	Problems     int               `json:"problems"` // distinct items needing attention
	Attention    int               `json:"attention"`
	Items        []digestQueueItem `json:"items"`
	ProblemItems []digestQueueItem `json:"problem_items"`
}

var arrURLs = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s<>"']+|magnet:\?[^\s<>"']+`)
var arrAuth = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[^\s,;"'<>]+`)
var arrSecrets = regexp.MustCompile(`(?i)\b(?:pass[_ -]?key|password|(?:(?:access|refresh)[_ -]?)?token|api[_ -]?key|authorization|bearer|(?:client[_ -]?)?secret)\b["']?\s*(?:[:=]\s*|\s+)(?:"[^"]*"|'[^']*'|[^\s,;"'<>]+)`)

func (a *digestArr) scrub(s string) string {
	if a.service.key != "" {
		s = strings.ReplaceAll(s, a.service.key, "[redacted]")
	}
	s = arrURLs.ReplaceAllString(s, "[redacted]")
	s = arrAuth.ReplaceAllString(s, "[redacted]")
	s = arrSecrets.ReplaceAllString(s, "[redacted]")
	return a.media.text(arrToken.ReplaceAllString(s, "[redacted]"))
}

func classifyQueueItem(it *digestQueueItem) bool {
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

func (a *digestArr) fetchQueue(ctx context.Context) (*Queue, error) {
	out := &Queue{Items: []digestQueueItem{}, ProblemItems: []digestQueueItem{}}
	q := url.Values{"pageSize": {"100"}}
	for n := 1; n <= 20; n++ {
		q.Set("page", strconv.Itoa(n))
		var page struct {
			TotalRecords *int              `json:"totalRecords"`
			Records      []digestQueueItem `json:"records"`
		}
		if err := a.get(ctx, "/queue", q, &page); err != nil {
			return nil, err
		}
		if page.TotalRecords == nil || *page.TotalRecords < 0 || page.Records == nil {
			return nil, fmt.Errorf("%s: incomplete queue response", a.service.id)
		}
		if n == 1 {
			out.Total = *page.TotalRecords
		}
		if out.Total != *page.TotalRecords || len(out.Items)+len(page.Records) > out.Total {
			return nil, fmt.Errorf("%s: queue changed during collection", a.service.id)
		}
		for _, it := range page.Records {
			problem := classifyQueueItem(&it)
			it.Title, it.Status = a.scrub(it.Title), a.scrub(it.Status)
			it.TrackedDownloadStatus, it.TrackedDownloadState = a.scrub(it.TrackedDownloadStatus), a.scrub(it.TrackedDownloadState)
			it.ErrorMessage, it.DownloadClient, it.Indexer, it.TimeLeft = a.scrub(it.ErrorMessage), a.scrub(it.DownloadClient), a.scrub(it.Indexer), a.scrub(it.TimeLeft)
			if it.StatusMessages == nil {
				it.StatusMessages = []QueueStatusMessage{}
			}
			for i := range it.StatusMessages {
				m := &it.StatusMessages[i]
				m.Title = a.scrub(m.Title)
				if m.Messages == nil {
					m.Messages = []string{}
				}
				for j := range m.Messages {
					m.Messages[j] = a.scrub(m.Messages[j])
				}
			}
			if it.DownloadState == "downloading" && strings.EqualFold(it.Status, "downloading") {
				out.Downloading++
			}
			if strings.EqualFold(it.Status, "queued") || it.DownloadState == "paused" {
				out.Queued++
			}
			if problem {
				out.Problems++
				out.ProblemItems = append(out.ProblemItems, it)
			}
			out.Items = append(out.Items, it)
		}
		if len(out.Items) == out.Total {
			out.Attention = out.Problems
			return out, nil
		}
		if len(page.Records) == 0 {
			break
		}
	}
	return nil, fmt.Errorf("%s: incomplete queue pagination", a.service.id)
}

type digestStatus struct {
	AppName string `json:"appName"`
	Version string `json:"version"`
}

type SonarrSummary struct {
	Status          digestStatus `json:"status"`
	SeriesTotal     int          `json:"series_total"`
	SeriesMonitored int          `json:"series_monitored"`
	Missing         int          `json:"missing"`
	MissingSoon     []WantedItem `json:"missing_soon"` // upcoming episodes still missing
	RecentAdds      []WantedItem `json:"recent_adds"`  // series added in the last week
	Queue           *Queue       `json:"queue"`
}

type WantedItem struct {
	Title   string    `json:"title"`
	AirDate time.Time `json:"air_date,omitempty"`
	Added   time.Time `json:"added,omitempty"`
	Season  int       `json:"season,omitempty"`
	Episode int       `json:"episode,omitempty"`
	Year    int       `json:"year,omitempty"`
}

func (a *digestArr) sonarrSummary(ctx context.Context, now time.Time) (*SonarrSummary, error) {
	out := &SonarrSummary{MissingSoon: []WantedItem{}, RecentAdds: []WantedItem{}}

	q := url.Values{}
	q.Set("pageSize", "200")
	q.Set("sortKey", "airDateUtc")
	q.Set("sortDirection", "ascending")
	var w wanted
	if err := a.get(ctx, "/wanted/missing", q, &w); err != nil {
		return nil, err
	}
	out.Missing = w.TotalRecords
	for _, r := range w.Records {
		if len(out.MissingSoon) >= 5 {
			break
		}
		if r.AirDateUTC == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, r.AirDateUTC); err == nil && t.After(now) {
			out.MissingSoon = append(out.MissingSoon, WantedItem{
				Title: r.Title, AirDate: t, Season: r.SeasonNumber, Episode: r.EpisodeNumber,
			})
		}
	}

	var series []struct {
		Title     string `json:"title"`
		Added     string `json:"added"`
		Monitored bool   `json:"monitored"`
	}
	if err := a.get(ctx, "/series", nil, &series); err != nil {
		return nil, err
	}
	out.SeriesTotal = len(series)
	week := now.AddDate(0, 0, -7)
	for _, sr := range series {
		if sr.Monitored {
			out.SeriesMonitored++
		}
		if sr.Added != "" {
			if t, err := time.Parse(time.RFC3339, sr.Added); err == nil && t.After(week) {
				out.RecentAdds = append(out.RecentAdds, WantedItem{Title: sr.Title, Added: t})
			}
		}
	}
	sort.Slice(out.RecentAdds, func(i, j int) bool { return out.RecentAdds[i].Added.After(out.RecentAdds[j].Added) })

	var st digestStatus
	if err := a.get(ctx, "/system/status", nil, &st); err != nil {
		return nil, err
	}
	out.Status = st

	queue, err := a.fetchQueue(ctx)
	if err != nil {
		return nil, err
	}
	out.Queue = queue
	return out, nil
}

type RadarrSummary struct {
	Status      digestStatus `json:"status"`
	MoviesTotal int          `json:"movies_total"`
	Missing     int          `json:"missing"`
	MissingSoon []WantedItem `json:"missing_soon"` // missing movies that have hit release
	RecentAdds  []WantedItem `json:"recent_adds"`  // movies added in the last week
	Queue       *Queue       `json:"queue"`
}

func (a *digestArr) radarrSummary(ctx context.Context, now time.Time) (*RadarrSummary, error) {
	out := &RadarrSummary{MissingSoon: []WantedItem{}, RecentAdds: []WantedItem{}}

	q := url.Values{}
	q.Set("pageSize", "100")
	q.Set("sortKey", "releaseDate")
	q.Set("sortDirection", "descending")
	var w wanted
	if err := a.get(ctx, "/wanted/missing", q, &w); err != nil {
		return nil, err
	}
	out.Missing = w.TotalRecords
	for _, m := range w.Records {
		if len(out.MissingSoon) >= 5 {
			break
		}
		if m.ReleaseDate == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, m.ReleaseDate); err == nil && !t.After(now.AddDate(0, 0, 30)) {
			out.MissingSoon = append(out.MissingSoon, WantedItem{Title: m.Title, Year: m.Year, AirDate: t})
		}
	}

	var movies []struct {
		Title string `json:"title"`
		Added string `json:"added"`
		Year  int    `json:"year"`
	}
	if err := a.get(ctx, "/movie", nil, &movies); err != nil {
		return nil, err
	}
	out.MoviesTotal = len(movies)
	week := now.AddDate(0, 0, -7)
	for _, m := range movies {
		if m.Added != "" {
			if t, err := time.Parse(time.RFC3339, m.Added); err == nil && t.After(week) {
				out.RecentAdds = append(out.RecentAdds, WantedItem{Title: m.Title, Year: m.Year, Added: t})
			}
		}
	}
	sort.Slice(out.RecentAdds, func(i, j int) bool { return out.RecentAdds[i].Added.After(out.RecentAdds[j].Added) })

	var st digestStatus
	if err := a.get(ctx, "/system/status", nil, &st); err != nil {
		return nil, err
	}
	out.Status = st

	queue, err := a.fetchQueue(ctx)
	if err != nil {
		return nil, err
	}
	out.Queue = queue
	return out, nil
}
