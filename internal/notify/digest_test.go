package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

func ago(d time.Duration) *time.Time { t := time.Now().Add(-d); return &t }

// A restart must not send a digest. The API is redeployed often enough that
// "send on startup" would turn a weekly summary into a daily one.
func TestFirstRunSeedsTheClockInsteadOfSending(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelCritical)
	now := time.Now()

	due, err := n.DigestDue(168*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if due {
		t.Fatal("a digest was due on the very first run")
	}

	// The clock is now seeded, so nothing is due until an interval has passed.
	if due, _ := n.DigestDue(168*time.Hour, now.Add(24*time.Hour)); due {
		t.Error("a digest was due a day after seeding")
	}
	if due, _ := n.DigestDue(168*time.Hour, now.Add(169*time.Hour)); !due {
		t.Error("no digest was due after a full week")
	}
	if len(cap.msgs) != 0 {
		t.Errorf("DigestDue sent something: %+v", cap.msgs)
	}
}

func TestDigestIntervalOfZeroDisablesIt(t *testing.T) {
	t.Parallel()
	var s DigestState
	s.LastSent = time.Now().Add(-10000 * time.Hour)
	if s.Due(0, time.Now()) {
		t.Error("a zero interval still fired")
	}
}

// A failed send must not advance the clock, or a transient apprise outage
// silently costs a week.
func TestFailedDigestIsRetried(t *testing.T) {
	t.Parallel()
	st := newMemState()
	cap := &capture{err: errTest}
	n := newNotifier(st, cap, alert.LevelCritical)
	now := time.Now()

	d := Digest{Since: now.Add(-168 * time.Hour), Now: now, Hosts: []string{"host-a"}}
	if err := n.SendDigest(context.Background(), d); err == nil {
		t.Fatal("expected the send error to surface")
	}
	var s DigestState
	if err := st.ReadState(DigestStateName, &s); err != nil {
		t.Fatal(err)
	}
	if !s.LastSent.IsZero() {
		t.Error("the clock advanced despite a failed send")
	}
}

// The digest gets its own channel when one is set: a weekly report must not
// compete with an alarm for the same endpoint. When no digest sender is
// configured it falls back to the alert sender, so an unset DIGEST_APPRISE_URL
// behaves exactly as it did before the field existed.
func TestDigestPrefersItsOwnSender(t *testing.T) {
	t.Parallel()
	st := newMemState()
	alerts, digest := &capture{}, &capture{}
	n := newNotifier(st, alerts, alert.LevelCritical)
	n.DigestSender = digest
	now := time.Now()

	d := Digest{Since: now.Add(-168 * time.Hour), Now: now, Hosts: []string{"host-a"}}
	if err := n.SendDigest(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(alerts.msgs) != 0 {
		t.Errorf("the digest went to the alert channel: %+v", alerts.msgs)
	}
	if len(digest.msgs) != 1 {
		t.Errorf("the digest channel got %d messages, want 1", len(digest.msgs))
	}

	// Without a digest sender the alert sender takes it, preserving the
	// behaviour of a deployment that never sets DIGEST_APPRISE_URL.
	n2 := newNotifier(st, alerts, alert.LevelCritical)
	if err := n2.SendDigest(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if len(alerts.msgs) != 1 {
		t.Errorf("no digest sender: the alert channel got %d messages, want 1", len(alerts.msgs))
	}
}

// The digest exists to mention exactly what the transition notifier is
// designed never to repeat.
func TestDigestNamesLongstandingConditions(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := Digest{
		Since: now.Add(-168 * time.Hour), Now: now,
		Counts: alert.Counts{Warning: 2, Total: 2}, Hosts: []string{"host-a", "host-c"},
		Longstanding: []alert.Alert{
			{Host: "host-a", Message: "immich_server is restarting", FirstSeen: ago(23 * 24 * time.Hour)},
		},
		Notes: []string{"24 of 35 containers still run a moving image tag"},
	}
	m := d.Message()

	if !strings.Contains(m.Body, "immich_server is restarting") {
		t.Errorf("body omitted the longstanding condition:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "23d") {
		t.Errorf("body omitted how long it has been true:\n%s", m.Body)
	}
	if !strings.Contains(m.Body, "moving image tag") {
		t.Errorf("body omitted the notes:\n%s", m.Body)
	}
	// A weekly summary coloured as an emergency teaches you to ignore the
	// colour, which is the one thing the transition notifier needs you not to
	// do.
	if m.Type != "info" {
		t.Errorf("digest type = %q, want info", m.Type)
	}
}

// Acknowledging something is a statement that you do not want to be told about
// it. A weekly reminder would make silence worth less than it should be.
func TestSilencedConditionsAreNotInTheDigest(t *testing.T) {
	t.Parallel()
	now := time.Now()
	alerts := []alert.Alert{
		{Host: "host-c", Message: "scrub cancelled", FirstSeen: ago(300 * 24 * time.Hour), Silenced: true},
		{Host: "host-a", Message: "immich restarting", FirstSeen: ago(23 * 24 * time.Hour)},
		{Host: "host-b", Message: "just started", FirstSeen: ago(time.Hour)},
	}
	got := LongstandingFrom(alerts, 24*time.Hour, now, 8)
	if len(got) != 1 || got[0].Host != "host-a" {
		t.Fatalf("LongstandingFrom = %+v, want only the host-a one", got)
	}
}

func TestQuietWeekReadsAsQuiet(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := Digest{Since: now.Add(-168 * time.Hour), Now: now, Hosts: []string{"a", "b"}}.Message()
	if !strings.Contains(m.Body, "Nothing is outstanding") {
		t.Errorf("a clean fleet did not read as clean:\n%s", m.Body)
	}
	if !strings.Contains(m.Title, "quiet") {
		t.Errorf("title = %q", m.Title)
	}
}

func TestSilencedCountIsShownNotHidden(t *testing.T) {
	t.Parallel()
	now := time.Now()
	m := Digest{
		Since: now.Add(-168 * time.Hour), Now: now, Hosts: []string{"a"},
		Counts: alert.Counts{Warning: 1, Total: 1}, Silenced: 4,
	}.Message()
	if !strings.Contains(m.Body, "4 silenced") {
		t.Errorf("the silenced count vanished; a quiet digest must not look like a healthy fleet:\n%s", m.Body)
	}
}
