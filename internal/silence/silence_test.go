package silence

import (
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
)

func rep(as ...alert.Alert) *alert.Report {
	r := &alert.Report{Alerts: as}
	for _, a := range as {
		switch a.Level {
		case alert.LevelCritical:
			r.Counts.Critical++
		case alert.LevelWarning:
			r.Counts.Warning++
		case alert.LevelInfo:
			r.Counts.Info++
		}
		r.Counts.Total++
	}
	return r
}

func warn(host, rule, subject string) alert.Alert {
	return alert.Alert{Host: host, Rule: rule, Subject: subject,
		Level: alert.LevelWarning, Message: rule + " on " + host}
}

// The central design decision: a silence removes something from the counts and
// from notification, and never from the report. Hiding it would let an
// acknowledgement conceal a condition that later got worse.
func TestSilencedAlertStaysVisibleButLeavesTheCounts(t *testing.T) {
	t.Parallel()
	now := time.Now()
	r := rep(
		warn("host-c", "zfs.pool.scrub", "media"),
		warn("host-c", "zfs.pool.scrub", "secure"),
		warn("host-b", "systemd.unit.failed", "zfs-import-scan.service"),
	)

	var b Book
	if _, err := b.Add(Silence{Host: "host-c", Rule: "zfs.pool.scrub", Subject: "media",
		Reason: "scrub scheduled for Sunday"}, now); err != nil {
		t.Fatal(err)
	}

	out := Apply(r, &b, now)
	if len(out.Alerts) != 3 {
		t.Fatalf("silencing removed an alert from the report: %d left", len(out.Alerts))
	}
	if out.Counts.Total != 2 || out.Counts.Warning != 2 {
		t.Errorf("counts = %+v, want 2 warnings (the silenced one excluded)", out.Counts)
	}
	if out.Silenced != 1 {
		t.Errorf("Silenced = %d, want 1 — a quiet dashboard must not look like a healthy one", out.Silenced)
	}

	var got alert.Alert
	for _, a := range out.Alerts {
		if a.Subject == "media" {
			got = a
		}
	}
	if !got.Silenced || got.SilenceReason != "scrub scheduled for Sunday" {
		t.Errorf("the silenced alert was not stamped: %+v", got)
	}

	// The input report must not have been mutated.
	if r.Counts.Total != 3 || r.Alerts[0].Silenced {
		t.Error("Apply mutated its input")
	}
}

// Subject is in the key, so acknowledging one pool must not quietly cover the
// other. media and secure both fail the same rule on the same host.
func TestSilenceDoesNotBleedAcrossSubjects(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var b Book
	b.Add(Silence{Host: "host-c", Rule: "zfs.pool.scrub", Subject: "media"}, now)

	if _, ok := b.Covers(warn("host-c", "zfs.pool.scrub", "secure"), now); ok {
		t.Error("silencing media also silenced secure")
	}
	if _, ok := b.Covers(warn("host-b", "zfs.pool.scrub", "media"), now); ok {
		t.Error("a silence leaked to another host")
	}
	if _, ok := b.Covers(warn("host-c", "zfs.pool.capacity", "media"), now); ok {
		t.Error("a silence leaked to another rule")
	}
	if _, ok := b.Covers(warn("host-c", "zfs.pool.scrub", "media"), now); !ok {
		t.Error("the silence did not cover its own alert")
	}
}

// Rewording a message must not break an existing silence — the same reason
// Rule is documented as the field not to reword.
func TestSilenceSurvivesMessageRewording(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var b Book
	b.Add(Silence{Host: "host-a", Rule: "docker.container.restarting", Subject: "immich_server"}, now)

	a := alert.Alert{Host: "host-a", Rule: "docker.container.restarting", Subject: "immich_server",
		Level: alert.LevelWarning, Message: "container immich_server has been up 1 second"}
	if _, ok := b.Covers(a, now); !ok {
		t.Fatal("silence did not match")
	}
	a.Message = "immich_server keeps restarting, it seems"
	if _, ok := b.Covers(a, now); !ok {
		t.Error("rewording the message broke the silence")
	}
}

func TestExpiryLapsesAndPrunes(t *testing.T) {
	t.Parallel()
	now := time.Now()
	until := now.Add(time.Hour)
	var b Book
	b.Add(Silence{Host: "host-a", Rule: "docker.container.restarting", Subject: "immich_server",
		Until: &until}, now)

	a := warn("host-a", "docker.container.restarting", "immich_server")
	if _, ok := b.Covers(a, now.Add(30*time.Minute)); !ok {
		t.Error("silence lapsed early")
	}
	if _, ok := b.Covers(a, now.Add(2*time.Hour)); ok {
		t.Error("silence did not lapse")
	}
	if len(b.List(now.Add(2*time.Hour))) != 0 {
		t.Error("a lapsed silence is still listed")
	}
	if !b.Prune(now.Add(2 * time.Hour)) {
		t.Error("Prune did not report the removal")
	}
	if len(b.Silences) != 0 {
		t.Error("Prune left the lapsed silence behind")
	}

	// An expiry in the past is a mistake, not an instruction.
	past := now.Add(-time.Hour)
	if _, err := b.Add(Silence{Host: "a", Rule: "b", Until: &past}, now); err == nil {
		t.Error("accepted an expiry in the past")
	}
}

// The notifier's input. An alert silenced while outstanding leaves the active
// set; when the silence lapses it re-enters and fires again, which is exactly
// what a temporary acknowledgement should do.
func TestUnsilencedIsWhatNotifiesAndLapsingRefires(t *testing.T) {
	t.Parallel()
	now := time.Now()
	r := rep(warn("host-a", "docker.container.restarting", "immich_server"),
		warn("host-b", "systemd.unit.failed", "zfs-import-scan.service"))

	until := now.Add(time.Hour)
	var b Book
	b.Add(Silence{Host: "host-a", Rule: "docker.container.restarting",
		Subject: "immich_server", Until: &until}, now)

	live := Unsilenced(Apply(r, &b, now).Alerts)
	if len(live) != 1 || live[0].Host != "host-b" {
		t.Fatalf("notifier input = %+v, want only the host-b alert", live)
	}

	// After the silence lapses the condition is notifiable again.
	live = Unsilenced(Apply(r, &b, now.Add(2*time.Hour)).Alerts)
	if len(live) != 2 {
		t.Errorf("a lapsed silence did not restore the alert: %+v", live)
	}
}

func TestAddIsIdempotentAndValidated(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var b Book

	first, err := b.Add(Silence{Host: "host-a", Rule: "r", Subject: "s", Reason: "one"}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Add(Silence{Host: "host-a", Rule: "r", Subject: "s", Reason: "two"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Error("silencing the same condition twice made two silences")
	}
	if len(b.Silences) != 1 {
		t.Errorf("book has %d entries, want 1", len(b.Silences))
	}
	if b.Silences[first.ID].Reason != "two" {
		t.Error("re-silencing did not update the reason")
	}

	if _, err := b.Add(Silence{Rule: "r"}, now); err == nil {
		t.Error("accepted a silence with no host")
	}
	if _, err := b.Add(Silence{Host: "h"}, now); err == nil {
		t.Error("accepted a silence with no rule")
	}

	if !b.Remove(first.ID, now) {
		t.Error("Remove did not find the silence")
	}
	if b.Remove("nope", now) {
		t.Error("Remove claimed to delete something that was not there")
	}
}

func TestIDHashIsPinned(t *testing.T) {
	if got := ID("node1", "systemd.unit.failed", "postfix@-.service"); got != "22cdca5a72e1" {
		t.Fatalf("ID = %q, want 22cdca5a72e1; stored silences would stop matching", got)
	}
}

// IDs go into URLs, so they must be short and free of anything needing escaping
// — a rule name is full of dots and a subject can be a systemd unit.
func TestIDIsURLSafeAndStable(t *testing.T) {
	t.Parallel()
	id := ID("node1", "systemd.unit.failed", "postfix@-.service")
	if len(id) != 12 {
		t.Errorf("id = %q, want 12 chars", id)
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("id %q contains %q, which is not URL-safe hex", id, r)
		}
	}
	if id != ID("node1", "systemd.unit.failed", "postfix@-.service") {
		t.Error("id is not stable")
	}
	if id == ID("node1", "systemd.unit.failed", "other.service") {
		t.Error("different subjects collide")
	}
}
