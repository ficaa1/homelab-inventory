// Package vpnwatch owns the API-side VPN observations and durable automatic
// reconnect cooldown. Discovery never executes commands inside containers.
package vpnwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/vpnprobe"
)

type Autofix struct {
	Enabled       bool       `json:"enabled"`
	AfterMinutes  int        `json:"afterMinutes"`
	State         string     `json:"state"`
	Reason        string     `json:"reason"`
	LastRun       *time.Time `json:"lastRun"`
	CooldownUntil *time.Time `json:"cooldownUntil"`
}
type Snapshot struct {
	State               string                   `json:"state"`
	Health              string                   `json:"health"`
	StartedAt           *time.Time               `json:"startedAt"`
	Host                string                   `json:"host"`
	Container           string                   `json:"container"`
	Status              string                   `json:"status"`
	Reason              string                   `json:"reason"`
	Since               time.Time                `json:"since"`
	HandshakeAgeSeconds *float64                 `json:"handshakeAgeSeconds"`
	Facts               vpnprobe.Facts           `json:"facts"`
	Dependents          []inventory.VPNDependent `json:"dependents"`
	Autofix             Autofix                  `json:"autofix"`
}
type durable struct {
	Host      string     `json:"host"`
	Container string     `json:"container"`
	BadSince  *time.Time `json:"badSince"`
	LastAuto  *time.Time `json:"lastAuto"`
}
type Options struct {
	Runner   *execallow.Runner
	Actions  *actions.Manager
	Settings func() (bool, int)
	Message  func(context.Context, notify.Message)
	// Probes and Now allow deterministic offline checks of cadence and cooldown.
	WG        func(context.Context, string) (vpnprobe.Facts, error)
	Exit      func(context.Context, string) (vpnprobe.Facts, error)
	Inspect   func(context.Context, string) (vpnprobe.Container, error)
	Reconnect func(context.Context, string) (actions.Entry, error)
	Direct    func(context.Context) error
	Now       func() time.Time
}
type gateway struct {
	saved                         durable
	snap                          Snapshot
	lastWG, lastExit, lastInspect time.Time
	exitOK                        bool
	failures                      int
	startedAt                     time.Time
	liveState, liveHealth         string
	autofixReason                 string
	verifiedAt                    time.Time
	revision                      uint64 // Invalidates in-flight probes when a manual action finishes.
}
type persisted struct {
	Gateways map[string]durable `json:"gateways"`
}
type Watch struct {
	mu       sync.Mutex
	checkMu  sync.Mutex
	opts     Options
	path     string
	saved    map[string]durable
	gateways map[string]gateway
}

func Open(dir string, opts Options) (*Watch, error) {
	w := &Watch{opts: opts, path: filepath.Join(dir, "state-vpn-watch.json"), saved: map[string]durable{}, gateways: map[string]gateway{}}
	if opts.Now == nil {
		w.opts.Now = time.Now
	}
	if opts.WG == nil {
		w.opts.WG = func(ctx context.Context, n string) (vpnprobe.Facts, error) {
			return vpnprobe.WireGuard(ctx, opts.Runner, n)
		}
	}
	if opts.Exit == nil {
		w.opts.Exit = func(ctx context.Context, n string) (vpnprobe.Facts, error) { return vpnprobe.Exit(ctx, opts.Runner, n) }
	}
	if opts.Inspect == nil {
		w.opts.Inspect = func(ctx context.Context, n string) (vpnprobe.Container, error) {
			return vpnprobe.Inspect(ctx, opts.Runner, n)
		}
	}
	if opts.Direct == nil {
		w.opts.Direct = directCheck
	}
	b, err := os.ReadFile(w.path)
	if err == nil {
		// The former single-gateway shape and malformed files start fresh.
		// This state has never been deployed; incompatible state must not block startup.
		var state persisted
		if json.Unmarshal(b, &state) == nil && state.Gateways != nil {
			for name, d := range state.Gateways {
				if name != "" && name == d.Container && d.Host != "" {
					w.saved[name] = d
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return w, nil
}
func (w *Watch) save() error {
	b, err := json.Marshal(persisted{Gateways: w.saved})
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(w.path, b, 0600)
}
func (w *Watch) settings() (bool, int) {
	if w.opts.Settings == nil {
		return false, 10
	}
	on, n := w.opts.Settings()
	if n < 1 || n > 1440 {
		n = 10
	}
	return on, n
}
func (w *Watch) snapshot(g gateway, now time.Time) Snapshot {
	s := g.snap
	if s.Facts.LatestHandshake != nil {
		a := now.Sub(*s.Facts.LatestHandshake).Seconds()
		s.HandshakeAgeSeconds = &a
	}
	on, n := w.settings()
	s.Autofix = Autofix{Enabled: on, AfterMinutes: n, State: "off", LastRun: g.saved.LastAuto}
	if on {
		s.Autofix.State = "watching"
		if g.autofixReason != "" {
			s.Autofix.State, s.Autofix.Reason = "skipped", g.autofixReason
		}
	}
	if g.saved.LastAuto != nil {
		t := g.saved.LastAuto.Add(time.Hour)
		s.Autofix.CooldownUntil = &t
		if on && now.Before(t) {
			s.Autofix.State = "cooldown"
		}
	}
	s.Dependents = append([]inventory.VPNDependent{}, s.Dependents...)
	return s
}

// Snapshot reads one gateway without waiting for Docker probes.
func (w *Watch) Snapshot(container string) Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshot(w.gateways[container], w.opts.Now().UTC())
}
func (w *Watch) Health() []alert.VPNHealth {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []alert.VPNHealth{}
	for _, g := range w.gateways {
		s := g.snap
		out = append(out, alert.VPNHealth{Host: s.Host, Container: s.Container, Status: s.Status, Reason: s.Reason, LatestHandshake: s.Facts.LatestHandshake, Since: &s.Since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Container < out[j].Container })
	return out
}
func (w *Watch) Loop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		_ = w.Check(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Check serializes probes with checkMu. mu protects only published state;
// probes work on copies so alerts and panel readers never wait for Docker.
func (w *Watch) Check(ctx context.Context, panel bool) error {
	if !w.checkMu.TryLock() {
		return nil
	}
	defer w.checkMu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w.opts.Runner == nil || w.opts.Runner.Inventory == nil {
		return nil
	}
	inv, err := w.opts.Runner.Inventory()
	if err != nil {
		return err
	}
	vs := inv.VPNGateways()
	w.mu.Lock()
	keep := map[string]bool{}
	for _, v := range vs {
		if v.Container != "" {
			keep[v.Container] = true
		}
	}
	for name, d := range w.saved {
		if !keep[name] || d.Host != inv.Host.Hostname {
			delete(w.saved, name)
			delete(w.gateways, name)
		}
	}
	for name := range w.gateways {
		if !keep[name] {
			delete(w.gateways, name)
		}
	}
	err = w.save()
	w.mu.Unlock()
	if err != nil {
		return err
	}
	if w.opts.Actions != nil && w.opts.Actions.Busy() {
		return nil
	}
	for _, v := range vs {
		if v.Container == "" {
			continue
		}
		now := w.opts.Now().UTC()
		w.mu.Lock()
		g, exists := w.gateways[v.Container]
		if !exists || g.snap.Host != inv.Host.Hostname {
			g = gateway{saved: w.saved[v.Container], snap: Snapshot{Host: inv.Host.Hostname, Container: v.Container, Status: "unknown", Reason: "waiting for a tunnel check", Since: now}}
			g.saved.Host, g.saved.Container = inv.Host.Hostname, v.Container
			w.gateways[v.Container] = g
		}
		g.snap.Dependents = append([]inventory.VPNDependent{}, v.Dependents...)
		w.mu.Unlock()
		if !exists && w.opts.Actions != nil {
			for _, e := range w.opts.Actions.Log(100) {
				if e.Kind == "vpn_reconnect" && e.Target == v.Container && e.Result == "ok" {
					g.verifiedAt = e.At
					break
				}
			}
		}
		if err := w.probe(ctx, panel, now, inv, &v, &g); err != nil {
			return err
		}
		w.mu.Lock()
		current := w.gateways[v.Container]
		if current.revision != g.revision {
			w.mu.Unlock()
			continue
		}
		w.gateways[v.Container], w.saved[v.Container] = g, g.saved
		err = w.save()
		w.mu.Unlock()
		if err != nil {
			return err
		}
		if !panel {
			if err := w.autofix(ctx, v.Container, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// probe mutates only a private copy; it never holds the reader mutex.
func (w *Watch) probe(ctx context.Context, panel bool, now time.Time, inv *inventory.Inventory, v *inventory.VPN, g *gateway) error {
	status, reason := "unknown", "waiting for a tunnel check"
	state := v.State
	g.snap.Dependents = append([]inventory.VPNDependent{}, v.Dependents...)
	inspectOK := false
	if now.Sub(g.lastInspect) >= 60*time.Second {
		g.lastInspect = now
		c, e := w.opts.Inspect(ctx, v.Container)
		if ctx.Err() != nil {
			g.lastInspect = time.Time{}
			return ctx.Err()
		}
		if e == nil {
			if !g.startedAt.IsZero() && !c.StartedAt.Equal(g.startedAt) {
				g.lastWG = time.Time{}
				g.lastExit = time.Time{}
				g.failures = 0
				g.exitOK = false
				g.snap.Facts = vpnprobe.Facts{}
			}
			state = c.State
			g.liveState = c.State
			g.liveHealth = c.Health
			g.startedAt = c.StartedAt
			inspectOK = true
		}
	} else {
		inspectOK = !g.startedAt.IsZero()
		if inspectOK {
			state = g.liveState
		}
	}
	g.snap.State = state
	g.snap.Health = g.liveHealth
	if !g.startedAt.IsZero() {
		t := g.startedAt
		g.snap.StartedAt = &t
	}
	if state != "running" {
		status, reason = "down", "gateway is not running"
		for i := range g.snap.Dependents {
			g.snap.Dependents[i].Attached = nil
		}
	} else {
		if now.Sub(g.lastWG) >= 60*time.Second {
			g.lastWG = now
			f, e := w.opts.WG(ctx, v.Container)
			if ctx.Err() != nil {
				g.lastWG = time.Time{}
				return ctx.Err()
			}
			if e == nil {
				g.snap.Facts.LatestHandshake = f.LatestHandshake
				g.snap.Facts.Endpoint = f.Endpoint
				g.snap.Facts.RXBytes = f.RXBytes
				g.snap.Facts.TXBytes = f.TXBytes
			} else {
				g.snap.Facts.LatestHandshake = nil
			}
		}
		h := g.snap.Facts.LatestHandshake
		due := (g.failures > 0 && now.Sub(g.lastExit) >= 30*time.Second) || (h != nil && now.Sub(*h) > 180*time.Second && now.Sub(g.lastExit) >= 60*time.Second) || ((h == nil || panel) && now.Sub(g.lastExit) >= 5*time.Minute)
		if due {
			f, e := w.opts.Exit(ctx, v.Container)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			g.lastExit = now
			if e != nil {
				g.failures++
				g.exitOK = false
			} else {
				g.failures = 0
				g.exitOK = true
				g.snap.Facts.ExitIP = f.ExitIP
				g.snap.Facts.Country = f.Country
				g.snap.Facts.City = f.City
				g.snap.Facts.Org = f.Org
			}
		}
		if g.failures >= 2 {
			status, reason = "down", "two exit checks failed at least 30 seconds apart"
		} else if g.exitOK || (h != nil && now.Sub(*h) >= 0 && now.Sub(*h) < 180*time.Second) {
			status, reason = "ok", "tunnel connectivity verified"
		} else {
			reason = "exit check failed; waiting for confirmation"
		}
		if status != "down" {
			for i, d := range g.snap.Dependents {
				cut := d.State == "running" && d.Attached != nil && !*d.Attached && !inv.CollectedAt.Before(g.verifiedAt)
				if inspectOK {
					c, e := w.opts.Inspect(ctx, d.Name)
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if e == nil {
						g.snap.Dependents[i].State = c.State
						g.snap.Dependents[i].StartedAt = &c.StartedAt
						if c.State != "running" {
							cut = false
							g.snap.Dependents[i].Attached = nil
						}
						if c.State == "running" && !c.StartedAt.IsZero() && !g.startedAt.IsZero() {
							cut = cut || c.StartedAt.Before(g.startedAt)
							a := !cut
							g.snap.Dependents[i].Attached = &a
						}
					}
				}
				if cut {
					status, reason = "cutoff", "a running dependent uses the gateway's previous network namespace"
				}
			}
		}
	}
	if status != g.snap.Status {
		g.snap.Since = now
		if (status == "down" || status == "cutoff") && g.saved.BadSince != nil {
			g.snap.Since = *g.saved.BadSince
		}
	}
	g.snap.Status, g.snap.Reason = status, reason
	bad := status == "down" || status == "cutoff"
	if bad {
		if g.saved.BadSince == nil {
			t := now
			g.saved.BadSince = &t
		}
	} else if status == "ok" {
		// Unknown probes after a restart do not prove recovery. Keep the timer,
		// but autofix below still requires a newly confirmed down/cutoff state.
		g.saved.BadSince = nil
		g.autofixReason = ""
	}
	return nil
}
func (w *Watch) autofix(ctx context.Context, container string, now time.Time) error {
	w.mu.Lock()
	g := w.gateways[container]
	w.mu.Unlock()
	on, n := w.settings()
	if !on || (g.snap.Status != "down" && g.snap.Status != "cutoff") || g.saved.BadSince == nil || now.Sub(*g.saved.BadSince) < time.Duration(n)*time.Minute || (g.saved.LastAuto != nil && now.Sub(*g.saved.LastAuto) < time.Hour) {
		return nil
	}
	if w.opts.Actions != nil && w.opts.Actions.Busy() {
		return nil
	}
	if w.opts.Reconnect == nil && w.opts.Actions == nil {
		return nil
	}
	// Only automatic recovery makes this request, outside the container. A
	// failed direct check must not spend the gateway's cooldown or notify.
	if err := w.opts.Direct(ctx); err != nil {
		w.mu.Lock()
		current := w.gateways[container]
		if current.revision == g.revision {
			current.autofixReason = "internet unreachable from the dashboard host; automatic reconnect skipped"
			w.gateways[container] = current
		}
		w.mu.Unlock()
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if on, _ := w.settings(); !on {
		return nil
	}
	// Reserve before execution, so a crash cannot cause an automatic retry loop.
	w.mu.Lock()
	current := w.gateways[container]
	if current.revision != g.revision {
		w.mu.Unlock()
		return nil
	}
	previousAuto := current.saved.LastAuto
	t := now
	current.saved.LastAuto = &t
	current.autofixReason = ""
	w.gateways[container], w.saved[container] = current, current.saved
	err := w.save()
	w.mu.Unlock()
	if err != nil {
		return err
	}
	status, badSince := g.snap.Status, *g.saved.BadSince
	actionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Second)
	defer cancel()
	var e actions.Entry
	if w.opts.Reconnect != nil {
		e, err = w.opts.Reconnect(actionCtx, container)
	} else {
		inv, x := w.opts.Runner.Inventory()
		err = x
		if err == nil {
			p, x := actions.Resolve(inv, "vpn_reconnect", container, w.opts.Runner.Policy)
			err = x
			if err == nil {
				e, err = w.opts.Actions.TryExecute(actionCtx, "vpn_reconnect", container, "auto", p.ContainerSetHash)
			}
		}
	}
	if errors.Is(err, actions.ErrBusy) {
		w.mu.Lock()
		current := w.gateways[container]
		current.saved.LastAuto = previousAuto
		w.gateways[container], w.saved[container] = current, current.saved
		err := w.save()
		w.mu.Unlock()
		return err
	}
	msg := notify.Message{Title: "Automatic VPN reconnect", Type: "failure"}
	if err == nil && e.Result == "ok" {
		msg.Type = "success"
		msg.Body = fmt.Sprintf("VPN %s was %s for %d min and was reconnected automatically.", container, status, int(now.Sub(badSince).Minutes()))
		if e.ExitIP != nil {
			msg.Body += " Exit " + *e.ExitIP
			if e.Country != nil {
				msg.Body += " (" + *e.Country + ")"
			}
			msg.Body += "."
		}
	} else {
		reason := e.Reason
		if err != nil {
			reason = "reconnect could not execute"
		}
		if reason == "" {
			reason = "tunnel verification failed"
		}
		msg.Body = fmt.Sprintf("Automatic VPN reconnect for %s failed: %s. Next automatic attempt after %s UTC.", container, reason, now.Add(time.Hour).Format("15:04"))
	}
	if w.opts.Message != nil {
		messageCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		w.opts.Message(messageCtx, msg)
		stop()
	}
	e.Target = container
	if err != nil {
		e.Result = "failed"
	}
	w.Reconnected(e)
	return nil
}

// resetObservations invalidates any in-flight copy and bypasses probe caches.
func (g *gateway) resetObservations(e actions.Entry) {
	if e.Result == "ok" {
		g.verifiedAt = e.At
	}
	g.lastWG, g.lastExit, g.lastInspect = time.Time{}, time.Time{}, time.Time{}
	g.failures, g.exitOK = 0, false
	g.autofixReason = ""
	g.revision++
}

// Reconnected refreshes observations after either a manual or automatic action.
func (w *Watch) Reconnected(e actions.Entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	g, ok := w.gateways[e.Target]
	if !ok {
		return
	}
	g.resetObservations(e)
	w.gateways[e.Target] = g
}
