package notify

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// Sender delivers a message. Apprise is the only implementation; the interface
// exists so the transition logic can be tested without a network.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// StateStore is the slice of internal/store this package needs. Depending on
// the two methods rather than on *store.Store keeps the transition logic
// testable with a map, and keeps the direction of the dependency honest:
// notify needs somewhere to put a small blob, not a run history.
type StateStore interface {
	ReadState(name string, v any) error
	WriteState(name string, v any) error
}

// Notifier evaluates one cycle: read what was reported, diff it against what
// is true now, send the difference, record it.
type Notifier struct {
	State  StateStore
	Sender Sender
	// DigestSender, when set, is where SendDigest posts instead of Sender.
	// The digest is a report and the transitions are alarms, and a report
	// should not compete with an alarm for the same channel — a weekly
	// summary over SMS is a 160-character fragment and an alarm over email
	// is one more thing to mute. Leave nil to send the digest where the
	// transitions go.
	DigestSender Sender
	// Min is the severity floor. Critical-only is the default because it is
	// the level that means "act now"; warnings are opt-in to limit noise.
	Min alert.Level
	Log *log.Logger

	// Options, when set, is consulted at the start of every cycle instead of
	// Min. Notifications are configurable from the dashboard, and a floor read
	// once at construction would mean every change needed a restart — which is
	// the friction that stops a setting ever being changed.
	Options func() Options
	// Recorder logs what was delivered. Optional: nil means no history.
	Recorder *Recorder

	mu       sync.Mutex
	lastRun  time.Time
	lastErr  string
	sent     int
	lastSent time.Time
}

// Options is the configuration read at the start of each cycle.
type Options struct {
	Min alert.Level
	// Enabled false suspends delivery without discarding the endpoint. The
	// cycle still runs and still advances state, so unmuting does not produce
	// a backlog of everything that happened while muted — a mute is "do not
	// tell me", not "tell me later".
	Enabled bool
}

// options resolves the current configuration, falling back to the static
// fields when no accessor is set.
func (n *Notifier) options() Options {
	if n.Options != nil {
		return n.Options()
	}
	return Options{Min: n.Min, Enabled: true}
}

// Run performs one evaluation cycle and returns what changed.
//
// State is written only after the messages are sent, so a failed send is
// retried on the next cycle rather than being silently swallowed — the
// alternative, recording first, turns a transient apprise outage into a
// permanently missed alert.
func (n *Notifier) Run(ctx context.Context, alerts []alert.Alert, now time.Time) (Change, error) {
	var prev State
	if err := n.State.ReadState(StateName, &prev); err != nil {
		n.record(now, err)
		return Change{}, err
	}
	if prev.Active == nil {
		prev.Active = map[string]Entry{}
	}

	opt := n.options()
	next, ch := Diff(prev, alerts, opt.Min, now)

	// First run after deployment: record what is already wrong, say nothing.
	// See State.Seeded.
	if !prev.Seeded {
		if n.Log != nil {
			n.Log.Printf("notify: seeding with %d active condition(s) at %s and above; "+
				"no notification sent for pre-existing alerts", len(next.Active), opt.Min)
		}
		err := n.State.WriteState(StateName, next)
		n.record(now, err)
		return Change{}, err
	}

	if ch.Empty() {
		n.record(now, nil)
		return ch, nil
	}

	// Muted: advance the state without sending. Recording the transitions as
	// seen is deliberate — unmuting should not deliver a backlog of everything
	// that happened while muted, because "do not tell me about this" and "tell
	// me all of it later" are different requests and only one was made.
	if !opt.Enabled {
		if n.Log != nil {
			n.Log.Printf("notify: muted; %d transition(s) recorded without sending", ch.Count())
		}
		err := n.State.WriteState(StateName, next)
		n.record(now, err)
		return ch, err
	}

	for _, m := range Messages(ch, now) {
		if err := n.Sender.Send(ctx, m); err != nil {
			// Do not persist: the next cycle will see the same transition and
			// try again.
			if n.Log != nil {
				n.Log.Printf("notify: %v (state not advanced; will retry)", err)
			}
			n.Recorder.Record(Delivery{
				At: now, Title: m.Title, Type: m.Type, Kind: "transition",
				OK: false, Error: err.Error(),
			})
			n.record(now, err)
			return ch, err
		}
		n.Recorder.Record(Delivery{
			At: now, Title: m.Title, Type: m.Type, Kind: "transition", OK: true,
		})
		n.mu.Lock()
		n.sent++
		n.lastSent = now
		n.mu.Unlock()
	}

	if n.Log != nil {
		n.Log.Printf("notify: %d new, %d escalated, %d resolved",
			len(ch.New), len(ch.Escalated), len(ch.Resolved))
	}
	err := n.State.WriteState(StateName, next)
	n.record(now, err)
	return ch, err
}

func (n *Notifier) record(at time.Time, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lastRun = at
	if err != nil {
		n.lastErr = err.Error()
	} else {
		n.lastErr = ""
	}
}

// Status is what GET /notify reports. It deliberately carries no URL: the
// apprise endpoint is a credential.
type Status struct {
	Enabled   bool        `json:"enabled"`
	MinLevel  alert.Level `json:"min_level"`
	Seeded    bool        `json:"seeded"`
	Active    int         `json:"active"`
	Sent      int         `json:"sent"`
	LastRun   *time.Time  `json:"last_run,omitempty"`
	LastSent  *time.Time  `json:"last_sent,omitempty"`
	LastError string      `json:"last_error,omitempty"`
}

// Status reports what the notifier is doing, for the API and for a human
// checking that it is alive at all.
func (n *Notifier) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()

	opt := n.options()
	st := Status{Enabled: opt.Enabled, MinLevel: opt.Min, Sent: n.sent, LastError: n.lastErr}
	if !n.lastRun.IsZero() {
		t := n.lastRun
		st.LastRun = &t
	}
	if !n.lastSent.IsZero() {
		t := n.lastSent
		st.LastSent = &t
	}
	var s State
	if err := n.State.ReadState(StateName, &s); err == nil {
		st.Seeded = s.Seeded
		st.Active = len(s.Active)
	}
	return st
}

// Loop runs Run until ctx is cancelled. alertsFn is called each time rather
// than the alerts being passed in once, because the whole point is to re-read
// what the agents have posted since.
//
// everyFn is consulted each cycle rather than a fixed interval being captured
// at construction, so shortening the interval from the dashboard takes effect
// on the next tick instead of at the next restart. The ticker itself runs at a
// fixed fine granularity and the interval is enforced by comparing elapsed
// time, which avoids having to tear down and rebuild a ticker on every change.
func (n *Notifier) Loop(ctx context.Context, everyFn func() time.Duration, alertsFn func() ([]alert.Alert, error)) {
	const granularity = 30 * time.Second
	t := time.NewTicker(granularity)
	defer t.Stop()

	var last time.Time
	for {
		if !last.IsZero() {
			every := everyFn()
			if every <= 0 {
				every = 5 * time.Minute
			}
			if time.Since(last) < every {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
				}
				continue
			}
		}
		last = time.Now()
		// Evaluate immediately on start as well as on every tick: a restart
		// should not mean waiting a full interval to notice a fire.
		alerts, err := alertsFn()
		if err != nil {
			if n.Log != nil {
				n.Log.Printf("notify: evaluating alerts: %v", err)
			}
		} else if _, err := n.Run(ctx, alerts, time.Now()); err != nil {
			// Run has already logged; nothing to add.
			_ = err
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
