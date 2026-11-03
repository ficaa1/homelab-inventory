package vpnwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/vpnprobe"
)

func TestAutofixThresholdCooldownReloadAndMuteSetting(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	on := false
	attempts, messages := 0, 0
	byGateway := map[string]int{}
	inv := &inventory.Inventory{VPN: &inventory.VPN{Container: "vpn", State: "exited"}}
	inv.Host.Hostname = "host-a"
	opts := Options{Now: func() time.Time { return now }, Settings: func() (bool, int) { return on, 10 }, Runner: &execallow.Runner{Inventory: func() (*inventory.Inventory, error) { return inv, nil }}, Inspect: func(context.Context, string) (vpnprobe.Container, error) {
		return vpnprobe.Container{}, errors.New("unavailable")
	}, Reconnect: func(_ context.Context, n string) (actions.Entry, error) {
		attempts++
		byGateway[n]++
		return actions.Entry{Target: n, Result: "ok", At: now}, nil
	}, Message: func(_ context.Context, m notify.Message) {
		messages++
		if !strings.Contains(m.Body, "vpn") && !strings.Contains(m.Body, "tunnel") {
			t.Error("message must identify gateway")
		}
	}}
	w, err := Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if err := w.Check(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	check()
	now = now.Add(12 * time.Minute)
	check()
	if attempts != 0 {
		t.Fatal("autofix ran while disabled")
	}
	on = true
	check()
	if attempts != 1 || messages != 1 {
		t.Fatalf("threshold attempt/message %d/%d", attempts, messages)
	}
	info, err := os.Stat(filepath.Join(dir, "state-vpn-watch.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state is not private", err)
	}
	w, err = Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	// A second gateway starts its own failure timer while vpn is cooling down.
	inv.VPNs = []inventory.VPN{*inv.VPN, {Container: "tunnel", State: "exited"}}
	now = now.Add(20 * time.Minute)
	check()
	if attempts != 1 {
		t.Fatal("new gateway inherited the old failure timer")
	}
	now = now.Add(10 * time.Minute)
	check()
	if byGateway["vpn"] != 1 || byGateway["tunnel"] != 1 {
		t.Fatalf("independent cooldowns: %v", byGateway)
	}
	w, err = Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(29 * time.Minute)
	check()
	if attempts != 2 {
		t.Fatal("reload lost gateway cooldowns")
	}
	now = now.Add(time.Minute)
	check()
	if byGateway["vpn"] != 2 || byGateway["tunnel"] != 1 || messages != 3 {
		t.Fatalf("next cooldown window: %v messages=%d", byGateway, messages)
	}
	on = false
	now = now.Add(2 * time.Hour)
	check()
	if attempts != 3 {
		t.Fatal("disabled setting ignored")
	}
	// Removed gateways leave neither live state nor durable cooldowns behind.
	inv.VPNs = inv.VPNs[:1]
	check()
	if len(w.Health()) != 1 || len(w.saved) != 1 || w.Snapshot("tunnel").Container != "" {
		t.Fatal("removed gateway retained")
	}
	w, err = Open(dir, opts)
	if err != nil || len(w.saved) != 1 {
		t.Fatalf("removed gateway persisted: %v", err)
	}
	for _, old := range []string{`{"host":"host-a","container":"vpn","lastAuto":"2026-01-02T03:04:05Z"}`, `{broken`, `{"gateways":{"vpn":{"host":"host-a","container":"other"}}}`} {
		if err := os.WriteFile(filepath.Join(dir, "state-vpn-watch.json"), []byte(old), 0600); err != nil {
			t.Fatal(err)
		}
		fresh, err := Open(dir, opts)
		if err != nil || len(fresh.saved) != 0 {
			t.Fatalf("incompatible state must start fresh: %v", err)
		}
	}
}

func TestExitFailureConfirmationAndCadence(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	started := now.Add(-time.Hour)
	h := now.Add(-4 * time.Minute)
	calls := 0
	inv := &inventory.Inventory{VPN: &inventory.VPN{Container: "vpn", State: "running"}}
	inv.Host.Hostname = "host-a"
	w, err := Open(t.TempDir(), Options{Now: func() time.Time { return now }, Runner: &execallow.Runner{Inventory: func() (*inventory.Inventory, error) { return inv, nil }}, WG: func(context.Context, string) (vpnprobe.Facts, error) { return vpnprobe.Facts{LatestHandshake: &h}, nil }, Inspect: func(context.Context, string) (vpnprobe.Container, error) {
		return vpnprobe.Container{State: "running", StartedAt: started}, nil
	}, Exit: func(context.Context, string) (vpnprobe.Facts, error) {
		calls++
		return vpnprobe.Facts{}, errors.New("unreachable")
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, advance := range []time.Duration{0, 10 * time.Second, 20 * time.Second} {
		now = now.Add(advance)
		if err = w.Check(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || w.Snapshot("vpn").Status != "down" {
		t.Fatalf("confirmation calls %d state %+v", calls, w.Snapshot("vpn"))
	}
	// Wedged Docker must not block panel or alert readers. A concurrent manual
	// reconnect also invalidates the older in-flight probe result.
	entered, release := make(chan struct{}), make(chan struct{})
	w.opts.Inspect = func(context.Context, string) (vpnprobe.Container, error) {
		close(entered)
		<-release
		return vpnprobe.Container{State: "exited", StartedAt: started}, nil
	}
	now = now.Add(time.Minute)
	done := make(chan error, 1)
	go func() { done <- w.Check(context.Background(), false) }()
	<-entered
	read := make(chan struct{})
	go func() { _ = w.Snapshot("vpn"); _ = w.Health(); close(read) }()
	select {
	case <-read:
	case <-time.After(time.Second):
		close(release)
		<-done
		t.Fatal("readers blocked behind a Docker probe")
	}
	w.Reconnected(actions.Entry{Target: "vpn", Result: "ok", At: now})
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if w.Snapshot("vpn").Reason == "gateway is not running" {
		t.Fatal("probe overwrote newer action evidence")
	}
	if w.gateways["vpn"].failures != 0 || w.gateways["vpn"].exitOK {
		t.Fatal("reconnect retained old exit evidence")
	}

}
