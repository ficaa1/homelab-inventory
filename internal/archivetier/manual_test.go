package archivetier

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// An operator approval overrides every automatic rule except freeleech, a
// matching season pack and an actual saving, through grab, import and verify.
func TestManualApprovalOverridesAutomaticRules(t *testing.T) {
	r, a, now, _ := approvalFixture(t)
	rel := a.releases[0]
	rel.Guid, rel.Title, rel.Seeders, rel.Quality = "fixture-720p", "Example Series.S03.720p.WEB-DL.AAC2.0.x264-SMALL", 2, qual(720)
	a.releases = []Release{rel}
	d := Select(target(), a.releases, r.Config)
	if len(d.Candidates) != 1 || d.Candidates[0].Rejection == "" {
		t.Fatalf("rule-breaking release must be offered with its reason: %+v", d.Candidates)
	}
	d.SearchedAt = now
	st := State{Lookups: map[string]Lookup{targetKey(target()): cachedLookup(target(), d, a.releases, now)}, Report: []Decision{sanitized(d)}}
	r.Store.Save(st)
	in := ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3, ReleaseID: d.Candidates[0].ID, IndexerID: d.Candidates[0].IndexerID}
	now = now.Add(10 * time.Hour)
	r.Now = func() time.Time { return now }
	// Recent play and a missing Plex identity are automatic-selection rules.
	r.Config.PlexKeys = nil
	if _, err := r.Approve(context.Background(), now, in); err != nil {
		t.Fatal(err)
	}
	st = check(t, r, now)
	if st.Job == nil || !st.Job.ManuallyApproved || a.grabbed.Guid != "fixture-720p" || len(st.Approvals) != 0 {
		t.Fatalf("approved release not grabbed: %s", st.Paused)
	}
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	observeQuiet(t, r, now.Add(5*time.Minute))
	now = now.Add(5 * time.Minute)
	if st = check(t, r, now); st.Job.Phase != Verifying {
		t.Fatalf("manual import: %s %s", st.Job.Phase, st.Paused)
	}
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = GB
		a.current.Files[i].Quality = qual(720)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 2, AudioCodec: "AAC", VideoCodec: "x264"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	now = now.Add(time.Minute)
	if st = check(t, r, now); st.Job.Phase != Complete {
		t.Fatalf("manual verification: %s %s", st.Job.Phase, st.Job.Message)
	}
}

func TestManualRulesStillRequireFreeleechPackAndSavings(t *testing.T) {
	c, tg := testConfig(), target()
	base := Release{Guid: "g", IndexerID: 10, Protocol: "torrent", Flags: json.RawMessage(`1`), FullSeason: true, Season: 3, Size: GB}
	if why := manualReject(base, tg, c); why != "" {
		t.Fatal(why)
	}
	for name, mutate := range map[string]func(*Release){
		"not freeleech":  func(r *Release) { r.Flags = json.RawMessage(`0`) },
		"single episode": func(r *Release) { r.FullSeason = false },
		"other season":   func(r *Release) { r.Season = 2 },
		"no saving":      func(r *Release) { r.Size = tg.Bytes() },
		"other indexer":  func(r *Release) { r.IndexerID = 99 },
	} {
		r := base
		mutate(&r)
		if manualReject(r, tg, c) == "" {
			t.Error(name, "accepted")
		}
	}
	old, current := tg, target()
	for i := range current.Files {
		current.Files[i].ID += 100
		current.Episodes[i].FileID = current.Files[i].ID
	}
	if Verified(old, current, c, ModeAny) {
		t.Fatal("manual verification accepted a replacement that saves nothing")
	}
	current.Files[0].Size, current.Files[1].Size = GB, GB
	if !Verified(old, current, c, ModeAny) {
		t.Fatal("manual verification refused a smaller replacement")
	}
}

func TestApprovalsQueueBehindActiveJob(t *testing.T) {
	r, _, now, in := approvalFixture(t)
	st, _ := r.Store.Load()
	other := movieTarget()
	st.Job = &Job{Phase: Downloading, Target: other}
	r.Store.Save(st)
	st, err := r.Approve(context.Background(), now, in)
	if err != nil || len(st.Approvals) != 1 {
		t.Fatalf("approval not queued behind another job: %v", err)
	}
	// Re-approving the same title replaces its queued choice.
	if st, err = r.Approve(context.Background(), now, in); err != nil || len(st.Approvals) != 1 {
		t.Fatalf("duplicate approval: %v %d", err, len(st.Approvals))
	}
}

func TestSearchTitleIgnoresAutomaticEligibility(t *testing.T) {
	r, a, _, now := runner(t)
	tg := target()
	for i := range tg.Files {
		tg.Files[i].Quality = qual(1080) // automatic selection wants 2160p originals
	}
	a.targets = []Target{tg}
	in := ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}
	now = now.Add(12 * time.Hour) // outside the nightly window
	st, err := r.SearchTitle(context.Background(), now, in)
	if err != nil || a.searches != 1 {
		t.Fatalf("search: %v", err)
	}
	if len(st.Report) != 1 || len(st.Report[0].Candidates) == 0 || st.Report[0].Release != nil || len(st.Lookups[targetKey(tg)].Releases) == 0 {
		t.Fatalf("search results not cached for approval: %+v", st.Report)
	}
	for i := 0; i < r.Config.SearchesPerMinute; i++ {
		st.SearchTimes = append(st.SearchTimes, now)
	}
	r.Store.Save(st)
	if _, err = r.SearchTitle(context.Background(), now, in); err == nil || a.searches != 1 {
		t.Fatal("search ignored the tracker rate limit")
	}
	a.targets = nil
	if _, err = r.SearchTitle(context.Background(), now.Add(time.Hour), in); err == nil {
		t.Fatal("untagged title searched")
	}
}

func TestBlockedApprovalDoesNotStallNightlyScan(t *testing.T) {
	r, a, now, in := approvalFixture(t)
	if _, err := r.Approve(context.Background(), now, in); err != nil {
		t.Fatal(err)
	}
	a.targets = nil // the title lost its tag: the approval blocks
	st := check(t, r, now)
	if len(st.Approvals) != 1 || !st.Approvals[0].Blocked || st.Approvals[0].Message == "" {
		t.Fatalf("approval not marked blocked: %+v", st.Approvals)
	}
	if st.Paused != "no eligible replacement" {
		t.Fatalf("nightly scan did not run: %q", st.Paused)
	}
	// Outside the window a blocked approval must not open it.
	st = check(t, r, now.Add(12*time.Hour))
	if st.Paused != "outside nightly window" {
		t.Fatalf("blocked approval opened the window: %q", st.Paused)
	}
}

func TestRecentPlayIsPerSeason(t *testing.T) {
	recent := map[string]bool{seasonKey("900", 5): true}
	s3, s5 := target(), target()
	s5.Season = 5
	if watched(recent, "900", s3) || !watched(recent, "900", s5) {
		t.Fatal("watching season 5 must block only season 5")
	}
	if !watched(map[string]bool{"900": true}, "900", s3) {
		t.Fatal("history without a season number must block the whole show")
	}
	if !watched(map[string]bool{"901": true}, "901", movieTarget()) {
		t.Fatal("movie play not detected")
	}
}

func TestSeasonChoiceLimitsSeries(t *testing.T) {
	r, a, _, now := runner(t)
	r.DryRun = true
	st, _ := r.Store.Load()
	st.Seasons = map[string][]int{"sonarr:1": {4}}
	r.Store.Save(st)
	if st = check(t, r, now); len(st.Report) != 0 || a.searches != 0 {
		t.Fatalf("unchosen season considered: %+v", st.Report)
	}
	r.DryRun = false
	if _, err := r.SearchTitle(context.Background(), now, ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}); err == nil || a.searches != 0 {
		t.Fatal("unchosen season searched")
	}
	if err := r.SetSeasons("sonarr:1", []int{3, 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SearchTitle(context.Background(), now, ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}); err != nil || a.searches != 1 {
		t.Fatalf("chosen season not searched: %v", err)
	}
	if err := r.SetSeasons("sonarr:1", nil); err != nil {
		t.Fatal(err)
	}
	if st, _ = r.Store.Load(); st.Seasons["sonarr:1"] != nil {
		t.Fatal("season choice not cleared")
	}
}

func TestQueuedSearchReusesCacheAndRateAdmission(t *testing.T) {
	r, a, _, now := runner(t)
	clock := now
	r.Now = func() time.Time { return clock }
	in := ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}
	if _, err := r.SearchTitleCached(context.Background(), now, in); err != nil || a.searches != 1 {
		t.Fatalf("initial search: %v", err)
	}
	if _, err := r.SearchTitleCached(context.Background(), now.Add(23*time.Hour), in); err != nil || a.searches != 1 {
		t.Fatalf("cache missed: %v", err)
	}
	st, _ := r.Store.Load()
	for i := 0; i < r.Config.SearchesPerMinute; i++ {
		st.SearchTimes = append(st.SearchTimes, now.Add(25*time.Hour))
	}
	r.Store.Save(st)
	clock = now.Add(25 * time.Hour)
	if _, err := r.SearchTitleCached(context.Background(), clock, in); err != ErrSearchRateLimit || a.searches != 1 {
		t.Fatalf("limit bypass: %v", err)
	}
	clock = clock.Add(time.Minute)
	if _, err := r.SearchTitleCached(context.Background(), clock, in); err != nil || a.searches != 2 {
		t.Fatalf("expired cache: %v", err)
	}
}

func TestQueuedSearchReusesEmptySearchResult(t *testing.T) {
	r, a, _, now := runner(t)
	a.releases = nil
	in := ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}
	if _, err := r.SearchTitleCached(context.Background(), now, in); err != nil {
		t.Fatal(err)
	}
	st, err := r.SearchTitleCached(context.Background(), now.Add(time.Hour), in)
	if err != nil || a.searches != 1 || len(st.Report[0].Candidates) != 0 || !st.Lookups[targetKey(target())].At.Equal(now) {
		t.Fatalf("empty result spent another search or changed timestamp: %v searches=%d", err, a.searches)
	}
}

func TestQueuedSearchCachePreservesPlexIdentity(t *testing.T) {
	for _, identity := range []PlexIdentity{{Key: "900"}, {Reason: "ambiguous Plex identity"}} {
		t.Run(identity.Key+identity.Reason, func(t *testing.T) {
			r, a, _, now := runner(t)
			r.Config.PlexKeys = nil // Arr targets have no resolved Plex identity.
			st, err := r.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			st.PlexIdentities = map[string]PlexIdentity{target().Key(): identity}
			if err = r.Store.Save(st); err != nil {
				t.Fatal(err)
			}
			in := ApprovalRequest{Kind: "sonarr", ID: 1, Season: 3}
			first, err := r.SearchTitleCached(context.Background(), now, in)
			if err != nil {
				t.Fatal(err)
			}
			cached, err := r.SearchTitleCached(context.Background(), now.Add(time.Hour), in)
			if err != nil {
				t.Fatal(err)
			}
			before, after := first.Report[0], cached.Report[0]
			if after.Target.PlexKey != identity.Key || after.Target.PlexReason != identity.Reason || after.Reason != before.Reason || (after.Release == nil) != (before.Release == nil) || a.searches != 1 {
				t.Fatalf("cached identity/eligibility changed: key=%q reason=%q eligibility=%q searches=%d", after.Target.PlexKey, after.Target.PlexReason, after.Reason, a.searches)
			}
			lookup := cached.Lookups[targetKey(target())]
			if lookup.Decision.Target.PlexKey != identity.Key || lookup.Decision.Target.PlexReason != identity.Reason || !lookup.At.Equal(now) {
				t.Fatalf("cached identity or timestamp lost: %+v", lookup)
			}
		})
	}
}
