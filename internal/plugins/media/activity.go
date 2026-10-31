package media

import (
	"context"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/media/activity"
	"net/url"
)

func (m *Media) libraryActivity(ctx context.Context) plugin.View {
	v := plugin.View{Title: "Library activity", UpdatedAt: m.host.Now(), RefreshSeconds: 10, Blocks: []plugin.Block{}}
	feeds := map[string]activity.Feed{}
	for _, id := range []string{"sonarr", "radarr"} {
		feed := activity.Feed{Status: activity.StatusUnconfigured}
		for _, s := range m.services {
			if s.id != id {
				continue
			}
			a, e := activity.Read(ctx, s.name, id == "sonarr", func(ctx context.Context, path string, q url.Values, out any) error {
				return m.get(ctx, s, "/api/v3"+path, q, out, true)
			}, m.text)
			feed.Status = activity.StatusUnavailable
			if e == nil {
				feed.Status = activity.StatusOK
				feed.Activity = a
			}
		}
		feeds[id] = feed
	}
	a := activity.Build(m.host.Now(), feeds["sonarr"], feeds["radarr"])
	for _, id := range []string{"sonarr", "radarr"} {
		if feeds[id].Status != activity.StatusOK {
			v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "warn", Text: id + " activity " + feeds[id].Status})
		}
	}
	items := []plugin.Item{}
	for _, title := range a.Titles {
		meta := title.State
		if title.Imported != nil && title.Total != nil {
			meta += fmt.Sprintf(" · %d/%d imported", *title.Imported, *title.Total)
		}
		item := plugin.Item{Title: m.text(title.Title), Subtitle: title.Kind, Meta: meta}
		if title.Percent != nil {
			item.Progress = &plugin.Progress{Value: float64(*title.Percent) / 100, Label: fmt.Sprintf("%d%%", *title.Percent)}
		}
		items = append(items, item)
		for _, s := range title.Seasons {
			meta := s.State
			if s.Imported != nil && s.Total != nil {
				meta += fmt.Sprintf(" · %d/%d imported", *s.Imported, *s.Total)
			}
			item := plugin.Item{Title: fmt.Sprintf("%s · Season %d", m.text(title.Title), s.Season), Meta: meta}
			if s.Percent != nil {
				item.Progress = &plugin.Progress{Value: float64(*s.Percent) / 100, Label: fmt.Sprintf("%d%%", *s.Percent)}
			}
			items = append(items, item)
		}
	}
	v.Blocks = append(v.Blocks, list("Import progress", "No active imports", items))
	return v
}
