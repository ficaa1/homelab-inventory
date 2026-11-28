package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
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
	case "proxy", "account", "sign-in":
		err = p.proxyView(ctx, &v, name, params)
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
			if data.Host.Name == host {
				p.forceActions(&v, data)
			}
		}
	case "skills":
		v.Title = "Skills"
		filters := skillFilters{source: params.Get("source"), mode: params.Get("mode"), origin: params.Get("origin")}
		if !filters.valid(params) {
			v.Blocks = []plugin.Block{notice("info", "Choose a filter from the Skills view.")}
			break
		}
		var data skillsResponse
		err = p.get(ctx, filters.path(), p.readToken, &data)
		if err == nil {
			v.Blocks = p.skills(data, filters)
			p.sourceActions(&v, data, filters)
		}
	case "skill":
		v.Title = "Skill"
		id := params.Get("id")
		filters := skillFilters{source: params.Get("source"), origin: params.Get("origin")}
		if len(params["id"]) != 1 || !skillID.MatchString(id) || !filters.validSource(params) {
			v.Blocks = []plugin.Block{notice("info", "Open a skill from Skills.")}
			break
		}
		var data skillsResponse
		err = p.get(ctx, filters.path(), p.readToken, &data)
		if err == nil {
			p.skillDetail(&v, data, id)
		}
	case "trials":
		v.Title = "Trials"
		var data trialsResponse
		err = p.get(ctx, "trials", p.readToken, &data)
		if err == nil {
			v.Blocks = p.trials(data)
			p.trialActions(&v, data)
		}
	default:
		return v, plugin.ErrNotFound
	}
	if err != nil {
		return v, err
	}
	actionData, _ := json.Marshal(v.Actions)
	v.Blocks = fitBlocks(v.Blocks, len(actionData))
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

func splitBlocks(blocks []plugin.Block) []plugin.Block {
	const limit = 500
	var result []plugin.Block
	for _, b := range blocks {
		if b.Type == "section" {
			b.Blocks = splitBlocks(b.Blocks)
		}
		for {
			chunk := b
			chunk.Rows = b.Rows[:min(limit, len(b.Rows))]
			chunk.Items = b.Items[:min(limit, len(b.Items))]
			b.Rows = b.Rows[len(chunk.Rows):]
			b.Items = b.Items[len(chunk.Items):]
			result = append(result, chunk)
			if len(b.Rows)+len(b.Items) == 0 {
				break
			}
		}
	}
	return result
}

func blockBytes(b plugin.Block) int {
	data, _ := json.Marshal(b)
	return len(data)
}

// Keep the leading rows or children that fit the space left in the view.
func fitBlock(b plugin.Block, slots, bytes int) (plugin.Block, bool) {
	if slots <= 0 || bytes <= 0 {
		return b, false
	}
	if 1+len(b.Blocks) <= slots && blockBytes(b) <= bytes {
		return b, true
	}
	if b.Type == "section" {
		children := b.Blocks
		b.Blocks = nil
		slots--
		bytes -= blockBytes(b) + len(`,"blocks":[]`)
		for _, child := range children {
			fit, ok := fitBlock(child, slots, bytes)
			if !ok {
				break
			}
			b.Blocks = append(b.Blocks, fit)
			slots--
			bytes -= blockBytes(fit) + 1
			if len(fit.Rows) < len(child.Rows) {
				break
			}
		}
		return b, len(b.Blocks) > 0
	}
	if len(b.Items) > 0 {
		items := b.Items
		n := sort.Search(len(items), func(i int) bool { b.Items = items[:i+1]; return blockBytes(b) > bytes })
		b.Items = items[:n]
		return b, n > 0
	}
	rows := sort.Search(len(b.Rows), func(i int) bool {
		prefix := b
		prefix.Rows = prefix.Rows[:i+1]
		return blockBytes(prefix) > bytes
	})
	b.Rows = b.Rows[:rows]
	return b, rows > 0
}

// A legal 4 MiB hub response can exceed the 1 MiB dashboard limit.
func fitBlocks(blocks []plugin.Block, actionBytes int) []plugin.Block {
	chunks := splitBlocks(blocks)
	var result []plugin.Block
	slots, bytes := 50, plugin.MaxViewBytes-2048-actionBytes // Leave room for the view envelope and cut notice.
	for i, b := range chunks {
		cost, size := 1+len(b.Blocks), blockBytes(b)+1
		if size <= bytes && (cost < slots || (cost == slots && i == len(chunks)-1)) {
			result = append(result, b)
			slots -= cost
			bytes -= size
			continue
		}
		fit, ok := fitBlock(b, slots-1, bytes-1)
		msg := "More results were cut to fit this view."
		if ok {
			result = append(result, fit)
		} else if b.Type == "section" {
			msg = "More trials were cut to fit this view."
		}
		return append(result, notice("info", msg))
	}
	return result
}

func join(parts ...string) string { return strings.Join(parts, " · ") }
