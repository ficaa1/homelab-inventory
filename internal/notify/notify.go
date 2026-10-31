// Package notify turns the alert set into notifications, and — the part that
// matters — sends one only when something has actually changed.
//
// The agents post a fresh inventory every 15 minutes and the alert set is
// mostly static: a cancelled scrub stays cancelled, a crash-looping container
// keeps crash-looping. Firing the same message at someone 96 times a day is
// how a monitoring system gets muted, and a muted monitoring system is worse
// than none, because it still looks like it is working.
//
// So this notifies on transitions rather than on state. An alert that appears
// is news. An alert that clears is news, and worth as much — "the pool
// finished scrubbing" is exactly what you want to hear at 09:00 after being
// woken at 03:00. An alert that is simply still true is not news, and is
// silent.
//
// That requires remembering what was already reported, which is what State is
// and what internal/store persists. The identity of a condition across
// evaluations is the Host + Rule + Subject triple: Message is prose and will
// be reworded, Rule is documented in internal/alert as the thing not to
// reword precisely so that this keeps working when it is.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// StateName is the key this package's state is stored under.
const StateName = "notify"

// Key identifies one condition across evaluations; see alert.Alert.Key.
func Key(a alert.Alert) string { return a.Key() }

// Entry is a condition that has been notified about and is still true.
type Entry struct {
	Host    string      `json:"host"`
	Rule    string      `json:"rule"`
	Subject string      `json:"subject,omitempty"`
	Level   alert.Level `json:"level"`
	Message string      `json:"message"`
	// FirstSeen is when this condition was first notified, not when the
	// underlying problem started — a scrub cancelled last July that this only
	// noticed today is FirstSeen today. It is what lets a resolution say how
	// long something was outstanding.
	FirstSeen time.Time `json:"first_seen"`
}

// State is what has already been reported. It is persisted between runs, and
// between restarts of the API, because a process that forgets on restart would
// re-announce the entire alert set every time it is deployed.
type State struct {
	// PendingPluginBaselines seeds only newly migrated plugin identities on their
	// first successful poll. Core transitions continue normally during outages.
	PendingPluginBaselines map[string]bool `json:"pending_plugin_baselines,omitempty"`
	// Seeded distinguishes "nothing is wrong" from "this has never run".
	// Without it, the first evaluation after deployment would notify about
	// every pre-existing alert at once — technically correct and practically
	// a wall of text about things the reader already knows, arriving as the
	// system's first impression. The first pass records silently instead.
	Seeded    bool             `json:"seeded"`
	UpdatedAt time.Time        `json:"updated_at"`
	Active    map[string]Entry `json:"active"`
}

// Change is the difference between two evaluations.
type Change struct {
	New       []Entry `json:"new,omitempty"`
	Resolved  []Entry `json:"resolved,omitempty"`
	Escalated []Entry `json:"escalated,omitempty"`
}

// Empty reports whether nothing happened, which is the common case.
func (c Change) Empty() bool {
	return len(c.New) == 0 && len(c.Resolved) == 0 && len(c.Escalated) == 0
}

// Count is how many conditions changed.
func (c Change) Count() int {
	return len(c.New) + len(c.Resolved) + len(c.Escalated)
}

// Diff folds an evaluation into the previous state and reports what changed.
//
// min is the severity floor: anything below it is not merely unreported, it is
// not tracked at all. That matters for what happens when the floor is later
// lowered — a warning that was already outstanding then reads as new, which is
// the honest answer, because it has never been reported.
func Diff(prev State, alerts []alert.Alert, min alert.Level, now time.Time) (State, Change) {
	next := State{Seeded: true, UpdatedAt: now, Active: map[string]Entry{}, PendingPluginBaselines: prev.PendingPluginBaselines}
	var ch Change

	for _, a := range alerts {
		if !a.Level.AtLeast(min) {
			continue
		}
		k := Key(a)
		e := Entry{
			Host:      a.Host,
			Rule:      a.Rule,
			Subject:   a.Subject,
			Level:     a.Level,
			Message:   a.Message,
			FirstSeen: now,
		}
		if old, ok := prev.Active[k]; ok {
			e.FirstSeen = old.FirstSeen
			// AtLeast is "at least as severe as", so its negation is "the old
			// level was less severe than the new one" — an escalation.
			if !old.Level.AtLeast(a.Level) {
				ch.Escalated = append(ch.Escalated, e)
			}
		} else {
			ch.New = append(ch.New, e)
		}
		// A duplicate key overwrites rather than appending twice; two alerts
		// with the same host, rule and subject are the same condition.
		next.Active[k] = e
	}

	for k, old := range prev.Active {
		if _, ok := next.Active[k]; !ok {
			ch.Resolved = append(ch.Resolved, old)
		}
	}

	sortEntries(ch.New)
	sortEntries(ch.Escalated)
	sortEntries(ch.Resolved)
	return next, ch
}

// sortEntries gives notifications a stable order: worst first, then by host,
// so the same set of changes always reads the same way.
func sortEntries(es []Entry) {
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		if a.Level != b.Level {
			return a.Level.AtLeast(b.Level)
		}
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Subject < b.Subject
	})
}

// Message is one notification ready to send.
type Message struct {
	Title string
	Body  string
	// Type is apprise's notification type: info, success, warning or failure.
	Type string
}

// Messages renders a Change as at most two notifications: one for things that
// started, one for things that stopped.
//
// Two rather than one because they are different news and apprise types them
// differently — a resolution should not arrive coloured as a failure. Two
// rather than one-per-alert because a host coming back after a reboot can
// resolve a dozen conditions at once, and twelve phone buzzes for one event is
// the noise this package exists to avoid.
func Messages(c Change, now time.Time) []Message {
	var out []Message

	if firing := append(append([]Entry{}, c.New...), c.Escalated...); len(firing) > 0 {
		sortEntries(firing)
		worst := firing[0].Level
		var b strings.Builder
		for _, e := range c.New {
			fmt.Fprintf(&b, "- **%s** [%s] %s\n", e.Host, e.Level, e.Message)
		}
		for _, e := range c.Escalated {
			fmt.Fprintf(&b, "- **%s** [%s, was lower] %s\n", e.Host, e.Level, e.Message)
		}
		out = append(out, Message{
			Title: fmt.Sprintf("homelab: %s", summarise(firing, "new")),
			Body:  strings.TrimRight(b.String(), "\n"),
			Type:  appriseType(worst),
		})
	}

	if len(c.Resolved) > 0 {
		var b strings.Builder
		for _, e := range c.Resolved {
			// "cleared —" in front of the original wording, because the stored
			// Message describes the problem ("unit nslcd.service has failed")
			// and reads as a fresh failure without it. Keeping the original
			// text rather than rewriting it is deliberate: it is the string
			// the reader was shown when this fired, so the two match up.
			b.WriteString(fmt.Sprintf("- **%s** cleared — %s", e.Host, e.Message))
			if !e.FirstSeen.IsZero() {
				fmt.Fprintf(&b, " _(outstanding %s)_", roughly(now.Sub(e.FirstSeen)))
			}
			b.WriteString("\n")
		}
		out = append(out, Message{
			Title: fmt.Sprintf("homelab: %s", summarise(c.Resolved, "resolved")),
			Body:  strings.TrimRight(b.String(), "\n"),
			Type:  "success",
		})
	}
	return out
}

func summarise(es []Entry, verb string) string {
	if len(es) == 1 {
		return fmt.Sprintf("%s %s on %s", es[0].Level, verb, es[0].Host)
	}
	hosts := map[string]bool{}
	for _, e := range es {
		hosts[e.Host] = true
	}
	if len(hosts) == 1 {
		return fmt.Sprintf("%d %s on %s", len(es), verb, es[0].Host)
	}
	return fmt.Sprintf("%d %s across %d hosts", len(es), verb, len(hosts))
}

func appriseType(l alert.Level) string {
	switch l {
	case alert.LevelCritical:
		return "failure"
	case alert.LevelWarning:
		return "warning"
	default:
		return "info"
	}
}

// roughly renders a duration the way a person would say it.
//
// Rounded, not truncated. Truncation makes something 23 days old read as "22d",
// which is the kind of small wrongness that quietly costs trust in every other
// number on the page.
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(math.Round(d.Minutes())))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(math.Round(d.Hours())))
	default:
		return fmt.Sprintf("%dd", int(math.Round(d.Hours()/24)))
	}
}

// Apprise posts notifications to an apprise instance.
//
// The whole URL is a credential: the path segment after /notify/ is the key
// that decides where the message actually goes, so it is treated exactly like
// API_TOKEN — read from the environment, never logged, never echoed by an
// endpoint.
type Apprise struct {
	URL string
	// URLFn, when set, is consulted per send instead of URL, so the endpoint
	// can be repointed from the dashboard without a restart.
	URLFn  func() string
	Client *http.Client
}

// url resolves the endpoint for this send.
func (a *Apprise) url() string {
	if a.URLFn != nil {
		if u := a.URLFn(); u != "" {
			return u
		}
	}
	return a.URL
}

// NewApprise returns a client with a timeout short enough that a wedged
// notification endpoint cannot stall the evaluation loop behind it.
func NewApprise(url string) *Apprise {
	return &Apprise{URL: url, Client: &http.Client{Timeout: 15 * time.Second}}
}

// Send posts one message. A non-2xx response is an error, but the status line
// is all that is reported: an apprise error body can echo the configured
// target URLs, which are themselves credentials.
func (a *Apprise) Send(ctx context.Context, m Message) error {
	payload := map[string]string{
		"title":  m.Title,
		"body":   m.Body,
		"type":   m.Type,
		"format": "markdown",
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("notify: encoding payload: %w", err)
	}
	target := a.url()
	if target == "" {
		return fmt.Errorf("notify: no apprise endpoint is configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(buf))
	if err != nil {
		// The URL is in this error, and the URL is a credential.
		return fmt.Errorf("notify: the apprise endpoint is not a usable URL")
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.Client.Do(req)
	if err != nil {
		// net/http puts the request URL in its error strings, which would put
		// the apprise key into the log. Say what happened without it.
		return fmt.Errorf("notify: posting to apprise failed (is it reachable?)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notify: apprise returned %s", resp.Status)
	}
	return nil
}
