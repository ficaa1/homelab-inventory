package archivetier

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNotificationQueueQuietHoursAndRestart(t *testing.T) {
	r, _, _, _ := runner(t)
	var messages []string
	r.Notify = func(_ context.Context, body string) error { messages = append(messages, body); return nil }
	night := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	st := State{Job: &Job{Target: target(), Phase: Complete, Started: night, Summary: "Example Series completed"}, Report: []Decision{{Target: movieTarget(), Reason: NoGoodRelease, SearchedAt: night}}}
	r.queueReports(&st, night)
	r.queueReports(&st, night)
	if len(st.Notifications) != 2 {
		t.Fatal("duplicate transitions", st.Notifications)
	}
	r.Store.Save(st)
	if e := r.sendReports(context.Background(), &st, night); e != nil || len(messages) != 0 {
		t.Fatal("night notification", e)
	}
	st, _ = r.Store.Load()
	for _, at := range []time.Time{night.Add(7 * time.Hour), night.Add(8 * time.Hour)} {
		if e := r.sendReports(context.Background(), &st, at); e != nil {
			t.Fatal(e)
		}
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "completed") || !strings.Contains(messages[0], "skipped") {
		t.Fatal(messages)
	}
	st, _ = r.Store.Load()
	if len(st.Notifications) != 0 || st.NotificationDay != "2026-10-06" {
		t.Fatal("delivery not durable", st)
	}
	st.Job.Phase = Failed
	st.Job.Message = "failure"
	r.queueReports(&st, night.Add(7*time.Hour))
	r.sendReports(context.Background(), &st, night.Add(8*time.Hour))
	if len(messages) != 1 {
		t.Fatal("more than one daily batch")
	}
	next := night.Add(32 * time.Hour)
	r.sendReports(context.Background(), &st, next)
	if len(messages) != 2 {
		t.Fatal("next day's pending message lost")
	}
}
func TestNotificationDisabledDryRunFailureAndDST(t *testing.T) {
	r, _, _, _ := runner(t)
	now := time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC)
	st := State{Job: &Job{Target: target(), Phase: Failed, Started: now, Message: "failure"}}
	r.queueReports(&st, now)
	if len(st.Notifications) != 0 {
		t.Fatal("unset notifier queued messages")
	}
	r.Notify = func(context.Context, string) error { return errors.New("secret endpoint error") }
	r.DryRun = true
	r.queueReports(&st, now)
	if len(st.Notifications) != 0 {
		t.Fatal("dry run queued messages")
	}
	r.DryRun = false
	r.queueReports(&st, now)
	r.Store.Save(st)
	r.sendReports(context.Background(), &st, now)
	if len(st.Notifications) != 1 || st.NotificationDay != "" || strings.Contains(st.NotificationError, "secret") {
		t.Fatal("failed send lost queue or leaked endpoint", st)
	}
	sent := 0
	r.Notify = func(context.Context, string) error { sent++; return nil }
	r.sendReports(context.Background(), &st, now.Add(-time.Minute))
	if sent != 0 {
		t.Fatal("DST quiet hour failed")
	}
	r.sendReports(context.Background(), &st, now)
	if sent != 1 {
		t.Fatal("retry failed")
	}
	st.Notifications = []Notification{{Key: "later", Body: "pending"}}
	st.NotificationDay = ""
	r.sendReports(context.Background(), &st, time.Date(2026, 10, 26, 21, 0, 0, 0, time.UTC))
	if sent != 1 {
		t.Fatal("night send after 21:00")
	}
}

func TestNightlyEligibilitySkipsAreQueuedWithoutSearch(t *testing.T) {
	r, _, _, now := runner(t)
	r.Notify = func(context.Context, string) error { return nil }
	st := State{Reported: now, Night: "2026-10-06", Report: []Decision{{Target: target(), Reason: "watched within idle window"}}}
	r.queueReports(&st, now)
	if len(st.Notifications) != 1 || !strings.Contains(st.Notifications[0].Body, "watched within idle window") {
		t.Fatal("eligibility skip lost", st.Notifications)
	}
	r.queueReports(&st, now)
	if len(st.Notifications) != 1 {
		t.Fatal("duplicate skip")
	}
	st = State{Reported: now.Add(8 * time.Hour), Report: []Decision{{Target: target(), Reason: "outside nightly window"}}}
	r.queueReports(&st, now.Add(8*time.Hour))
	if len(st.Notifications) != 0 {
		t.Fatal("daytime idle polling queued skips")
	}
}

// The last job stays in state after it ends. It is announced once, not again
// when its deduplication key ages out, and a job completed before summaries
// existed is not announced with an empty body.
func TestFinishedJobIsAnnouncedOnce(t *testing.T) {
	r, _, _, _ := runner(t)
	r.Notify = func(context.Context, string) error { return nil }
	day := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	st := State{Job: &Job{Target: target(), Phase: Complete, Started: day, Summary: "Example Series completed"}}
	r.queueReports(&st, day)
	r.Store.Save(st)
	if e := r.sendReports(context.Background(), &st, day); e != nil || len(st.Notifications) != 0 {
		t.Fatal("first announcement not sent", e, st.Notifications)
	}
	for i := 1; i <= 40; i++ {
		r.queueReports(&st, day.AddDate(0, 0, i))
	}
	if len(st.Notifications) != 0 {
		t.Fatal("finished job announced again", st.Notifications)
	}
	old := State{Job: &Job{Target: movieTarget(), Phase: Complete, Started: day}}
	r.queueReports(&old, day)
	if len(old.Notifications) != 0 {
		t.Fatal("empty summary queued", old.Notifications)
	}
}
