package archivetier

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestImportRequiresFiveQuietMinutesAcrossRestart(t *testing.T) {
	r, a, p, now := runner(t)
	r.Config.TimeoutHours = 100
	check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	p.active = true
	first := now.Add(time.Hour)
	s := check(t, r, first)
	if a.imports != 0 || !s.LastStreamSeen.Equal(first) || !s.StreamQuietSince.IsZero() {
		t.Fatal("stream not persisted")
	}
	restarted := *r
	s = check(t, &restarted, first.Add(10*time.Hour))
	if a.imports != 0 || !s.LastStreamSeen.Equal(first.Add(10*time.Hour)) {
		t.Fatal("active stream bypassed after restart")
	}
	p.active = false
	quiet := first.Add(11 * time.Hour)
	check(t, &restarted, quiet)
	check(t, &restarted, quiet.Add(5*time.Minute-time.Second))
	if a.imports != 0 {
		t.Fatal("import before five quiet minutes")
	}
	p.active = true
	check(t, &restarted, quiet.Add(5*time.Minute))
	p.active = false
	quiet = quiet.Add(6 * time.Minute)
	check(t, &restarted, quiet)
	p.err = errors.New("unknown activity")
	_, _ = restarted.Check(context.Background(), quiet.Add(4*time.Minute))
	p.err = nil
	quiet = quiet.Add(5 * time.Minute)
	check(t, &restarted, quiet)
	observeQuiet(t, r, quiet.Add(5*time.Minute))
	r.DState = func() (int, error) { return r.Config.MaxDState, nil }
	s = check(t, r, quiet.Add(5*time.Minute))
	if a.imports != 0 || !strings.Contains(s.Paused, "D-state") {
		t.Fatal("host pressure bypassed")
	}
	r.DState = func() (int, error) { return 0, nil }
	s = check(t, r, quiet.Add(5*time.Minute))
	if a.imports != 1 || s.Job.Phase != Verifying {
		t.Fatal("quiet import did not run")
	}
}

func TestPreImportProblemsNeedApproval(t *testing.T) {
	for _, scenario := range []string{"rejection", "other download", "tag", "version", "selection", "identity", "queue change", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, _, now := runner(t)
			s := check(t, r, now)
			a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
			switch scenario {
			case "rejection":
				a.items[0].Rejections = []struct {
					Reason string `json:"reason"`
				}{{"Not a valid upgrade"}, {"Unrecognized extra"}}
			case "other download":
				a.queue = append(a.queue, QueueItem{SeriesID: 1, DownloadID: "other"})
			case "tag":
				a.opted = false
			case "version":
				s.Job.PolicyVersion = "old"
				r.Store.Save(s)
			case "selection":
				s.Job.Selection.MinScore = -1
				r.Store.Save(s)
			case "identity":
				r.Arr["sonarr"] = &recoveringArr{fakeArr: a, historyErr: errors.New("identity unavailable")}
			case "queue change":
				r.Arr["sonarr"] = &changingImportQueue{fakeArr: a}
			case "timeout":
				now = now.Add(25 * time.Hour)
			}
			s = check(t, r, now)
			if s.Job.Phase != NeedsApproval || !s.Job.HoldsLease() || !Healthy(s) || a.imports != 0 || len(s.FailedJobs) != 0 || len(s.History) != 0 || s.Job.ApprovalReason == "" {
				t.Fatalf("unclean exit: %+v", s)
			}
			if !SameFiles(s.Job.Target, a.current) || len(a.queue) == 0 {
				t.Fatal("library or download lost")
			}
		})
	}
}

type changingImportQueue struct {
	*fakeArr
	reads int
}

func (a *changingImportQueue) Queue(ctx context.Context) ([]QueueItem, error) {
	a.reads++
	if a.reads >= 3 {
		return append(a.queue, QueueItem{SeriesID: 1, DownloadID: "other"}), nil
	}
	return a.fakeArr.Queue(ctx)
}

func TestImportPreflightWaitsThenNeedsApproval(t *testing.T) {
	for _, baseline := range []bool{false, true} {
		r, a, _, now := runner(t)
		s := check(t, r, now)
		a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
		if baseline {
			s.Job.RecycleRoot = "/changed"
			r.Store.Save(s)
		} else {
			a.safetyErr = errors.New("disk unavailable")
		}
		observeQuiet(t, r, now.Add(time.Hour))
		s = check(t, r, now.Add(time.Hour))
		if s.Job.Phase != Downloading || s.Paused == "" || a.imports != 0 {
			t.Fatal("preflight did not wait", s)
		}
		s = check(t, r, now.Add(25*time.Hour))
		if s.Job.Phase != NeedsApproval || !s.Job.HoldsLease() {
			t.Fatal("preflight timeout did not end", s)
		}
	}
}

func TestLegacyFailedJobMigratesAndOtherTitleContinues(t *testing.T) {
	r, a, _, now := runner(t)
	failed := &Job{Target: Target{Kind: "sonarr", ID: 9, Season: 1}, Phase: Failed, Started: now.Add(-time.Hour), Message: "unproven import", ImportedAt: now}
	b, err := json.Marshal(State{Job: failed})
	if err != nil {
		t.Fatal(err)
	}
	var legacy State
	if err := json.Unmarshal(b, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Job != nil || len(legacy.FailedJobs) != 1 || !legacy.FailedJobs[0].HasImportIntent() || Healthy(legacy) {
		t.Fatal("failed legacy state lost", legacy)
	}
	r.Store.Save(legacy)
	s := check(t, r, now)
	if s.Job == nil || s.Job.Phase != Downloading || s.Job.Target.ID != a.current.ID || a.grabs != 1 || len(s.FailedJobs) != 1 || Healthy(s) {
		t.Fatal("failure blocked other title", s)
	}
	if _, err := r.ClearFailed(context.Background(), now, JobID(*failed)); err == nil {
		t.Fatal("clear bypassed import intent")
	}
}

func TestQuietWindowResetsAfterUnobservedTime(t *testing.T) {
	for _, scenario := range []string{"gap", "restart", "backward clock", "forward clock"} {
		t.Run(scenario, func(t *testing.T) {
			r, _, _, now := runner(t)
			st := State{}
			ctx := context.Background()
			for i := 0; i <= 4; i++ {
				if _, err := r.observePlayback(ctx, now.Add(time.Duration(i)*time.Minute), &st); err != nil {
					t.Fatal(err)
				}
			}
			at := now.Add(5 * time.Minute)
			switch scenario {
			case "gap", "forward clock":
				at = now.Add(time.Hour)
			case "backward clock":
				at = now.Add(3 * time.Minute)
			case "restart":
				// Persisted quiet time cannot prove playback while the worker was absent.
				restarted := &Runner{Plex: r.Plex, Store: r.Store, DState: r.DState, Config: r.Config}
				r = restarted
			}
			if err := r.importAdmission(ctx, at, &st); err == nil || !st.StreamQuietSince.Equal(at) {
				t.Fatal("unobserved time admitted an import")
			}
			for i := 1; i < 5; i++ {
				if err := r.importAdmission(ctx, at.Add(time.Duration(i)*time.Minute), &st); err == nil {
					t.Fatal("quiet window ended early")
				}
			}
			if err := r.importAdmission(ctx, at.Add(5*time.Minute), &st); err != nil {
				t.Fatal("observed quiet window did not admit import", err)
			}
		})
	}
}
