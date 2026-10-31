package plugin

import (
	"context"
	"testing"
	"time"
)

func TestInitialPollReadinessAndPartialFailures(t *testing.T) {
	p := &fakePlugin{fail: true}
	r, e := Open(t.TempDir(), []Builtin{{ID: "sample", New: func(Host) Plugin { return p }}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	enabled := true
	if _, e = r.Update("sample", Update{Enabled: &enabled, Settings: Config{"endpoint": "http://service.invalid"}}); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if e = r.WaitInitialPoll(ctx); e == nil {
		t.Fatal("cold registry reported poll ready")
	}
	for i := 1; i <= 3; i++ {
		r.Poll(context.Background())
		if e = r.WaitInitialPoll(context.Background()); e != nil {
			t.Fatal(e)
		}
		if r.NotificationSources()["sample"] {
			t.Fatal("failed poll reported successful")
		}
		if !r.UncertainNotificationSources()["sample"] {
			t.Fatal("outage escalation readiness changed", i)
		}
	}
	p.fail = false
	r.Poll(context.Background())
	if !r.NotificationSources()["sample"] || r.UncertainNotificationSources()["sample"] {
		t.Fatal("successful poll not ready")
	}
}
