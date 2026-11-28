package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func New(h plugin.Host) plugin.Plugin { return &Fleet{host: h} }

func (p *Fleet) View(ctx context.Context, name string, params url.Values) (plugin.View, error) {
	v := plugin.View{UpdatedAt: p.host.Now(), RefreshSeconds: 60}
	var err error
	switch name {
	case "overview":
		v.Title = "Overview"
		var data overviewResponse
		err = p.get(ctx, "overview", p.readToken, &data)
		if err == nil {
			v.Blocks, err = p.overview(data)
		}
	case "hosts":
		v.Title = "Hosts"
		var data hostsResponse
		err = p.get(ctx, "hosts", p.readToken, &data)
		if err == nil {
			v.Blocks, err = p.hosts(data)
		}
	case "host":
		v.Title = "Host"
		host := params.Get("name")
		if len(params["name"]) != 1 || !hostID.MatchString(host) {
			v.Blocks = []plugin.Block{notice("info", "Open a host from Hosts or Overview.")}
			break
		}
		var data hostResponse
		err = p.get(ctx, "hosts/"+url.PathEscape(host), p.readToken, &data)
		if he, ok := err.(*hubError); ok && he.status == 404 {
			v.Blocks = []plugin.Block{notice("info", "No host named "+p.word(host)+" reports to this hub.")}
			err = nil
		} else if err == nil {
			v.Title = p.word(data.Host.Name)
			v.Blocks, err = p.hostDetail(data)
		}
	case "skills":
		v.Title = "Skills"
		src, mode := params.Get("source"), params.Get("mode")
		if (params.Has("source") && (len(params["source"]) != 1 || !sourceID.MatchString(src))) || (params.Has("mode") && (len(params["mode"]) != 1 || modes[mode] == "")) {
			v.Blocks = []plugin.Block{notice("info", "Choose a source and mode from the Skills filters.")}
			break
		}
		var data skillsResponse
		err = p.get(ctx, "skills", p.readToken, &data)
		if err == nil {
			v.Blocks = p.skills(data, src, mode)
		}
	case "trials":
		v.Title = "Trials"
		var data trialsResponse
		err = p.get(ctx, "trials", p.readToken, &data)
		if err == nil {
			v.Blocks = p.trials(data)
		}
	default:
		return v, plugin.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	v.Blocks = fitBlocks(v.Blocks)
	return v, nil
}

func notice(tone, text string) plugin.Block {
	return plugin.Block{Type: "notice", Tone: tone, Text: text}
}
func col(key, label string) plugin.Column { return plugin.Column{Key: key, Label: label} }
func number(key, label string) plugin.Column {
	return plugin.Column{Key: key, Label: label, Format: "number", Align: "right"}
}
func table(title, empty string, cols ...plugin.Column) plugin.Block {
	return plugin.Block{Type: "table", Title: title, Empty: empty, Columns: cols}
}
func badge(text, tone string) plugin.Badge { return plugin.Badge{Text: text, Tone: tone} }
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
func age(now, then time.Time) string {
	if then.IsZero() {
		return "Not reported"
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return plural(int(d/(24*time.Hour)), "day", "days") + " ago"
	}
}
func shortDate(s string) string {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return "Not reported"
	}
	return t.Format("2 Jan")
}
func daysUntil(now time.Time, s string) (int, bool) {
	date, err := time.Parse(time.DateOnly, s)
	if err != nil || now.IsZero() {
		return 0, false
	}
	return int(date.Sub(now.UTC().Truncate(24*time.Hour)) / (24 * time.Hour)), true
}
func (p *Fleet) enum(raw string, labels map[string]plugin.Badge) plugin.Badge {
	if b, ok := labels[raw]; ok {
		return b
	}
	return badge(p.word(raw), "muted")
}
func displayCount(n *int) any {
	if n == nil {
		return "No data"
	}
	return *n
}

// Split long tables and lists without losing rows. Bound the complete payload
// too: a legal 4 MiB hub response can expand beyond the dashboard's 1 MiB view.
func fitBlocks(blocks []plugin.Block) []plugin.Block {
	const limit = 500
	var result []plugin.Block
	count, size := 0, 0
	for i, b := range blocks {
		for {
			chunk := b
			chunk.Rows = b.Rows[:min(limit, len(b.Rows))]
			chunk.Items = b.Items[:min(limit, len(b.Items))]
			b.Rows = b.Rows[len(chunk.Rows):]
			b.Items = b.Items[len(chunk.Items):]
			cost := 1 + len(chunk.Blocks)
			data, _ := json.Marshal(chunk)
			last := i == len(blocks)-1 && len(b.Rows)+len(b.Items) == 0
			if count+cost > 50 || (count+cost == 50 && !last) || size+len(data) > plugin.MaxViewBytes-2048 {
				// Keep the leading rows when only the remainder of this table is too large.
				if count < 49 && size+len(data) > plugin.MaxViewBytes-2048 && len(chunk.Rows) > 1 {
					for len(chunk.Rows) > 1 && size+len(data) > plugin.MaxViewBytes-2048 {
						chunk.Rows = chunk.Rows[:len(chunk.Rows)/2]
						data, _ = json.Marshal(chunk)
					}
					if size+len(data) <= plugin.MaxViewBytes-2048 {
						result = append(result, chunk)
					}
				}
				msg := "More results were cut to fit this view."
				if chunk.Type == "section" {
					msg = "More trials were cut to fit this view."
				}
				return append(result, notice("info", msg))
			}
			result = append(result, chunk)
			size += len(data) + 1
			count += cost
			if len(b.Rows)+len(b.Items) == 0 {
				break
			}
		}
	}
	return result
}
func join(parts ...string) string { return strings.Join(parts, " · ") }
