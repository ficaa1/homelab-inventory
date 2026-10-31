package media

import (
	"context"
	"net/url"
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
				StreamCount number
				Sessions    []struct{ TranscodeDecision string }
			}
			if m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_activity"}}, &a, true) == nil {
				direct, stream, transcode := 0, 0, 0
				for _, v := range a.Sessions {
					switch v.TranscodeDecision {
					case "direct play":
						direct++
					case "copy":
						stream++
					case "transcode":
						transcode++
					}
				}
				px["status"] = "ok"
				px["streams"] = int(a.StreamCount)
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
			for _, r := range rows {
				title, id := r.title()
				percent := any(nil)
				if r.Size > 0 {
					percent = int(100 * (1 - max(0, min(r.Size, r.Sizeleft))/r.Size))
				}
				state := "downloading"
				if r.warning() {
					state = "attention"
				}
				var seconds any
				if d := eta(r.Timeleft); d > 0 {
					seconds = int64(d.Seconds())
				}
				titles = append(titles, map[string]any{"kind": s.id, "id": id, "title": m.text(title), "state": state, "percent": percent, "eta_seconds": seconds, "imported": nil, "total": nil})
			}
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
