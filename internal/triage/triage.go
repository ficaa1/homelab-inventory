// Package triage builds read-only dashboard views from the same fleet snapshot
// and unfiltered alerts used by the existing alert endpoint.
package triage

import (
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/silence"
)

type Counts struct {
	FailedUnits  int `json:"failed_units"`
	HostsUnknown int `json:"hosts_unknown"`
	StillTrue    int `json:"still_true"`
	Cleared      int `json:"cleared"`
	Unknown      int `json:"unknown"`
}

type Report struct {
	Counts        Counts   `json:"counts"`
	Hosts         []Host   `json:"hosts"`
	Silences      []Review `json:"silences"`
	SilenceStatus string   `json:"silence_status"`
}
type Host struct {
	Host        string    `json:"host"`
	Status      string    `json:"status"`
	CollectedAt time.Time `json:"collected_at"`
	FailedCount *int      `json:"failed_count"`
	Units       []Unit    `json:"units"`
}
type Unit struct {
	Name           string       `json:"name"`
	Description    string       `json:"description"`
	Enabled        string       `json:"enabled"`
	Result         *string      `json:"result"`
	StateChangedAt *time.Time   `json:"state_changed_at"`
	Alert          *alert.Alert `json:"alert"`
	Silenced       *bool        `json:"silenced"`
}
type Review struct {
	silence.Silence
	Condition   string       `json:"condition"` // still_true, cleared, or unknown
	Alert       *alert.Alert `json:"alert"`
	CollectedAt *time.Time   `json:"collected_at"`
}

// Build never infers an exit result or failure onset from an observed alert age.
// Existing inventory units do not collect either; null preserves that gap.
// A missing host cannot establish that a silenced condition has cleared.
func Build(invs []*inventory.Inventory, alerts []alert.Alert, book *silence.Book, now time.Time) Report {
	out := Report{Hosts: []Host{}, SilenceStatus: "unavailable"}
	if book != nil {
		out.SilenceStatus = "ok"
		out.Silences = []Review{}
	}
	byKey := map[string]*alert.Alert{}
	for i := range alerts {
		a := &alerts[i]
		byKey[silence.ID(a.Host, a.Rule, a.Subject)] = a
	}
	hosts := map[string]*inventory.Inventory{}
	for _, inv := range invs {
		if inv == nil {
			continue
		}
		name := inv.Host.Hostname
		hosts[name] = inv
		h := Host{Host: name, Status: "unavailable", CollectedAt: inv.CollectedAt}
		if sd := inv.Systemd; sd != nil && sd.Available && sd.Units != nil {
			h.Status = "ok"
			h.Units = []Unit{}
			n := 0
			h.FailedCount = &n
			for _, u := range sd.Units {
				if u.Active != "failed" && u.Sub != "failed" {
					continue
				}
				n++
				a := byKey[silence.ID(name, "systemd.unit.failed", u.Name)]
				v := Unit{Name: u.Name, Description: u.Description, Enabled: u.UnitFileState, Alert: a}
				if book != nil {
					_, covered := book.Covers(alert.Alert{Host: name, Rule: "systemd.unit.failed", Subject: u.Name}, now)
					v.Silenced = &covered
				}
				h.Units = append(h.Units, v)
			}
			sort.Slice(h.Units, func(i, j int) bool { return h.Units[i].Name < h.Units[j].Name })
		}
		if h.FailedCount == nil {
			out.Counts.HostsUnknown++
		} else {
			out.Counts.FailedUnits += *h.FailedCount
		}
		out.Hosts = append(out.Hosts, h)
	}
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Host < out.Hosts[j].Host })
	if book != nil {
		for _, s := range book.List(now) {
			v := Review{Silence: s, Condition: "unknown", Alert: byKey[s.ID]}
			if inv := hosts[s.Host]; inv != nil {
				v.CollectedAt = &inv.CollectedAt
				v.Condition = "cleared"
				// An uncollected source cannot prove recovery.
				if !conditionCollected(inv, s.Rule) {
					v.Condition = "unknown"
				}
			}
			if v.Alert != nil {
				v.Condition = "still_true"
			}
			switch v.Condition {
			case "still_true":
				out.Counts.StillTrue++
			case "cleared":
				out.Counts.Cleared++
			default:
				out.Counts.Unknown++
			}
			out.Silences = append(out.Silences, v)
		}
		sort.SliceStable(out.Silences, func(i, j int) bool {
			return out.Silences[i].Condition == "cleared" && out.Silences[j].Condition != "cleared"
		})
	}
	return out
}

// A missing source is unknown, not evidence that a condition recovered.
func conditionCollected(inv *inventory.Inventory, rule string) bool {
	switch {
	case strings.HasPrefix(rule, "systemd."):
		return inv.Systemd != nil && inv.Systemd.Available && inv.Systemd.Units != nil
	case strings.HasPrefix(rule, "zfs.snapshot."):
		return inv.ZFS != nil && inv.ZFS.Available && inv.ZFS.Pools != nil && inv.ZFS.Snapshots != nil
	case strings.HasPrefix(rule, "zfs."):
		return inv.ZFS != nil && inv.ZFS.Available && inv.ZFS.Pools != nil
	case strings.HasPrefix(rule, "proxmox.backup."):
		p := inv.Proxmox
		return p != nil && p.Available && p.Guests != nil && p.Storages != nil && p.BackupJobs != nil && p.Backups != nil
	case strings.HasPrefix(rule, "proxmox."):
		return inv.Proxmox != nil && inv.Proxmox.Available && inv.Proxmox.Guests != nil && inv.Proxmox.Storages != nil
	case strings.HasPrefix(rule, "docker."):
		return inv.Docker != nil && inv.Docker.Available && inv.Docker.Containers != nil
	case rule == "firewall.unknown":
		return inv.Firewall != nil
	case strings.HasPrefix(rule, "firewall."):
		return inv.Firewall != nil && inv.Firewall.Available && inv.Network != nil && inv.Network.Listeners != nil
	case rule == "host.filesystem.capacity":
		return inv.Host.Filesystems != nil
	case rule == "host.stale" || rule == "host.schema" || rule == "agent.outdated":
		return true
	default:
		return false
	}
}
