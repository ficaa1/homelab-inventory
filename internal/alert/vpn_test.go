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
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now, Tool: inventory.Tool{Revision: inventory.AgentRevision}, SchemaVersion: inventory.SchemaVersion,
		VPN: v, VPNs: []inventory.VPN{*v, {Container: "tunnel", State: "paused"}},
		Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "vpn", State: "running", Status: "unhealthy"}, {Name: "tunnel", State: "paused"}}}}
	rep = Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), now)
	for _, a := range rep.Alerts {
		if a.Rule == "docker.container.unhealthy" {
			t.Fatalf("duplicate docker alert for gateway: %+v", a)
		}
	}
	assertVPN := func(want map[string]string) {
		t.Helper()
		got := map[string]string{}
		for _, a := range rep.Alerts {
			if a.Rule == "vpn.down" || a.Rule == "vpn.cutoff" || a.Rule == "vpn.handshake.stale" {
				got[a.Subject] = a.Rule
			}
		}
		if len(got) != len(want) {
			t.Fatalf("VPN alerts: %v want %v", got, want)
		}
		for container, rule := range want {
			if got[container] != rule {
				t.Fatalf("VPN alerts: %v want %v", got, want)
			}
		}
	}
	assertVPN(map[string]string{"vpn": "vpn.cutoff", "tunnel": "vpn.down"})
	rep.ApplyVPN(&inv.VPNs[0], live, now)
	assertVPN(map[string]string{"vpn": "vpn.handshake.stale", "tunnel": "vpn.down"})
	rep.ApplyVPN(&inv.VPNs[1], VPNHealth{Host: "host-b", Container: "tunnel", Status: "ok"}, now)
	assertVPN(map[string]string{"vpn": "vpn.handshake.stale", "tunnel": "vpn.down"})
	inv.VPNs = nil // Old agents still use the same alert path.
	rep = Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), now)
	assertVPN(map[string]string{"vpn": "vpn.cutoff"})

}
