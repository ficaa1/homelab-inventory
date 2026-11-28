package fleet

import (
	"context"
	"fmt"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (p *Fleet) Alerts(ctx context.Context) ([]plugin.Alert, error) {
	var data overviewResponse
	if err := p.get(ctx, "overview", p.readToken, &data); err != nil {
		return nil, err
	}
	if !data.Policy.complete() || data.Hosts == nil || data.Trials == nil {
		return nil, errResponse
	}
	alerts := []plugin.Alert{}
	add := func(rule, subject, severity, summary string) {
		alerts = append(alerts, plugin.Alert{Rule: rule, Subject: subject, Severity: severity, Summary: summary})
	}
	for _, h := range data.Hosts {
		name := p.word(h.Name)
		if h.Seen == nil {
			add("host_stale", name, "warning", name+" has never reported")
		} else if h.stale(data.hostEnvelope) {
			add("host_stale", name, "warning", name+" last reported "+age(data.Generated, *h.Seen))
		}
		if c := h.Counts; c != nil {
			var reasons []string
			if c.Drift > 0 {
				reasons = append(reasons, plural(c.Drift, "drifted file", "drifted files"))
			}
			if c.Broken > 0 {
				reasons = append(reasons, plural(c.Broken, "broken link", "broken links"))
			}
			if len(reasons) > 0 {
				add("drift", name, "warning", name+": "+strings.Join(reasons, ", "))
			}
			if c.Refused > 0 {
				add("refused", name, "warning", name+": "+plural(c.Refused, "refused change", "refused changes"))
			}
		}
		if b := h.Disk.status(data.Policy); b.Text != "" {
			severity := "warning"
			if b.Tone == "crit" {
				severity = "critical"
			}
			add("disk", name, severity, fmt.Sprintf("%s: %s · %s", name, b.Text, h.Disk.label()))
		}
	}
	for _, t := range data.Trials {
		if t.State == "due" {
			source := p.word(t.Source)
			add("trial_due", source, "info", source+" trial past end date ("+shortDate(t.Ends)+")")
		}
	}
	return alerts, nil
}
