package media

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

func (m *Media) tvMedia(ctx context.Context) any {
	now := m.host.Now().UTC().Format(time.RFC3339)
	dl := map[string]any{"checked_at": now, "sonarr": "unconfigured", "radarr": "unconfigured", "titles": []any{}, "truncated": 0}
	px := map[string]any{"checked_at": now, "status": "unconfigured", "streams": nil, "direct_play": nil, "direct_stream": nil, "transcode": nil}
	upcoming := map[string]any{"checked_at": now, "sonarr": "unconfigured", "radarr": "unconfigured", "status": "unconfigured", "episodes": []any{}, "until": m.host.Now().AddDate(0, 0, m.days).UTC().Format(time.RFC3339), "truncated": 0}
	titles := []any{}
	events := []any{}
	for _, s := range m.services {
		if s.id == "tautulli" {
			px["status"] = "unavailable"
			var a struct {
				StreamCount *number `json:"stream_count"`
				Sessions    []struct {
					TranscodeDecision string `json:"transcode_decision"`
				} `json:"sessions"`
			}
			if m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_activity"}}, &a, true) == nil && a.StreamCount != nil && a.Sessions != nil && int(*a.StreamCount) == len(a.Sessions) {
				direct, stream, transcode := 0, 0, 0
				valid := true
				for _, v := range a.Sessions {
					switch v.TranscodeDecision {
					case "direct play":
						direct++
					case "copy":
						stream++
					case "transcode":
						transcode++
					default:
						valid = false
					}
				}
				if !valid {
					continue
				}
				px["status"] = "ok"
				px["streams"] = int(*a.StreamCount)
				px["direct_play"] = direct
				px["direct_stream"] = stream
				px["transcode"] = transcode
			}
			continue
		}
		if s.id != "sonarr" && s.id != "radarr" {
			continue
		}
		dl[s.id] = "unavailable"
		rows, err := m.queue(ctx, s)
		if err == nil {
			dl[s.id] = "ok"
			titles = append(titles, m.tvQueueTitles(s.id, rows)...)
		}
		if s.id == "sonarr" {
			upcoming["status"] = "unavailable"
			var rows []struct {
				ID, SeasonNumber, EpisodeNumber int
				Title, AirDateUTC               string
				Series                          *mediaTitle
			}
			start := m.host.Now().UTC()
			end := start.AddDate(0, 0, m.days)
			err := m.get(ctx, s, "/api/v3/calendar", url.Values{"start": {start.Truncate(time.Hour).Format(time.RFC3339)}, "end": {end.Format(time.RFC3339)}, "includeSeries": {"true"}, "unmonitored": {"false"}}, &rows, true)
			if err == nil && rows != nil {
				upcoming["status"] = "ok"
				for _, v := range rows {
					at, e := time.Parse(time.RFC3339, v.AirDateUTC)
					if e != nil || at.Before(start) || !at.Before(end) || v.Series == nil || v.ID <= 0 {
						continue
					}
					events = append(events, map[string]any{"id": v.ID, "series": m.text(v.Series.Title), "title": m.text(v.Title), "season": v.SeasonNumber, "episode": v.EpisodeNumber, "air_at": at.UTC().Format(time.RFC3339)})
				}
			}
		}
	}
	if len(titles) > 40 {
		dl["truncated"] = len(titles) - 40
		titles = titles[:40]
	}
	if len(events) > 40 {
		upcoming["truncated"] = len(events) - 40
		events = events[:40]
	}
	dl["titles"] = titles
	upcoming["episodes"] = events
	return map[string]any{"checked_at": now, "downloads": dl, "plex": px, "upcoming": upcoming}
}

// Aggregate queue episodes into library titles, counting a shared season-pack
// download once for byte progress. Preserve unavailable totals as null.
func (m *Media) tvQueueTitles(kind string, rows []queueItem) []any {
	type group struct {
		title string
		id    int
		rows  []queueItem
	}
	groups := []*group{}
	index := map[string]*group{}
	for _, r := range rows {
		title, id := r.title()
		k := fmt.Sprint(id)
		if id <= 0 {
			k = fmt.Sprint("unknown-", r.ID)
		}
		g := index[k]
		if g == nil {
			g = &group{title: title, id: id}
			index[k] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	out := []any{}
	for _, g := range groups {
		row := tvQueueSummary(g.rows)
		row["kind"] = kind
		row["id"] = g.id
		row["title"] = m.text(g.title)
		seasons := []any{}
		order := []int{}
		bySeason := map[int][]queueItem{}
		for _, r := range g.rows {
			if r.Episode != nil && r.Episode.SeasonNumber > 0 {
				n := r.Episode.SeasonNumber
				if _, ok := bySeason[n]; !ok {
					order = append(order, n)
				}
				bySeason[n] = append(bySeason[n], r)
			}
		}
		for _, n := range order {
			v := tvQueueSummary(bySeason[n])
			v["season"] = n
			seasons = append(seasons, v)
		}
		if len(seasons) > 0 {
			row["seasons"] = seasons
		}
		out = append(out, row)
	}
	return out
}
func tvQueueSummary(rows []queueItem) map[string]any {
	state := "queued"
	rank := map[string]int{"queued": 0, "paused": 1, "waiting": 2, "importing": 3, "downloading": 4, "attention": 5, "failed": 6}
	seen := map[string]bool{}
	size, left := 0.0, 0.0
	seconds := int64(0)
	for _, r := range rows {
		st := "downloading"
		switch {
		case strings.EqualFold(r.Status, "failed") || strings.EqualFold(r.TrackedDownloadState, "downloadFailed"):
			st = "failed"
		case r.warning():
			st = "attention"
		case strings.EqualFold(r.TrackedDownloadState, "importing"):
			st = "importing"
		case strings.EqualFold(r.Status, "completed") || strings.EqualFold(r.TrackedDownloadState, "importPending"):
			st = "waiting"
		case strings.EqualFold(r.Status, "paused"):
			st = "paused"
		case strings.EqualFold(r.Status, "queued") || strings.EqualFold(r.Status, "delay"):
			st = "queued"
		}
		if rank[st] > rank[state] {
			state = st
		}
		k := r.DownloadID
		if k == "" {
			k = fmt.Sprint(r.ID)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		if r.Size > 0 {
			size += r.Size
			left += max(0, min(r.Size, r.Sizeleft))
		}
		if d := int64(eta(r.Timeleft).Seconds()); d > seconds {
			seconds = d
		}
	}
	var percent, secondsValue any
	if size > 0 {
		percent = int(100 * (1 - left/size))
	}
	if seconds > 0 {
		secondsValue = seconds
	}
	return map[string]any{"state": state, "percent": percent, "eta_seconds": secondsValue, "imported": nil, "total": nil}
}
