package notify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// DigestStateName is where the digest's own bookkeeping lives. It is separate
// from the transition state so that clearing one does not disturb the other.
const DigestStateName = "digest"

// DigestState remembers when the last digest went out.
//
// Persisted, because the alternative is a restart re-sending one. The API gets
// redeployed often enough that "send on startup" would be a weekly summary
// arriving several times a day.
type DigestState struct {
	LastSent time.Time `json:"last_sent"`
}

// Digest is the periodic summary: what is outstanding, and what moved.
//
// It is a different job from the transition notifications, and the two should
// not be confused. A transition answers "something just happened" and must be
// immediate and rare. A digest answers "what is the state of things" and is
// worth reading precisely because nothing in it is urgent — it is the report
// that catches the slow problems the transition notifier is designed never to
// mention twice.
type Digest struct {
	Since time.Time
	Now   time.Time

	Counts   alert.Counts
	Silenced int
	// Longstanding are the conditions that have been true for a while. They
	// are the point of the whole exercise: the transition notifier said them
	// once, weeks ago, and has correctly said nothing since.
	Longstanding []alert.Alert
	// Hosts is every host with its alert count, so a quiet fleet reads as
	// quiet rather than as a failure to report.
	Hosts []string
	// Notes are free-form lines a caller has assembled — capacity movement,
	// services that appeared or vanished, images that changed.
	Notes []string
}

// Due reports whether a digest should be sent now.
func (s DigestState) Due(every time.Duration, now time.Time) bool {
	if every <= 0 {
		return false
	}
	if s.LastSent.IsZero() {
		// Never sent. Wait a full interval rather than firing on first start,
		// so a fresh deployment does not immediately summarise a fleet it has
		// only just met.
		return false
	}
	return now.Sub(s.LastSent) >= every
}

// Message renders the digest for apprise.
func (d Digest) Message() Message {
	var b strings.Builder

	span := d.Now.Sub(d.Since)
	fmt.Fprintf(&b, "**%s** across %d hosts.\n\n", roughly(span)+" of homelab", len(d.Hosts))

	if d.Counts.Total == 0 {
		b.WriteString("Nothing is outstanding.\n")
	} else {
		fmt.Fprintf(&b, "**Outstanding:** %d critical, %d warning, %d info",
			d.Counts.Critical, d.Counts.Warning, d.Counts.Info)
		if d.Silenced > 0 {
			fmt.Fprintf(&b, " (plus %d silenced)", d.Silenced)
		}
		b.WriteString("\n")
	}

	if len(d.Longstanding) > 0 {
		b.WriteString("\n**Still true after a while:**\n")
		for _, a := range d.Longstanding {
			age := ""
			if a.FirstSeen != nil {
				age = " _(" + roughly(d.Now.Sub(*a.FirstSeen)) + ")_"
			}
			fmt.Fprintf(&b, "- **%s** %s%s\n", a.Host, a.Message, age)
		}
	}

	if len(d.Notes) > 0 {
		b.WriteString("\n**Changed:**\n")
		for _, n := range d.Notes {
			fmt.Fprintf(&b, "- %s\n", n)
		}
	}

	title := "homelab: quiet week"
	switch {
	case d.Counts.Critical > 0:
		title = fmt.Sprintf("homelab: weekly summary — %d critical outstanding", d.Counts.Critical)
	case d.Counts.Total > 0:
		title = fmt.Sprintf("homelab: weekly summary — %d outstanding", d.Counts.Total)
	}

	// Info, never failure. A digest that arrives coloured as an emergency every
	// week teaches you to ignore the colour, which is the one thing the
	// transition notifier needs you not to do.
	return Message{Title: title, Body: strings.TrimRight(b.String(), "\n"), Type: "info"}
}

// SendDigest renders and sends a digest, and records that it went.
func (n *Notifier) SendDigest(ctx context.Context, d Digest) error {
	sender := n.Sender
	if n.DigestSender != nil {
		sender = n.DigestSender
	}
	if err := sender.Send(ctx, d.Message()); err != nil {
		// State is not advanced, so the next tick tries again — same rule as
		// the transition notifier, for the same reason.
		if n.Log != nil {
			n.Log.Printf("digest: %v (will retry)", err)
		}
		return err
	}
	st := DigestState{LastSent: d.Now}
	if err := n.State.WriteState(DigestStateName, st); err != nil {
		return err
	}
	if n.Log != nil {
		n.Log.Printf("digest sent covering %s", roughly(d.Now.Sub(d.Since)))
	}
	return nil
}

// DigestDue reads the stored state and reports whether one is owed. A digest
// that has never been sent seeds the clock instead of firing immediately.
func (n *Notifier) DigestDue(every time.Duration, now time.Time) (bool, error) {
	var st DigestState
	if err := n.State.ReadState(DigestStateName, &st); err != nil {
		return false, err
	}
	if st.LastSent.IsZero() {
		st.LastSent = now
		if n.Log != nil {
			n.Log.Printf("digest: first run, scheduling the first summary in %s", roughly(every))
		}
		return false, n.State.WriteState(DigestStateName, st)
	}
	return st.Due(every, now), nil
}

// LongstandingFrom picks the oldest outstanding conditions for a digest.
//
// Silenced ones are excluded: acknowledging something is a statement that you
// do not want to be told about it, and a weekly reminder would make silence
// worth less than it should be.
func LongstandingFrom(alerts []alert.Alert, minAge time.Duration, now time.Time, limit int) []alert.Alert {
	var out []alert.Alert
	for _, a := range alerts {
		if a.Silenced || a.FirstSeen == nil {
			continue
		}
		if now.Sub(*a.FirstSeen) >= minAge {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].FirstSeen.Before(*out[j].FirstSeen)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
