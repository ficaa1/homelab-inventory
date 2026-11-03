package alert

import (
	"fmt"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// VPNHealth is the API watch's authoritative observation of one gateway.
// Unknown deliberately carries no failure: unavailable evidence is not a diagnosis.
type VPNHealth struct {
	Host, Container, Status, Reason string
	LatestHandshake                 *time.Time
	Since                           *time.Time
}

// VPNAlerts uses one path for both inventory and live evidence, preserving the
// real Host and stable rule keys independently of plugin enablement.
func VPNAlerts(host string, v *inventory.VPN, live *VPNHealth, now time.Time) []Alert {
	if v == nil || v.Container == "" {
		return nil
	}
	status, reason := "unknown", ""
	if v.State != "running" {
		status, reason = "down", "gateway is not running"
	} else if !v.InterfaceUp {
		status, reason = "down", "gateway has no tunnel interface"
	} else {
		for _, d := range v.Dependents {
			if d.State == "running" && d.Attached != nil && !*d.Attached {
				status, reason = "cutoff", d.Name+" is not using the gateway's current network"
				break
			}
		}
	}
	if live != nil && live.Host == host && live.Container == v.Container {
		status, reason = live.Status, live.Reason
	}
	out := []Alert{}
	if status == "down" || status == "cutoff" {
		var since *time.Time
		if live != nil && live.Host == host && live.Container == v.Container {
			since = live.Since
		}
		out = append(out, Alert{Since: since, Host: host, Rule: "vpn." + status, Subject: v.Container, Level: LevelCritical, Message: fmt.Sprintf("VPN %s: %s", v.Container, reason)})
	}
	if live != nil && live.Host == host && live.Container == v.Container && live.LatestHandshake != nil && now.Sub(*live.LatestHandshake) > 3*time.Minute {
		out = append(out, Alert{Host: host, Rule: "vpn.handshake.stale", Subject: v.Container, Level: LevelWarning, Message: fmt.Sprintf("VPN %s handshake is older than 3 minutes", v.Container), Detail: "Idle tunnels may have stale handshakes; the exit check determines connectivity."})
	}
	return out
}

// ApplyVPN replaces inventory VPN alerts only for the gateway covered by live
// evidence, then restores counts even when the replacement is empty.
func (r *Report) ApplyVPN(v *inventory.VPN, live VPNHealth, now time.Time) {
	if v == nil || v.Container != live.Container {
		return
	}
	out := r.Alerts[:0]
	for _, a := range r.Alerts {
		if a.Host == live.Host && a.Subject == live.Container && (a.Rule == "vpn.down" || a.Rule == "vpn.cutoff" || a.Rule == "vpn.handshake.stale") {
			continue
		}
		out = append(out, a)
	}
	r.Alerts = append(out, VPNAlerts(live.Host, v, &live, now)...)
	sortAlerts(r.Alerts)
	r.Counts = count(r.Alerts)
}
