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
	Now       func() time.Time
}
type Watch struct {
	mu                            sync.Mutex
	checkMu                       sync.Mutex
	opts                          Options
	path                          string
	saved                         durable
	snap                          Snapshot
	lastWG, lastExit, lastInspect time.Time
	exitOK                        bool
	failures                      int
	startedAt                     time.Time
	liveState                     string
	liveHealth                    string
	verifiedAt                    time.Time
}

func Open(dir string, opts Options) (*Watch, error) {
	w := &Watch{opts: opts, path: filepath.Join(dir, "state-vpn-watch.json")}
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
	b, err := os.ReadFile(w.path)
	if err == nil {
		if json.Unmarshal(b, &w.saved) != nil {
			return nil, errors.New("invalid VPN watch state")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return w, nil
}
func (w *Watch) save() error {
	b, err := json.Marshal(w.saved)
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
func (w *Watch) snapshot(now time.Time) Snapshot {
	s := w.snap
	if s.Facts.LatestHandshake != nil {
		a := now.Sub(*s.Facts.LatestHandshake).Seconds()
		s.HandshakeAgeSeconds = &a
	}
	on, n := w.settings()
	s.Autofix = Autofix{Enabled: on, AfterMinutes: n, State: "off", LastRun: w.saved.LastAuto}
	if on {
		s.Autofix.State = "watching"
	}
	if w.saved.LastAuto != nil {
		t := w.saved.LastAuto.Add(time.Hour)
		s.Autofix.CooldownUntil = &t
		if on && now.Before(t) {
			s.Autofix.State = "cooldown"
		}
	}
	s.Dependents = append([]inventory.VPNDependent{}, s.Dependents...)
	return s
}
func (w *Watch) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshot(w.opts.Now().UTC())
}
func (w *Watch) Health() *alert.VPNHealth {
	s := w.Snapshot()
	if s.Container == "" {
		return nil
	}
	return &alert.VPNHealth{Host: s.Host, Container: s.Container, Status: s.Status, Reason: s.Reason, LatestHandshake: s.Facts.LatestHandshake, Since: &s.Since}
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

// Check performs fixed local probes. Panel reads share the five-minute exit
// cache; a failed exit check is retried after thirty seconds regardless of it.
func (w *Watch) Check(ctx context.Context, panel bool) error {
	if !w.checkMu.TryLock() {
		return nil
	}
	defer w.checkMu.Unlock()
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.opts.Now().UTC()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if w.opts.Actions != nil && w.opts.Actions.Busy() {
		return nil
	}
	if w.opts.Runner == nil || w.opts.Runner.Inventory == nil {
		return nil
	}
	inv, err := w.opts.Runner.Inventory()
	if err != nil {
		return err
	}
	if inv == nil || inv.VPN == nil || inv.VPN.Container == "" {
		w.snap = Snapshot{}
		w.saved.BadSince = nil
		return w.save()
	}
	v := inv.VPN
	if w.snap.Host != inv.Host.Hostname || w.snap.Container != v.Container {
		w.verifiedAt = time.Time{}
		if w.opts.Actions != nil {
			for _, e := range w.opts.Actions.Log(100) {
				if e.Kind == "vpn_reconnect" && e.Target == v.Container && e.Result == "ok" {
					w.verifiedAt = e.At
					break
				}
			}
		}
		w.snap = Snapshot{Host: inv.Host.Hostname, Container: v.Container, Status: "unknown", Reason: "waiting for a tunnel check", Since: now}
		w.lastWG = time.Time{}
		w.lastExit = time.Time{}
		w.lastInspect = time.Time{}
		w.failures = 0
		w.exitOK = false
		w.startedAt = time.Time{}
		if w.saved.Host != inv.Host.Hostname || w.saved.Container != v.Container {
			w.saved.BadSince = nil
		}
		w.saved.Host = inv.Host.Hostname
		w.saved.Container = v.Container
	}
	status, reason := "unknown", "waiting for a tunnel check"
	state := v.State
	w.snap.Dependents = append([]inventory.VPNDependent{}, v.Dependents...)
	inspectOK := false
	if now.Sub(w.lastInspect) >= 60*time.Second {
		w.lastInspect = now
		c, e := w.opts.Inspect(ctx, v.Container)
		if ctx.Err() != nil {
			w.lastInspect = time.Time{}
			return ctx.Err()
		}
		if e == nil {
			if !w.startedAt.IsZero() && !c.StartedAt.Equal(w.startedAt) {
				w.lastWG = time.Time{}
				w.lastExit = time.Time{}
				w.failures = 0
				w.exitOK = false
				w.snap.Facts = vpnprobe.Facts{}
			}
			state = c.State
			w.liveState = c.State
			w.liveHealth = c.Health
			w.startedAt = c.StartedAt
			inspectOK = true
		}
	} else {
		inspectOK = !w.startedAt.IsZero()
		if inspectOK {
			state = w.liveState
		}
	}
	w.snap.State = state
	w.snap.Health = w.liveHealth
	if !w.startedAt.IsZero() {
		t := w.startedAt
		w.snap.StartedAt = &t
	}
	if state != "running" {
		status, reason = "down", "gateway is not running"
		for i := range w.snap.Dependents {
			w.snap.Dependents[i].Attached = nil
		}
	} else {
		if now.Sub(w.lastWG) >= 60*time.Second {
			w.lastWG = now
			f, e := w.opts.WG(ctx, v.Container)
			if ctx.Err() != nil {
				w.lastWG = time.Time{}
				return ctx.Err()
			}
			if e == nil {
				w.snap.Facts.LatestHandshake = f.LatestHandshake
				w.snap.Facts.Endpoint = f.Endpoint
				w.snap.Facts.RXBytes = f.RXBytes
				w.snap.Facts.TXBytes = f.TXBytes
			} else {
				w.snap.Facts.LatestHandshake = nil
			}
		}
		h := w.snap.Facts.LatestHandshake
		due := (w.failures > 0 && now.Sub(w.lastExit) >= 30*time.Second) || (h != nil && now.Sub(*h) > 180*time.Second && now.Sub(w.lastExit) >= 60*time.Second) || ((h == nil || panel) && now.Sub(w.lastExit) >= 5*time.Minute)
		if due {
			f, e := w.opts.Exit(ctx, v.Container)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			w.lastExit = now
			if e != nil {
				w.failures++
				w.exitOK = false
			} else {
				w.failures = 0
				w.exitOK = true
				w.snap.Facts.ExitIP = f.ExitIP
				w.snap.Facts.Country = f.Country
				w.snap.Facts.City = f.City
				w.snap.Facts.Org = f.Org
			}
		}
		if w.failures >= 2 {
			status, reason = "down", "two exit checks failed at least 30 seconds apart"
		} else if w.exitOK || (h != nil && now.Sub(*h) >= 0 && now.Sub(*h) < 180*time.Second) {
			status, reason = "ok", "tunnel connectivity verified"
		} else {
			reason = "exit check failed; waiting for confirmation"
		}
		if status != "down" {
			for i, d := range w.snap.Dependents {
				cut := d.State == "running" && d.Attached != nil && !*d.Attached && !inv.CollectedAt.Before(w.verifiedAt)
				if inspectOK {
					c, e := w.opts.Inspect(ctx, d.Name)
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if e == nil {
						w.snap.Dependents[i].State = c.State
						w.snap.Dependents[i].StartedAt = &c.StartedAt
						if c.State != "running" {
							cut = false
							w.snap.Dependents[i].Attached = nil
						}
						if c.State == "running" && !c.StartedAt.IsZero() && !w.startedAt.IsZero() {
							cut = cut || c.StartedAt.Before(w.startedAt)
							a := !cut
							w.snap.Dependents[i].Attached = &a
						}
					}
				}
				if cut {
					status, reason = "cutoff", "a running dependent uses the gateway's previous network namespace"
				}
			}
		}
	}
	if status != w.snap.Status {
		w.snap.Since = now
		if (status == "down" || status == "cutoff") && w.saved.BadSince != nil {
			w.snap.Since = *w.saved.BadSince
		}
	}
	w.snap.Status, w.snap.Reason = status, reason
	bad := status == "down" || status == "cutoff"
	if bad {
		if w.saved.BadSince == nil {
			t := now
			w.saved.BadSince = &t
		}
	} else if status == "ok" {
		// Unknown probes after a restart do not prove recovery. Keep the timer,
		// but autofix below still requires a newly confirmed down/cutoff state.
		w.saved.BadSince = nil
	}
	if err = w.save(); err != nil {
		return err
	}
	if panel {
		return nil
	}
	return w.autofix(ctx, now)
}
func (w *Watch) autofix(ctx context.Context, now time.Time) error {
	on, n := w.settings()
	if !on || (w.snap.Status != "down" && w.snap.Status != "cutoff") || w.saved.BadSince == nil || now.Sub(*w.saved.BadSince) < time.Duration(n)*time.Minute || (w.saved.LastAuto != nil && now.Sub(*w.saved.LastAuto) < time.Hour) {
		return nil
	}
	if w.opts.Actions != nil && w.opts.Actions.Busy() {
		return nil
	}
	if w.opts.Reconnect == nil && w.opts.Actions == nil {
		return nil
	}
	// Reserve before execution, so a crash cannot cause an automatic retry loop.
	previousAuto := w.saved.LastAuto
	t := now
	w.saved.LastAuto = &t
	if err := w.save(); err != nil {
		return err
	}
	container, status, badSince := w.snap.Container, w.snap.Status, *w.saved.BadSince
	w.mu.Unlock()
	actionCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 300*time.Second)
	defer cancel()
	var e actions.Entry
	var err error
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
		w.saved.LastAuto = previousAuto
		return w.save()
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
		msg.Body = fmt.Sprintf("Automatic VPN reconnect failed: %s. Next automatic attempt after %s UTC.", reason, now.Add(time.Hour).Format("15:04"))
	}
	if w.opts.Message != nil {
		messageCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		w.opts.Message(messageCtx, msg)
		stop()
	}
	w.mu.Lock()
	if err == nil && e.Result == "ok" {
		w.verifiedAt = e.At
	}
	w.lastWG = time.Time{}
	w.lastExit = time.Time{}
	w.lastInspect = time.Time{}
	return nil
}

// Reconnected refreshes observations after an action, bypassing the exit cache.
// A successful verified action is newer evidence than the previous inventory.
func (w *Watch) Reconnected(e actions.Entry) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.snap.Container != e.Target {
		return
	}
	if e.Result == "ok" {
		w.verifiedAt = e.At
	}
	w.lastWG = time.Time{}
	w.lastExit = time.Time{}
	w.lastInspect = time.Time{}
	w.failures = 0
	w.exitOK = false
}
