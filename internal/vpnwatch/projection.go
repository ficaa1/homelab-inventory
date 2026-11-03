package vpnwatch

import (
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// Project exposes only public VPN observations and action outcomes to Network.
func Project(inv *inventory.Inventory, v *inventory.VPN, watch *Watch, manager *actions.Manager, actionsHost string, allowed bool) plugin.NetworkHost {
	h := plugin.NetworkHost{Host: inv.Host.Hostname, CollectedAt: inv.CollectedAt, InterfaceUp: v.InterfaceUp, LatestHandshake: v.LatestHandshake, Endpoint: v.Endpoint, ExitIP: v.ExitIP, Country: v.Country, City: v.City, Org: v.Org, RXBytes: v.RXBytes, TXBytes: v.TXBytes,
		Container: v.Container, State: v.State, Health: v.Health, Interface: v.Interface, StartedAt: v.StartedAt, Dependents: v.Dependents, Status: "unknown", Reason: "Live checks need actions on this host"}
	for _, a := range alert.VPNAlerts(h.Host, v, nil, time.Now()) {
		if a.Rule == "vpn.down" {
			h.Status = "down"
			h.Reason = a.Message
		}
		if a.Rule == "vpn.cutoff" {
			h.Status = "cutoff"
			h.Reason = a.Message
		}
	}
	if watch != nil {
		snap := watch.Snapshot(v.Container)
		if snap.Host == h.Host && snap.Container == v.Container {
			h.Status, h.Reason, h.Since, h.HandshakeAgeSeconds, h.Live = snap.Status, snap.Reason, &snap.Since, snap.HandshakeAgeSeconds, true
			h.Dependents = snap.Dependents
			h.State, h.Health, h.StartedAt = snap.State, snap.Health, snap.StartedAt
			f := snap.Facts
			h.LatestHandshake = f.LatestHandshake
			h.Endpoint = f.Endpoint
			if f.RXBytes != nil {
				h.RXBytes = f.RXBytes
			}
			if f.TXBytes != nil {
				h.TXBytes = f.TXBytes
			}
			h.ExitIP, h.Country, h.City, h.Org = f.ExitIP, f.Country, f.City, f.Org
			a := snap.Autofix
			h.Autofix = plugin.NetworkAutofix{Enabled: a.Enabled, AfterMinutes: a.AfterMinutes, State: a.State, Reason: a.Reason, LastRun: a.LastRun, CooldownUntil: a.CooldownUntil}
		}
	}
	h.ReconnectAllowed = manager != nil && allowed && h.Host == actionsHost && v.Container != ""
	if manager != nil {
		for _, e := range manager.Log(100) {
			if e.Kind == "vpn_reconnect" && e.Target == v.Container && h.Host == actionsHost {
				h.LastReconnect = &plugin.NetworkReconnect{At: e.At, Actor: e.Actor, Result: e.Result, Reason: e.Reason, ExitIP: e.ExitIP, Country: e.Country}
				break
			}
		}
	}
	return h
}
