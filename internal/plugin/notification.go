package plugin

import (
	"context"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// WaitInitialPoll prevents the notifier from interpreting a cold cache as a
// successful empty result. It is only used after runtime workers have started.
func (r *Registry) WaitInitialPoll(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.initialPoll:
		return ctx.Err()
	}
}

// NotificationSources reports which enabled alert sources currently have a
// complete successful poll. A failed provider read never proves resolution.
func (r *Registry) NotificationSources() map[string]bool {
	out := map[string]bool{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.mu.Lock()
		_, source := e.plugin.(AlertSource)
		if source && e.stored.Enabled && e.ready {
			out[id] = e.alertPollSeen && e.alertPollOK
		}
		e.mu.Unlock()
	}
	return out
}
func (r *Registry) UncertainNotificationSources() map[string]bool {
	out := map[string]bool{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.mu.Lock()
		_, source := e.plugin.(AlertSource)
		if source && e.stored.Enabled && e.ready && (!e.alertPollSeen || !e.alertPollOK) {
			out[id] = true
		}
		e.mu.Unlock()
	}
	return out
}

// NotificationSource recognizes only server-owned plugin alert namespaces.
func NotificationSource(a alert.Alert) string {
	if a.Host == "api" && a.Rule == "security.canary.tripped" {
		return "security"
	}
	if strings.HasPrefix(a.Rule, a.Host+".") || a.Rule == "plugin.unreachable" {
		return a.Host
	}
	return ""
}

// LockNotificationSnapshot keeps projected alerts and their readiness from
// different poll generations from being combined into one notification cycle.
func (r *Registry) LockNotificationSnapshot() func() {
	r.notificationCycle.RLock()
	return r.notificationCycle.RUnlock
}
