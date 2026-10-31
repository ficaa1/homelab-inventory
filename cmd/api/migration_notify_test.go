package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/backup"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/silence"
)

type notificationPlugin struct {
	fail    bool
	subject string
}

func (*notificationPlugin) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "media", Name: "Media", Version: "1"}
}
func (*notificationPlugin) Configure(plugin.Config) error { return nil }
func (*notificationPlugin) Test(context.Context) plugin.TestResult {
	return plugin.TestResult{OK: true}
}
func (*notificationPlugin) View(context.Context, string, url.Values) (plugin.View, error) {
	return plugin.View{}, nil
}
func (*notificationPlugin) Close() error { return nil }
func (p *notificationPlugin) Alerts(context.Context) ([]plugin.Alert, error) {
	if p.fail {
		return nil, errors.New("synthetic provider failure")
	}
	return []plugin.Alert{{Rule: "health", Subject: p.subject, Severity: "warning", Summary: "Attention"}}, nil
}

type notificationSender struct{ sent int }

func (s *notificationSender) Send(context.Context, notify.Message) error { s.sent++; return nil }
func notificationFixture(t *testing.T) (*server, *notificationPlugin, *notificationSender) {
	s := testServer(t, "admin")
	p := &notificationPlugin{subject: "queue"}
	r, e := plugin.Open(s.store.Dir(), []plugin.Builtin{{ID: "media", New: func(plugin.Host) plugin.Plugin { return p }}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(r.Close)
	on := true
	if _, e = r.Update("media", plugin.Update{Enabled: &on}); e != nil {
		t.Fatal(e)
	}
	s.plugins = r
	s.defaults.NotifyLevel = alert.LevelWarning
	sender := &notificationSender{}
	s.notifier = &notify.Notifier{State: s.store, Sender: sender, Min: alert.LevelWarning}
	return s, p, sender
}
func TestNotificationColdFailuresRetainPersistedPluginConditions(t *testing.T) {
	s, p, sender := notificationFixture(t)
	old := alert.Alert{Host: "media", Rule: "media.health", Subject: "queue", Level: alert.LevelWarning, Message: "Attention"}
	state, _ := notify.Diff(notify.State{}, []alert.Alert{old}, alert.LevelWarning, time.Now().Add(-time.Hour))
	if e := s.store.WriteState(notify.StateName, state); e != nil {
		t.Fatal(e)
	}
	p.fail = true
	for i := 0; i < 4; i++ {
		s.plugins.Poll(context.Background())
		a, e := s.migrationNotificationAlerts(context.Background(), func() ([]alert.Alert, error) { return s.plugins.Alerts(), nil })
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, entry := range a {
			if entry.Key() == old.Key() {
				found = true
			}
		}
		if !found {
			t.Fatal("cold failed poll lost old condition", a)
		}
		if _, e = s.notifier.Run(context.Background(), a, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	if sender.sent != 1 {
		t.Fatal("only explicit unreachable should notify during failed polls", sender.sent)
	}
	p.fail = false
	s.plugins.Poll(context.Background())
	a, e := s.migrationNotificationAlerts(context.Background(), func() ([]alert.Alert, error) { return s.plugins.Alerts(), nil })
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent != 2 {
		t.Fatal("only unreachable recovery should notify after successful warm poll", sender.sent)
	}
}
func TestMigrationPluginBaselineDoesNotMuteCoreOrFuturePluginTransitions(t *testing.T) {
	s, p, sender := notificationFixture(t)
	state := notify.State{Seeded: true, Active: map[string]notify.Entry{}, PendingPluginBaselines: map[string]bool{"media": true}}
	s.store.WriteState(notify.StateName, state)
	p.fail = true
	s.plugins.Poll(context.Background())
	core := alert.Alert{Host: "node", Rule: "unit.failed", Subject: "service", Level: alert.LevelWarning, Message: "Failed"}
	evaluate := func() ([]alert.Alert, error) { return append(s.plugins.Alerts(), core), nil }
	a, e := s.migrationNotificationAlerts(context.Background(), evaluate)
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent != 1 {
		t.Fatal("core transition muted during incomplete migration baseline", sender.sent)
	}
	p.fail = false
	s.plugins.Poll(context.Background())
	a, e = s.migrationNotificationAlerts(context.Background(), evaluate)
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent != 1 {
		t.Fatal("migration baseline sent", sender.sent)
	}
	p.subject = "new-queue"
	s.plugins.Poll(context.Background())
	a, e = s.migrationNotificationAlerts(context.Background(), evaluate)
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent <= 1 {
		t.Fatal("future plugin transition muted")
	}
}
func TestUncertainRetainedPluginConditionRespectsCurrentSilence(t *testing.T) {
	s, p, _ := notificationFixture(t)
	old := alert.Alert{Host: "media", Rule: "media.health", Subject: "queue", Level: alert.LevelWarning}
	state, _ := notify.Diff(notify.State{}, []alert.Alert{old}, alert.LevelWarning, time.Now())
	s.store.WriteState(notify.StateName, state)
	book := silence.Book{}
	if _, e := book.Add(silence.Silence{Host: old.Host, Rule: old.Rule, Subject: old.Subject}, time.Now()); e != nil {
		t.Fatal(e)
	}
	s.store.WriteState(silence.StateName, book)
	p.fail = true
	s.plugins.Poll(context.Background())
	a, e := s.migrationNotificationAlerts(context.Background(), func() ([]alert.Alert, error) { return nil, nil })
	if e != nil {
		t.Fatal(e)
	}
	if len(a) != 0 {
		t.Fatal("silenced condition reinserted", a)
	}
}
func TestMigrationBaselineKeepsOriginalLedgerAndOnlyEnabledNewSources(t *testing.T) {
	dir := t.TempDir()
	if e := os.MkdirAll(filepath.Join(dir, "state-migration"), 0700); e != nil {
		t.Fatal(e)
	}
	raw := []byte("{\n  \"seeded\":true,\"active\":{},\"future_field\":null\n}\n")
	os.WriteFile(filepath.Join(dir, "state-notify.json"), raw, 0600)
	os.WriteFile(filepath.Join(dir, "state-plugins.json"), []byte(`{"plugins":{"media":{"enabled":true},"network":{"enabled":false},"security":{"enabled":true}}}`), 0600)
	if e := prepareMigrationNotificationBaseline(dir, &backup.Plan{Manifest: backup.Manifest{Product: "homelab-dash"}}); e != nil {
		t.Fatal(e)
	}
	original, e := os.ReadFile(filepath.Join(dir, "state-migration", "original-notify.json"))
	if e != nil || !bytes.Equal(raw, original) {
		t.Fatal("original ledger changed", e)
	}
	var state notify.State
	b, _ := os.ReadFile(filepath.Join(dir, "state-notify.json"))
	if e = json.Unmarshal(b, &state); e != nil {
		t.Fatal(e)
	}
	if len(state.PendingPluginBaselines) != 1 || !state.PendingPluginBaselines["media"] {
		t.Fatal("baseline includes disabled or already-compatible alert sources", state.PendingPluginBaselines)
	}
}
func TestMigrationBaselinePreservesOutageRecoveryNotification(t *testing.T) {
	s, p, sender := notificationFixture(t)
	s.store.WriteState(notify.StateName, notify.State{Seeded: true, Active: map[string]notify.Entry{}, PendingPluginBaselines: map[string]bool{"media": true}})
	p.fail = true
	for i := 0; i < 3; i++ {
		s.plugins.Poll(context.Background())
	}
	evaluate := func() ([]alert.Alert, error) { return s.plugins.Alerts(), nil }
	a, e := s.migrationNotificationAlerts(context.Background(), evaluate)
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent != 1 {
		t.Fatal("unreachable alert muted", sender.sent)
	}
	p.fail = false
	s.plugins.Poll(context.Background())
	a, e = s.migrationNotificationAlerts(context.Background(), evaluate)
	if e != nil {
		t.Fatal(e)
	}
	s.notifier.Run(context.Background(), a, time.Now())
	if sender.sent != 2 {
		t.Fatal("outage recovery muted", sender.sent)
	}
}
