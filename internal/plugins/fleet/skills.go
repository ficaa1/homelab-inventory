package fleet

import (
	"cmp"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

var modes = map[string]string{"model": "Model-invoked", "user": "User-only", "exclude": "Excluded"}
var suggestions = map[string]string{"model": "Keep model-invoked", "user": "Keep user-only", "exclude": "Drop"}

type skillFilters struct{ source, mode, origin string }

func (f skillFilters) valid(params url.Values) bool {
	return (!params.Has("source") || (len(params["source"]) == 1 && sourceID.MatchString(f.source))) &&
		(!params.Has("mode") || (len(params["mode"]) == 1 && modes[f.mode] != "")) &&
		(!params.Has("origin") || (len(params["origin"]) == 1 && (f.origin == "own" || f.origin == "third_party")))
}

func (f skillFilters) values() url.Values {
	params := url.Values{}
	for key, value := range map[string]string{"source": f.source, "mode": f.mode, "origin": f.origin} {
		if value != "" {
			params.Set(key, value)
		}
	}
	return params
}

func (f skillFilters) link() *plugin.Link {
	params := map[string]string{}
	for key, values := range f.values() {
		params[key] = values[0]
	}
	return &plugin.Link{View: "skills", Params: params}
}
func (p *Fleet) trialLabel(t *trialDates, source bool) string {
	if t == nil {
		return "—"
	}
	switch t.State {
	case "running":
		if source {
			return "On trial until " + shortDate(t.Ends)
		}
		return "Until " + shortDate(t.Ends)
	case "due":
		if source {
			return "Trial past end date"
		}
		return "Past end date"
	default:
		return p.word(t.State)
	}
}
func (p *Fleet) byHost(usage []hostUsage, hosts []installation) string {
	var positive []hostUsage
	for _, h := range usage {
		if h.Threads > 0 {
			positive = append(positive, h)
		}
	}
	if len(positive) > 0 {
		slices.SortFunc(positive, func(a, b hostUsage) int {
			if n := cmp.Compare(b.Threads, a.Threads); n != 0 {
				return n
			}
			return strings.Compare(a.Host, b.Host)
		})
		var parts []string
		for _, h := range positive {
			parts = append(parts, fmt.Sprintf("%s %d", p.word(h.Host), h.Threads))
		}
		return join(parts...)
	}
	var absent []string
	for _, h := range hosts {
		if h.Installed {
			break
		}
		absent = append(absent, p.word(h.Host))
	}
	if len(absent) > 0 && len(absent) == len(hosts) {
		return "Not installed on " + strings.Join(absent, ", ")
	}
	if usage == nil {
		return "No data"
	}
	return "—"
}

func (c *skillCounts) label() string {
	if c == nil {
		return "Counts not reported"
	}
	return join(plural(c.Skills, "skill", "skills"), fmt.Sprintf("%d model-invoked", c.Model))
}

func (p *Fleet) skills(data skillsResponse, filters skillFilters) []plugin.Block {
	show := plugin.Block{Type: "list", Title: "Show"}
	sourceFilter := filters.source
	if p.word(sourceFilter) != sourceFilter {
		sourceFilter = ""
	}
	for _, key := range []string{"", "model", "user", "exclude"} {
		title := modes[key]
		if key == "" {
			title = "All skills"
		}
		link := skillFilters{source: sourceFilter, mode: key, origin: filters.origin}.link()
		it := plugin.Item{Title: title, Link: link}
		if key == filters.mode {
			it.Badges = []plugin.Badge{badge("Showing", "info")}
		}
		show.Items = append(show.Items, it)
	}
	sources := plugin.Block{Type: "list", Title: "Sources"}
	trials := map[string]*trialDates{}
	for _, s := range data.Sources {
		if s.Origin == "own" {
			continue
		}
		trials[s.Name] = s.Trial
		it := plugin.Item{Title: p.word(s.Name), Meta: s.Counts.label()}
		if sourceID.MatchString(s.Name) && it.Title == s.Name {
			origin := filters.origin
			if origin == "own" {
				origin = ""
			}
			it.Link = skillFilters{source: s.Name, mode: filters.mode, origin: origin}.link()
		}
		if filters.source == s.Name {
			it.Badges = []plugin.Badge{badge("Showing", "info")}
		}
		switch {
		case s.Trial != nil:
			it.Subtitle = p.trialLabel(s.Trial, true)
			if s.Trial.State != "due" && s.Trial.State != "running" {
				it.Tone = "muted"
			}
		case s.Defined == "policy":
			it.Subtitle = "Added from the dashboard"
		case s.Defined == "config":
			it.Subtitle = "Hand-written"
		default:
			it.Subtitle, it.Tone = p.word(s.Defined), "muted"
		}
		sources.Items = append(sources.Items, it)
	}
	own := plugin.Item{Title: "Own skills", Meta: data.OwnCounts.label(), Link: skillFilters{mode: filters.mode, origin: "own"}.link()}
	if filters.origin == "own" {
		own.Badges = []plugin.Badge{badge("Showing", "info")}
	}
	sources.Items = append(sources.Items, own)
	rows := plugin.Block{Type: "list", Title: "Skills", Empty: "No skills match this filter"}
	if data.Skills == nil {
		rows.Empty = "Skills not reported"
	}
	for _, s := range data.Skills {
		source, trial := "Own", "—"
		if s.Source != nil {
			source = p.word(*s.Source)
			trial = p.trialLabel(trials[*s.Source], false)
		}
		set := p.word(s.ModeFrom)
		switch s.ModeFrom {
		case "source":
			set = "Source lists"
		case "decision":
			set = "Decision, " + shortDate(s.DecidedAt)
		case "frontmatter":
			set = "SKILL.md"
		}
		var threads any = "No data"
		var byHost []hostUsage
		if s.Usage != nil {
			threads = displayCount(s.Usage.Threads)
			byHost = s.Usage.ByHost
		}
		item := plugin.Item{Title: p.word(s.Name), Subtitle: join(source, p.label(s.Mode, modes), set),
			Meta: join(fmt.Sprintf("Threads (%d days): %v", data.Window, threads), p.byHost(byHost, s.Hosts))}
		if trial != "—" {
			item.Badges = []plugin.Badge{badge(trial, "info")}
		}
		if skillID.MatchString(s.ID) && p.word(s.ID) == s.ID {
			item.Link = &plugin.Link{View: "skill", Params: map[string]string{"id": s.ID}}
		}
		rows.Items = append(rows.Items, item)
	}
	blocks := []plugin.Block{show, sources}
	if data.Truncated {
		blocks = append(blocks, notice("info", "The hub returned the first 2000 skills."))
	}
	return append(blocks, rows)
}

func (p *Fleet) trials(data trialsResponse) []plugin.Block {
	if data.Trials == nil {
		return []plugin.Block{notice("info", "Trials not reported.")}
	}
	if len(data.Trials) == 0 {
		return []plugin.Block{notice("info", "No trials are running.")}
	}
	var sections []plugin.Block
	for _, t := range data.Trials {
		ends := plugin.Item{Label: "Ends", Value: "Not reported"}
		if days, ok := daysUntil(data.Generated, t.Ends); ok {
			switch {
			case days < 0:
				ends.Value = plural(-days, "day", "days") + " ago"
			case days == 0:
				ends.Value = "Today"
			default:
				ends.Value = "in " + plural(days, "day", "days")
			}
		}
		if t.State == "due" {
			ends.Tone, ends.Hint = "warn", "Past end date"
		} else if t.State != "running" {
			ends.Hint, ends.Tone = p.word(t.State), "muted"
		}
		hosts := "All hosts"
		if t.Hosts != nil {
			var names []string
			for _, h := range t.Hosts {
				names = append(names, p.word(h))
			}
			hosts = strings.Join(names, ", ")
			if hosts == "" {
				hosts = "No hosts"
			}
		}
		rows := table("Decision table", "No skills on this trial", col("skill", "Skill"), number("threads", "Threads"), col("suggestion", "Suggestion"), col("mode", "Mode now"), col("decided", "Decided"), col("hosts", "By host"))
		undecided := 0
		for _, s := range t.Skills {
			decided := "Yes"
			if !s.Decided {
				undecided++
				decided = "No"
			}
			rows.Rows = append(rows.Rows, map[string]any{"skill": p.word(s.Name), "threads": displayCount(s.Threads), "suggestion": p.label(s.Suggestion, suggestions), "mode": p.label(s.Mode, modes), "decided": decided, "hosts": p.byHost(s.ByHost, nil)})
		}
		count := strconv.Itoa(undecided)
		if t.Skills == nil {
			count = "Not reported"
			rows.Empty = "Skills not reported"
		}
		children := []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "Since", Value: shortDate(t.Since)}, ends, {Label: "Undecided", Value: count}, {Label: "Hosts", Value: hosts}}}}
		if t.State == "due" {
			msg := "This trial ended on " + shortDate(t.Ends)
			if t.Skills != nil {
				msg += " with " + plural(undecided, "undecided skill", "undecided skills")
			}
			children = append(children, notice("warn", msg+". Decide each one, then end the trial."))
		}
		children = append(children, rows)
		sections = append(sections, plugin.Block{Type: "section", Title: join(p.word(t.Source), "ends "+shortDate(t.Ends)), Blocks: children})
	}
	return sections
}

func (p *Fleet) sourceActions(v *plugin.View, data skillsResponse, filters skillFilters) {
	p.offer(v, "add_trial_source", "", "Add a trial source? The selected hosts install its skills at their next sync.", nil, nil)
	for _, s := range data.Sources {
		if s.Removable && filters.source == s.Name && sourceID.MatchString(s.Name) {
			p.offer(v, "remove_source", "Remove source "+s.Name, "Remove "+s.Name+"? Its skills are uninstalled on every host at their next sync. Hand-edited copies stay and are reported.", plugin.Config{"source": s.Name}, nil)
			return
		}
	}
}

func (p *Fleet) trialActions(v *plugin.View, data trialsResponse) {
	for _, t := range data.Trials {
		if !sourceID.MatchString(t.Source) {
			continue
		}
		affected := "Undecided skills get the mode you pick here."
		if t.Skills != nil {
			n := 0
			for _, s := range t.Skills {
				if !s.Decided {
					n++
				}
			}
			affected = "Its " + plural(n, "undecided skill gets", "undecided skills get") + " the mode you pick here."
		}
		p.offer(v, "end_trial", "End trial: "+t.Source, "End the trial for "+t.Source+"? "+affected+" Every host applies the change at its next sync.", plugin.Config{"source": t.Source}, nil)
	}
}

func (p *Fleet) skillDetail(v *plugin.View, data skillsResponse, id string) {
	for _, s := range data.Skills {
		if s.ID != id {
			continue
		}
		v.Title = p.word(s.Name)
		source, trial := "Own", "No"
		if s.Source != nil {
			source = p.word(*s.Source)
			for _, src := range data.Sources {
				if src.Name == *s.Source && src.Trial != nil {
					trial = p.trialLabel(src.Trial, true)
				}
			}
		}
		threads := "No data"
		usage := map[string]int{}
		var byHost []hostUsage
		if s.Usage != nil {
			threads = fmt.Sprint(displayCount(s.Usage.Threads))
			byHost = s.Usage.ByHost
		}
		for _, h := range byHost {
			usage[h.Host] = h.Threads
		}
		mode := p.label(s.Mode, modes)
		v.Blocks = []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "Mode", Value: mode}, {Label: "Source", Value: source}, {Label: "Trial", Value: trial}, {Label: "Threads", Value: threads}}}}
		rows := table("Hosts", "No hosts", col("host", "Host"), col("installed", "Installed"), number("threads", "Threads"))
		if s.Hosts == nil {
			rows.Empty = "Hosts not reported"
		}
		installed := map[string]bool{}
		for _, h := range s.Hosts {
			flag := "No"
			if h.Installed {
				flag = "Yes"
			}
			var n any = "No data"
			if byHost != nil {
				n = usage[h.Host]
			}
			rows.Rows = append(rows.Rows, map[string]any{"host": p.word(h.Host), "installed": flag, "threads": n})
			installed[h.Host] = true
		}
		for _, h := range byHost {
			if !installed[h.Host] {
				rows.Rows = append(rows.Rows, map[string]any{"host": p.word(h.Host), "installed": "Not reported", "threads": h.Threads})
			}
		}
		v.Blocks = append(v.Blocks, rows)
		if s.Editable {
			defaults := plugin.Config{}
			if modes[s.Mode] != "" {
				defaults["mode"] = s.Mode
			}
			p.offer(v, "set_skill_mode", "", "Change the mode for "+p.clean(s.Name, 120)+"? Hosts apply the new mode at their next sync.", plugin.Config{"skill": id}, defaults)
		}
		return
	}
	message := "This skill was not found."
	if data.Truncated {
		message = "This skill was not among the first 2000 skills returned by the hub."
	}
	v.Blocks = []plugin.Block{notice("info", message)}
}
