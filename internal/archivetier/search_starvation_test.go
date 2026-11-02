package archivetier

import (
	"strings"
	"testing"
	"time"
)

func TestReportLookupsDoNotStarvePreGrabRefresh(t *testing.T) {
	for _, dryFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "applied", true: "dry-run then apply"}[dryFirst], func(t *testing.T) {
			r, a, _, now := runner(t)
			m := movieTarget()
			release := testMovieRelease(t)
			release.Size = 7 * GB
			movie := &fakeArr{targets: []Target{m}, current: m, releases: []Release{release}, opted: true, present: true}
			r.Arr["radarr"] = movie
			r.Config.SearchesPerMinute = 2
			for night := 0; night < 1; night++ {
				at := now.Add(time.Duration(night) * 24 * time.Hour)
				r.Now = func() time.Time { return at }
				// Simulate a completed, reconciled job between nights; retain lookup
				// cooldown and nightly counters so the original starvation recurs.
				st, err := r.Store.Load()
				if err != nil {
					t.Fatal(err)
				}
				st.Job = nil
				if err = r.Store.Save(st); err != nil {
					t.Fatal(err)
				}
				if dryFirst {
					r.DryRun = true
					s := check(t, r, at)
					if s.SearchesUsed != 1 || s.Job != nil {
						t.Fatalf("dry-run consumed refresh budget: %+v", s)
					}
				}
				r.DryRun = false
				s := check(t, r, at)
				if s.Job == nil || s.Job.Phase != Downloading || s.SearchesUsed != 2 || movie.grabs != night+1 || a.grabs != 0 {
					t.Fatalf("night %d: refresh starved: searches=%d grabs=%d job=%+v", night, s.SearchesUsed, movie.grabs, s.Job)
				}
			}
			if movie.searches != 2 || a.searches != 0 {
				t.Fatalf("unexpected tracker lookups: movie=%d series=%d", movie.searches, a.searches)
			}
		})
	}
}

func TestRollingLookupRatePersistsAcrossRestart(t *testing.T) {
	r, a, _, now := runner(t)
	a.releases = nil
	a.targets = nil
	for id := 1; id <= 7; id++ {
		tt := target()
		tt.ID = id
		a.targets = append(a.targets, tt)
		r.Config.PlexKeys[tt.Key()] = "900"
	}
	s := check(t, r, now)
	if a.searches != 5 || s.SearchesLastMinute(now) != 5 || s.SearchesUsed != 5 {
		t.Fatalf("expected all five rate slots (no reserved refresh): searches=%d state=%+v", a.searches, s)
	}
	restarted := *r
	at := now.Add(59 * time.Second)
	restarted.Now = func() time.Time { return at }
	s = check(t, &restarted, at)
	if a.searches != 5 || !strings.Contains(s.Report[5].Reason, "Search limit") {
		t.Fatalf("restart refunded lookups: %d %+v", a.searches, s.Report)
	}
	at = now.Add(time.Minute)
	s = check(t, &restarted, at)
	if a.searches != 7 || s.SearchesLastMinute(at) != 2 || s.SearchesUsed != 7 {
		t.Fatalf("minute boundary failed to refill while preserving nightly total/cache: %d %+v", a.searches, s)
	}
}

func TestPreGrabRefreshWaitsForRateThenRetriesCachedCandidate(t *testing.T) {
	r, a, _, now := runner(t)
	st, _ := r.Store.Load()
	for i := 0; i < 4; i++ {
		st.SearchTimes = append(st.SearchTimes, now)
	}
	st.SearchNight, st.SearchesUsed = "2026-10-06", 4
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	s := check(t, r, now)
	if a.searches != 1 || a.grabs != 0 || s.SearchesUsed != 5 || !strings.Contains(s.Paused, "Search limit") {
		t.Fatalf("refresh escaped rate limit: searches=%d grabs=%d %+v", a.searches, a.grabs, s)
	}
	at := now.Add(time.Minute)
	r.Now = func() time.Time { return at }
	s = check(t, r, at)
	if a.searches != 2 || a.grabs != 1 || s.SearchesUsed != 6 || s.Job == nil {
		t.Fatalf("cached candidate was not freshly searched/grabbed next cycle: %+v", s)
	}
}
