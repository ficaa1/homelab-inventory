package archivetier

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeArr struct {
	grabbed                  Release
	failImport               bool
	targets                  []Target
	current                  Target
	queue                    []QueueItem
	releases                 []Release
	items                    []ManualItem
	grabs, imports, searches int
	failGrab                 bool
	imported                 []map[string]any
	preflights               int
	safetyErr                error
	recoveryErr              error
	lifecycleErr             error
	present                  bool
	opted                    bool
	grabbedID                string // "" means "hash", the ID the queue fixtures use
	noGrabHistory            bool
	rescans                  int
}

func (f *fakeArr) Rescan(context.Context, Target) (int, error) { f.rescans++; return 77, nil }

func (f *fakeArr) GrabbedDownload(context.Context, *Job) (string, error) {
	if f.noGrabHistory {
		return "", nil
	}
	if f.grabbedID == "" {
		return "hash", nil
	}
	return f.grabbedID, nil
}

func (f *fakeArr) Targets(context.Context) ([]Target, error)       { return f.targets, nil }
func (f *fakeArr) Current(context.Context, Target) (Target, error) { return f.current, nil }
func (f *fakeArr) Queue(context.Context) ([]QueueItem, error)      { return f.queue, nil }
func (f *fakeArr) Search(context.Context, Target) ([]Release, error) {
	f.searches++
	return f.releases, nil
}
func (f *fakeArr) Grab(_ context.Context, rel Release) error {
	f.grabbed = rel
	f.grabs++
	if f.failGrab {
		return errors.New("lost response")
	}
	return nil
}
func (f *fakeArr) Manual(context.Context, QueueItem) ([]ManualItem, error) { return f.items, nil }
func (f *fakeArr) Import(_ context.Context, files []map[string]any) (int, error) {
	f.imports++
	f.imported = files
	if f.failImport {
		return 0, errors.New("lost import response")
	}
	return 42, nil
}
func (f *fakeArr) Command(context.Context, int) (string, error)  { return "completed", nil }
func (f *fakeArr) Preflight(context.Context) error               { return nil }
func (f *fakeArr) Present(context.Context, Target) (bool, error) { return f.present, nil }
func (f *fakeArr) Opted(context.Context, Target) (bool, error)   { return f.opted, nil }

type fakePlex struct {
	active bool
	recent map[string]bool
	err    error
}

func (f *fakePlex) Active(context.Context) (bool, error) { return f.active, f.err }
func (f *fakePlex) Recent(context.Context, time.Time, int) (map[string]bool, error) {
	return f.recent, f.err
}
func runner(t *testing.T) (*Runner, *fakeArr, *fakePlex, time.Time) {
	t.Helper()
	st, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(st.Close)
	arr := &fakeArr{targets: []Target{target()}, current: target(), releases: fixture[[]Release](t, "sonarr.json"), items: fixture[[]ManualItem](t, "manual-sonarr.json"), present: true, opted: true}
	plex := &fakePlex{}
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	if err := st.Save(State{StreamQuietSince: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	r := &Runner{playbackObservedAt: now, Probe: func(_ context.Context, f File) (*MediaInfo, error) {
		if f.MediaInfo != nil {
			return f.MediaInfo, nil
		}
		return &MediaInfo{AudioChannels: 6, AudioCodec: "EAC3", VideoCodec: "HEVC", VideoDynamicRangeType: "HDR10"}, nil
	}, Config: testConfig(), Store: st, Arr: map[string]ArrAPI{"sonarr": arr, "radarr": &fakeArr{}}, Plex: plex, DState: func() (int, error) { return 0, nil }, DownloadFree: func() (int64, error) { return 10000 * GB, nil }, Now: func() time.Time { return now }}
	return r, arr, plex, now
}

// observeQuiet supplies actual playback polls for fixtures testing other guards.
func observeQuiet(t *testing.T, r *Runner, at time.Time) {
	t.Helper()
	st, err := r.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for i := 5; i >= 0; i-- {
		if _, err := r.observePlayback(context.Background(), at.Add(-time.Duration(i)*time.Minute), &st); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
}

func check(t *testing.T, r *Runner, now time.Time) State {
	t.Helper()
	s, e := r.Check(context.Background(), now)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestRunnerDryRunAndGuards(t *testing.T) {
	r, a, p, now := runner(t)
	r.DryRun = true
	s := check(t, r, now)
	if s.ProjectedSavings <= 0 || len(s.Report) != 1 || a.grabs != 0 || a.imports != 0 {
		t.Fatal("dry run failed")
	}
	if s.Report[0].Release.Guid != "" {
		t.Fatal("tracker GUID leaked into report")
	}
	p.active = true
	s = check(t, r, now)
	if len(s.Report) != 1 || s.Report[0].Reason != "Plex sessions active" || a.grabs != 0 {
		t.Fatal("active report failed")
	}
	p.active = false
	p.recent = map[string]bool{"900": true}
	s = check(t, r, now)
	if s.Report[0].Reason != "watched within idle window" {
		t.Fatal("recent play ignored")
	}
	p.recent = nil
	a.queue = []QueueItem{{SeriesID: 1}}
	s = check(t, r, now)
	if s.Report[0].Reason != "title in application queue" {
		t.Fatal("queue ignored")
	}
}
func TestRunnerSwapAndVerification(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	if s.Job == nil || s.Job.Phase != Downloading || a.grabs != 1 {
		t.Fatal("grab failed")
	}
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	observeQuiet(t, r, now.Add(time.Hour))
	s = check(t, r, now.Add(time.Hour))
	if s.Job.Phase != Verifying || a.imports != 1 {
		t.Fatalf("import failed %+v", s)
	}
	if a.imported[0]["downloadId"] != nil {
		t.Fatal("tracked import would risk cleanup")
	}
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = 2 * GB
		a.current.Files[i].Quality = qual(1080)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	observeQuiet(t, r, now.Add(2*time.Hour))
	s = check(t, r, now.Add(2*time.Hour))
	if s.Job.Phase != Complete || s.Job.ActualBytes != 4*GB {
		t.Fatal("verification failed")
	}
	want := Decision{Target: a.current, Reason: "already archived"}
	if len(s.Report) != 1 || !reflect.DeepEqual(s.Report[0], want) || s.ProjectedSavings != 0 {
		t.Fatalf("completed job left stale report: %+v, projected savings %d", s.Report, s.ProjectedSavings)
	}
	persisted, err := r.Store.Load()
	if err != nil || !reflect.DeepEqual(persisted.Report, s.Report) {
		t.Fatalf("completed report not persisted: %+v, %v", persisted.Report, err)
	}
	observeQuiet(t, r, now.Add(3*time.Hour))
	s = check(t, r, now.Add(3*time.Hour))
	if a.grabs != 1 || a.imports != 1 {
		t.Fatal("duplicate swap")
	}
	audit, e := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
	if e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"grab_intent", "grab_accepted", "library_replacement_intent", "manual_import_accepted", "verified"} {
		if !strings.Contains(string(audit), action) {
			t.Fatal("missing audit action", action)
		}
	}
}
func TestRunnerCrashTimeoutAndMissingFiles(t *testing.T) {
	for _, scenario := range []string{"ambiguous grab", "timeout", "missing file", "removed tag", "partial library change"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, _, now := runner(t)
			if scenario == "ambiguous grab" {
				a.failGrab = true
			}
			s, e := r.Check(context.Background(), now)
			if scenario == "ambiguous grab" {
				if e != nil || testStateJob(s).Phase != GrabIntent {
					t.Fatal("uncertainty lost")
				}
				_, e = r.Check(context.Background(), now)
				if e != nil || a.grabs != 1 {
					t.Fatal("ambiguous grab retried")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			switch scenario {
			case "timeout":
				now = now.Add(25 * time.Hour)
			case "missing file":
				a.queue = []QueueItem{{SeriesID: 1, Title: testStateJob(s).Release.Title, DownloadID: "hash", State: "importPending"}}
				check(t, r, now)
				a.imports = 0
				for i := range a.current.Files {
					a.current.Files[i].ID += 100
					a.current.Files[i].Size = GB
					a.current.Files[i].Quality = qual(1080)
					a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
					a.current.Episodes[i].FileID = a.current.Files[i].ID
				}
				a.present = false
			case "removed tag":
				a.opted = false
				a.queue = []QueueItem{{SeriesID: 1, Title: testStateJob(s).Release.Title, DownloadID: "hash", State: "importPending"}}
			case "partial library change":
				a.current.Files[0].Size--
			}
			s, e = r.Check(context.Background(), now)
			if scenario == "timeout" || scenario == "removed tag" {
				if e != nil || testStateJob(s).Phase != NeedsApproval || !testStateJob(s).HoldsLease() || a.imports != 0 {
					t.Fatalf("pre-import failure did not end cleanly: %+v %v", s, e)
				}
				return
			}
			if e == nil || testStateJob(s).Phase != Failed || a.imports != 0 {
				t.Fatalf("unsafe scenario not halted %+v %v", s, e)
			}
			_, _ = r.Check(context.Background(), now)
			if a.grabs != 1 {
				t.Fatal("halted job repeated")
			}
		})
	}
}
func TestRunnerWindowAndBudget(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now.Add(6*time.Hour))
	if a.grabs != 0 || s.Paused != "outside nightly window" {
		t.Fatal("daytime grab")
	}
}
func TestRunnerImportsOutsideWindow(t *testing.T) {
	r, a, p, now := runner(t)
	s := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	p.recent = map[string]bool{"900": true}
	s.Job.Target.PlexKey = ""
	r.Store.Save(s)
	r.Now = func() time.Time { return now.Add(7 * time.Hour) }
	observeQuiet(t, r, now.Add(7*time.Hour))
	s = check(t, r, now.Add(7*time.Hour))
	if a.imports != 1 || s.Job.Phase != Verifying {
		t.Fatalf("daytime import blocked: %+v", s)
	}
}

func TestImportBudgetOnNextNight(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	r.Config.TimeoutHours = 48
	now = now.Add(24 * time.Hour)
	r.Now = func() time.Time { return now }
	observeQuiet(t, r, now)
	s = check(t, r, now)
	if s.Job.Phase != Verifying {
		t.Fatal("next-night NAS writes not budgeted")
	}
}
func TestExclusiveStoreAndDState(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if other, e := OpenStore(dir); e == nil {
		other.Close()
		t.Fatal("concurrent workers allowed")
	}
	proc := t.TempDir()
	os.Mkdir(filepath.Join(proc, "123"), 0700)
	os.WriteFile(filepath.Join(proc, "123", "stat"), []byte("123 (name with ) parentheses) D 0 0 0"), 0600)
	n, e := CountDState(proc)
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
}

func TestAmbiguousImportNeverRetries(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, Title: testStateJob(s).Release.Title, DownloadID: "hash", State: "importPending"}}
	a.failImport = true
	s, e := r.Check(context.Background(), now)
	if e == nil || testStateJob(s).Phase != ImportIntent || a.imports != 1 {
		t.Fatal("import uncertainty not preserved")
	}
	s, e = r.Check(context.Background(), now)
	if e == nil || testStateJob(s).Phase != Failed || a.imports != 1 {
		t.Fatal("ambiguous import retried")
	}
}
func TestUnavailablePlexFailsHealthState(t *testing.T) {
	r, a, p, now := runner(t)
	p.err = errors.New("Tautulli unavailable")
	s, e := r.Check(context.Background(), now)
	if e == nil || s.Error == "" || a.grabs != 0 {
		t.Fatal("unknown Plex activity treated as idle")
	}
}

func (f *fakeArr) TargetPreflight(_ context.Context, t Target, _ []ManualItem, _ int64) (string, []Evidence, error) {
	f.preflights++
	return "/library/recycle", []Evidence{{Path: "/library/recycle", Device: 1, Directory: true}}, f.safetyErr
}
func (f *fakeArr) Recovery(_ context.Context, j *Job) ([]Evidence, error) {
	out := []Evidence{}
	for _, v := range j.Target.Files {
		out = append(out, Evidence{Path: "/library/recycle/" + filepath.Base(v.Path), Size: v.Size, Device: 1, Modified: 2})
	}
	return out, f.recoveryErr
}
func (f *fakeArr) Lifecycle(context.Context, *Job, Target) error {
	return f.lifecycleErr
}

func TestCompletedPartialImportFailsImmediatelyAndPersists(t *testing.T) {
	for _, scenario := range []string{"partial", "unchanged", "missing files", "recovery missing", "lifecycle unresolved"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, _, now := runner(t)
			s := check(t, r, now)
			a.queue = []QueueItem{{SeriesID: 1, Title: testStateJob(s).Release.Title, DownloadID: "hash", State: "importPending"}}
			check(t, r, now)
			a.current = replacement()
			switch scenario {
			case "partial":
				a.current.Episodes[1].FileID = 11
			case "unchanged":
				a.current = target()
			case "missing files":
				a.present = false
			case "recovery missing":
				a.recoveryErr = errors.New("missing old file ID 10")
			case "lifecycle unresolved":
				a.lifecycleErr = errors.New("no downloadId history association")
			}
			s, e := r.Check(context.Background(), now)
			if e == nil || testStateJob(s).Phase != Failed || Healthy(s) {
				t.Fatal("completed command did not halt immediately", s, e)
			}
			if scenario == "partial" && (len(testStateJob(s).MissingEpisodeIDs) == 0 || len(testStateJob(s).MissingFileIDs) == 0) {
				t.Fatal("missing IDs not persisted")
			}
			if scenario == "missing files" && (len(testStateJob(s).MissingEpisodeIDs) != 2 || len(testStateJob(s).MissingFileIDs) != 2) {
				t.Fatal("missing physical file IDs not recorded")
			}
			// Reconstruct the runner with the same persisted store and switch to read-only.
			restarted := *r
			restarted.DryRun = true
			s, e = restarted.Check(context.Background(), now.Add(time.Minute))
			if e != nil || Healthy(s) || testStateJob(s).Phase != Failed || a.imports != 1 || a.grabs != 1 {
				t.Fatal("restart hid or retried failed job", s, e)
			}
			// Even a state snapshot with its transient Error cleared remains unhealthy.
			s.Error = ""
			if Healthy(s) {
				t.Fatal("failed phase alone appeared healthy")
			}
		})
	}
}
func TestSafetyPreflightBeforeEveryWriteAndResume(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	if a.preflights != 1 || s.Job.RecycleBaseline == nil || s.Job.RecycleRoot == "" {
		t.Fatal("grab baseline/preflight missing")
	}
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	restarted := *r
	a.safetyErr = errors.New("source/device safety changed")
	s, e := restarted.Check(context.Background(), now)
	if e != nil || s.Job.Phase != Downloading || s.Paused == "" || a.preflights != 2 || a.imports != 0 {
		t.Fatal("resumed import bypassed preflight", s, e)
	}
}
func TestNoSearchAllPaths(t *testing.T) {
	r, a, _, now := runner(t)
	r.NoSearch = true
	r.DryRun = true
	s := check(t, r, now)
	if a.searches != 0 || a.grabs != 0 || len(s.Report) != 1 || s.Report[0].Reason != "release search disabled" {
		t.Fatal("sanity check searched tracker")
	}
	// Cached reports and pending jobs also cannot escape the search-disabled mode.
	r.DryRun = false
	r.NoSearch = false
	s = check(t, r, now)
	r.NoSearch = true
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	before := a.searches
	check(t, r, now)
	if a.searches != before || a.imports != 0 {
		t.Fatal("sanity check resumed pending write")
	}
}
func TestLeaseConflictsSkipTargetsAndReleaseAfterCycle(t *testing.T) {
	r, a, _, now := runner(t)
	m := movieTarget()
	release := testMovieRelease(t)
	release.Size = 7 * GB
	movie := &fakeArr{targets: []Target{m}, current: m, releases: []Release{release}, opted: true, present: true, grabbedID: "moviehash"}
	r.Arr["radarr"] = movie
	held := false
	releases := 0
	r.Acquire = func(target Target) (func(), error) {
		if target.Kind == "radarr" {
			return nil, errors.New("seeding owns movie")
		}
		if held {
			t.Fatalf("duplicate acquire for %s", target.Key())
		}
		held = true
		return func() { held = false; releases++ }, nil
	}
	s := check(t, r, now)
	if s.Job == nil || s.Job.Target.Kind != "sonarr" || movie.searches != 0 || a.grabs != 1 || held || releases != 1 {
		t.Fatal("conflict starved free target or lease leaked", s)
	}
	a.queue = []QueueItem{{SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	check(t, r, now)
	if held || releases != 2 || a.imports != 1 {
		t.Fatal("resume lease leaked")
	}
	a.current = replacement()
	a.lifecycleErr = errors.New("queue unresolved")
	if _, e := r.Check(context.Background(), now); e == nil || held || releases != 3 {
		t.Fatal("verification failure leaked lease")
	}
}
func TestSharedStoreHooks(t *testing.T) {
	r, a, _, now := runner(t)
	saved := State{}
	loads, saves, audits := 0, 0, 0
	r.Store = &Store{LoadState: func() (State, error) { loads++; return saved, nil }, SaveState: func(s State) error { saves++; saved = s; return nil }, AuditRecord: func(_ string, _ *Job) error { audits++; return nil }}
	s := check(t, r, now)
	if loads != 1 || saves < 3 || audits != 2 || s.Job.Phase != Downloading || a.grabs != 1 {
		t.Fatal("shared hooks not used", loads, saves, audits, s)
	}
	r.Store.Close() // Hook-only stores have no package-owned lock.
	r.DryRun = true
	check(t, r, now)
	if loads != 2 || a.grabs != 1 {
		t.Fatal("shared state not loaded on restart")
	}
}
func TestFreshFlagsBeforeGrab(t *testing.T) {
	r, a, _, now := runner(t)
	selected := Select(target(), a.releases, r.Config)
	a.releases[2].Flags = []byte(`["G_Freeleech",null]`)
	// Remove all other candidates so the fresh search must reject the old choice.
	a.releases = a.releases[2:3]
	s := State{}
	if e := r.start(context.Background(), now, &s, selected); e != nil {
		t.Fatal(e)
	}
	if a.searches != 1 || a.grabs != 0 || s.Job != nil {
		t.Fatal("stale freeleech selection grabbed")
	}
}

// Read-only verification of an existing job runs in dry-run and with
// admission closed; only the import submission waits for admission.
func TestExistingJobProgressNeedsNoWriteAdmission(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	a.queue = []QueueItem{{ID: 9, SeriesID: 1, Title: s.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	observeQuiet(t, r, now.Add(time.Hour))
	s, e := r.CheckAdmission(context.Background(), now.Add(time.Hour), Admission{Reason: "write admission closed"})
	if e != nil || s.Job.Phase != Downloading || a.imports != 0 || !strings.Contains(s.Paused, "write admission closed") {
		t.Fatalf("import submitted without admission: %+v %v", s, e)
	}
	observeQuiet(t, r, now.Add(time.Hour))
	s = check(t, r, now.Add(time.Hour))
	if s.Job.Phase != Verifying || a.imports != 1 {
		t.Fatal("admitted import did not run")
	}
	a.queue = nil
	a.current = replacement()
	r.DryRun = true
	s, e = r.CheckAdmission(context.Background(), now.Add(2*time.Hour), Admission{Reason: "write admission closed"})
	if e != nil || s.Job.Phase != Complete {
		t.Fatalf("dry-run or closed admission skipped read-only verification: %+v %v", s, e)
	}
}

// The dry-run used to search every eligible title on every two-minute tick,
// day and night (about 720 tracker lookups per title per day).
func TestLookupsAreCooledBudgetedAndNightlyInBothModes(t *testing.T) {
	t.Run("cooldown survives restart", func(t *testing.T) {
		r, a, _, now := runner(t)
		r.DryRun = true
		for i := 0; i < 30; i++ {
			check(t, r, now.Add(time.Duration(i)*2*time.Minute))
		}
		restarted := *r
		s := check(t, &restarted, now.Add(time.Hour))
		if a.searches != 1 || s.Report[0].Release == nil || !s.Report[0].SearchedAt.Equal(now) {
			t.Fatalf("dry-run looked up %d times: %+v", a.searches, s.Report)
		}
		check(t, r, now.Add(25*time.Hour))
		if a.searches != 2 {
			t.Fatal("expired cooldown did not refresh the lookup")
		}
	})
	t.Run("daytime", func(t *testing.T) {
		for _, dry := range []bool{true, false} {
			r, a, _, now := runner(t)
			r.DryRun = dry
			for i := 0; i < 20; i++ {
				check(t, r, now.Add(10*time.Hour+time.Duration(i)*2*time.Minute))
			}
			if a.searches != 0 {
				t.Fatalf("dry=%v: unattended lookup outside the nightly window", dry)
			}
		}
		r, _, _, now := runner(t)
		r.DryRun = true
		if s := check(t, r, now.Add(10*time.Hour)); s.Report[0].Reason != "release lookup waits for the nightly window" {
			t.Fatalf("daytime report: %+v", s.Report[0])
		}
	})
	t.Run("lookup accounting", func(t *testing.T) {
		for _, dry := range []bool{true, false} {
			r, a, _, now := runner(t)
			r.DryRun = dry
			r.Config.SearchesPerMinute = 2
			a.targets = nil
			for _, id := range []int{1, 3, 5} {
				tt := target()
				tt.ID = id
				a.targets = append(a.targets, tt)
				r.Config.PlexKeys[tt.Key()] = "9" + tt.Key()
			}
			for i := 0; i < 5; i++ {
				check(t, r, now.Add(time.Duration(i)*2*time.Minute))
			}
			want := 2
			if dry {
				want = 1
			} // stop reporting once a usable candidate is found
			if a.searches != want {
				t.Fatalf("dry=%v: %d lookups, want %d", dry, a.searches, want)
			}
			s, _ := r.Store.Load()
			if s.SearchesUsed != want {
				t.Fatalf("dry=%v: lookup budget not persisted: %+v", dry, s)
			}
		}
	})
}

// A candidate larger than the nightly GB budget used to block every later
// candidate forever.

// growingRelease answers the report lookup with a release that fits and the
// fresh pre-grab lookup with a larger one.
type growingRelease struct{ *fakeArr }

func (g growingRelease) Search(ctx context.Context, t Target) ([]Release, error) {
	rs, e := g.fakeArr.Search(ctx, t)
	if g.searches > 1 {
		bigger := rs[0]
		bigger.Size = 7 * GB
		rs = []Release{bigger}
	}
	return rs, e
}

// The same holds when only the fresh pre-grab lookup reveals the size.

// Queue identity follows the grab history download ID.
func TestQueueMatchesSelectedDownloadByGrabHistoryID(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	if s.Job == nil || s.Job.Phase != Downloading {
		t.Fatal("grab failed")
	}
	torrentName := "Some.Torrent.Name.Unlike.The.Indexer.Title.mkv"
	a.queue = []QueueItem{{SeriesID: 1, Title: torrentName, DownloadID: "hash", State: "importPending"}}
	a.noGrabHistory = true
	observeQuiet(t, r, now.Add(time.Hour))
	s = check(t, r, now.Add(time.Hour))
	if s.Job.Phase != NeedsApproval || s.Job.ApprovalReason != "Selected download unidentified" {
		t.Fatalf("missing grab history should wait, got %s %q", s.Job.Phase, s.Paused)
	}
	a.noGrabHistory = false
	if _, err := r.DecideJob(context.Background(), now.Add(time.Hour), JobID(*s.Job), true); err != nil {
		t.Fatal(err)
	}
	observeQuiet(t, r, now.Add(2*time.Hour))
	s = check(t, r, now.Add(2*time.Hour))
	if s.Job.Phase != Verifying || a.imports != 1 || s.Job.DownloadID != "hash" {
		t.Fatalf("own download under its torrent name not imported: %+v", s.Job)
	}

	r, a, _, now = runner(t)
	check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, Title: torrentName, DownloadID: "other", State: "importPending"}}
	s, e := r.Check(context.Background(), now.Add(time.Hour))
	if e != nil || s.Job.Phase != NeedsApproval || !s.Job.HoldsLease() || a.imports != 0 {
		t.Fatalf("a different download for the title must fail: %+v", s.Job)
	}
}

// Sonarr lists a season pack once per episode, every entry with the same
// download ID. That is one
// download, not several; a different ID among them still fails.
func TestSeasonPackQueueEntriesPerEpisodeAreOneDownload(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	if s.Job == nil || s.Job.Phase != Downloading {
		t.Fatal("grab failed")
	}
	pack := []QueueItem{}
	for i := 0; i < 3; i++ {
		pack = append(pack, QueueItem{ID: i + 1, SeriesID: 1, Title: "pack", DownloadID: "hash", State: "importPending"})
	}
	a.queue = pack
	observeQuiet(t, r, now.Add(time.Hour))
	s = check(t, r, now.Add(time.Hour))
	if s.Job.Phase != Verifying || a.imports != 1 {
		t.Fatalf("season pack listed per episode not imported: %+v", s.Job)
	}

	r, a, _, now = runner(t)
	check(t, r, now)
	a.queue = append(pack[:2:2], QueueItem{ID: 9, SeriesID: 1, Title: "pack", DownloadID: "other", State: "importPending"})
	s, e := r.Check(context.Background(), now.Add(time.Hour))
	if e != nil || s.Job.Phase != NeedsApproval || !s.Job.HoldsLease() || a.imports != 0 {
		t.Fatalf("a second download for the title must fail: %+v", s.Job)
	}
}

// Grabs keep the download disk above its minimum free space, the same floor
// the old storage manager used (300 GB); a lower reading pauses, and an
// unknown reading pauses too.
func TestGrabKeepsDownloadDiskAboveMinimum(t *testing.T) {
	r, a, _, now := runner(t)
	r.Config.DownloadMinFreeGB = 300
	r.DownloadFree = func() (int64, error) { return 301 * GB, nil }
	s := check(t, r, now)
	if a.grabs != 0 || s.Job != nil || s.Paused != "download disk would drop below 300 GB free" {
		t.Fatalf("grab below the download-disk minimum: grabs=%d paused=%q", a.grabs, s.Paused)
	}
	r, a, _, now = runner(t)
	r.DownloadFree = nil
	s, _ = r.Check(context.Background(), now)
	if a.grabs != 0 || s.Job != nil {
		t.Fatal("grab with unknown download-disk space")
	}
	r, a, _, now = runner(t)
	r.Config.DownloadMinFreeGB = 300
	r.DownloadFree = func() (int64, error) { return 1000 * GB, nil }
	if s = check(t, r, now); a.grabs != 1 || s.Job == nil {
		t.Fatal("grab refused with enough download-disk space")
	}
}

// With no nightly byte cap (the default) a release far larger than any old
// budget is still grabbed; the download-disk minimum is the real limit.
func TestNoNightlyByteCapByDefault(t *testing.T) {
	r, a, _, now := runner(t)
	if s := check(t, r, now); a.grabs != 1 || s.Job == nil {
		t.Fatalf("grab refused without a nightly byte cap: %q", s.Paused)
	}
}

func TestDefaultUnlimitedReplacementsStaySequential(t *testing.T) {
	r, a, _, now := runner(t)
	m := movieTarget()
	release := testMovieRelease(t)
	release.Size = 7 * GB
	movie := &fakeArr{targets: []Target{m}, current: m, releases: []Release{release}, opted: true, present: true, grabbedID: "moviehash"}
	r.Arr["radarr"] = movie
	// Old reservations must not act as a default byte or title cap.
	st, _ := r.Store.Load()
	st.Night = "2026-10-06"
	r.Store.Save(st)
	s := check(t, r, now) // radarr sorts first
	if movie.grabs != 1 {
		t.Fatal("first title not grabbed")
	}
	movie.queue = []QueueItem{{MovieID: 2, Title: s.Job.Release.Title, DownloadID: "moviehash", State: "importPending"}}
	item := ManualItem{Path: "/downloads/movie/movie.1080p.x265.DDP5.1.mkv", Size: 7 * GB, Quality: release.Quality, Languages: release.Languages}
	item.Movie.ID = 2
	movie.items = []ManualItem{item}
	if s := check(t, r, now); a.grabs != 0 || s.Job.Phase != Verifying {
		t.Fatal("second job started before first completed")
	}
	movie.current.Files[0].ID += 100
	movie.current.Files[0].Size = 7 * GB
	movie.current.Files[0].Quality = qual(1080)
	movie.current.Files[0].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
	check(t, r, now)
	s = check(t, r, now)
	if a.grabs != 1 || s.Job == nil || s.Job.Target.Kind != "sonarr" || s.Job.Phase != Downloading {
		t.Fatalf("second title did not start after completion: grabs=%d %+v", a.grabs, s)
	}
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending", Size: 4 * GB}}
	s = check(t, r, now)
	if a.imports != 1 || s.Job.Phase != Verifying {
		t.Fatalf("uncapped second import blocked: %+v", s)
	}
	if movie.grabs != 1 || a.grabs != 1 {
		t.Fatal("jobs were not sequential")
	}
}

func TestRunnerArchivedReportIgnoresCachedCandidates(t *testing.T) {
	r, a, _, now := runner(t)
	r.DryRun = true
	s := check(t, r, now)
	if len(s.Report) != 1 || s.Report[0].Release == nil || len(s.Report[0].Candidates) == 0 {
		t.Fatal("missing cached selection", s.Report)
	}
	s.CompletedFiles = map[string]string{targetKey(a.current): filesKey(a.current)}
	if err := r.Store.Save(s); err != nil {
		t.Fatal(err)
	}
	want := Decision{Target: s.Report[0].Target, Reason: "already archived"}
	s = check(t, r, now)
	if len(s.Report) != 1 || !reflect.DeepEqual(s.Report[0], want) || s.ProjectedSavings != 0 {
		t.Fatalf("archived report retained cached selection: %+v", s.Report)
	}
	if a.searches != 1 {
		t.Fatal("archived target searched again", a.searches)
	}
}

// testStateJob lets existing recovery assertions inspect either persisted slot.
func testStateJob(st State) *Job {
	if st.Job != nil {
		return st.Job
	}
	if len(st.FailedJobs) > 0 {
		return st.FailedJobs[len(st.FailedJobs)-1]
	}
	return nil
}
