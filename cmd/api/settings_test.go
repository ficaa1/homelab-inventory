package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/settings"
)

func settingsServer(t *testing.T) *server {
	t.Helper()
	s := testServer(t, "")
	s.defaults = settings.Defaults{
		NotifyLevel:    alert.LevelCritical,
		NotifyInterval: 5 * time.Minute,
		DigestInterval: 168 * time.Hour,
		AppriseURL:     "http://apprise.example/notify/env-key",
	}
	s.notifier = &notify.Notifier{
		State:    s.store,
		Min:      alert.LevelCritical,
		Recorder: &notify.Recorder{State: s.store},
		Options: func() notify.Options {
			e := s.effective()
			return notify.Options{Min: e.NotifyLevel, Enabled: e.Sending()}
		},
	}
	return s
}

func getSettings(t *testing.T, h http.Handler) map[string]any {
	t.Helper()
	rec := doRequest(t, h, "GET", "/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d: %s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// The whole reason this endpoint exists. Notifications were on, the floor was
// critical, and the fleet had warnings and no criticals — so nothing was ever
// sent and every individual field still read as healthy. The endpoint has to
// state the arithmetic, not just the settings.
func TestSettingsExplainsWhyNothingIsBeingSent(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	// A fleet with warnings and nothing critical: exactly the live situation.
	doRequest(t, s.routes(), "POST", "/inventory", warningOnlyInventory("host-a"))
	h := s.routes()

	out := getSettings(t, h)
	if out["sending"] != true {
		t.Fatalf("sending = %v, want true", out["sending"])
	}
	if got := out["at_or_above_floor"].(float64); got != 0 {
		t.Errorf("at_or_above_floor = %v, want 0", got)
	}
	if got := out["total_alerts"].(float64); got == 0 {
		t.Fatal("total_alerts = 0; the fixture produced no alerts, so this proves nothing")
	}

	// And it must say what a lower floor would have covered, because that is
	// the actual next action.
	cov := out["coverage"].([]any)
	byLevel := map[string]float64{}
	for _, c := range cov {
		m := c.(map[string]any)
		byLevel[m["level"].(string)] = m["count"].(float64)
	}
	if byLevel["critical"] != 0 {
		t.Errorf("critical coverage = %v, want 0", byLevel["critical"])
	}
	if byLevel["warning"] == 0 {
		t.Error("warning coverage = 0; lowering the floor would be pointless, which is not the case here")
	}
}

// An empty settings blob must leave the environment in charge — see the
// pointer discussion in internal/settings.
func TestSettingsDefaultsToTheEnvironment(t *testing.T) {
	t.Parallel()
	out := getSettings(t, settingsServer(t).routes())

	if out["notify_level"] != "critical" {
		t.Errorf("notify_level = %v", out["notify_level"])
	}
	if out["notify_interval"] != "5m0s" {
		t.Errorf("notify_interval = %v", out["notify_interval"])
	}
	src := out["configured_here"].(map[string]any)
	for k, v := range src {
		if v == true {
			t.Errorf("%s is marked as configured here, but nothing has been saved", k)
		}
	}
}

func TestSettingsUpdateTakesEffectImmediately(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	h := s.routes()

	rec := doRequest(t, h, "PUT", "/settings", []byte(`{"notify_level":"warning"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}

	// The notifier reads its floor through Options, so the change must be
	// visible without anything being restarted or re-registered.
	if got := s.notifier.Status().MinLevel; got != alert.LevelWarning {
		t.Errorf("the notifier's floor is %q; a saved setting did not reach it", got)
	}
	out := getSettings(t, h)
	if out["notify_level"] != "warning" {
		t.Errorf("notify_level = %v", out["notify_level"])
	}
	if out["configured_here"].(map[string]any)["notify_level"] != true {
		t.Error("notify_level is not marked as configured here after being set")
	}
	// Untouched fields must still come from the environment.
	if out["notify_interval"] != "5m0s" {
		t.Errorf("notify_interval = %v; setting the level disturbed it", out["notify_interval"])
	}
}

// The apprise URL can be set here, so it must never come back out: the path
// segment after /notify/ is the key that decides where messages go.
func TestSettingsNeverReturnsTheAppriseURL(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	h := s.routes()

	const secret = "http://apprise.internal:3010/notify/abc-super-secret-key"
	rec := doRequest(t, h, "PUT", "/settings",
		[]byte(`{"apprise_url":"`+secret+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "abc-super-secret-key") {
		t.Fatalf("the PUT response echoed the key back: %s", rec.Body)
	}

	got := doRequest(t, h, "GET", "/settings", nil).Body.String()
	if strings.Contains(got, "abc-super-secret-key") {
		t.Errorf("GET /settings leaked the apprise key: %s", got)
	}
	if !strings.Contains(got, "apprise.internal") {
		t.Error("the hint drops the host too; there is then no way to tell one endpoint from another")
	}
}

func TestSettingsRejectsBadValuesWithoutSavingAnything(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	h := s.routes()

	for _, body := range []string{
		`{"notify_level":"catastrophic"}`,
		`{"notify_interval":"1s"}`,
		`{"apprise_url":"file:///etc/passwd"}`,
		`{"nonsense":true}`,
	} {
		rec := doRequest(t, h, "PUT", "/settings", []byte(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, rec.Code)
		}
	}
	if out := getSettings(t, h); out["notify_level"] != "critical" {
		t.Errorf("a rejected update changed the level to %v", out["notify_level"])
	}
}

func TestSettingsResetReturnsToTheEnvironment(t *testing.T) {
	t.Parallel()
	h := settingsServer(t).routes()

	doRequest(t, h, "PUT", "/settings", []byte(`{"notify_level":"info"}`))
	if out := getSettings(t, h); out["notify_level"] != "info" {
		t.Fatalf("setup failed: notify_level = %v", out["notify_level"])
	}

	if rec := doRequest(t, h, "POST", "/settings/reset", nil); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d: %s", rec.Code, rec.Body)
	}
	out := getSettings(t, h)
	if out["notify_level"] != "critical" {
		t.Errorf("notify_level = %v after reset, want the env default", out["notify_level"])
	}
	if out["configured_here"].(map[string]any)["notify_level"] != false {
		t.Error("the field is still marked as an override after a reset")
	}
}

// Muting must not queue anything up: unmuting should be quiet, not a flood.
func TestMutingSuppressesDeliveryAndDoesNotBacklog(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	sent := &countingSender{}
	s.notifier.Sender = sent
	h := s.routes()

	off := false
	stored, err := settings.Apply(settings.Stored{}, settings.Update{NotifyEnabled: &off}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.WriteState(settings.StateName, stored); err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	alerts := []alert.Alert{{
		Host: "host-a", Rule: "docker.container.restarting", Subject: "immich",
		Level: alert.LevelCritical, Message: "restarting",
	}}
	// Seed, then evaluate for real.
	if _, err := s.notifier.Run(context.Background(), alerts, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.notifier.Run(context.Background(), nil, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if sent.n != 0 {
		t.Errorf("%d message(s) sent while muted", sent.n)
	}

	// Unmute. The condition that resolved while muted must not now arrive.
	if rec := doRequest(t, h, "PUT", "/settings", []byte(`{"notify_enabled":true}`)); rec.Code != http.StatusOK {
		t.Fatalf("unmute = %d: %s", rec.Code, rec.Body)
	}
	if _, err := s.notifier.Run(context.Background(), nil, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if sent.n != 0 {
		t.Errorf("unmuting delivered a backlog of %d message(s)", sent.n)
	}
}

// The delivery log is what separates "working and quiet" from "broken and
// quiet", so a failure has to be recorded, not just a success.
func TestDeliveryLogRecordsFailures(t *testing.T) {
	t.Parallel()
	s := settingsServer(t)
	s.notifier.Sender = &countingSender{fail: true}
	h := s.routes()

	rec := doRequest(t, h, "POST", "/notify/test", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("test send = %d, want 502", rec.Code)
	}

	log := s.notifier.Recorder.Read()
	if len(log.Deliveries) != 1 {
		t.Fatalf("%d deliveries logged, want 1", len(log.Deliveries))
	}
	d := log.Deliveries[0]
	if d.OK {
		t.Error("a failed send was logged as OK")
	}
	if d.Kind != "test" {
		t.Errorf("Kind = %q, want test", d.Kind)
	}
	if log.Attempted != 1 || log.Delivered != 0 {
		t.Errorf("Attempted=%d Delivered=%d, want 1 and 0", log.Attempted, log.Delivered)
	}
}

// warningOnlyInventory produces a host with failed units and nothing critical:
// the shape the live fleet has, and the one that makes a critical-only floor
// silent.
func warningOnlyInventory(host string) []byte {
	inv := inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   time.Now(),
		Host:          inventory.Host{Hostname: host, OS: inventory.OSRelease{Pretty: "Ubuntu 22.04"}},
		Systemd: &inventory.Systemd{
			Available: true,
			Units: []inventory.Unit{
				{Name: "nslcd.service", Load: "loaded", Active: "failed", Sub: "failed",
					Description: "LSB: LDAP connection daemon"},
				{Name: "postfix@-.service", Load: "loaded", Active: "failed", Sub: "failed",
					Description: "Postfix Mail Transport Agent"},
			},
			FailedCount: 2,
		},
	}
	data, _ := json.Marshal(inv)
	return data
}

// countingSender records how many messages were sent without a network.
type countingSender struct {
	n    int
	fail bool
}

func (c *countingSender) Send(ctx context.Context, m notify.Message) error {
	if c.fail {
		return errors.New("notify: apprise returned 500 Internal Server Error")
	}
	c.n++
	return nil
}
