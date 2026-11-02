package archivetier

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func approvalFixture(t *testing.T) (*Runner, *fakeArr, time.Time, ApprovalRequest) {
	t.Helper()
	r, a, _, now := runner(t)
	// An oversized (but below the cap) encode scores poorly on size fit.
	rel := a.releases[0]
	rel.Title, rel.Guid, rel.Size, rel.Seeders = "Example Series.S03.1080p.WEB-DL.DD5.1.x264-TayTO", "fixture-approval", 17*GB, 58
	rel.Quality, rel.Flags, rel.IndexerID, rel.Protocol = qual(1080), json.RawMessage(`1`), 10, "torrent"
	rel.Languages, rel.Rejections, rel.FullSeason, rel.Season = []Language{{Name: "English"}}, nil, true, 3
	a.releases = []Release{rel}
	d := Select(target(), a.releases, r.Config)
	if d.Reason != NoGoodRelease || len(d.Candidates) != 1 {
		t.Fatalf("fixture must be below threshold: %+v", d)
	}
	d.SearchedAt = now
	st := State{Lookups: map[string]Lookup{targetKey(target()): cachedLookup(target(), d, a.releases, now)}, Report: []Decision{sanitized(d)}}
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	c := d.Candidates[0]
	return r, a, now, ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3, ReleaseID: c.ID, IndexerID: c.IndexerID}
}

func TestApprovalValidation(t *testing.T) {
	for _, scenario := range []string{"hard reject", "missing", "future", "changed files", "changed mode", "untagged", "wrong release", "wrong indexer", "dry run", "same title active"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, now, in := approvalFixture(t)
			st, _ := r.Store.Load()
			l := st.Lookups[targetKey(target())]
			switch scenario {
			case "hard reject":
				l.Releases[0].Flags = json.RawMessage(`0`)
			case "missing":
				st.Lookups = nil
			case "stale":
				l.At = now.Add(-24 * time.Hour)
			case "future":
				l.At = now.Add(time.Minute)
			case "changed files":
				a.targets = []Target{target()}
				a.targets[0].Files[0].ID++
			case "changed mode":
				a.targets[0].FourK = true
			case "untagged":
				a.targets = nil
			case "wrong release":
				in.ReleaseID = "unknown"
			case "wrong indexer":
				in.IndexerID++
			case "dry run":
				r.DryRun = true
			case "same title active":
				st.Job = &Job{Phase: Downloading, Target: target()}
			}
			if st.Lookups != nil {
				st.Lookups[targetKey(target())] = l
			}
			r.Store.Save(st)
			_, err := r.Approve(context.Background(), now, in)
			if err == nil {
				t.Fatal("unsafe approval accepted")
			}
			st, _ = r.Store.Load()
			if len(st.Approvals) != 0 || a.grabs != 0 || a.searches != 0 {
				t.Fatal("refusal mutated work or searched")
			}
		})
	}
}

func TestApprovalQueuesExactReleaseAndMarksHistory(t *testing.T) {
	r, a, now, in := approvalFixture(t)
	now = now.Add(10 * time.Hour) // Daytime: approval must progress outside the nightly window.
	r.Now = func() time.Time { return now }
	st, err := r.Approve(context.Background(), now, in)
	if err != nil || len(st.Approvals) == 0 || a.grabs != 0 {
		t.Fatalf("queue: %v", err)
	}
	// A better-scoring release in the fresh lookup must not replace the approved identity.
	best := a.releases[0]
	best.Guid, best.Size = "better", 12*GB
	a.releases = append(a.releases, best)
	st = check(t, r, now)
	if len(st.Approvals) != 0 || st.Job == nil || !st.Job.ManuallyApproved || st.Job.Release.Size != 17*GB || a.grabbed.Guid != "fixture-approval" || st.Job.Score.Total >= r.Config.Selection.MinScore || a.grabs != 1 {
		t.Fatal("did not grab the approved release")
	}
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	observeQuiet(t, r, now.Add(5*time.Minute))
	now = now.Add(5 * time.Minute)
	st = check(t, r, now)
	if st.Job.Phase != Verifying {
		t.Fatalf("daytime import: %s %s", st.Job.Phase, st.Paused)
	}
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = 2 * GB
		a.current.Files[i].Quality = qual(1080)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	now = now.Add(time.Minute)
	st = check(t, r, now)
	if st.Job.Phase != Complete || len(st.History) != 1 || !st.History[0].Job.ManuallyApproved || !strings.Contains(st.Job.Summary, "Manually approved") {
		t.Fatal("approval history marking missing")
	}
}

func TestPendingApprovalSafetyAndClear(t *testing.T) {
	for _, scenario := range []string{"disk", "rate", "fresh hard reject", "fresh missing", "admission", "plex active", "dstate"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, now, in := approvalFixture(t)
			if _, err := r.Approve(context.Background(), now, in); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "disk":
				r.DownloadFree = func() (int64, error) { return 300 * GB, nil }
			case "rate":
				st, _ := r.Store.Load()
				for i := 0; i < 5; i++ {
					st.SearchTimes = append(st.SearchTimes, now)
				}
				r.Store.Save(st)
			case "fresh hard reject":
				a.releases[0].Flags = json.RawMessage(`0`)
			case "fresh missing":
				a.releases = nil
			case "plex active":
				r.Plex.(*fakePlex).active = true
			case "dstate":
				r.DState = func() (int, error) { return r.Config.MaxDState, nil }
			}
			adm := Admission{NewWrites: scenario != "admission", Reason: "admission closed"}
			st, err := r.CheckAdmission(context.Background(), now, adm)
			if err != nil || len(st.Approvals) == 0 || a.grabs != 0 || st.Paused == "" {
				t.Fatalf("guard lost approval: %v", err)
			}
			in.Clear = true
			st, err = r.Approve(context.Background(), now, in)
			if err != nil || len(st.Approvals) != 0 {
				t.Fatalf("clear: %v", err)
			}
		})
	}
}

func TestApprovalCandidatesAndMorningSummary(t *testing.T) {
	r, a, now, _ := approvalFixture(t)
	rs := []Release{}
	for i := 0; i < 5; i++ {
		rel := a.releases[0]
		rel.Guid = string(rune('a' + i))
		rs = append(rs, rel)
	}
	bad := a.releases[0]
	bad.Guid = "bad"
	bad.Flags = json.RawMessage(`0`)
	rs = append(rs, bad)
	d := Select(target(), rs, r.Config)
	if len(d.Candidates) != 5 {
		t.Fatalf("every freeleech release is offered: %d", len(d.Candidates))
	}
	for _, c := range d.Candidates {
		if c.ID == releaseID(bad) {
			t.Fatal("hard rejection offered")
		}
	}
	st := State{Report: []Decision{d}}
	st.Report[0].SearchedAt = now
	r.Notify = func(context.Context, string) error { return nil }
	r.queueReports(&st, now)
	if len(st.Notifications) != 1 || !strings.Contains(st.Notifications[0].Body, "awaiting approval") || !strings.Contains(st.Notifications[0].Body, "TayTO") {
		t.Fatal("approval summary missing")
	}
	private, _ := r.Store.Load()
	public := PublicState(private)
	if len(public.Lookups[targetKey(target())].Releases) != 0 || len(private.Lookups[targetKey(target())].Releases) == 0 {
		t.Fatal("public view leaked or destroyed tracker cache")
	}
}

func TestSummaryDropsApprovedCandidate(t *testing.T) {
	r, _, now, in := approvalFixture(t)
	st, _ := r.Store.Load()
	r.Notify = func(_ context.Context, body string) error {
		t.Fatal("obsolete awaiting-approval summary sent")
		return nil
	}
	r.queueReports(&st, now)
	st.Approvals = []Approval{{ApprovalRequest: in}}
	if err := r.sendReports(context.Background(), &st, now.Add(10*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(st.Notifications) != 0 {
		t.Fatal("obsolete awaiting line retained")
	}
}

func TestLegacyBelowThresholdLookupRefreshes(t *testing.T) {
	r, a, now, _ := legacyBelowThresholdFixture(t)
	r.DryRun = true
	st := check(t, r, now)
	if a.searches != 1 || len(st.Report[0].Candidates) != 1 || len(st.Lookups[targetKey(target())].Releases) != 1 {
		t.Fatal("legacy lookup was not refreshed")
	}
}

func legacyBelowThresholdFixture(t *testing.T) (*Runner, *fakeArr, time.Time, ApprovalRequest) {
	t.Helper()
	r, a, now, in := approvalFixture(t)
	st, _ := r.Store.Load()
	l := st.Lookups[targetKey(target())]
	l.Releases, l.Decision.Candidates = nil, nil
	st.Lookups[targetKey(target())] = l
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	return r, a, now, in
}

func TestQueuedSearchRefreshesLegacyCacheUnderRateLimit(t *testing.T) {
	r, a, now, in := legacyBelowThresholdFixture(t)
	r.Now = func() time.Time { return now }
	st, _ := r.Store.Load()
	for i := 0; i < r.Config.SearchesPerMinute; i++ {
		st.SearchTimes = append(st.SearchTimes, now)
	}
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SearchTitleCached(context.Background(), now, in); err != ErrSearchRateLimit || a.searches != 0 {
		t.Fatalf("unusable legacy cache bypassed rate admission: %v searches=%d", err, a.searches)
	}
	st, _ = r.Store.Load()
	if l := st.Lookups[targetKey(target())]; l.Decision.Reason != NoGoodRelease || len(l.Releases) != 0 {
		t.Fatalf("rate refusal rewrote legacy cache: %+v", l)
	}
	now = now.Add(time.Minute)
	st, err := r.SearchTitleCached(context.Background(), now, in)
	if err != nil || a.searches != 1 || len(st.Report[0].Candidates) != 1 || st.Report[0].Reason != NoGoodRelease {
		t.Fatalf("legacy lookup was not refreshed for approval: %v report=%+v searches=%d", err, st.Report, a.searches)
	}
	l := st.Lookups[targetKey(target())]
	if len(l.Releases) != 1 || !l.At.Equal(now) || st.SearchesLastMinute(now) != 1 {
		t.Fatalf("fresh lookup identities, timestamp or rate usage missing: %+v", l)
	}
	st, err = r.SearchTitleCached(context.Background(), now.Add(time.Hour), in)
	if err != nil || a.searches != 1 || len(st.Report[0].Candidates) != 1 || !st.Lookups[targetKey(target())].At.Equal(now) {
		t.Fatalf("usable refreshed cache not reused: %v searches=%d", err, a.searches)
	}
}
