package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/backup"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/silence"
)

// Called only during runtime notification evaluation, never staging validation.
func (s *server) migrationNotificationAlerts(ctx context.Context, evaluate func() ([]alert.Alert, error)) ([]alert.Alert, error) {
	if s.plugins != nil {
		if e := s.plugins.WaitInitialPoll(ctx); e != nil {
			return nil, e
		}
	}
	if s.plugins != nil {
		unlock := s.plugins.LockNotificationSnapshot()
		defer unlock()
	}
	current, e := evaluate()
	if e != nil {
		return nil, e
	}
	var previous notify.State
	if e = s.store.ReadState(notify.StateName, &previous); e != nil {
		return nil, e
	}
	if s.plugins == nil {
		return current, nil
	}
	uncertain := s.plugins.UncertainNotificationSources()
	seen := map[string]bool{}
	for _, a := range current {
		seen[a.Key()] = true
	}
	for key, old := range previous.Active {
		if uncertain[plugin.NotificationSource(alert.Alert{Host: old.Host, Rule: old.Rule})] && !seen[key] {
			current = append(current, alert.Alert{Host: old.Host, Rule: old.Rule, Subject: old.Subject, Level: old.Level, Message: old.Message})
		}
	}
	book, e := s.silences()
	if e != nil {
		return nil, e
	}
	report := &alert.Report{Alerts: current}
	current = silence.Unsilenced(silence.Apply(report, &book, time.Now()).Alerts)
	successful := s.plugins.NotificationSources()
	changed := false
	for id, pending := range previous.PendingPluginBaselines {
		if !pending || !successful[id] {
			continue
		}
		subset := []alert.Alert{}
		for _, a := range current {
			if plugin.NotificationSource(a) == id {
				subset = append(subset, a)
			}
		}
		seed, _ := notify.Diff(previous, subset, s.effective().NotifyLevel, time.Now())
		if previous.Active == nil {
			previous.Active = map[string]notify.Entry{}
		}
		for key, old := range previous.Active {
			if plugin.NotificationSource(alert.Alert{Host: old.Host, Rule: old.Rule}) == id && old.Rule != "plugin.unreachable" {
				delete(previous.Active, key)
			}
		}
		for key, a := range seed.Active {
			previous.Active[key] = a
		}
		delete(previous.PendingPluginBaselines, id)
		changed = true
	}
	if changed {
		if e = s.store.WriteState(notify.StateName, previous); e != nil {
			return nil, e
		}
	}
	return current, nil
}
func prepareMigrationNotificationBaseline(dir string, p *backup.Plan) error {
	if p.Manifest.Product != "homelab-dash" {
		return nil
	}
	file := filepath.Join(dir, "state-notify.json")
	raw, e := os.ReadFile(file)
	if os.IsNotExist(e) {
		raw = []byte(`{}`)
	} else if e != nil {
		return e
	}
	var state notify.State
	if json.Unmarshal(raw, &state) != nil {
		return backup.ErrInvalid
	}
	if e = fsx.WriteAtomic(filepath.Join(dir, "state-migration", "original-notify.json"), raw, 0600); e != nil {
		return e
	}
	state.PendingPluginBaselines = map[string]bool{}
	var configured struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	pluginsRaw, readErr := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if readErr == nil && json.Unmarshal(pluginsRaw, &configured) != nil {
		return backup.ErrInvalid
	}
	for _, id := range []string{"media", "network"} {
		var item struct {
			Enabled bool `json:"enabled"`
		}
		if b, ok := configured.Plugins[id]; ok {
			if json.Unmarshal(b, &item) != nil {
				return backup.ErrInvalid
			}
			if item.Enabled {
				state.PendingPluginBaselines[id] = true
			}
		}
	}
	b, e := json.Marshal(state)
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(file, b, 0600)
}
