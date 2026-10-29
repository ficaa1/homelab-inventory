package media

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func notice(err error) plugin.Block {
	return plugin.Block{Type: "notice", Tone: "warn", Text: err.Error()}
}
func list(title, empty string, items []plugin.Item) plugin.Block {
	if len(items) > 500 {
		items = items[:500]
	}
	return plugin.Block{Type: "list", Title: title, Empty: empty, Items: items}
}
func byteSize(n float64) string {
	if n < 1024 {
		return fmt.Sprintf("%.0f B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	for _, unit := range units {
		n /= 1024
		if n < 1024 || unit == "PiB" {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
	}
	return ""
}
func (m *Media) View(ctx context.Context, name string, _ url.Values) (plugin.View, error) {
	if name != "now" && name != "library" {
		return plugin.View{}, plugin.ErrNotFound
	}
	if len(m.services) == 0 {
		return plugin.View{}, errors.New("Media is not configured")
	}
	v := plugin.View{Title: "Now", UpdatedAt: m.host.Now().UTC(), RefreshSeconds: 30, Blocks: []plugin.Block{}}
	if name == "library" {
		v.Title = "Library"
		m.library(ctx, &v)
	} else {
		m.now(ctx, &v)
	}
	return v, nil
}
func (m *Media) now(ctx context.Context, v *plugin.View) {
	stats := []plugin.Item{}
	playing, downloading := []plugin.Item{}, []plugin.Item{}
	upcoming := []scheduled{}
	notices := []plugin.Block{}
	hasTau, hasArr := false, false
	queueOK, calendarOK := false, false
	downloadingCount := 0
	for _, s := range m.services {
		// Reserve time for the other services within the host's ten-second deadline.
		serviceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		switch s.id {
		case "tautulli":
			hasTau = true
			items, counts, err := m.playing(serviceCtx, s)
			if err != nil {
				notices = append(notices, notice(err))
			} else {
				playing = items
				stats = append(stats, counts...)
			}
		case "sonarr", "radarr":
			hasArr = true
			rows, err := m.queue(serviceCtx, s)
			if err != nil {
				notices = append(notices, notice(err))
			} else {
				queueOK = true
				items, count := m.downloads(s, rows)
				downloading = append(downloading, items...)
				downloadingCount += count
			}
			scheduled, err := m.upcoming(serviceCtx, s)
			if err != nil {
				notices = append(notices, notice(err))
			} else {
				calendarOK = true
				upcoming = append(upcoming, scheduled...)
			}
		}
		cancel()
	}
	if queueOK {
		stats = append(stats, plugin.Item{Label: "Downloading", Value: strconv.Itoa(downloadingCount)})
	}
	if calendarOK {
		stats = append(stats, plugin.Item{Label: "Upcoming", Value: strconv.Itoa(len(upcoming))})
	}
	if len(stats) > 0 {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "stats", Items: stats})
	}
	if hasTau {
		v.Blocks = append(v.Blocks, list("Now playing", "Nothing playing", playing))
	}
	if hasArr {
		v.Blocks = append(v.Blocks, list("Downloading", "Nothing downloading", downloading))
		sort.SliceStable(upcoming, func(i, j int) bool { return upcoming[i].at.Before(upcoming[j].at) })
		posters := []plugin.Item{}
		for _, r := range upcoming {
			if len(posters) == 500 {
				break
			}
			r.item.Image = m.poster(r.service, r.id)
			posters = append(posters, r.item)
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "posters", Title: "Upcoming", Empty: "Nothing upcoming", Items: posters})
	}
	if !hasTau && !hasArr {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Configure Sonarr, Radarr or Tautulli to show current activity. Overseerr requests are in Library."})
	}
	v.Blocks = append(v.Blocks, notices...)
}
func (m *Media) playing(ctx context.Context, s service) ([]plugin.Item, []plugin.Item, error) {
	var data struct {
		StreamCount *number `json:"stream_count"`
		Transcodes  number  `json:"stream_count_transcode"`
		Bandwidth   number  `json:"total_bandwidth"`
		Sessions    []struct {
			Title            string  `json:"title"`
			GrandparentTitle string  `json:"grandparent_title"`
			Progress         number  `json:"progress_percent"`
			LegacyProgress   number  `json:"progress"`
			Duration         number  `json:"duration"`
			Offset           *number `json:"view_offset"`
		} `json:"sessions"`
	}
	if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_activity"}}, &data, true); err != nil {
		return nil, nil, err
	}
	if data.StreamCount == nil || data.Sessions == nil {
		return nil, nil, errors.New("Tautulli: incomplete activity")
	}
	items := []plugin.Item{}
	for _, r := range data.Sessions {
		title := r.Title
		if r.GrandparentTitle != "" {
			title = r.GrandparentTitle + " · " + title
		}
		p := float64(r.Progress) / 100
		if r.Progress == 0 {
			p = float64(r.LegacyProgress) / 100
		}
		if r.Offset != nil && r.Duration > 0 {
			p = float64(*r.Offset / r.Duration)
		}
		items = append(items, plugin.Item{Title: m.text(title), Subtitle: "a user", Progress: progress(p)})
	}
	counts := []plugin.Item{{Label: "Streams", Value: fmt.Sprintf("%.0f", *data.StreamCount)}, {Label: "Transcodes", Value: fmt.Sprintf("%.0f", data.Transcodes)}, {Label: "Total bandwidth", Value: fmt.Sprintf("%.1f Mbps", float64(data.Bandwidth)/1000)}}
	return items, counts, nil
}
func (m *Media) library(ctx context.Context, v *plugin.View) {
	rows := []map[string]any{}
	sections := []plugin.Block{}
	for _, s := range m.services {
		serviceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		section := plugin.Block{Type: "section", Title: s.name, Blocks: []plugin.Block{}}
		switch s.id {
		case "sonarr", "radarr":
			imported, err := m.history(serviceCtx, s)
			if err != nil {
				section.Blocks = append(section.Blocks, notice(err))
			} else {
				rows = append(rows, imported...)
			}
			stats, err := m.arrStats(serviceCtx, s)
			if err != nil {
				section.Blocks = append(section.Blocks, notice(err))
			} else {
				section.Blocks = append(section.Blocks, stats)
			}
		case "tautulli":
			stats, err := m.tautulliStats(serviceCtx, s)
			if err != nil {
				section.Blocks = append(section.Blocks, notice(err))
			} else {
				section.Blocks = append(section.Blocks, stats)
			}
		case "overseerr":
			section.Blocks = m.requests(serviceCtx, s)
		}
		cancel()
		sections = append(sections, section)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i]["time"].(string) > rows[j]["time"].(string) })
	if len(rows) > 50 {
		rows = rows[:50]
	}
	v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Recently imported", Empty: "No recent imports", Columns: []plugin.Column{{Key: "title", Label: "Title", Format: "text"}, {Key: "quality", Label: "Quality", Format: "text"}, {Key: "size", Label: "Size", Format: "bytes", Align: "right"}, {Key: "time", Label: "Imported", Format: "time"}}, Rows: rows})
	v.Blocks = append(v.Blocks, sections...)
}
func (m *Media) tautulliStats(ctx context.Context, s service) (plugin.Block, error) {
	var libraries []struct {
		Count       *number
		SectionType string `json:"section_type"`
	}
	if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_libraries"}}, &libraries, true); err != nil {
		return plugin.Block{}, err
	}
	if libraries == nil {
		return plugin.Block{}, errors.New("Tautulli: incomplete libraries")
	}
	var movies, shows number
	for _, l := range libraries {
		if l.Count == nil {
			return plugin.Block{}, errors.New("Tautulli: incomplete library counts")
		}
		if l.SectionType == "movie" {
			movies += *l.Count
		}
		if l.SectionType == "show" {
			shows += *l.Count
		}
	}
	return plugin.Block{Type: "stats", Items: []plugin.Item{{Label: "Libraries", Value: strconv.Itoa(len(libraries))}, {Label: "Movies", Value: fmt.Sprintf("%.0f", movies)}, {Label: "Series", Value: fmt.Sprintf("%.0f", shows)}}}, nil
}
func (m *Media) requests(ctx context.Context, s service) []plugin.Block {
	blocks := []plugin.Block{}
	var counts struct{ Pending, Movie, TV *int }
	if err := m.get(ctx, s, "/api/v1/request/count", nil, &counts, true); err != nil {
		blocks = append(blocks, notice(err))
	} else if counts.Pending == nil || counts.Movie == nil || counts.TV == nil {
		blocks = append(blocks, notice(errors.New("Overseerr: incomplete request counts")))
	} else {
		blocks = append(blocks, plugin.Block{Type: "stats", Items: []plugin.Item{{Label: "Movie requests", Value: strconv.Itoa(*counts.Movie)}, {Label: "Series requests", Value: strconv.Itoa(*counts.TV)}, {Label: "Pending", Value: strconv.Itoa(*counts.Pending)}}})
	}
	var page struct {
		PageInfo struct{ Results *int }
		Results  []struct {
			Type, CreatedAt string
			Status          int
			Media           struct {
				TMDBID int `json:"tmdbId"`
			}
		}
	}
	err := m.get(ctx, s, "/api/v1/request", url.Values{"take": {"50"}, "skip": {"0"}, "filter": {"pending"}, "sort": {"added"}}, &page, true)
	if err != nil {
		return append(blocks, notice(err))
	}
	if page.PageInfo.Results == nil || page.Results == nil {
		return append(blocks, notice(errors.New("Overseerr: incomplete requests")))
	}
	items := []plugin.Item{}
	failed := false
	for _, r := range page.Results {
		if r.Status != 1 {
			continue
		}
		kind := r.Type
		if (kind != "movie" && kind != "tv") || r.Media.TMDBID <= 0 {
			failed = true
			continue
		}
		var details struct{ Title, Name string }
		err := m.get(ctx, s, "/api/v1/"+kind+"/"+strconv.Itoa(r.Media.TMDBID), nil, &details, true)
		title := details.Title
		if title == "" {
			title = details.Name
		}
		if err != nil || title == "" {
			failed = true
			title = "Title unavailable"
		}
		at, err := time.Parse(time.RFC3339, r.CreatedAt)
		if err != nil {
			failed = true
			continue
		}
		items = append(items, plugin.Item{Title: m.text(title), Meta: at.UTC().Format(time.RFC3339)})
	}
	blocks = append(blocks, list("Pending requests", "No pending requests", items))
	if failed {
		blocks = append(blocks, notice(errors.New("Overseerr: some request details are unavailable")))
	}
	if *page.PageInfo.Results > 50 {
		blocks = append(blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Showing the latest 50 pending requests."})
	}
	return blocks
}
