// Package seen remembers when each condition was first observed, so an alert
// can say how long it has been true.
//
// "container immich_server is restarting" and "container immich_server has been
// restarting for 23 days" are different sentences. The first is a fact; the
// second is a judgement about whether anyone is going to do anything about it.
//
// # Why not reuse what internal/notify already stores
//
// The notifier records a FirstSeen per condition, but only for alerts at or
// above its severity floor — critical by default. Warnings would therefore
// have no age. This ledger tracks everything the
// rules produce, regardless of what is worth waking someone for.
//
// # Why a grace period
//
// The obvious implementation resets the clock whenever a condition is absent
// from an evaluation. That is wrong here: an agent missing one push, or a
// collector failing once, makes every alert on that host briefly disappear —
// and a 23-day-old problem would silently become a fresh one. So an entry
// survives a gap, and only a genuine absence longer than the grace period
// counts as the condition having cleared.
package seen

import (
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// StateName is the key this ledger is stored under.
const StateName = "seen"

// Grace is how long a condition may be absent before its clock restarts.
//
// The agents push every 15 minutes and the staleness rule only calls a host
// late after 45, so two hours comfortably survives a missed run, a reboot, or
// a collector that failed once — while still being far shorter than any
// interval a human would call "it came back".
const Grace = 2 * time.Hour

// Forget drops entries nothing has reported for this long, so the ledger does
// not accumulate every condition the fleet has ever had.
const Forget = 30 * 24 * time.Hour

// refreshEvery bounds how often a still-true condition rewrites its Last
// timestamp. /alerts is served on request and the dashboard polls every 60
// seconds from every open tab, so recording each sighting would rewrite the
// file constantly. One push interval keeps Last accurate enough for the grace
// check, which is the only thing that reads it.
const refreshEvery = 15 * time.Minute

// Entry is one condition's history.
type Entry struct {
	// First is when this condition most recently *started*. It is not the
	// first time it was ever seen: a problem that genuinely cleared and came
	// back a month later starts a new clock, because that is what a reader
	// means by "how long has this been failing".
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Book is the ledger, persisted between runs.
type Book struct {
	Entries   map[string]Entry `json:"entries"`
	UpdatedAt time.Time        `json:"updated_at,omitempty"`
}

// Key identifies a condition, using the same Host + Rule + Subject triple that
// internal/notify and internal/silence key on. Message is prose and will be
// reworded; Rule is documented as the field that will not be.
func Key(a alert.Alert) string { return a.Key() }

// Stamp records the current alerts and fills in each one's FirstSeen.
//
// It reports whether the ledger changed, so a caller can persist only when
// something actually started or stopped rather than on every read.
func (b *Book) Stamp(alerts []alert.Alert, now time.Time) bool {
	if b.Entries == nil {
		b.Entries = map[string]Entry{}
	}
	changed := false

	for i := range alerts {
		k := Key(alerts[i])
		e, ok := b.Entries[k]
		switch {
		case !ok:
			e = Entry{First: now, Last: now}
			changed = true
		case now.Sub(e.Last) > Grace:
			// Genuinely absent long enough to count as having cleared and
			// returned, so the clock restarts.
			e = Entry{First: now, Last: now}
			changed = true
		default:
			// Only persist a Last update when it has moved meaningfully;
			// otherwise every page load would rewrite the file.
			if now.Sub(e.Last) >= refreshEvery {
				changed = true
			}
			e.Last = now
		}
		b.Entries[k] = e
		alerts[i].FirstSeen = &e.First
	}

	for k, e := range b.Entries {
		if now.Sub(e.Last) > Forget {
			delete(b.Entries, k)
			changed = true
		}
	}
	if changed {
		b.UpdatedAt = now
	}
	return changed
}

// Oldest returns the longest-standing conditions, newest ages last. Useful for
// a digest: "these have been true for weeks" is the part worth reading.
func (b *Book) Oldest(alerts []alert.Alert, now time.Time, n int) []alert.Alert {
	out := make([]alert.Alert, 0, len(alerts))
	for _, a := range alerts {
		if a.FirstSeen != nil {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(*out[j].FirstSeen) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out
}
