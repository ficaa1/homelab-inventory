package media

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type mediaTitle struct {
	ID         int
	Title      string
	Monitored  bool
	HasFile    bool
	SizeOnDisk *int64
	Statistics struct {
		EpisodeCount, EpisodeFileCount int
		SizeOnDisk                     *int64
	}
}
type queueItem struct {
	ID, SeriesID, MovieID                                           int
	DownloadID, Status, TrackedDownloadStatus, TrackedDownloadState string
	Size, Sizeleft                                                  float64
	Timeleft                                                        string
	Series, Movie                                                   *mediaTitle
}

func (q queueItem) title() (string, int) {
	if q.Series != nil {
		id := q.Series.ID
		if id == 0 {
			id = q.SeriesID
		}
		return q.Series.Title, id
	}
	if q.Movie != nil {
		id := q.Movie.ID
		if id == 0 {
			id = q.MovieID
		}
		return q.Movie.Title, id
	}
	id := q.SeriesID
	if id == 0 {
		id = q.MovieID
	}
	// Unknown releases have no library identity, so do not expose their raw release name.
	return "Unknown title", id
}
func (q queueItem) warning() bool {
	state := strings.ToLower(q.TrackedDownloadState)
	status := strings.ToLower(q.Status)
	tracked := strings.ToLower(q.TrackedDownloadStatus)
	return tracked == "warning" || tracked == "error" || status == "failed" || status == "error" || status == "stalled" || state == "importblocked" || state == "failedpending" || state == "downloadfailed" || state == "failed"
}
func include(s service) string {
	if s.id == "sonarr" {
		return "includeSeries"
	}
	return "includeMovie"
}
func (m *Media) queue(ctx context.Context, s service) ([]queueItem, error) {
	out := []queueItem{}
	seen := map[int]bool{}
	total := -1
	for page := 1; page <= 20; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "pageSize": {"100"}, include(s): {"true"}}
		var data struct {
			TotalRecords *int
			Records      []queueItem
		}
		if err := m.get(ctx, s, "/api/v3/queue", q, &data, true); err != nil {
			return nil, err
		}
		if data.TotalRecords == nil || *data.TotalRecords < 0 || data.Records == nil || len(data.Records) > 100 {
			return nil, errors.New(s.name + ": incomplete queue")
		}
		if total < 0 {
			total = *data.TotalRecords
		}
		if total != *data.TotalRecords || len(out)+len(data.Records) > total {
			return nil, errors.New(s.name + ": queue changed during pagination")
		}
		for _, r := range data.Records {
			if r.ID <= 0 || seen[r.ID] {
				return nil, errors.New(s.name + ": invalid queue identity")
			}
			seen[r.ID] = true
			out = append(out, r)
		}
		if len(out) == total {
			return out, nil
		}
		if len(data.Records) == 0 {
			break
		}
	}
	return nil, errors.New(s.name + ": queue exceeds pagination limit")
}
func progress(v float64) *plugin.Progress {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	return &plugin.Progress{Value: v, Label: fmt.Sprintf("%.0f%%", 100*v)}
}
func eta(raw string) time.Duration {
	fields := strings.Split(raw, ":")
	if len(fields) != 3 {
		return 0
	}
	var parts [3]int
	for i, s := range fields {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 100000 {
			return 0
		}
		parts[i] = n
	}
	return time.Duration(parts[0])*time.Hour + time.Duration(parts[1])*time.Minute + time.Duration(parts[2])*time.Second
}
func (m *Media) downloads(s service, rows []queueItem) ([]plugin.Item, int) {
	type group struct {
		title      string
		id         int
		size, left float64
		eta        time.Duration
		warn       bool
		downloads  map[string]bool
	}
	groups := map[string]*group{}
	order := []string{}
	for _, r := range rows {
		title, id := r.title()
		key := strconv.Itoa(id)
		if id == 0 {
			key = "unknown:" + strconv.Itoa(r.ID)
		}
		g := groups[key]
		if g == nil {
			g = &group{title: title, id: id, downloads: map[string]bool{}}
			groups[key] = g
			order = append(order, key)
		}
		g.warn = g.warn || r.warning()
		// Sonarr season packs appear once per episode; count their bytes once.
		download := r.DownloadID
		if download == "" {
			download = strconv.Itoa(r.ID)
		}
		download = strings.ToUpper(download)
		if !g.downloads[download] {
			g.downloads[download] = true
			if r.Size > 0 {
				g.size += r.Size
				g.left += max(0, min(r.Size, r.Sizeleft))
			}
		}
		if d := eta(r.Timeleft); d > g.eta {
			g.eta = d
		}
	}
	items := []plugin.Item{}
	for _, key := range order {
		if len(items) == 500 {
			break
		}
		g := groups[key]
		item := plugin.Item{Title: m.text(g.title), Subtitle: s.name, Image: m.poster(s, g.id), Tone: "info"}
		if g.warn {
			item.Tone = "warn"
			item.Subtitle += " · Needs attention"
		}
		if g.size > 0 {
			item.Progress = progress(1 - g.left/g.size)
		}
		if g.eta > 0 {
			item.Meta = g.eta.Round(time.Minute).String() + " left"
		}
		items = append(items, item)
	}
	return items, len(order)
}

type scheduled struct {
	item    plugin.Item
	at      time.Time
	service service
	id      int
}

func (m *Media) upcoming(ctx context.Context, s service) ([]scheduled, error) {
	start := m.host.Now().UTC()
	end := start.AddDate(0, 0, m.days)
	// A stable hourly query allows refreshes to share the cache; filter precisely below.
	q := url.Values{"start": {start.Truncate(time.Hour).Format(time.RFC3339)}, "end": {end.Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339)}, "unmonitored": {"false"}}
	var rows []struct {
		ID, SeasonNumber, EpisodeNumber                               int
		Title, AirDateUTC, DigitalRelease, PhysicalRelease, InCinemas string
		Monitored                                                     bool
		Series                                                        *mediaTitle
	}
	if s.id == "sonarr" {
		q.Set("includeSeries", "true")
	}
	if err := m.get(ctx, s, "/api/v3/calendar", q, &rows, true); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, errors.New(s.name + ": incomplete calendar")
	}
	out := []scheduled{}
	for _, r := range rows {
		if !r.Monitored {
			continue
		}
		title, id, subtitle := r.Title, r.ID, ""
		dates := []string{r.DigitalRelease, r.PhysicalRelease, r.InCinemas}
		if s.id == "sonarr" {
			if r.Series == nil || !r.Series.Monitored {
				continue
			}
			title, id = r.Series.Title, r.Series.ID
			subtitle = fmt.Sprintf("S%02d E%02d · %s", r.SeasonNumber, r.EpisodeNumber, r.Title)
			dates = []string{r.AirDateUTC}
		}
		at := time.Time{}
		for _, raw := range dates {
			t, err := time.Parse(time.RFC3339, raw)
			if err == nil && !t.Before(start) && t.Before(end) && (at.IsZero() || t.Before(at)) {
				at = t
			}
		}
		if at.IsZero() {
			continue
		}
		out = append(out, scheduled{at: at, service: s, id: id, item: plugin.Item{Title: m.text(title), Subtitle: m.text(subtitle), Badge: &plugin.Badge{Text: at.UTC().Format("Mon 02 Jan"), Tone: "info"}}})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out, nil
}
func (m *Media) history(ctx context.Context, s service) ([]map[string]any, error) {
	q := url.Values{"page": {"1"}, "pageSize": {"50"}, "sortKey": {"date"}, "sortDirection": {"descending"}, "eventType": {"3"}, include(s): {"true"}}
	var page struct {
		TotalRecords *int
		Records      []struct {
			EventType, Date string
			Series, Movie   *mediaTitle
			Episode         struct{ Title string }
			Quality         struct{ Quality struct{ Name string } }
			Data            struct {
				Size         number
				ImportedSize number
			}
		}
	}
	if err := m.get(ctx, s, "/api/v3/history", q, &page, true); err != nil {
		return nil, err
	}
	if page.TotalRecords == nil || *page.TotalRecords < 0 || page.Records == nil {
		return nil, errors.New(s.name + ": incomplete import history")
	}
	rows := []map[string]any{}
	for _, r := range page.Records {
		if r.EventType != "downloadFolderImported" {
			continue
		}
		title := "Unknown title"
		if r.Series != nil {
			title = r.Series.Title
			if r.Episode.Title != "" {
				title += " · " + r.Episode.Title
			}
		}
		if r.Movie != nil {
			title = r.Movie.Title
		}
		at, err := time.Parse(time.RFC3339, r.Date)
		if err != nil {
			continue
		}
		size := r.Data.ImportedSize
		if size == 0 {
			size = r.Data.Size
		}
		rows = append(rows, map[string]any{"title": m.text(title), "quality": m.text(r.Quality.Quality.Name), "size": float64(size), "time": at.UTC().Format(time.RFC3339)})
	}
	return rows, nil
}
func (m *Media) arrStats(ctx context.Context, s service) (plugin.Block, error) {
	path, label := "/api/v3/series", "Series"
	if s.id == "radarr" {
		path, label = "/api/v3/movie", "Movies"
	}
	var titles []mediaTitle
	if err := m.get(ctx, s, path, nil, &titles, true); err != nil {
		return plugin.Block{}, err
	}
	if titles == nil {
		return plugin.Block{}, errors.New(s.name + ": incomplete library")
	}
	monitored, missing := 0, 0
	var size int64
	hasSize := false
	for _, t := range titles {
		if t.Monitored {
			monitored++
		}
		if s.id == "sonarr" {
			missing += max(0, t.Statistics.EpisodeCount-t.Statistics.EpisodeFileCount)
			if t.Statistics.SizeOnDisk != nil {
				size += *t.Statistics.SizeOnDisk
				hasSize = true
			}
		} else {
			if t.Monitored && !t.HasFile {
				missing++
			}
			if t.SizeOnDisk != nil {
				size += *t.SizeOnDisk
				hasSize = true
			}
		}
	}
	missingLabel := "Missing movies"
	if s.id == "sonarr" {
		missingLabel = "Missing episodes"
	}
	items := []plugin.Item{{Label: label, Value: strconv.Itoa(len(titles))}, {Label: "Monitored", Value: strconv.Itoa(monitored)}, {Label: missingLabel, Value: strconv.Itoa(missing)}}
	if hasSize {
		items = append(items, plugin.Item{Label: "Disk use", Value: byteSize(float64(size))})
	}
	return plugin.Block{Type: "stats", Items: items}, nil
}
