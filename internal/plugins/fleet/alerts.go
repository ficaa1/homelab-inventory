package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (p *Fleet) Alerts(ctx context.Context) ([]plugin.Alert, error) {
	var data overviewResponse
	if err := p.get(ctx, "overview", p.readToken, &data); err != nil {
		return nil, err
	}
	if data.Truncated {
		return nil, errors.New("The hub's host list was cut short; alerts need the full list.")
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
		if h.stale(data.hostEnvelope) {
			summary := name + " has never reported"
			if h.Seen != nil {
				summary = name + " last reported " + age(data.Generated, *h.Seen)
			}
			add("host_stale", name, "warning", summary)
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
			decisions := "all decided, end the trial"
			if t.Undecided > 0 {
				decisions = fmt.Sprintf("%d undecided", t.Undecided)
			}
			add("trial_due", source, "info", join(source+" trial ended on "+shortDate(t.Ends), decisions))
		}
	}
	var proxy proxyResponse
	err := p.get(ctx, "proxy", p.readToken, &proxy)
	var cached []plugin.Alert
	if p.base == p.proxyAlertsBase {
		cached = p.proxyAlerts
	}
	var hub *hubError
	if errors.As(err, &hub) && hub.status == http.StatusNotFound {
		return append(alerts, cached...), nil
	}
	if err != nil {
		return nil, err
	}
	if proxy.Proxy.State != "ok" || proxy.Accounts == nil {
		text := "Proxy accounts not reported"
		if proxy.Proxy.State != "ok" {
			text = p.proxyNotice(proxy).Text
		}
		add("proxy_unreachable", "proxy", "warning", text)
		return append(alerts, cached...), nil
	}
	if proxy.Truncated {
		return nil, errors.New("Proxy alerts need the full account list.")
	}
	firstProxyAlert := len(alerts)
	type group struct{ enabled, cooling int }
	providers := map[string]group{}
	for _, a := range proxy.Accounts {
		provider, id := p.word(a.Provider), p.word(a.ID)
		label := join(p.word(a.Label), provider)
		if a.needsSignIn() {
			add("proxy_sign_in", id, "warning", label+" needs sign-in")
		}
		if a.Expires != nil && a.Renewal == "failed" && !a.Expires.Before(proxy.Generated) && a.Expires.Sub(proxy.Generated) <= 24*time.Hour {
			add("proxy_renewal", id, "warning", label+" access expires within 24 hours and renewal failed")
		}
		if !a.Disabled && a.Status != "disabled" {
			g := providers[provider]
			g.enabled++
			if a.Status == "cooling_down" {
				g.cooling++
			}
			providers[provider] = g
		}
	}
	var cooling []string
	for provider, g := range providers {
		if g.cooling == g.enabled {
			cooling = append(cooling, provider)
		}
	}
	sort.Strings(cooling)
	for _, provider := range cooling {
		add("proxy_cooling", provider, "critical", "Every enabled "+provider+" account is cooling down")
	}
	p.proxyAlerts = append([]plugin.Alert(nil), alerts[firstProxyAlert:]...)
	p.proxyAlertsBase = p.base
	return alerts, nil
}
