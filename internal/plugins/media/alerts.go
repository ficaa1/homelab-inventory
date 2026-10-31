package media

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// Rule names are durable notification keys; see the alert table in docs/plugins.md.
func (m *Media) Alerts(ctx context.Context) ([]plugin.Alert, error) {
	next := map[string]time.Time{}
	for key, at := range m.stuck {
		next[key] = at
	}
	alerts := []plugin.Alert{}
	for _, s := range m.services {
		if s.id != "sonarr" && s.id != "radarr" {
			continue
		}
		serviceCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var health []struct{ Source, Type, Message string }
		err := m.get(serviceCtx, s, "/api/v3/health", nil, &health, true)
		if err != nil || health == nil {
			cancel()
			return nil, errors.New(s.name + ": health is unavailable")
		}
		for _, h := range health {
			if h.Source == "" || h.Type == "" {
				cancel()
				return nil, errors.New(s.name + ": incomplete health response")
			}
			// Arr health is housekeeping (indexers, updates, removed titles), not an
			// outage: its errors are warnings and its warnings are info, so the
			// default critical notification floor stays quiet about them.
			severity := ""
			switch strings.ToLower(h.Type) {
			case "warning":
				severity = "info"
			case "error":
				severity = "warning"
			}
			if severity != "" {
				summary := s.name + " health needs attention"
				if msg := m.text(h.Message); msg != "" {
					summary = s.name + ": " + msg
				}
				alerts = append(alerts, plugin.Alert{Rule: s.id + ".health", Subject: m.text(h.Source), Severity: severity, Summary: summary})
			}
		}
		queue, err := m.queue(serviceCtx, s)
		cancel()
		if err != nil {
			return nil, err
		}
		active := map[string]bool{}
		warned := map[string]bool{}
		now := m.host.Now()
		for _, r := range queue {
			if !r.warning() {
				continue
			}
			key := s.id + ":" + strconv.Itoa(r.ID)
			active[key] = true
			since, ok := next[key]
			if !ok {
				next[key] = now
				continue
			}
			title, _ := r.title()
			title = m.text(title)
			if now.Sub(since) > time.Hour && !warned[title] {
				warned[title] = true
				alerts = append(alerts, plugin.Alert{Rule: "queue.stuck", Subject: title, Severity: "warning", Summary: s.name + " download needs attention", Detail: "Warning or error observed for over an hour"})
			}
		}
		for key := range next {
			if strings.HasPrefix(key, s.id+":") && !active[key] {
				delete(next, key)
			}
		}
	}
	if err := m.saveStuck(next); err != nil {
		return nil, err
	}
	m.stuck = next
	return alerts, nil
}
