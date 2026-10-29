package seen

import (
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

func warn(host, rule, subject string) alert.Alert {
	return alert.Alert{Host: host, Rule: rule, Subject: subject, Level: alert.LevelWarning}
}

func TestAgeAccumulatesAcrossEvaluations(t *testing.T) {
	t.Parallel()
	var b Book
	t0 := time.Now().Add(-23 * 24 * time.Hour)

	a := []alert.Alert{warn("host-a", "docker.container.restarting", "immich_server")}
	if !b.Stamp(a, t0) {
		t.Fatal("first sight should be worth persisting")
	}
	if a[0].FirstSeen == nil || !a[0].FirstSeen.Equal(t0) {
		t.Fatalf("FirstSeen = %v, want %v", a[0].FirstSeen, t0)
	}

	// Evaluated again every 15 minutes for 23 days.
	for d := time.Duration(0); d < 23*24*time.Hour; d += 15 * time.Minute {
		a = []alert.Alert{warn("host-a", "docker.container.restarting", "immich_server")}
		b.Stamp(a, t0.Add(d))
	}
	if a[0].FirstSeen == nil || !a[0].FirstSeen.Equal(t0) {
		t.Errorf("FirstSeen drifted to %v; the clock must not restart while the condition holds", a[0].FirstSeen)
	}
}

// The failure this grace period exists to prevent: an agent misses one push,
// every alert on that host briefly vanishes, and a three-week-old problem
// silently becomes a fresh one.
func TestOneMissedCollectionDoesNotResetTheClock(t *testing.T) {
	t.Parallel()
	var b Book
	t0 := time.Now().Add(-10 * 24 * time.Hour)

	a := []alert.Alert{warn("host-c", "zfs.pool.scrub", "media")}
	b.Stamp(a, t0)

	// The host goes quiet for half an hour — one or two missed pushes — then
	// reports the same condition again.
	a = []alert.Alert{warn("host-c", "zfs.pool.scrub", "media")}
	b.Stamp(a, t0.Add(30*time.Minute))
	if !a[0].FirstSeen.Equal(t0) {
		t.Errorf("a 30-minute gap reset the clock to %v", a[0].FirstSeen)
	}

	// A gap longer than the grace period is a genuine clear-and-return.
	a = []alert.Alert{warn("host-c", "zfs.pool.scrub", "media")}
	later := t0.Add(30*time.Minute + Grace + time.Minute)
	b.Stamp(a, later)
	if !a[0].FirstSeen.Equal(later) {
		t.Errorf("FirstSeen = %v, want the clock restarted at %v", a[0].FirstSeen, later)
	}
}

// The reason this does not just reuse what the notifier stores: that only
// tracks conditions at or above the notification floor, and warnings are
// exactly where "how long has this been going on" matters most.
func TestTracksEveryLevelNotJustNotifiableOnes(t *testing.T) {
	t.Parallel()
	var b Book
	now := time.Now()
	a := []alert.Alert{
		{Host: "host-b", Rule: "proxmox.storage.allocation", Subject: "nvme", Level: alert.LevelInfo},
		{Host: "node1", Rule: "systemd.unit.failed", Subject: "postfix@-.service", Level: alert.LevelWarning},
		{Host: "host-b", Rule: "zfs.pool.health", Subject: "media", Level: alert.LevelCritical},
	}
	b.Stamp(a, now)
	for _, x := range a {
		if x.FirstSeen == nil {
			t.Errorf("%s alert %s got no age", x.Level, x.Rule)
		}
	}
}

func TestSubjectAndRuleSeparateConditions(t *testing.T) {
	t.Parallel()
	var b Book
	t0 := time.Now().Add(-time.Hour)
	b.Stamp([]alert.Alert{warn("host-c", "zfs.pool.scrub", "media")}, t0)

	now := time.Now()
	a := []alert.Alert{
		warn("host-c", "zfs.pool.scrub", "media"),
		warn("host-c", "zfs.pool.scrub", "secure"),
		warn("host-c", "zfs.pool.capacity", "media"),
		warn("host-b", "zfs.pool.scrub", "media"),
	}
	b.Stamp(a, now)
	if !a[0].FirstSeen.Equal(t0) {
		t.Error("the known condition lost its age")
	}
	for _, x := range a[1:] {
		if !x.FirstSeen.Equal(now) {
			t.Errorf("%s/%s/%s inherited another condition's age", x.Host, x.Rule, x.Subject)
		}
	}
}

// /alerts is served on request and the dashboard polls every 60 seconds. A
// ledger that reported a change every time would rewrite the file on every
// poll, from every open tab.
func TestSteadyStateDoesNotRewrite(t *testing.T) {
	t.Parallel()
	var b Book
	now := time.Now()
	a := []alert.Alert{warn("host-a", "docker.container.restarting", "immich_server")}

	if !b.Stamp(a, now) {
		t.Fatal("the first sighting should persist")
	}
	if b.Stamp(a, now.Add(time.Minute)) {
		t.Error("a poll a minute later wanted to write")
	}
	if b.Stamp(a, now.Add(5*time.Minute)) {
		t.Error("a poll five minutes later wanted to write")
	}
	// Eventually Last is stale enough to be worth recording.
	if !b.Stamp(a, now.Add(20*time.Minute)) {
		t.Error("Last is never refreshed, so a real gap could not be detected")
	}
}

func TestForgetsLongGoneConditions(t *testing.T) {
	t.Parallel()
	var b Book
	old := time.Now().Add(-Forget - 24*time.Hour)
	b.Stamp([]alert.Alert{warn("gone", "some.rule", "x")}, old)
	if len(b.Entries) != 1 {
		t.Fatal("setup")
	}
	b.Stamp([]alert.Alert{warn("host-a", "other.rule", "y")}, time.Now())
	if len(b.Entries) != 1 {
		t.Errorf("entries = %d; the long-gone condition should have been forgotten", len(b.Entries))
	}
}

func TestOldestFirst(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// Constructed directly rather than by stamping long-ago times: a condition
	// last seen 30 days ago is, correctly, treated as having cleared and
	// returned. To have a genuinely 30-day-old alert it must have been present
	// continuously, which means a recent Last.
	b := Book{Entries: map[string]Entry{
		Key(warn("a", "r", "1")): {First: now.Add(-30 * 24 * time.Hour), Last: now.Add(-time.Minute)},
		Key(warn("b", "r", "2")): {First: now.Add(-time.Hour), Last: now.Add(-time.Minute)},
	}}

	a := []alert.Alert{warn("b", "r", "2"), warn("a", "r", "1")}
	b.Stamp(a, now)
	got := b.Oldest(a, now, 5)
	if len(got) != 2 || got[0].Host != "a" {
		t.Errorf("Oldest = %+v, want the 30-day-old one first", got)
	}
	if len(b.Oldest(a, now, 1)) != 1 {
		t.Error("the limit was not applied")
	}
}
