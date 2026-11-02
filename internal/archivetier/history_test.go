package archivetier

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// completeJob runs the fake title through grab, import and verification.
func completeJob(t *testing.T) (*Runner, *fakeArr, time.Time) {
	t.Helper()
	r, a, _, now := runner(t)
	s := check(t, r, now)
	if !s.Job.PhaseStarted.Equal(now) {
		t.Fatal("download phase start was not recorded")
	}
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	r.Now = func() time.Time { return now.Add(time.Minute) }
	imported := check(t, r, now.Add(time.Minute))
	if imported.Job.Phase != Verifying || !imported.Job.PhaseStarted.Equal(now.Add(time.Minute)) {
		t.Fatal("verification phase start was not recorded")
	}
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = 2 * GB
		a.current.Files[i].Quality = qual(1080)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	a.queue = nil
	s = check(t, r, now.Add(2*time.Minute))
	if s.Job.Phase != Complete || !s.Job.PhaseStarted.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("job did not complete: %+v", s.Job)
	}
	return r, a, now.Add(2 * time.Minute)
}

var errFake = errors.New("fake")

func TestHistoryRecordsFinishedJobsOnceAndAgesThem(t *testing.T) {
	r, _, now := completeJob(t)
	present := true
	r.RecycleCheck = func(Evidence) (bool, error) { return present, nil }
	s := check(t, r, now.Add(time.Minute))
	s = check(t, r, now.Add(2*time.Minute))
	if len(s.History) != 1 || s.History[0].Status != HistoryAvailable || s.History[0].ID != JobID(*s.Job) {
		t.Fatalf("completed job not recorded exactly once: %+v", s.History)
	}
	h := s.History[0]
	if h.Job.RecycleBaseline != nil || len(h.Job.Recovery) != 2 || !h.UndoUntil.Equal(s.Job.ImportedAt.AddDate(0, 0, UndoDays)) {
		t.Fatalf("history entry lacks evidence or carries the baseline: %+v", h)
	}
	if h.InRecycle == nil || !*h.InRecycle {
		t.Fatal("recycle presence not recorded")
	}
	present = false
	s = check(t, r, now.Add(3*time.Minute))
	if *s.History[0].InRecycle {
		t.Fatal("a missing original still reads as in the recycle bin")
	}
	s = check(t, r, h.UndoUntil.Add(time.Minute))
	if s.History[0].Status != HistoryExpired {
		t.Fatalf("entry past the recycle cleanup not expired: %+v", s.History[0])
	}
}

func TestHistoryIsBoundedNewestFirst(t *testing.T) {
	r, _, now := completeJob(t)
	st, _ := r.Store.Load()
	for i := 0; i < HistoryLimit+5; i++ {
		st.History = append(st.History, HistoryEntry{ID: "old", Status: HistoryExpired})
	}
	st.History = st.History[1:] // drop the current job's entry; the cycle refiles it
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	s := check(t, r, now.Add(time.Minute))
	if len(s.History) != HistoryLimit || s.History[0].ID != JobID(*s.Job) {
		t.Fatalf("history not bounded newest first: %d, first %q", len(s.History), s.History[0].ID)
	}
}

func TestFailedJobWithoutEvidenceIsNotRestorable(t *testing.T) {
	r, a, _, now := runner(t)
	check(t, r, now)
	a.recoveryErr = errFake
	a.lifecycleErr = errFake
	a.current.Files[0].Size--
	a.queue = []QueueItem{}
	s, _ := r.Check(context.Background(), now.Add(time.Minute))
	if testStateJob(s).Phase != Failed || len(s.History) != 0 {
		t.Fatalf("failed job without evidence filed as restorable: %+v", s.History)
	}
	a.recoveryErr = nil
	st, _ := r.Store.Load()
	testStateJob(st).Recovery, _ = a.Recovery(context.Background(), testStateJob(st))
	r.Store.Save(st)
	s, _ = r.Check(context.Background(), now.Add(2*time.Minute))
	if len(s.History) != 1 || s.History[0].Job.Phase != Failed {
		t.Fatalf("failed job with evidence not restorable: %+v", s.History)
	}
}

// Check now refreshes candidates outside the window, but cannot create a grab intent.
func TestCheckNowRespectsAutomaticWindow(t *testing.T) {
	for _, outside := range []bool{true, false} {
		t.Run(map[bool]string{true: "outside", false: "inside"}[outside], func(t *testing.T) {
			r, a, _, now := runner(t)
			if outside {
				now = now.Add(10 * time.Hour)
			}
			r.Now = func() time.Time { return now }
			st, err := r.CheckAdmission(context.Background(), now, Admission{NewWrites: true, CheckNow: true})
			if err != nil {
				t.Fatal(err)
			}
			audit, err := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if outside {
				if a.grabs != 0 || st.Job != nil || strings.Contains(string(audit), "grab_intent") {
					t.Fatalf("daytime check created a grab: %+v", st)
				}
				if len(st.Report) != 1 || st.Report[0].Release == nil || len(st.Report[0].Candidates) == 0 || a.searches != 1 {
					t.Fatalf("daytime check did not refresh candidates: %+v", st.Report)
				}
				// Re-evaluate cached releases without spending another lookup.
				st, err = r.CheckAdmission(context.Background(), now.Add(time.Minute), Admission{NewWrites: true, CheckNow: true})
				if err != nil || a.searches != 1 || a.grabs != 0 || len(st.Report[0].Candidates) == 0 {
					t.Fatalf("cached check failed: searches=%d %v", a.searches, err)
				}
			} else if a.grabs != 1 || st.Job == nil || st.Job.Phase != Downloading || !strings.Contains(string(audit), "grab_intent") {
				t.Fatalf("nightly check did not grab: %+v", st)
			}
		})
	}
}

func TestCheckNowDiscoversOtherTitlesAfterUsableCandidate(t *testing.T) {
	r, a, _, night := runner(t)
	second := target()
	second.ID = 2
	r.Config.PlexKeys["sonarr:2"] = "901"
	a.targets = append(a.targets, second)
	noon := night.Add(10 * time.Hour)
	r.Now = func() time.Time { return noon }
	st, err := r.CheckAdmission(context.Background(), noon, Admission{NewWrites: true, CheckNow: true})
	if err != nil || a.searches != 2 || a.grabs != 0 || len(st.Report) != 2 || len(st.Report[1].Candidates) == 0 {
		t.Fatalf("second title hidden: searches=%d %v %+v", a.searches, err, st.Report)
	}
}

func TestCheckNowImportsAutomaticJobOutsideWindow(t *testing.T) {
	r, a, _, night := runner(t)
	check(t, r, night)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	second := target()
	second.ID = 2
	r.Config.PlexKeys["sonarr:2"] = "901"
	a.targets = append(a.targets, second)
	observeQuiet(t, r, night.Add(10*time.Hour))
	st, err := r.CheckAdmission(context.Background(), night.Add(10*time.Hour), Admission{NewWrites: true, CheckNow: true})
	if err != nil || a.imports != 1 || a.grabs != 1 || st.Job.Phase != Verifying || len(st.Report) != 2 || len(st.Report[1].Candidates) == 0 {
		t.Fatalf("daytime check advanced automatic import: %v %+v", err, st.Job)
	}
}

// Check now keeps rolling rate admission even outside the nightly window.
func TestCheckNowKeepsRollingLookupRate(t *testing.T) {
	r, a, _, night := runner(t)
	st, _ := r.Store.Load()
	st.Night = "2026-10-06"
	st.SearchNight, st.SearchesUsed = "2026-10-06", r.Config.SearchesPerMinute
	noon := night.Add(10 * time.Hour)
	r.Now = func() time.Time { return noon }
	for i := 0; i < r.Config.SearchesPerMinute; i++ {
		st.SearchTimes = append(st.SearchTimes, noon)
	}
	r.Store.Save(st)
	s, err := r.CheckAdmission(context.Background(), night.Add(10*time.Hour), Admission{NewWrites: true, CheckNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.grabs != 0 || a.searches != 0 || !strings.Contains(s.Report[0].Reason, "Search limit") {
		t.Fatalf("run now spent past the night's budgets: grabs=%d searches=%d %+v", a.grabs, a.searches, s.Report)
	}
}

func TestDownloadProgressCountsASeasonPackOnce(t *testing.T) {
	r, a, _, now := runner(t)
	check(t, r, now)
	eta := now.Add(40 * time.Minute)
	for i := 0; i < 3; i++ {
		a.queue = append(a.queue, QueueItem{ID: i + 1, SeriesID: 1, DownloadID: "hash", Size: 30 * GB, SizeLeft: 18 * GB, Status: "downloading", State: "downloading", TimeLeft: "00:40:00", EstimatedCompletion: &eta})
	}
	s := check(t, r, now.Add(time.Minute))
	p := s.Job.Progress
	if p == nil || p.BytesTotal != 30*GB || p.BytesLeft != 18*GB || p.TimeLeft != "00:40:00" || !p.ETA.Equal(eta) || p.DownloadStatus != "downloading" {
		t.Fatalf("progress wrong: %+v", p)
	}
	a.queue[0].SizeLeft = 12 * GB
	searches := a.searches
	s, _ = r.RefreshProgress(context.Background(), now.Add(time.Minute+5*time.Second))
	if s.Job.Progress.BytesLeft != 18*GB {
		t.Fatal("progress refreshed inside the minimum interval")
	}
	s, _ = r.RefreshProgress(context.Background(), now.Add(time.Minute+ProgressInterval))
	if s.Job.Progress.BytesLeft != 12*GB || a.searches != searches || a.grabs != 1 {
		t.Fatalf("refresh wrong or not read-only: %+v", s.Job.Progress)
	}
}

func TestWindowBounds(t *testing.T) {
	c := testConfig()
	c.Start, c.End = "01:00", "06:00"
	zone, _ := time.LoadLocation("UTC")
	open, start, end := c.WindowBounds(time.Date(2026, 10, 6, 12, 0, 0, 0, zone))
	if open || !start.Equal(time.Date(2026, 10, 7, 1, 0, 0, 0, zone)) || !end.Equal(time.Date(2026, 10, 7, 6, 0, 0, 0, zone)) {
		t.Fatalf("next window wrong: %v %v %v", open, start, end)
	}
	open, start, _ = c.WindowBounds(time.Date(2026, 10, 6, 2, 0, 0, 0, zone))
	if !open || !start.Equal(time.Date(2026, 10, 6, 1, 0, 0, 0, zone)) {
		t.Fatal("current window wrong")
	}
	c.Start, c.End = "23:00", "05:00"
	open, start, end = c.WindowBounds(time.Date(2026, 10, 6, 2, 0, 0, 0, zone))
	if !open || !start.Equal(time.Date(2026, 10, 5, 23, 0, 0, 0, zone)) || !end.Equal(time.Date(2026, 10, 6, 5, 0, 0, 0, zone)) {
		t.Fatalf("overnight window wrong: %v %v %v", open, start, end)
	}
}

func TestRestoreConfigValidation(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"quarantine inside the library": func(c *Config) { c.QuarantineDir = "/library/quarantine" },
		"quarantine is the library":     func(c *Config) { c.QuarantineDir = "/library" },
		"unclean quarantine":            func(c *Config) { c.QuarantineDir = "/data/../q" },
		"root mapping":                  func(c *Config) { c.RestoreMounts = []MountMapping{{AppRoot: "/", LocalRoot: "/mnt"}} },
		"relative local root":           func(c *Config) { c.RestoreMounts = []MountMapping{{AppRoot: "/data", LocalRoot: "mnt"}} },
	} {
		c := testConfig()
		edit(&c)
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	c := testConfig()
	c.RestoreMounts = []MountMapping{{AppRoot: "/library", LocalRoot: "/mnt/share"}}
	c.QuarantineDir = "/archive-quarantine"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Each slow fake search rolls past the minute limit, reproducing a long press
// without sleeping or contacting a tracker.
type slowLookupArr struct {
	*fakeArr
	clock *time.Time
}

func (a *slowLookupArr) Search(ctx context.Context, target Target) ([]Release, error) {
	*a.clock = a.clock.Add(61 * time.Second)
	return a.fakeArr.Search(ctx, target)
}

func TestCheckNowCapsSlowUncachedLookupsAcrossApps(t *testing.T) {
	r, sonarr, _, night := runner(t)
	sonarr.targets = nil
	sonarr.releases = []Release{{Title: "non-freeleech"}}
	radarr := &fakeArr{releases: sonarr.releases}
	for i := 1; i <= 10; i++ {
		title := target()
		title.ID = i
		if i <= 5 {
			sonarr.targets = append(sonarr.targets, title)
		} else {
			title.Kind = "radarr"
			radarr.targets = append(radarr.targets, title)
		}
		r.Config.PlexKeys[title.Key()] = "900"
	}
	clock := night.Add(10 * time.Hour)
	r.Now = func() time.Time { return clock }
	r.Arr["sonarr"] = &slowLookupArr{sonarr, &clock}
	r.Arr["radarr"] = &slowLookupArr{radarr, &clock}
	press := func() State {
		t.Helper()
		st, err := r.CheckAdmission(context.Background(), clock, Admission{NewWrites: true, CheckNow: true})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := press()
	if sonarr.searches+radarr.searches != 5 || len(st.Lookups) != 5 || len(st.Report) != 10 {
		t.Fatalf("slow press exceeded cap or lost deferred titles: searches=%d cache=%d report=%d", sonarr.searches+radarr.searches, len(st.Lookups), len(st.Report))
	}
	deferred := 0
	for _, d := range st.Report {
		if strings.Contains(d.Reason, "check lookup limit reached") {
			deferred++
		}
	}
	if deferred != 5 {
		t.Fatalf("deferred %d titles, want 5", deferred)
	}
	// Cache hits cost nothing; a fresh press can search the remaining titles.
	st = press()
	if sonarr.searches+radarr.searches != 10 || len(st.Lookups) != 10 {
		t.Fatalf("second press did not use cache and reset budget: %+v", st.Lookups)
	}
	press()
	if sonarr.searches+radarr.searches != 10 {
		t.Fatal("24h cache spent lookups")
	}
	// An automatic nightly cycle can still scan all ten with a rolling clock.
	clock = night.Add(48 * time.Hour)
	st = check(t, r, clock)
	if sonarr.searches+radarr.searches != 20 || len(st.Lookups) != 10 {
		t.Fatalf("nightly scan inherited per-press cap: searches=%d", sonarr.searches+radarr.searches)
	}
}
