//go:build linux

package archivetier

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sidecarSetup(t *testing.T) (Config, *Job, Target, string) {
	t.Helper()
	root := t.TempDir()
	state := t.TempDir()
	c := testConfig()
	c.Mounts = []MountMapping{{AppRoot: "/library", LocalRoot: root}}
	c.SidecarMounts = []MountMapping{{AppRoot: "/library/Example Series", LocalRoot: filepath.Join(root, "Example Series")}}
	if err := os.Mkdir(filepath.Join(root, "Example Series"), 0700); err != nil {
		t.Fatal(err)
	}
	old := target()
	j := &Job{Target: old, Started: time.Now()}
	new := target()
	for i := range new.Files {
		new.Files[i].ID += 100
		new.Files[i].Path = "/library/Example Series/new" + string(rune('1'+i)) + ".mkv"
		new.Episodes[i].FileID = new.Files[i].ID
	}
	return c, j, new, state
}
func putSidecar(t *testing.T, c Config, name, body string) string {
	t.Helper()
	p := filepath.Join(c.Mounts[0].LocalRoot, "Example Series", name)
	if err := os.WriteFile(p, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestSidecarsStageRenameAndIdempotence(t *testing.T) {
	c, j, new, state := sidecarSetup(t)
	for _, n := range []string{"ep1.sr.srt", "ep1.en.forced.srt", "ep1.sdh.ass", "ep2.en.idx", "ep2.en.sub", "ep2.vtt"} {
		putSidecar(t, c, n, n)
	}
	putSidecar(t, c, "ep10.en.srt", "unrelated")
	putSidecar(t, c, "ep1.txt", "unrelated")
	if err := StageSidecars(j, c, state); err != nil {
		t.Fatal(err)
	}
	if len(j.Sidecars) != 6 {
		t.Fatalf("inventory %+v", j.Sidecars)
	}
	// Arr has already moved originals away; carry-over must read staged copies.
	for _, n := range []string{"ep1.sr.srt", "ep1.en.forced.srt", "ep1.sdh.ass", "ep2.en.idx", "ep2.en.sub", "ep2.vtt"} {
		os.Remove(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", n))
	}
	// Reordering current files and episodes must not change the ID mapping.
	new.Files[0], new.Files[1] = new.Files[1], new.Files[0]
	if err := CarrySidecars(j, new, c); err != nil {
		t.Fatal(err)
	}
	if j.SidecarsCarried != 6 {
		t.Fatal(j.SidecarsCarried)
	}
	b, err := os.ReadFile(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.en.forced.srt"))
	if err != nil || string(b) != "ep1.en.forced.srt" {
		t.Fatal(string(b), err)
	}
	if err := CarrySidecars(j, new, c); err != nil {
		t.Fatal("identical retry", err)
	}
	if err := os.WriteFile(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt"), []byte("owner edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CarrySidecars(j, new, c); err == nil {
		t.Fatal("overwrote owner's subtitles")
	}
	b, _ = os.ReadFile(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt"))
	if string(b) != "owner edit" {
		t.Fatal("different file overwritten")
	}
}
func TestSidecarPathAndChecksumSafety(t *testing.T) {
	for _, attack := range []string{"source symlink", "parent symlink", "destination symlink", "outside root", "traversal", "checksum", "episode grouping"} {
		t.Run(attack, func(t *testing.T) {
			c, j, new, state := sidecarSetup(t)
			p := putSidecar(t, c, "ep1.sr.srt", "subtitles")
			if attack == "source symlink" {
				os.Remove(p)
				os.Symlink(filepath.Join(t.TempDir(), "outside"), p)
				if StageSidecars(j, c, state) == nil {
					t.Fatal("source symlink accepted")
				}
				return
			}
			if err := StageSidecars(j, c, state); err != nil {
				t.Fatal(err)
			}
			switch attack {
			case "parent symlink":
				os.Rename(filepath.Dir(p), filepath.Dir(p)+"-old")
				os.Symlink(filepath.Dir(p)+"-old", filepath.Dir(p))
			case "destination symlink":
				os.Symlink(p, filepath.Join(filepath.Dir(p), "new1.sr.srt"))
			case "outside root":
				new.Files[0].Path = "/other/new1.mkv"
			case "traversal":
				new.Files[0].Path = "/library/Example Series/../new1.mkv"
			case "checksum":
				os.WriteFile(filepath.Join(j.SidecarStage, j.Sidecars[0].Staged), []byte("tampered"), 0600)
			case "episode grouping":
				new.Episodes[1].FileID = new.Files[0].ID
			}
			if CarrySidecars(j, new, c) == nil {
				t.Fatal("unsafe carry-over accepted")
			}
		})
	}
}
func TestSidecarMovieMapping(t *testing.T) {
	c, j, _, state := sidecarSetup(t)
	j.Target = movieTarget()
	j.Target.Files[0].Path = "/library/Example Series/movie.mkv"
	putSidecar(t, c, "movie.en.forced.srt", "movie subtitle")
	if e := StageSidecars(j, c, state); e != nil {
		t.Fatal(e)
	}
	current := j.Target
	current.Files = []File{{ID: 200, Path: "/library/Example Series/replacement.mkv"}}
	if e := CarrySidecars(j, current, c); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "replacement.en.forced.srt"))
	if e != nil || string(b) != "movie subtitle" {
		t.Fatal(e, string(b))
	}
}
func TestVerifiedReplacementSurvivesSubtitleFailure(t *testing.T) {
	r, a, _, now := runner(t)
	s := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	s = check(t, r, now)
	a.current = target()
	for i := range a.current.Files {
		a.current.Files[i].ID += 100
		a.current.Files[i].Size = 2 * GB
		a.current.Files[i].Quality = qual(1080)
		a.current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		a.current.Episodes[i].FileID = a.current.Files[i].ID
	}
	s.Job.SidecarMessage = ""
	s.Job.Sidecars = []Sidecar{{Suffix: ".sr.srt"}}
	s.Job.SidecarStage = filepath.Join(t.TempDir(), "missing")
	r.Store.Save(s)
	s = check(t, r, now)
	if s.Job.Phase != Complete || !strings.Contains(s.Job.Summary, "subtitles not carried over") || !strings.Contains(s.Job.Summary, s.Job.SidecarStage) {
		t.Fatalf("subtitle failure blocked completion: %+v", s.Job)
	}
}
func TestSummaryIncludesSelectionRecoveryAndRuntime(t *testing.T) {
	j := Job{Target: selectionTarget("sonarr"), Release: Release{Title: "Example Series 1080p WEBRip 10Bit DDP5.1 H265-trusted", Seeders: 110}, Mode: Mode1080p, Score: &Score{Total: 89.9}, RunnerUp: &Candidate{Group: "Lootera", Score: Score{Total: 46}}, ActualBytes: 21400000000, SidecarsCarried: 10, Completed: time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC), Note: "runtime unknown; assumed 45 min"}
	s := JobSummary(j)
	for _, v := range []string{"Example Series S03 replaced: 100.5 -> 21.4 GB (79.1 GB saved)", "trusted 1080p x265 10-bit DDP5.1, 110 seeders, score 89.9", "runner-up Lootera 46.0", "Subtitles carried over: 10 (timing unverified)", "Undo until 2026-10-13", "assumed 45 min"} {
		if !strings.Contains(s, v) {
			t.Fatalf("missing %q in %s", v, s)
		}
	}
}

func TestDryRunDoesNotWriteSidecarsDuringVerification(t *testing.T) {
	r, a, _, now := runner(t)
	st := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	st = check(t, r, now)
	c, j, current, dir := sidecarSetup(t)
	putSidecar(t, c, "ep1.sr.srt", "subtitle")
	if err := StageSidecars(j, c, dir); err != nil {
		t.Fatal(err)
	}
	for i := range current.Files {
		current.Files[i].Quality = qual(1080)
		current.Files[i].Size = 2 * GB
		current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
	}
	a.current = current
	st.Job.Sidecars = j.Sidecars
	st.Job.SidecarStage = j.SidecarStage
	st.Job.SidecarMessage = ""
	r.Store.Save(st)
	r.Config.SidecarMounts = c.SidecarMounts
	r.DryRun = true
	st = check(t, r, now)
	if st.Job.Phase != Complete || !strings.Contains(st.Job.SidecarMessage, "write admission closed") {
		t.Fatal("dry-run verification", st.Job)
	}
	if _, e := os.Stat(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt")); !os.IsNotExist(e) {
		t.Fatal("dry-run wrote subtitles", e)
	}
}

// A crash between creating the temporary and renaming it must not block the
// retry on the next cycle.
func TestSidecarStaleTemporaryIsReplaced(t *testing.T) {
	d := t.TempDir()
	body := []byte("1\n00:00:01,000 --> 00:00:02,000\nhello\n")
	if err := os.WriteFile(filepath.Join(d, ".subtitle-"+checksum(body)[:20]), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := openDir(d)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := atomicSidecar(dir, "ep1.en.srt", body); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(d, "ep1.en.srt"))
	if err != nil || string(got) != string(body) {
		t.Fatal("subtitle not published", err)
	}
	if names, _ := filepath.Glob(filepath.Join(d, ".subtitle-*")); len(names) != 0 {
		t.Fatal("temporary left behind", names)
	}
}

// A supervisor admission closure at the verification tick delays the subtitle
// carry-over to a later cycle instead of dropping it.
func TestClosedAdmissionDelaysSidecarCarryOver(t *testing.T) {
	r, a, _, now := runner(t)
	st := check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	st = check(t, r, now)
	c, j, current, dir := sidecarSetup(t)
	putSidecar(t, c, "ep1.sr.srt", "subtitle")
	if err := StageSidecars(j, c, dir); err != nil {
		t.Fatal(err)
	}
	for i := range current.Files {
		current.Files[i].Quality = qual(1080)
		current.Files[i].Size = 2 * GB
		current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
	}
	a.current = current
	st.Job.Sidecars, st.Job.SidecarStage, st.Job.SidecarMessage = j.Sidecars, j.SidecarStage, ""
	r.Store.Save(st)
	r.Config.SidecarMounts = c.SidecarMounts
	st, err := r.CheckAdmission(context.Background(), now, Admission{Reason: "write admission closed"})
	if err != nil || st.Job.Phase != Verifying || !strings.Contains(st.Paused, "write admission closed") {
		t.Fatalf("closed admission: %v %q %+v", err, st.Paused, st.Job)
	}
	st = check(t, r, now.Add(2*time.Minute))
	if st.Job.Phase != Complete || st.Job.SidecarsCarried != 1 || st.Job.SidecarMessage != "" {
		t.Fatalf("subtitles not carried after admission reopened: %+v", st.Job)
	}
}

func TestAutomaticImportPreservesSubtitlesStagedBeforeGrab(t *testing.T) {
	for _, scenario := range []string{"success", "admission closed", "dry run"} {
		t.Run(scenario, func(t *testing.T) {
			r, fake, _, now := runner(t)
			c, _, current, _ := sidecarSetup(t)
			r.Config.Mounts, r.Config.SidecarMounts = c.Mounts, c.SidecarMounts
			putSidecar(t, c, "ep1.sr.srt", "Serbian episode one")
			putSidecar(t, c, "ep2.sr.srt", "Serbian episode two")
			st := check(t, r, now)
			if !st.Job.SidecarsStaged || len(st.Job.Sidecars) != 2 {
				t.Fatal("subtitles were not preserved before handing Arr the download")
			}
			// Sonarr recycles the subtitles along with the old videos. Archive
			// must use its persisted staged copies, not the old library names.
			for _, n := range []string{"ep1.sr.srt", "ep2.sr.srt"} {
				if err := os.Remove(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", n)); err != nil {
					t.Fatal(err)
				}
			}
			for i := range current.Files {
				current.Files[i].Size = 2 * GB
				current.Files[i].Quality = qual(1080)
			}
			srv := lifecycleServer(t, automaticHistory(t, current, now.Add(time.Minute)), []json.RawMessage{})
			defer srv.Close()
			fake.current = current
			r.Arr["sonarr"] = &automaticArr{fakeArr: fake, client: NewClient("sonarr", srv.URL, "fixture", false), t: t}
			if scenario == "dry run" {
				r.DryRun = true
			}
			if scenario == "admission closed" {
				var err error
				st, err = r.CheckAdmission(context.Background(), now.Add(2*time.Minute), Admission{Reason: "admission closed"})
				if err != nil || st.Job.Phase != Verifying || !strings.Contains(st.Paused, "admission closed") {
					t.Fatalf("closed admission: %+v %v", st.Job, err)
				}
				if _, err := os.Stat(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt")); !os.IsNotExist(err) {
					t.Fatal("closed admission wrote subtitles")
				}
			}
			st = check(t, r, now.Add(3*time.Minute))
			if st.Job.Phase != Complete {
				t.Fatal("automatic import did not complete", st.Job)
			}
			if scenario == "dry run" {
				if st.Job.SidecarsCarried != 0 || !strings.Contains(st.Job.SidecarMessage, "write admission closed") {
					t.Fatal("dry-run warning missing")
				}
				if _, err := os.Stat(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt")); !os.IsNotExist(err) {
					t.Fatal("dry run wrote subtitles")
				}
			} else {
				if st.Job.SidecarsCarried != 2 || st.Job.SidecarMessage != "" {
					t.Fatalf("carry-over missing: %+v", st.Job)
				}
				b, err := os.ReadFile(filepath.Join(c.Mounts[0].LocalRoot, "Example Series", "new1.sr.srt"))
				if err != nil || string(b) != "Serbian episode one" {
					t.Fatalf("wrong subtitle: %q %v", b, err)
				}
			}
			if fake.imports != 0 || fake.grabs != 1 {
				t.Fatal("automatic import was repeated")
			}
		})
	}
}

type stagingGuardPlex struct {
	*fakePlex
	stateDir   string
	afterStage func()
}

func (p *stagingGuardPlex) Active(ctx context.Context) (bool, error) {
	staged, err := filepath.Glob(filepath.Join(p.stateDir, "subtitles-*"))
	if err != nil {
		return false, err
	}
	if len(staged) > 0 {
		p.afterStage()
	}
	return p.fakePlex.Active(ctx)
}

func TestGrabRepeatsGuardsAfterSubtitleStaging(t *testing.T) {
	for _, scenario := range []string{"playback", "D-state", "window"} {
		t.Run(scenario, func(t *testing.T) {
			r, fake, plex, now := runner(t)
			c, _, _, _ := sidecarSetup(t)
			r.Config.Mounts, r.Config.SidecarMounts = c.Mounts, c.SidecarMounts
			putSidecar(t, c, "ep1.sr.srt", "saved before final guards")
			clockNow, dstate := now, 0
			r.Now = func() time.Time { return clockNow }
			r.DState = func() (int, error) { return dstate, nil }
			r.Plex = &stagingGuardPlex{fakePlex: plex, stateDir: r.Store.Dir, afterStage: func() {
				switch scenario {
				case "playback":
					plex.active = true
				case "D-state":
					dstate = r.Config.MaxDState
				case "window":
					clockNow = now.Add(6 * time.Hour)
				}
			}}
			st := check(t, r, now)
			if st.Job != nil || fake.grabs != 0 || fake.imports != 0 || st.Paused == "" {
				t.Fatalf("stale guards allowed a grab: %+v grabs=%d", st, fake.grabs)
			}
			audit, err := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if strings.Contains(string(audit), `"action":"grab_intent"`) {
				t.Fatal("unsubmitted grab left ambiguous intent")
			}
		})
	}
}

func TestAtomicSidecarRenameFallback(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EINVAL, syscall.ENOSYS, syscall.EOPNOTSUPP} {
		t.Run(errno.Error(), func(t *testing.T) {
			root := t.TempDir()
			dir, err := openDir(root)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			rename := func(*os.File, string, string) syscall.Errno { return errno }
			body := []byte("subtitle")
			for _, contents := range []string{"", "different", "subtitle"} {
				t.Run(contents, func(t *testing.T) {
					name := "movie.en.srt"
					path := filepath.Join(root, name)
					if contents != "" {
						if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
							t.Fatal(err)
						}
					}
					err := atomicSidecarWithRename(dir, name, body, rename)
					want := "subtitle"
					if contents == "different" {
						want = contents
						if err == nil || err.Error() != "subtitle destination differs or is unsafe" {
							t.Fatalf("differing destination accepted: %v", err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(path)
					if err != nil || string(got) != want {
						t.Fatalf("destination %q, error %v", got, err)
					}
					entries, err := os.ReadDir(root)
					if err != nil || len(entries) != 1 {
						t.Fatalf("temporary not cleaned up: %v, %v", entries, err)
					}
				})
			}
		})
	}
}

func TestAtomicSidecarRenameError(t *testing.T) {
	dir, err := openDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	err = atomicSidecarWithRename(dir, "movie.srt", []byte("subtitle"), func(*os.File, string, string) syscall.Errno { return syscall.EACCES })
	if err == nil || !strings.Contains(err.Error(), syscall.EACCES.Error()) {
		t.Fatalf("rename errno missing: %v", err)
	}
}
