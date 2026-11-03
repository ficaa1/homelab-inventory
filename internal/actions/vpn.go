package actions

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/deps"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/vpnprobe"
)

func resolveVPN(inv *inventory.Inventory, target string, p Preview) (Preview, error) {
	gateway, err := execallow.FindContainer(inv, target)
	if err != nil || inv.VPN == nil || inv.VPN.Container != target {
		return p, errTargetMissing
	}
	names := map[string]bool{}
	for _, d := range inv.VPN.Dependents {
		names[d.Name] = true
	}
	for _, c := range inv.Docker.Containers {
		reference, ok := strings.CutPrefix(c.NetworkMode, "container:")
		if !ok || reference == "" || c.Name == target {
			continue
		}
		if reference == target || reference == gateway.ID && gateway.ID != "" {
			names[c.Name] = true
			continue
		}
		// Docker commonly supplies a full ID while inventory stores its short ID.
		// Accept a prefix only if it identifies exactly one inventory container.
		matches := []string{}
		for _, candidate := range inv.Docker.Containers {
			if candidate.ID != "" && (strings.HasPrefix(reference, candidate.ID) || strings.HasPrefix(candidate.ID, reference)) {
				matches = append(matches, candidate.Name)
			}
		}
		if len(matches) == 1 && matches[0] == target {
			names[c.Name] = true
		}
	}
	delete(names, target)
	dependents := []string{}
	for name := range names {
		if _, err := execallow.FindContainer(inv, name); err != nil {
			return p, errors.New("VPN dependent is not in latest Docker inventory")
		}
		dependents = append(dependents, name)
	}
	sort.Strings(dependents)
	p.Containers = append([]string{target}, dependents...)
	p.ContainerSetHash = ContainerSetHash(p.Containers)
	for _, name := range p.Containers {
		p.Operations = append(p.Operations, Operation{"container-restart", map[string]string{"container": name}})
	}
	for _, name := range dependents {
		p.Impact = append(p.Impact, Impact{target, deps.Dependent{Name: name, Kind: deps.KindNetwork, Effect: "restarts after the VPN gateway to restore networking"}})
	}
	return p, nil
}

const reconnectWait = 90 * time.Second
const reconnectInterval = 3 * time.Second

func recentHandshake(f vpnprobe.Facts, now time.Time) bool {
	if f.LatestHandshake == nil {
		return false
	}
	age := now.Sub(*f.LatestHandshake)
	return age >= 0 && age < 60*time.Second
}
func (m *Manager) awaitTunnel(ctx context.Context, target string) bool {
	wait, cancel := context.WithTimeout(ctx, reconnectWait)
	defer cancel()
	ticker := time.NewTicker(reconnectInterval)
	defer ticker.Stop()
	for {
		c, err := vpnprobe.Inspect(wait, m.runner, target)
		if err == nil && c.State == "running" && (c.Health == "" || c.Health == "healthy") {
			f, err := vpnprobe.WireGuard(wait, m.runner, target)
			if err == nil && recentHandshake(f, time.Now()) {
				return true
			}
			if _, err := vpnprobe.Exit(wait, m.runner, target); err == nil {
				return true
			}
		}
		select {
		case <-wait.Done():
			return false
		case <-ticker.C:
		}
	}
}

// reconnect always attempts every dependent after a gateway restart, including
// when tunnel readiness times out. Child probe failures never enter the audit.
func (m *Manager) reconnect(ctx context.Context, p Preview, e Entry) Entry {
	e.Result = "ok"
	e.ExitCode = 0
	gateway := m.runner.Run(ctx, "container-restart", p.Operations[0].Args)
	if gateway.ExitCode != 0 || gateway.Error != "" {
		e.Result = "failed"
		e.ExitCode = gateway.ExitCode
		e.Reason = "VPN gateway restart failed"
		e.Results = append(e.Results, ContainerResult{Container: p.Target, Status: "failed", Error: e.Reason})
		for _, name := range p.Containers[1:] {
			e.Results = append(e.Results, ContainerResult{Container: name, Status: "skipped"})
		}
		return e
	}
	e.Results = append(e.Results, ContainerResult{Container: p.Target, Status: "restarted"})
	ready := m.awaitTunnel(ctx, p.Target)
	if !ready {
		e.Result = "partial"
		e.ExitCode = -1
		e.Reason = "VPN tunnel readiness timed out; dependent recovery was attempted"
	}
	// Preserve the dependent recovery attempt when only the readiness deadline
	// expires. A caller cancellation still bounds the entire action.
	for _, op := range p.Operations[1:] {
		res := m.runner.Run(ctx, op.Command, op.Args)
		result := ContainerResult{Container: op.Args["container"], Status: "restarted"}
		if res.ExitCode != 0 || res.Error != "" {
			result.Status = "failed"
			result.Error = "VPN dependent restart failed"
			e.Result = "partial"
			e.ExitCode = -1
		}
		e.Results = append(e.Results, result)
	}
	exit, exitErr := vpnprobe.Exit(ctx, m.runner, p.Target)
	if exitErr == nil {
		e.ExitIP = exit.ExitIP
		e.Country = exit.Country
	} else {
		e.Result = "partial"
		e.ExitCode = -1
		e.Reason = "VPN exit verification failed"
	}
	state, stateErr := vpnprobe.Inspect(ctx, m.runner, p.Target)
	if stateErr != nil || state.State != "running" || (state.Health != "" && state.Health != "healthy") {
		e.Result = "partial"
		e.ExitCode = -1
		e.Reason = "VPN gateway verification failed"
	}
	for i, name := range p.Containers[1:] {
		child, err := vpnprobe.Inspect(ctx, m.runner, name)
		if err != nil || stateErr != nil || child.State != "running" || !child.StartedAt.After(state.StartedAt) {
			e.Result = "partial"
			e.ExitCode = -1
			if e.Results[i+1].Status != "failed" {
				e.Results[i+1].Status = "failed"
				e.Results[i+1].Error = "VPN dependent state verification failed"
			}
		}
	}
	if e.Result == "ok" {
		e.Reason = "VPN tunnel and dependent containers verified"
	}
	if e.Result == "partial" && e.Reason == "" {
		e.Reason = "VPN dependent recovery incomplete"
	}
	if !ready && exitErr == nil && stateErr == nil && e.Reason == "VPN tunnel readiness timed out; dependent recovery was attempted" {
		e.Reason = "VPN tunnel readiness timed out; final exit verification succeeded"
	}
	return e
}
