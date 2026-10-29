// Package silence is the "yes, I know" control.
//
// Alerting and notifications are only useful while you believe them, and the
// fastest way to stop believing them is a standing warning you have already
// decided not to act on. `immich_server` has been restarting for weeks; the
// two cancelled scrubs are a real finding and will stay true until somebody
// runs a scrub. Without a way to say "known, stop counting it", the honest
// response to a permanently non-empty alert list is to stop reading it.
//
// # What a silence does and does not do
//
// A silenced alert is still evaluated, still returned, and still visible. It is
// removed from the counts and it never notifies. That distinction is the whole
// design: hiding the alert would mean a silence could conceal a condition that
// later got worse, and nobody would see it. Muting the *noise* while keeping
// the *fact* costs nothing and cannot mislead.
//
// Identity is the same Host + Rule + Subject triple internal/notify keys on,
// for the same reason: Message is prose and will be reworded, Rule is
// documented as the thing not to reword, and Subject separates two pools
// failing one rule. A silence created for `media` does not quietly cover
// `secure`.
package silence

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// StateName is the key this package's state is stored under.
const StateName = "silences"

// Silence is one condition a human has said they know about.
type Silence struct {
	// ID is a stable hash of the triple below, so the same condition silenced
	// twice is one silence rather than two, and so a caller can address it in a
	// URL without escaping a rule name full of dots.
	ID      string `json:"id"`
	Host    string `json:"host"`
	Rule    string `json:"rule"`
	Subject string `json:"subject,omitempty"`
	// Reason is free text and exists to be read months later by the person who
	// wrote it. Silencing something without saying why is how a fleet ends up
	// with alerts nobody dares remove.
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// Until is when the silence lapses. Nil means indefinite — which is a
	// legitimate answer for "this pool is small on purpose", but the UI should
	// nudge toward an expiry, because a silence you have forgotten about is
	// indistinguishable from a monitoring gap.
	Until *time.Time `json:"until,omitempty"`
}

// Matches reports whether s covers a.
func (s Silence) Matches(a alert.Alert) bool {
	return s.Host == a.Host && s.Rule == a.Rule && s.Subject == a.Subject
}

// Expired reports whether the silence has lapsed.
func (s Silence) Expired(now time.Time) bool {
	return s.Until != nil && now.After(*s.Until)
}

// Book is the set of silences, persisted between runs.
type Book struct {
	Silences  map[string]Silence `json:"silences"`
	UpdatedAt time.Time          `json:"updated_at,omitempty"`
}

// ID derives the stable identifier for a condition.
func ID(host, rule, subject string) string {
	sum := sha256.Sum256([]byte(alert.Alert{Host: host, Rule: rule, Subject: subject}.Key()))
	return hex.EncodeToString(sum[:])[:12]
}

// Add records a silence. Silencing something already silenced replaces it,
// which is what "change the reason" and "extend it" both look like.
func (b *Book) Add(s Silence, now time.Time) (Silence, error) {
	if s.Host == "" || s.Rule == "" {
		return Silence{}, fmt.Errorf("silence: host and rule are both required")
	}
	if len(s.Reason) > 300 {
		return Silence{}, fmt.Errorf("silence: reason is too long")
	}
	if s.Until != nil && !s.Until.After(now) {
		return Silence{}, fmt.Errorf("silence: expiry is already in the past")
	}
	if b.Silences == nil {
		b.Silences = map[string]Silence{}
	}
	s.ID = ID(s.Host, s.Rule, s.Subject)
	s.CreatedAt = now
	b.Silences[s.ID] = s
	b.UpdatedAt = now
	return s, nil
}

// Remove deletes a silence by ID, and reports whether there was one.
func (b *Book) Remove(id string, now time.Time) bool {
	if b.Silences == nil {
		return false
	}
	if _, ok := b.Silences[id]; !ok {
		return false
	}
	delete(b.Silences, id)
	b.UpdatedAt = now
	return true
}

// Prune drops lapsed silences and reports whether anything went.
//
// Unlike a service disappearing from the catalog, this deletion is safe to do
// automatically: an expiry is an instruction a human gave, not a signal
// inferred from absent data. Removing it is carrying out the instruction.
func (b *Book) Prune(now time.Time) bool {
	changed := false
	for id, s := range b.Silences {
		if s.Expired(now) {
			delete(b.Silences, id)
			changed = true
		}
	}
	if changed {
		b.UpdatedAt = now
	}
	return changed
}

// List returns the live silences, newest first.
func (b *Book) List(now time.Time) []Silence {
	out := make([]Silence, 0, len(b.Silences))
	for _, s := range b.Silences {
		if !s.Expired(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// Covers returns the silence matching a, if any is live.
func (b *Book) Covers(a alert.Alert, now time.Time) (Silence, bool) {
	s, ok := b.Silences[ID(a.Host, a.Rule, a.Subject)]
	if !ok || s.Expired(now) {
		return Silence{}, false
	}
	return s, true
}

// Apply stamps every silenced alert in rep and recomputes the counts.
//
// The report keeps every alert — silencing changes how loudly something is
// reported, never whether it was found. Counts drop the silenced ones, because
// a headline figure that includes things you have explicitly acknowledged is
// the noise this exists to remove.
func Apply(rep *alert.Report, b *Book, now time.Time) *alert.Report {
	if rep == nil {
		return rep
	}
	out := *rep
	out.Alerts = make([]alert.Alert, len(rep.Alerts))
	copy(out.Alerts, rep.Alerts)

	var counts alert.Counts
	silenced := 0
	for i := range out.Alerts {
		a := &out.Alerts[i]
		if s, ok := b.Covers(*a, now); ok {
			a.Silenced = true
			a.SilenceReason = s.Reason
			a.SilenceUntil = s.Until
			silenced++
			continue
		}
		a.Silenced = false
		a.SilenceReason = ""
		a.SilenceUntil = nil
		switch a.Level {
		case alert.LevelCritical:
			counts.Critical++
		case alert.LevelWarning:
			counts.Warning++
		case alert.LevelInfo:
			counts.Info++
		}
		counts.Total++
	}
	out.Counts = counts
	out.Silenced = silenced
	return &out
}

// Unsilenced returns the alerts that should be acted on — the input to
// notification, and nothing else.
func Unsilenced(alerts []alert.Alert) []alert.Alert {
	out := make([]alert.Alert, 0, len(alerts))
	for _, a := range alerts {
		if !a.Silenced {
			out = append(out, a)
		}
	}
	return out
}

// Describe renders a silence for a log line or a notification.
func Describe(s Silence) string {
	var b strings.Builder
	b.WriteString(s.Host)
	b.WriteString(" ")
	b.WriteString(s.Rule)
	if s.Subject != "" {
		b.WriteString(" (")
		b.WriteString(s.Subject)
		b.WriteString(")")
	}
	if s.Reason != "" {
		b.WriteString(": ")
		b.WriteString(s.Reason)
	}
	return b.String()
}
