package vpnwatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	inv := &inventory.Inventory{VPN: &inventory.VPN{Container: "vpn", State: "exited"}}
	inv.Host.Hostname = "host-a"
	opts := Options{Now: func() time.Time { return now }, Settings: func() (bool, int) { return on, 10 }, Runner: &execallow.Runner{Inventory: func() (*inventory.Inventory, error) { return inv, nil }}, Inspect: func(context.Context, string) (vpnprobe.Container, error) {
		return vpnprobe.Container{}, errors.New("unavailable")
	}, Reconnect: func(_ context.Context, n string) (actions.Entry, error) {
		attempts++
		return actions.Entry{Target: n, Result: "ok"}, nil
	}, Message: func(context.Context, notify.Message) { messages++ }}
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
	info, err := os.Stat(filepath.Join(dir, "vpn-watch.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("state is not private", err)
	}
	w, err = Open(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(59 * time.Minute)
	check()
	if attempts != 1 {
		t.Fatal("reload lost cooldown")
	}
	now = now.Add(time.Minute)
	check()
	if attempts != 2 || messages != 2 {
		t.Fatalf("next cooldown window %d/%d", attempts, messages)
	}
	on = false
	now = now.Add(2 * time.Hour)
	check()
	if attempts != 2 {
		t.Fatal("disabled setting ignored")
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
	if calls != 2 || w.Snapshot().Status != "down" {
		t.Fatalf("confirmation calls %d state %+v", calls, w.Snapshot())
	}
}
