// Package agenthealth projects rollout facts into one durable view for Hosts
// and alerts. Release authority remains entirely in agentoffers.
package agenthealth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/agentstatus"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

const LagBudget = 45 * time.Minute

var mu sync.Mutex

type Projection struct {
	State     string                 `json:"state"`
	Label     string                 `json:"label"`
	Reason    string                 `json:"reason,omitempty"`
	Published uint64                 `json:"published,omitempty"`
	Offered   uint64                 `json:"offered,omitempty"`
	Running   uint64                 `json:"running,omitempty"`
	Rejected  uint64                 `json:"rejected,omitempty"`
	Updater   string                 `json:"updater,omitempty"`
	Paused    bool                   `json:"paused,omitempty"`
	Unhealthy bool                   `json:"unhealthy"`
	Critical  bool                   `json:"critical"`
	Update    *inventory.AgentUpdate `json:"update,omitempty"`
	Received  time.Time              `json:"received,omitempty"`
}
type entry struct {
	Withdrawn uint64     `json:"withdrawn,omitempty"`
	View      Projection `json:"view"`
	LagAt     time.Time  `json:"lagAt,omitempty"`
	LagTarget uint64     `json:"lagTarget,omitempty"`
	Since     time.Time  `json:"since,omitempty"`
	Evidence  time.Time  `json:"evidence,omitempty"`
}
type Snapshot struct {
	Hosts       map[string]Projection `json:"hosts"`
	Unavailable bool                  `json:"unavailable"`
	Sequence    uint64                `json:"sequence,omitempty"`
	Revision    int                   `json:"revision,omitempty"`
	Version     string                `json:"version,omitempty"`
	Mode        string                `json:"mode,omitempty"`
	SSHHold     bool                  `json:"ssh_hold"`
	Configured  bool                  `json:"configured"`
}

func (s Snapshot) Alerts() []alert.Alert {
	out := []alert.Alert{}
	for host, p := range s.Hosts {
		if p.Unhealthy {
			level := alert.LevelWarning
			if p.Critical {
				level = alert.LevelCritical
			}
			msg := p.Label
			if p.Paused {
				msg += " (paused)"
			}
			if p.Reason != "" {
				msg += ": " + p.Reason
			}
			out = append(out, alert.Alert{Host: host, Rule: "agent.update.unhealthy", Subject: "agent", Level: level, Message: msg})
		}
	}
	if s.Unavailable {
		out = append(out, alert.Alert{Host: "api", Rule: "agent.release.unavailable", Subject: "agent", Level: alert.LevelWarning, Message: "Agent release or offer state unavailable; retaining last known update health"})
	}
	return out
}

// Reason maps safe codes only; arbitrary agent error text is never displayed.
func Reason(u *inventory.AgentUpdate) string {
	if u == nil {
		return "reason not reported by this updater"
	}
	f := u.FirstFailure
	if f == nil {
		return "reason not reported by this updater"
	}
	if f.Code == "unknown" && u.Rejected != 0 && f.Stage == "state" {
		return "rejected by an earlier updater"
	}
	reasons := map[string]string{
		"network_unreachable": "API unreachable", "http_status": "API returned an unsuccessful HTTP status", "signature_invalid": "release signature invalid", "hash_mismatch": "download hash or size mismatch", "platform_missing": "release does not support this platform", "disk_full": "state storage is full", "permission_denied": "permission denied", "read_only_fs": "state filesystem is read-only", "noexec": "execution denied by filesystem", "exec_format": "unsupported executable format", "selftest_timeout": "collector self-test timed out", "selftest_failed": "collector self-test failed", "trial_exit": "candidate collector exited unsuccessfully", "trial_timeout": "candidate collector timed out", "trial_invalid_output": "candidate output or build identity invalid", "trial_new_errors": "candidate produced new error diagnostics", "post_rejected": "inventory delivery failed", "lock_busy": "another updater run holds the lock", "state_corrupt": "updater state is unreadable or corrupt", "manifest_conflict": "different manifest bytes at the reserved sequence", "replay_refused": "release is below the reserved sequence", "earlier_updater": "rejected by an earlier updater", "unknown": "failure reason unknown",
	}
	reason := reasons[f.Code]
	if reason == "" {
		reason = "reason not reported by this updater"
	}
	if agentstatus.ValidStage(f.Stage) {
		return f.Stage + ": " + reason
	}
	return reason
}

// Evaluate serializes the persisted lag/incident ledger across Hosts, notifier
// and digest callers. A ledger I/O error aborts evaluation, never resolves alerts.
func Evaluate(dir, releaseDir string, invs []*inventory.Inventory, now time.Time) (Snapshot, error) {
	mu.Lock()
	defer mu.Unlock()
	ledger := map[string]entry{}
	stateDir := filepath.Join(dir, "agent-health")
	path := filepath.Join(stateDir, "state.json")
	if b, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(b, &ledger); err != nil {
			return Snapshot{}, err
		}
	} else if !os.IsNotExist(err) {
		return Snapshot{}, err
	}
	snap := Snapshot{Hosts: map[string]Projection{}, Configured: releaseDir != ""}
	names := map[string]*inventory.Inventory{}
	for _, inv := range invs {
		if inv != nil {
			names[inv.Host.Hostname] = inv
		}
	}
	hosts, err := agentstatus.Hosts(dir)
	if err != nil {
		return snap, err
	}
	for _, host := range hosts {
		if _, ok := names[host]; !ok {
			names[host] = nil
		}
	}
	for host := range ledger {
		if _, ok := names[host]; !ok {
			names[host] = nil
		}
	}
	var m agentrelease.Manifest
	var c agentrelease.Control
	var offers agentoffers.State
	if snap.Configured {
		m, _, c, err = agentrelease.Read(releaseDir)
		if err == nil {
			offers, _, err = agentoffers.Read(filepath.Join(dir, "agent-offers.jsonl"))
		}
		if err == nil && (c.Sequence != m.Sequence || c.Mode != "auto" && c.Mode != "promote" && c.Mode != "hold") {
			err = fmt.Errorf("invalid release control")
		}
		snap.Unavailable = err != nil
		if !snap.Unavailable {
			snap.Sequence = m.Sequence
			snap.Revision = m.Revision
			snap.Version = m.Version
			snap.Mode = agentoffers.Effective(m, c, offers)
			snap.SSHHold = c.Mode == "hold"
		}
	}
	for host, inv := range names {
		e := ledger[host]
		if snap.Unavailable {
			p := e.View
			if p.Label == "" {
				p = Projection{State: "not_reported", Label: "Update status not reported"}
			}
			p.Critical = p.Unhealthy && !e.Since.IsZero() && now.Sub(e.Since) >= 24*time.Hour
			e.View = p
			ledger[host] = e
			snap.Hosts[host] = p
			continue
		}
		receipt, err := agentstatus.Latest(dir, host)
		if err != nil {
			return snap, err
		}
		u := (*inventory.AgentUpdate)(nil)
		if !receipt.Received.IsZero() {
			v := receipt.Update
			u = &v
		} else if inv != nil {
			u = inv.AgentUpdate
		}
		p := Projection{Published: m.Sequence, Update: u, Received: receipt.Received}
		if u != nil {
			p.Running = u.Sequence
			p.Rejected = u.Rejected
			p.Updater = u.Updater
		}
		offered := snap.Configured && agentoffers.Offered(host, m, c, offers)
		held := snap.Mode == "hold"
		if offered {
			p.Offered = m.Sequence
		}
		// Withdrawal means loss of eligibility outside a hold. Holds pause incidents.
		withdrawn := !held && !offered && (e.View.Offered != 0 || e.View.Paused)
		fresh := !receipt.Received.IsZero() && now.Sub(receipt.Received) <= LagBudget && !receipt.Received.After(now)
		success := u != nil && (u.Result == "ok" || u.Result == "installed") && u.Failures == 0 && u.FirstFailure == nil && u.Rejected == 0
		recovered := offered && fresh && success && u.Sequence >= m.Sequence && receipt.Received.After(e.Evidence)
		if withdrawn {
			e.Withdrawn = e.View.Offered
			if e.Withdrawn == 0 {
				e.Withdrawn = m.Sequence
			}
		}
		if offered {
			e.Withdrawn = 0
		}
		if withdrawn || recovered || !snap.Configured {
			e.Since = time.Time{}
			e.LagAt = time.Time{}
			e.LagTarget = 0
		}
		reportedFailure := u != nil && u.Mode != "unsigned" && (u.Failures > 0 || u.Result == "failed" || u.Result == "rollback" || u.Result == "blocked" || u.Rejected != 0 || u.FirstFailure != nil)
		if !snap.Configured {
			p.State = "not_set_up"
			p.Label = "Signed updates not set up"
		} else if reportedFailure {
			p.State = "failed"
			p.Label = "Failed"
			p.Reason = Reason(u)
			if u.Result == "rollback" {
				p.State = "rollback"
				p.Label = "Rolled back"
			}
			if u.Result == "blocked" || u.Rejected != 0 {
				p.State = "blocked"
				p.Label = fmt.Sprintf("Blocked: release %d was rejected", u.Rejected)
				if u.Rejected == 0 {
					p.Label = fmt.Sprintf("Blocked: release %d was rejected", u.Target)
				}
			}
			// A withdrawn target must not recreate its incident from unchanged evidence.
			if !withdrawn && (e.Withdrawn == 0 || u.Target != 0 && u.Target != e.Withdrawn) && e.Since.IsZero() {
				e.Since = now
				e.Evidence = receipt.Received
			}
		} else if u != nil && u.Mode == "unsigned" {
			p.State = "unsigned"
			p.Label = "Unsigned — updates off"
		} else if u == nil {
			p.State = "not_reported"
			p.Label = "Update status not reported"
		} else if held {
			p.State = "held"
			p.Label = "Held"
		} else if !offered {
			p.State = "waiting"
			p.Label = "Waiting for canary/promotion"
		} else if u.Result == "trial" {
			p.State = "updating"
			p.Label = "Updating"
		} else if success && u.Sequence >= m.Sequence {
			p.State = "current"
			p.Label = "Current"
		} else {
			p.State = "behind"
			p.Label = "Behind"
		}
		if offered && p.Running < m.Sequence && !(u != nil && u.Mode == "unsigned") {
			if e.LagTarget != m.Sequence {
				e.LagTarget = m.Sequence
				e.LagAt = now
			}
			if !e.LagAt.IsZero() && now.Sub(e.LagAt) > LagBudget && e.Since.IsZero() {
				e.Since = e.LagAt
				e.Evidence = receipt.Received
			}
		}
		if e.Since.IsZero() && e.Withdrawn != 0 && !offered && !held {
			p.State = "waiting"
			p.Label = "Waiting for canary/promotion"
		}
		// Missing/old evidence cannot paint an unresolved incident green.
		if !e.Since.IsZero() && !reportedFailure && p.State != "behind" && p.State != "updating" {
			p.State = e.View.State
			p.Label = e.View.Label
			p.Reason = e.View.Reason
		}
		p.Unhealthy = !e.Since.IsZero()
		p.Paused = held && p.Unhealthy
		p.Critical = p.Unhealthy && now.Sub(e.Since) >= 24*time.Hour
		if p.Paused {
			p.Offered = e.View.Offered
		}
		e.View = p
		ledger[host] = e
		snap.Hosts[host] = p
	}
	if err = os.MkdirAll(stateDir, 0700); err != nil {
		return snap, err
	}
	b, err := json.Marshal(ledger)
	if err != nil {
		return snap, err
	}
	if err = fsx.WriteAtomic(path, b, 0600); err != nil {
		return snap, err
	}
	return snap, nil
}
