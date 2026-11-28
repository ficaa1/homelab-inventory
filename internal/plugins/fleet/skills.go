package fleet

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

var modes = map[string]string{"model": "Model-invoked", "user": "User-only", "exclude": "Excluded"}
var suggestions = map[string]string{"model": "Keep model-invoked", "user": "Keep user-only", "exclude": "Drop"}

func skillLink(src, mode string) *plugin.Link {
	params := map[string]string{}
	if src != "" {
		params["source"] = src
	}
	if mode != "" {
		params["mode"] = mode
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
func (p *Fleet) skillHosts(s skill) string {
	var positive []hostUsage
	if s.Usage != nil {
		for _, h := range s.Usage.ByHost {
			if h.Threads > 0 {
				positive = append(positive, h)
			}
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
		for _, h := range positive[:min(3, len(positive))] {
			parts = append(parts, fmt.Sprintf("%s %d", p.word(h.Host), h.Threads))
		}
		if len(positive) > 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(positive)-3))
		}
		return join(parts...)
	}
	var absent []string
	for _, h := range s.Hosts {
		if h.Installed {
			break
		}
		absent = append(absent, p.word(h.Host))
	}
	if len(absent) > 0 && len(absent) == len(s.Hosts) {
		hosts := strings.Join(absent[:min(3, len(absent))], ", ")
		if len(absent) > 3 {
			hosts += fmt.Sprintf(" +%d more", len(absent)-3)
		}
		return "Not installed on " + hosts
	}
	if s.Usage == nil {
		return "No data"
	}
	return "—"
}
func (p *Fleet) skills(data skillsResponse, src, mode string) []plugin.Block {
	show := plugin.Block{Type: "list", Title: "Show"}
	for _, key := range []string{"", "model", "user", "exclude"} {
		title := modes[key]
		if key == "" {
			title = "All skills"
		}
		it := plugin.Item{Title: title, Link: skillLink(p.word(src), key)}
		if key == mode {
			it.Badges = []plugin.Badge{badge("Showing", "info")}
		}
		show.Items = append(show.Items, it)
	}
	type count struct{ total, model int }
	counts := map[string]count{}
	for _, s := range data.Skills {
		key := ""
		if s.Source != nil {
			key = *s.Source
		}
		c := counts[key]
		c.total++
		if s.Mode == "model" {
			c.model++
		}
		counts[key] = c
	}
	sources := plugin.Block{Type: "list", Title: "Sources", Empty: "No skill sources"}
	if data.Sources == nil {
		sources.Empty = "Sources not reported"
	}
	trials := map[string]*trialDates{}
	own := counts[""].total > 0
	for _, s := range data.Sources {
		if s.Origin == "own" {
			own = true
			continue
		}
		trials[s.Name] = s.Trial
		c := counts[s.Name]
		it := plugin.Item{Title: p.word(s.Name), Meta: join(plural(c.total, "skill", "skills"), fmt.Sprintf("%d model-invoked", c.model))}
		if data.Skills == nil {
			it.Meta = "Skills not reported"
		}
		if sourceID.MatchString(s.Name) && it.Title == s.Name {
			it.Link = skillLink(s.Name, mode)
		}
		if src == s.Name {
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
	if own {
		meta := plural(counts[""].total, "skill", "skills")
		if data.Skills == nil {
			meta = "Skills not reported"
		}
		sources.Items = append(sources.Items, plugin.Item{Title: "Own skills", Meta: meta})
	}
	rows := table("Skills", "No skills match this filter", col("skill", "Skill"), number("threads", fmt.Sprintf("Threads (%d days)", data.Window)), col("source", "Source"), col("mode", "Mode"), col("set", "Set by"), col("trial", "Trial"), col("hosts", "By host"))
	if data.Skills == nil {
		rows.Empty = "Skills not reported"
	}
	for _, s := range data.Skills {
		if (src != "" && (s.Source == nil || *s.Source != src)) || (mode != "" && mode != s.Mode) {
			continue
		}
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
		if s.Usage != nil {
			threads = displayCount(s.Usage.Threads)
		}
		rows.Rows = append(rows.Rows, map[string]any{"skill": p.word(s.Name), "threads": threads, "source": source, "mode": p.label(s.Mode, modes), "set": set, "trial": trial, "hosts": p.skillHosts(s)})
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
			var byHost []string
			for _, h := range s.ByHost {
				byHost = append(byHost, fmt.Sprintf("%s %d", p.word(h.Host), h.Threads))
			}
			by := join(byHost...)
			if by == "" {
				by = "—"
			}
			if s.ByHost == nil {
				by = "No data"
			}
			rows.Rows = append(rows.Rows, map[string]any{"skill": p.word(s.Name), "threads": displayCount(s.Threads), "suggestion": p.label(s.Suggestion, suggestions), "mode": p.label(s.Mode, modes), "decided": decided, "hosts": by})
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
		sections = append(sections, plugin.Block{Type: "section", Title: join(p.word(t.Source), "ends "+shortDate(t.Ends)), Blocks: fitBlocks(children)})
	}
	return sections
}
