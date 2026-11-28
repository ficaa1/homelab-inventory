package fleet

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (d *disk) ratio() (float64, bool) {
	if d == nil || d.Total <= 0 {
		return 0, false
	}
	return float64(d.Free) / float64(d.Total), true
}
func (d *disk) label() string {
	r, ok := d.ratio()
	if !ok {
		return "Disk not reported"
	}
	return fmt.Sprintf("%.0f GB free of %.0f GB (%.0f%%)", float64(d.Free)/1e9, float64(d.Total)/1e9, r*100)
}
func (d *disk) status(pol *policy) plugin.Badge {
	r, ok := d.ratio()
	switch {
	case !ok:
		return plugin.Badge{}
	case r < *pol.CritFree:
		return badge("Disk critical", "crit")
	case r < *pol.WarnFree:
		return badge("Disk low", "warn")
	default:
		return plugin.Badge{}
	}
}
func (c *hostCounts) attention() int {
	if c == nil {
		return 0
	}
	return c.Drift + c.Refused + c.Broken
}
func (h hostSummary) pending(head string) bool { return h.Commit != nil && *h.Commit != head }
func (h hostSummary) stale(env hostEnvelope) bool {
	since := h.Seen
	if since == nil {
		since = h.ProfileSince
	}
	return since == nil || env.Generated.Sub(*since).Seconds() > float64(*env.Policy.StaleSeconds)
}
func (h hostSummary) status(env hostEnvelope) plugin.Badge {
	d := h.Disk.status(env.Policy)
	switch {
	case h.stale(env):
		return badge("Stale", "warn")
	case h.Seen == nil:
		return badge("Waiting for first report", "muted")
	case d.Tone == "crit":
		return d
	case h.Counts.attention() > 0 || d.Tone == "warn":
		return badge("Attention", "warn")
	case h.pending(env.Head):
		return badge("Pending sync", "info")
	default:
		return badge("OK", "ok")
	}
}

func (p *Fleet) hostList(data hostsResponse, counts bool) plugin.Block {
	list := plugin.Block{Type: "list", Title: "Hosts", Empty: "No hosts have a profile yet"}
	if data.Hosts == nil {
		list.Empty = "Hosts not reported"
	}
	for _, h := range data.Hosts {
		status := h.status(data.hostEnvelope)
		item := plugin.Item{Title: p.word(h.Name), Subtitle: "Never reported", Tone: status.Tone, Meta: h.Disk.label()}
		if hostID.MatchString(h.Name) && item.Title == h.Name {
			item.Link = &plugin.Link{View: "host", Params: map[string]string{"name": h.Name}}
		}
		if h.Seen != nil {
			item.Subtitle = "Reported " + age(data.Generated, *h.Seen)
			if h.Version != nil {
				item.Subtitle = join(item.Subtitle, "fleet "+p.word(*h.Version))
			}
		}
		if c := h.Counts; c != nil {
			if c.Drift > 0 {
				item.Badges = append(item.Badges, badge(fmt.Sprintf("%d drift", c.Drift), "warn"))
			}
			if c.Refused > 0 {
				item.Badges = append(item.Badges, badge(fmt.Sprintf("%d refused", c.Refused), "warn"))
			}
			if c.Broken > 0 {
				item.Badges = append(item.Badges, badge(plural(c.Broken, "broken link", "broken links"), "warn"))
			}
			if counts {
				item.Meta = join(item.Meta, plural(c.Updates, "update", "updates"), fmt.Sprintf("%d pending", c.Pending))
			}
		} else if counts {
			item.Meta = join(item.Meta, "Counts not reported")
		}
		if h.pending(data.Head) {
			item.Badges = append(item.Badges, badge("Pending sync", "info"))
		}
		switch status.Text {
		case "Stale", "OK":
			item.Badges = append(item.Badges, status)
		case "Waiting for first report":
			item.Subtitle = status.Text
		}
		if d := h.Disk.status(data.Policy); d.Text != "" {
			item.Badges = append(item.Badges, d)
		}
		list.Items = append(list.Items, item)
	}
	return list
}

func (data hostsResponse) hostListNotices() []plugin.Block {
	if data.Truncated {
		return []plugin.Block{notice("info", fmt.Sprintf("The hub sent only the first %d hosts.", len(data.Hosts)))}
	}
	return nil
}

func (p *Fleet) hosts(data hostsResponse) ([]plugin.Block, error) {
	if !data.Policy.complete() {
		return nil, errResponse
	}
	counts := table("Counts", "No host counts to show", col("host", "Host"), col("status", "Status"), number("drift", "Drift"), number("refused", "Refused"), number("broken", "Broken links"), number("pending", "Pending"), number("updates", "Updates"), plugin.Column{Key: "free", Label: "Free", Format: "percent", Align: "right"})
	for _, h := range data.Hosts {
		row := map[string]any{"host": p.word(h.Name), "status": h.status(data.hostEnvelope).Text, "drift": "—", "refused": "—", "broken": "—", "pending": "—", "updates": "—", "free": "—"}
		if c := h.Counts; c != nil {
			row["drift"], row["refused"], row["broken"], row["pending"], row["updates"] = c.Drift, c.Refused, c.Broken, c.Pending, c.Updates
		}
		if r, ok := h.Disk.ratio(); ok {
			row["free"] = r
		}
		counts.Rows = append(counts.Rows, row)
	}
	return append(data.hostListNotices(), p.hostList(data, true), counts), nil
}

var receiptStates = map[string]plugin.Badge{"done": badge("Done", "ok"), "working": badge("Working", "info"), "failed": badge("Failed", "crit")}
var actionWords = map[string]string{"add_trial_source": "Add trial source", "set_skill_mode": "Set skill mode", "end_trial": "End trial", "remove_source": "Remove source", "force_file": "Force file",
	"proxy_set_strategy": "Change routing strategy", "proxy_set_account_routing": "Change account routing", "proxy_set_account_enabled": "Enable or disable account",
	"proxy_refresh_account": "Renew access", "proxy_reset_cooldown": "Reset cooldown"}
var reauthChanges = map[string]string{"proxy_reauth_start": "Sign-in started for ", "proxy_reauth_finish": "Sign-in finished for "}

func (p *Fleet) overview(ctx context.Context, data overviewResponse) ([]plugin.Block, error) {
	if !data.Policy.complete() {
		return nil, errResponse
	}
	var stale, waiting, attention, affected, pending, reportedCounts, knownCommits int
	cost := table("Model-invoked skills", "No context cost reported", col("host", "Host"), col("harness", "Harness"), number("tokens", "Tokens (est.)"), number("skills", "Skills"), number("chars", "Characters"))
	var skipped []string
	for _, h := range data.Hosts {
		if h.stale(data.hostEnvelope) {
			stale++
		} else if h.Seen == nil {
			waiting++
		}
		if h.Counts != nil {
			reportedCounts++
		}
		if n := h.Counts.attention(); n > 0 {
			attention += n
			affected++
		}
		if h.Commit != nil {
			knownCommits++
		}
		if h.pending(data.Head) {
			pending++
		}
		if h.Cost == nil {
			skipped = append(skipped, p.word(h.Name))
		}
		for _, c := range h.Cost {
			cost.Rows = append(cost.Rows, map[string]any{"host": p.word(h.Name), "harness": p.word(c.Harness), "tokens": c.Tokens, "skills": c.Skills, "chars": c.Chars})
		}
	}
	running, due, soon := 0, 0, 0
	for _, t := range data.Trials {
		switch t.State {
		case "running":
			running++
			if days, ok := daysUntil(data.Generated, t.Ends); ok && days >= 0 && days <= *data.Policy.DueSoon {
				soon++
			}
		case "due":
			due++
		}
	}
	hosts := plugin.Item{Label: "Hosts", Value: strconv.Itoa(len(data.Hosts)), Hint: "All reporting"}
	if stale > 0 {
		hosts.Hint = fmt.Sprintf("%d stale", stale)
		hosts.Tone = "warn"
	}
	if waiting > 0 {
		hint := fmt.Sprintf("%d waiting for first report", waiting)
		if stale > 0 {
			hint = join(hosts.Hint, hint)
		}
		hosts.Hint = hint
	}
	att := plugin.Item{Label: "Attention", Value: strconv.Itoa(attention), Hint: "Nothing to fix"}
	if attention > 0 {
		att.Hint = "on " + plural(affected, "host", "hosts")
		att.Tone = "warn"
	}
	sync := plugin.Item{Label: "Pending sync", Value: strconv.Itoa(pending)}
	if pending > 0 {
		sync.Tone = "info"
	}
	if data.Hosts == nil {
		hosts.Value, hosts.Hint = "Not reported", ""
	}
	if (len(data.Hosts) > 0 || data.Hosts == nil) && reportedCounts == 0 {
		att.Value, att.Hint = "Not reported", ""
	} else if reportedCounts < len(data.Hosts) {
		att.Hint = fmt.Sprintf("from %d of %d hosts", reportedCounts, len(data.Hosts))
	}
	if (len(data.Hosts) > 0 || data.Hosts == nil) && knownCommits == 0 {
		sync.Value = "Not reported"
	}
	trials := plugin.Item{Label: "Trials", Value: strconv.Itoa(running), Hint: fmt.Sprintf("%d end within %s", soon, plural(*data.Policy.DueSoon, "day", "days"))}
	if due > 0 {
		trials.Hint = fmt.Sprintf("%d past end date", due)
		if soon > 0 {
			trials.Hint = join(trials.Hint, fmt.Sprintf("%d end within %s", soon, plural(*data.Policy.DueSoon, "day", "days")))
		}
		trials.Tone = "warn"
	} else if soon == 0 {
		trials.Hint = "None end within " + plural(*data.Policy.DueSoon, "day", "days")
	}
	if data.Trials == nil {
		trials.Value, trials.Hint = "Not reported", ""
	}
	blocks := append(data.hostListNotices(), plugin.Block{Type: "stats", Items: []plugin.Item{hosts, att, trials, sync}}, p.hostList(data.hostsResponse, false), cost)
	if len(skipped) > 0 {
		blocks = append(blocks, notice("info", "Context cost not reported for "+strings.Join(skipped, ", ")))
	}
	recent := plugin.Block{Type: "list", Title: "Recent changes", Empty: "No dashboard changes yet"}
	if data.Recent == nil {
		recent.Empty = "Recent changes not reported"
	}
	labels := p.recentAccountLabels(ctx, data)
	for _, r := range data.Recent {
		item := plugin.Item{Subtitle: age(data.Generated, r.Created)}
		if change := reauthChanges[r.Action]; change != "" {
			label := labels[r.Account]
			if label == "" {
				label = "an account"
			}
			item.Title = change + label
			state := receiptStates[r.State]
			if state.Text == "" {
				state = badge("Unknown", "muted")
			}
			item.Badges = []plugin.Badge{state}
		} else {
			item.Title = p.clean(r.Summary, 400)
			item.Subtitle = join(p.label(r.Action, actionWords), item.Subtitle)
			item.Badges = []plugin.Badge{p.enum(r.State, receiptStates)}
		}
		if r.Pending > 0 {
			item.Badges = append(item.Badges, badge("Pending on "+plural(r.Pending, "host", "hosts"), "info"))
		}
		recent.Items = append(recent.Items, item)
	}
	return append(blocks, recent), nil
}

func (p *Fleet) recentAccountLabels(ctx context.Context, data overviewResponse) map[string]string {
	for _, r := range data.Recent {
		if reauthChanges[r.Action] == "" || !proxyID.MatchString(r.Account) {
			continue
		}
		var proxy proxyResponse
		if err := p.get(ctx, "proxy", p.readToken, &proxy); err != nil || proxy.Proxy.State != "ok" {
			return nil
		}
		labels := make(map[string]string, len(proxy.Accounts))
		for _, account := range proxy.Accounts {
			labels[account.ID] = p.word(account.Label)
		}
		return labels
	}
	return nil
}

var itemStates = map[string]string{"pending": "Pending", "drift": "Drift", "refused": "Refused", "broken_link": "Broken link", "remove": "To remove"}
var applyWords = map[string]string{"auto": "fleet sync", "manual": "fleet on the host", "report": "Another tool"}
var forceStates = map[string]plugin.Badge{"queued": badge("Queued", "info"), "applied": badge("Applied", "ok"), "stale": badge("Stale, not applied", "warn"), "failed": badge("Failed", "crit")}

func (p *Fleet) label(raw string, words map[string]string) string {
	if s, ok := words[raw]; ok {
		return s
	}
	return p.word(raw)
}

func (p *Fleet) providerEndpoint(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "Not a URL"
	}
	path := u.EscapedPath()
	for _, token := range []string{p.readToken, p.operatorToken} {
		if token != "" && (strings.Contains(u.Path, token) || strings.Contains(path, token)) {
			path = ""
			break
		}
	}
	return p.word(u.Scheme + "://" + u.Host + path)
}

func (p *Fleet) hostDetail(data hostResponse) ([]plugin.Block, error) {
	h, r := data.Host, data.Report
	if !data.Policy.complete() {
		return nil, errResponse
	}
	status := h.status(data.hostEnvelope)
	if r == nil {
		return []plugin.Block{notice(status.Tone, status.Text+" · "+p.word(h.Name)+" hasn't reported yet. Run fleet sync on it.")}, nil
	}
	last := plugin.Item{Label: "Last report", Value: "Not reported", Hint: join(p.word(h.Name), status.Text), Tone: status.Tone}
	if h.Seen != nil {
		last.Value = age(data.Generated, *h.Seen)
	}
	synced := plugin.Item{Label: "Synced", Value: "Not reported"}
	if h.Commit != nil {
		commit := []rune(p.word(*h.Commit))
		synced.Value = string(commit[:min(7, len(commit))])
		synced.Hint = "Up to date"
		if h.pending(data.Head) {
			synced.Hint, synced.Tone = "Pending sync", "info"
		}
	}
	free := plugin.Item{Label: "Free disk", Value: "Not reported"}
	if _, ok := h.Disk.ratio(); ok {
		free.Value = fmt.Sprintf("%.0f GB", float64(h.Disk.Free)/1e9)
		b := h.Disk.status(data.Policy)
		ratio, _ := h.Disk.ratio()
		free.Hint = fmt.Sprintf("%.0f%% free of %.0f GB", ratio*100, float64(h.Disk.Total)/1e9)
		if b.Text != "" {
			free.Hint, free.Tone = join(b.Text, free.Hint), b.Tone
		}
	}
	matches := plugin.Item{Label: "Match fleet", Value: "Not reported"}
	if r.OK != nil {
		matches.Value, matches.Hint = strconv.Itoa(*r.OK), "files"
	}
	blocks := []plugin.Block{{Type: "stats", Items: []plugin.Item{last, synced, free, matches}}}
	if c := h.Counts; c != nil {
		switch {
		case c.Refused == 1:
			blocks = append(blocks, notice("warn", "1 file changed on the host and has a newer version waiting. Fleet won't overwrite it. Review the file on the host and keep the hand edit or restore the fleet version."))
		case c.Refused > 1:
			blocks = append(blocks, notice("warn", fmt.Sprintf("%d files changed on the host and have a newer version waiting. Fleet won't overwrite them. Review each file on the host and keep the hand edit or restore the fleet version.", c.Refused)))
		}
		if c.Drift > 0 {
			blocks = append(blocks, notice("warn", plural(c.Drift, "file has", "files have")+" drifted from the fleet version. Review the changes on the host and decide which version to keep."))
		}
		if c.Broken > 0 {
			blocks = append(blocks, notice("warn", plural(c.Broken, "broken link has", "broken links have")+" a missing target. Restore the target or correct the link on the host."))
		}
	}
	items := table("Needs attention", "Nothing needs attention", col("path", "Path"), plugin.Column{Key: "lines", Label: "Lines changed", Align: "right"}, col("harness", "Harness"), col("kind", "Kind"), col("state", "State"), col("detail", "Detail"))
	if r.Items == nil {
		items.Empty = "Attention items not reported"
	}
	for _, item := range r.Items {
		lines := "—"
		if d := item.Diff; d != nil {
			lines = fmt.Sprintf("+%d −%d", d.Added, d.Removed)
		}
		items.Rows = append(items.Rows, map[string]any{"path": p.clean(item.Path, 400), "lines": lines, "harness": p.word(item.Harness), "kind": p.word(item.Kind), "state": p.label(item.State, itemStates), "detail": p.clean(item.Detail, 400)})
	}
	providers := table("Providers", "No providers configured", col("harness", "Harness"), col("provider", "Provider"), col("endpoint", "Endpoint"))
	if r.Providers == nil {
		providers.Empty = "Providers not reported"
	}
	for _, v := range r.Providers {
		providers.Rows = append(providers.Rows, map[string]any{"harness": p.word(v.Harness), "provider": p.word(v.Provider), "endpoint": p.providerEndpoint(v.Endpoint)})
	}
	updates := table("Updates", "Everything is current", col("component", "Component"), col("installed", "Installed"), col("latest", "Latest"), col("apply", "Applied by"))
	if r.Updates == nil {
		updates.Empty = "Updates not reported"
	}
	for _, v := range r.Updates {
		updates.Rows = append(updates.Rows, map[string]any{"component": p.word(v.Component), "installed": p.word(v.Installed), "latest": p.word(v.Latest), "apply": p.label(v.Apply, applyWords)})
	}
	blocks = append(blocks, items, providers, updates)
	if len(r.Requests) > 0 {
		list := plugin.Block{Type: "list", Title: "Force requests"}
		for _, req := range r.Requests {
			list.Items = append(list.Items, plugin.Item{Title: p.clean(req.Item, 400), Subtitle: "Requested " + age(data.Generated, req.Requested), Badges: []plugin.Badge{p.enum(req.State, forceStates)}})
		}
		blocks = append(blocks, list)
	}
	return blocks, nil
}

func (p *Fleet) forceActions(v *plugin.View, data hostResponse) {
	if data.Report == nil || p.operatorToken == "" {
		return
	}
	host := data.Host.Name
	for _, item := range data.Report.Items {
		if !item.Forceable {
			continue
		}
		edit := "The hand edit is lost."
		if d := item.Diff; d != nil {
			edit = fmt.Sprintf("The hand edit (+%d −%d lines) is lost.", d.Added, d.Removed)
		}
		suffix := " on " + host + " with the fleet version? " + edit + " " + host + " applies it at its next sync, and only if the file hasn't changed again."
		confirm := "Overwrite " + p.clean(item.Path, 300-len([]rune("Overwrite "+suffix))) + suffix
		p.offer(v, "force_file", "Overwrite "+p.clean(item.Path, 400), confirm, plugin.Config{"host": host, "item": item.ID, "live_sha256": item.LiveHash}, nil)
		if len(v.Actions) >= 20 {
			return
		}
	}
}
