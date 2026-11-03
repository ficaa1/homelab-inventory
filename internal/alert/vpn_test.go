package alert

import (
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"testing"
	"time"
)

func TestVPNInventoryAndWatchRules(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	no := false
	v := &inventory.VPN{Container: "vpn", State: "exited"}
	a := VPNAlerts("host-a", v, nil, now)
	if len(a) != 1 || a[0].Rule != "vpn.down" || a[0].Subject != "vpn" || a[0].Host != "host-a" || a[0].Level != LevelCritical {
		t.Fatalf("down: %+v", a)
	}
	v.State = "running"
	v.InterfaceUp = true
	v.Dependents = []inventory.VPNDependent{{Name: "downloader", State: "running", Attached: &no}}
	a = VPNAlerts("host-a", v, nil, now)
	if len(a) != 1 || a[0].Rule != "vpn.cutoff" {
		t.Fatalf("cutoff: %+v", a)
	}
	live := VPNHealth{Host: "host-a", Container: "vpn", Status: "ok"}
	if a = VPNAlerts("host-a", v, &live, now); len(a) != 0 {
		t.Fatalf("watch did not override inventory: %+v", a)
	}
	old := now.Add(-4 * time.Minute)
	live.LatestHandshake = &old
	a = VPNAlerts("host-a", v, &live, now)
	if len(a) != 1 || a[0].Rule != "vpn.handshake.stale" || a[0].Level != LevelWarning {
		t.Fatalf("stale: %+v", a)
	}
	rep := &Report{Alerts: VPNAlerts("host-a", v, nil, now)}
	rep.ApplyVPN(v, live, now)
	if rep.Counts.Critical != 0 || rep.Counts.Warning != 1 {
		t.Fatalf("override counts: %+v", rep)
	}
}
