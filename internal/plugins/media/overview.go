package media

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/overview"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (m *Media) Overview(ctx context.Context) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 2800*time.Millisecond)
	defer cancel()
	out := map[string]any{}
	arr := map[string]any{"sonarr": overview.Unavailable{Status: "not_configured", Error: "Sonarr is not configured"}, "radarr": overview.Unavailable{Status: "not_configured", Error: "Radarr is not configured"}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range m.services {
		if s.id != "tautulli" && s.id != "sonarr" && s.id != "radarr" {
			continue
		}
		wg.Add(1)
		go func(s service) {
			defer wg.Done()
			if s.id == "tautulli" {
				value := m.overviewStreams(ctx, s)
				mu.Lock()
				out["streams"] = value
				mu.Unlock()
			} else {
				value := m.overviewQueue(ctx, s)
				mu.Lock()
				arr[s.id] = value
				out["arr"] = arr
				mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	return out
}

func (m *Media) overviewStreams(ctx context.Context, s service) any {
	var data struct {
		StreamCount  *number `json:"stream_count"`
		DirectPlay   number  `json:"stream_count_direct_play"`
		DirectStream number  `json:"stream_count_direct_stream"`
		Transcode    number  `json:"stream_count_transcode"`
		Bandwidth    number  `json:"total_bandwidth"`
		Sessions     []struct {
			User             string  `json:"friendly_name"`
			Title            string  `json:"title"`
			GrandparentTitle string  `json:"grandparent_title"`
			State            string  `json:"state"`
			Decision         string  `json:"transcode_decision"`
			Quality          string  `json:"quality_profile"`
			Resolution       string  `json:"video_resolution"`
			Duration         number  `json:"duration"`
			Offset           *number `json:"view_offset"`
		} `json:"sessions"`
	}
	if err := m.get(ctx, s, "/api/v2", url.Values{"cmd": {"get_activity"}}, &data, true); err != nil || data.StreamCount == nil || data.Sessions == nil {
		return overview.Unavailable{Status: "unavailable", Error: "Tautulli activity unavailable"}
	}
	out := overview.Streams{Status: "ok", Count: int(*data.StreamCount), Direct: int(data.DirectPlay + data.DirectStream), Transcode: int(data.Transcode), BandwidthKbps: int(data.Bandwidth), Sessions: []overview.Session{}}
	for _, v := range data.Sessions {
		title := v.Title
		if v.GrandparentTitle != "" {
			title = v.GrandparentTitle + ": " + title
		}
		row := overview.Session{User: m.text(v.User), Title: m.text(title), State: m.text(v.State), Decision: m.text(v.Decision), Quality: m.text(v.Quality)}
		if row.Quality == "" {
			row.Quality = m.text(v.Resolution)
		}
		if v.Duration > 0 && v.Offset != nil {
			pct := max(0, min(100, 100*float64(*v.Offset)/float64(v.Duration)))
			row.ProgressPct = &pct
		}
		out.Sessions = append(out.Sessions, row)
	}
	if out.Count != len(out.Sessions) {
		return overview.Unavailable{Status: "unavailable", Error: "Tautulli activity incomplete"}
	}
	return out
}

func (m *Media) overviewQueue(ctx context.Context, s service) any {
	rows, err := m.queue(ctx, s)
	if err != nil {
		return map[string]any{"status": "unavailable", "error": s.name + " queue unavailable"}
	}
	problems := 0
	for _, row := range rows {
		if row.warning() || strings.EqualFold(row.TrackedDownloadState, "stalled") || strings.TrimSpace(row.ErrorMessage) != "" {
			problems++
		}
	}
	return overview.ArrQueue{Status: "ok", Total: len(rows), Problems: problems, Attention: problems}
}

var _ plugin.OverviewSource = (*Media)(nil)
