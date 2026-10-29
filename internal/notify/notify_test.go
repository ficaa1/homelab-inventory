package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

// memState is the two-method slice of internal/store the Notifier needs,
// backed by a map so the transition logic can be tested without a filesystem.
type memState struct {
	data     map[string][]byte
	writeErr error
	writes   int
}

func newMemState() *memState { return &memState{data: map[string][]byte{}} }

func (m *memState) ReadState(name string, v any) error {
	b, ok := m.data[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(b, v)
}

func (m *memState) WriteState(name string, v any) error {
	if m.writeErr != nil {
		return m.writeErr
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.data[name] = b
	m.writes++
	return nil
}

type capture struct {
	msgs []Message
	err  error
}

func (c *capture) Send(ctx context.Context, m Message) error {
	if c.err != nil {
		return c.err
	}
	c.msgs = append(c.msgs, m)
	return nil
}

var errTest = errors.New("apprise is down")

func crit(host, rule, subject, msg string) alert.Alert {
	return alert.Alert{Host: host, Rule: rule, Subject: subject, Level: alert.LevelCritical, Message: msg}
}

func warn(host, rule, subject, msg string) alert.Alert {
	return alert.Alert{Host: host, Rule: rule, Subject: subject, Level: alert.LevelWarning, Message: msg}
}

func newNotifier(st StateStore, s Sender, min alert.Level) *Notifier {
	return &Notifier{State: st, Sender: s, Min: min, Log: log.New(&strings.Builder{}, "", 0)}
}

// The behaviour the whole package exists for: a condition that is simply still
// true does not generate a second notification. immich_server has been
// restarting for days; at a 15-minute push interval, notifying on state rather
// than on transitions would be 96 messages a day about it.
func TestPersistingAlertNotifiesOnce(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelCritical)
	now := time.Now()

	alerts := []alert.Alert{crit("host-a", "docker.container.unhealthy", "immich_server", "immich_server is restarting")}

	// Seed: the first pass records silently.
	if _, err := n.Run(context.Background(), alerts, now); err != nil {
		t.Fatal(err)
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("seed run sent %d message(s), want 0: %+v", len(cap.msgs), cap.msgs)
	}

	// Five more evaluations of the identical alert set.
	for i := 1; i <= 5; i++ {
		if _, err := n.Run(context.Background(), alerts, now.Add(time.Duration(i)*15*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("an unchanged alert sent %d message(s), want 0", len(cap.msgs))
	}
}

func TestNewAlertNotifiesAndResolutionNotifies(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelCritical)
	t0 := time.Now()

	// Seed with nothing wrong.
	if _, err := n.Run(context.Background(), nil, t0); err != nil {
		t.Fatal(err)
	}

	fire := []alert.Alert{crit("host-b", "proxmox.storage.thinpool", "local-lvm", "local-lvm is 96% full")}
	ch, err := n.Run(context.Background(), fire, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.New) != 1 {
		t.Fatalf("New = %d, want 1", len(ch.New))
	}
	if len(cap.msgs) != 1 {
		t.Fatalf("sent %d message(s), want 1", len(cap.msgs))
	}
	if cap.msgs[0].Type != "failure" {
		t.Errorf("type = %q, want failure", cap.msgs[0].Type)
	}
	if !strings.Contains(cap.msgs[0].Body, "local-lvm is 96% full") {
		t.Errorf("body does not carry the message: %q", cap.msgs[0].Body)
	}

	// It clears. A resolution is worth as much as a failure.
	ch, err = n.Run(context.Background(), nil, t0.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Resolved) != 1 {
		t.Fatalf("Resolved = %d, want 1", len(ch.Resolved))
	}
	if len(cap.msgs) != 2 {
		t.Fatalf("sent %d message(s), want 2", len(cap.msgs))
	}
	if cap.msgs[1].Type != "success" {
		t.Errorf("resolution type = %q, want success", cap.msgs[1].Type)
	}
	// It was outstanding two hours; the resolution should say so.
	if !strings.Contains(cap.msgs[1].Body, "2h") {
		t.Errorf("resolution does not report how long it was outstanding: %q", cap.msgs[1].Body)
	}
}

// Subject is part of the identity, so two pools failing the same rule are two
// notifications rather than one that swallows the other. This is exactly the
// media/secure cancelled-scrub pair on the real fleet.
func TestSubjectDistinguishesAlertsOfTheSameRule(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelWarning)
	t0 := time.Now()

	if _, err := n.Run(context.Background(), nil, t0); err != nil {
		t.Fatal(err)
	}
	both := []alert.Alert{
		warn("host-c", "zfs.pool.scrub", "media", "pool media: last scrub was cancelled"),
		warn("host-c", "zfs.pool.scrub", "secure", "pool secure: last scrub was cancelled"),
	}
	ch, err := n.Run(context.Background(), both, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.New) != 2 {
		t.Fatalf("New = %d, want 2 (one per pool); keys collapsed", len(ch.New))
	}

	// One of them gets scrubbed. The other must stay quiet, not re-fire.
	ch, err = n.Run(context.Background(), both[:1], t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Resolved) != 1 || ch.Resolved[0].Subject != "secure" {
		t.Fatalf("Resolved = %+v, want just secure", ch.Resolved)
	}
	if len(ch.New) != 0 {
		t.Fatalf("New = %+v, want none", ch.New)
	}
}

func TestSeverityFloorIsRespected(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelCritical)
	t0 := time.Now()

	if _, err := n.Run(context.Background(), nil, t0); err != nil {
		t.Fatal(err)
	}
	ch, err := n.Run(context.Background(), []alert.Alert{
		warn("host-f", "systemd.unit.failed", "apparmor.service", "unit apparmor.service has failed"),
	}, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Empty() {
		t.Fatalf("a warning crossed a critical-only floor: %+v", ch)
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("sent %d message(s) below the floor", len(cap.msgs))
	}
}

// A warning that becomes critical is news even though the condition is not
// new, so it is reported rather than being folded into "still true".
func TestEscalationNotifies(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelWarning)
	t0 := time.Now()

	if _, err := n.Run(context.Background(), nil, t0); err != nil {
		t.Fatal(err)
	}
	a := warn("host-b", "proxmox.storage.thinpool", "local-lvm", "local-lvm is 78% full")
	if _, err := n.Run(context.Background(), []alert.Alert{a}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cap.msgs = nil

	a.Level = alert.LevelCritical
	a.Message = "local-lvm is 91% full"
	ch, err := n.Run(context.Background(), []alert.Alert{a}, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Escalated) != 1 {
		t.Fatalf("Escalated = %d, want 1", len(ch.Escalated))
	}
	if len(ch.New) != 0 {
		t.Errorf("an escalation was reported as new: %+v", ch.New)
	}
	if len(cap.msgs) != 1 || cap.msgs[0].Type != "failure" {
		t.Fatalf("messages = %+v, want one failure", cap.msgs)
	}

	// Dropping back down is not worth a notification of its own, and must not
	// re-fire as new on the pass after that.
	a.Level = alert.LevelWarning
	if _, err := n.Run(context.Background(), []alert.Alert{a}, t0.Add(3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	ch, err = n.Run(context.Background(), []alert.Alert{a}, t0.Add(4*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Empty() {
		t.Fatalf("de-escalation left the state unstable: %+v", ch)
	}
}

// A failed send must not advance the state, or a transient apprise outage
// turns into a permanently missed alert: the condition would be recorded as
// reported without anyone having been told.
func TestFailedSendDoesNotAdvanceState(t *testing.T) {
	t.Parallel()
	st := newMemState()
	cap := &capture{}
	n := newNotifier(st, cap, alert.LevelCritical)
	t0 := time.Now()

	if _, err := n.Run(context.Background(), nil, t0); err != nil {
		t.Fatal(err)
	}
	writesAfterSeed := st.writes

	fire := []alert.Alert{crit("host-b", "zfs.pool.health", "media", "pool media is DEGRADED")}
	cap.err = errors.New("apprise is down")
	if _, err := n.Run(context.Background(), fire, t0.Add(time.Hour)); err == nil {
		t.Fatal("expected the send error to surface")
	}
	if st.writes != writesAfterSeed {
		t.Fatalf("state was written despite a failed send (writes %d -> %d)", writesAfterSeed, st.writes)
	}

	// Apprise comes back. The alert must still be delivered.
	cap.err = nil
	ch, err := n.Run(context.Background(), fire, t0.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.New) != 1 {
		t.Fatalf("New = %d after recovery, want 1 — the alert was lost", len(ch.New))
	}
	if len(cap.msgs) != 1 {
		t.Fatalf("sent %d message(s) after recovery, want 1", len(cap.msgs))
	}
}

// Seeding exists so a deployment does not announce the entire standing alert
// set as though it had all just happened.
func TestFirstRunSeedsSilently(t *testing.T) {
	t.Parallel()
	st, cap := newMemState(), &capture{}
	n := newNotifier(st, cap, alert.LevelWarning)
	now := time.Now()

	many := []alert.Alert{
		warn("host-c", "zfs.pool.scrub", "media", "scrub cancelled"),
		warn("host-c", "zfs.pool.scrub", "secure", "scrub cancelled"),
		warn("node1", "systemd.unit.failed", "postfix@-.service", "unit failed"),
	}
	ch, err := n.Run(context.Background(), many, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Empty() {
		t.Fatalf("the seeding run reported changes: %+v", ch)
	}
	if len(cap.msgs) != 0 {
		t.Fatalf("the seeding run sent %d message(s)", len(cap.msgs))
	}

	var s State
	if err := st.ReadState(StateName, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Seeded {
		t.Error("state was not marked seeded")
	}
	if len(s.Active) != 3 {
		t.Errorf("Active = %d, want 3", len(s.Active))
	}

	// A genuinely new one after seeding does notify.
	ch, err = n.Run(context.Background(), append(many, warn("host-b", "systemd.unit.failed", "zfs-import-scan.service", "unit failed")), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.New) != 1 || ch.New[0].Host != "host-b" {
		t.Fatalf("New = %+v, want just the host-b unit", ch.New)
	}
}

// A batch of transitions is one message, not one per alert: a host coming back
// after a reboot can resolve a dozen conditions at once.
func TestMessagesBatch(t *testing.T) {
	t.Parallel()
	now := time.Now()
	ch := Change{
		New: []Entry{
			{Host: "host-b", Level: alert.LevelCritical, Message: "a"},
			{Host: "host-a", Level: alert.LevelCritical, Message: "b"},
		},
		Resolved: []Entry{
			{Host: "host-c", Level: alert.LevelCritical, Message: "c", FirstSeen: now.Add(-25 * time.Hour)},
		},
	}
	msgs := Messages(ch, now)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (one firing, one resolved)", len(msgs))
	}
	if !strings.Contains(msgs[0].Body, "a") || !strings.Contains(msgs[0].Body, "b") {
		t.Errorf("firing message dropped an alert: %q", msgs[0].Body)
	}
	if msgs[0].Type != "failure" || msgs[1].Type != "success" {
		t.Errorf("types = %q, %q; want failure, success", msgs[0].Type, msgs[1].Type)
	}
	// Under two days it stays in hours, which is more use than "1d" when you
	// are working out whether it spanned a nightly job.
	if !strings.Contains(msgs[1].Body, "25h") {
		t.Errorf("resolution should say how long it was outstanding: %q", msgs[1].Body)
	}
}

func TestKeyIgnoresMessageWording(t *testing.T) {
	t.Parallel()
	a := crit("host-b", "zfs.pool.capacity", "media", "pool media is 91% full")
	b := crit("host-b", "zfs.pool.capacity", "media", "media has reached 91 percent capacity")
	if Key(a) != Key(b) {
		t.Error("rewording the message changed the identity; it must not")
	}
	c := crit("host-b", "zfs.pool.capacity", "secure", "pool secure is 91% full")
	if Key(a) == Key(c) {
		t.Error("different subjects collapsed to one key")
	}
}
