package plugin

import (
	"context"
	"errors"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

const PollInterval = 5 * time.Minute

// Poll refreshes each enabled alert source. Failed polls preserve last good
// evidence until the third failure replaces it with an unreachable condition.
func (r *Registry) Poll(ctx context.Context) {
	for _, id := range r.ids() {
		if ctx.Err() != nil {
			return
		}
		e := r.entries[id]
		e.op.Lock()
		e.mu.Lock()
		source, ok := e.plugin.(AlertSource)
		if !ok || !e.stored.Enabled || !e.ready {
			e.mu.Unlock()
			e.op.Unlock()
			continue
		}
		e.mu.Unlock()
		pollCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		alerts, err := source.Alerts(pollCtx)
		if pollCtx.Err() != nil {
			err = errors.New("plugin alert poll timed out")
		}
		cancel()
		if err == nil {
			err = validateAlerts(alerts)
		}
		e.mu.Lock()
		at := e.host.Now().UTC()
		e.checked = &at
		if err != nil {
			e.failures++
			e.state = "error"
			e.message = e.host.safe(err.Error())
		} else {
			e.failures = 0
			e.state = "ok"
			e.message = ""
			e.alerts = append([]Alert(nil), alerts...)
		}
		e.mu.Unlock()
		e.op.Unlock()
	}
}
func validateAlerts(alerts []Alert) error {
	for _, a := range alerts {
		if a.Rule == "" {
			return errors.New("invalid plugin alert rule")
		}
		switch a.Severity {
		case "critical", "warning", "info":
		default:
			return errors.New("invalid plugin alert severity")
		}
	}
	return nil
}
func (r *Registry) Alerts() []alert.Alert {
	out := []alert.Alert{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.mu.Lock()
		if e.stored.Enabled && e.ready {
			if e.failures >= 3 {
				out = append(out, alert.Alert{Host: id, Rule: "plugin.unreachable", Source: "plugin", Level: alert.LevelWarning, Subject: id, Message: "plugin alert source is unreachable", Detail: e.message})
			} else {
				for _, a := range e.alerts {
					host := id
					// Canary identity predates the plugin boundary; preserve notification,
					// silence and age keys when importing existing trip history.
					if id == "security" && a.Rule == "canary.tripped" {
						host = "api"
					}
					out = append(out, alert.Alert{Host: host, Rule: id + "." + a.Rule, Source: "plugin", Level: alert.Level(a.Severity), Subject: e.host.safe(a.Subject), Message: e.host.safe(a.Summary), Detail: e.host.safe(a.Detail)})
				}
			}
		}
		e.mu.Unlock()
	}
	return out
}
func (r *Registry) Loop(ctx context.Context) {
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()
	for {
		r.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.wake:
		}
	}
}
