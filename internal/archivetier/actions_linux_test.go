package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// actionFixture is a completed Example Movie job on a real temporary filesystem
// mapped at /data: the replacement and its carried subtitle in the library,
// the original in the recycle bin, the original subtitle staged.
type actionFixture struct {
	r                                       *Runner
	arr                                     *fakeArr
	root, original, recycled, replacement   string
	carried, originalSub, quarantine, stage string
	now                                     time.Time
	id                                      string
}

const origName = "Example Movie.2025.2160p.UHD.Blu-ray.Remux.DV.HDR.HEVC.TrueHD.Atmos.7.1-CiNEPHiLES.mkv"

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	f := &actionFixture{root: t.TempDir(), now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	movie := filepath.Join(f.root, "media", "movies", "Example Movie (2025)")
	bin := filepath.Join(f.root, ".recycle", "radarr", "Example Movie (2025)")
	f.quarantine = filepath.Join(f.root, ".archive-quarantine")
	for _, d := range []string{movie, bin, f.quarantine} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	f.original = filepath.Join(movie, origName)
	f.recycled = filepath.Join(bin, origName)
	f.replacement = filepath.Join(movie, "Example Movie (2025) Bluray-1080p.mkv")
	f.carried = filepath.Join(movie, "Example Movie (2025) Bluray-1080p.en.srt")
	f.originalSub = filepath.Join(movie, strings.TrimSuffix(origName, ".mkv")+".en.srt")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, 0o644)
	}
	write(f.recycled, "original 2160p bytes")
	write(f.replacement, "1080p")
	write(f.carried, "1\n00:00:01,000 --> 00:00:02,000\nHello\n")

	state := t.TempDir()
	f.stage = filepath.Join(state, "subtitles-test")
	os.Mkdir(f.stage, 0o700)
	write(filepath.Join(f.stage, "292-abc.srt"), "1\n00:00:01,000 --> 00:00:02,000\nHello\n")

	st, raw := statOf(t, f.recycled)
	size := st.Size()
	job := Job{Phase: Complete, Mode: "1080p", Started: f.now.Add(-30 * time.Hour), ImportedAt: f.now.Add(-26 * time.Hour), Completed: f.now.Add(-25 * time.Hour),
		RecycleRoot: "/data/.recycle/radarr", SidecarStage: f.stage, ActualBytes: 5,
		Sidecars: []Sidecar{{FileID: 292, Suffix: ".en.srt", Staged: "292-abc.srt", SHA256: checksum([]byte("1\n00:00:01,000 --> 00:00:02,000\nHello\n"))}},
		Target:   Target{Kind: "radarr", ID: 293, Title: "Example Movie (2025)", Files: []File{{ID: 292, Path: "/data/media/movies/Example Movie (2025)/" + origName, Size: size}}},
		Recovery: []Evidence{{Path: "/data/.recycle/radarr/Example Movie (2025)/" + origName, Size: size, Inode: raw.Ino, Modified: st.ModTime().UnixNano()}}}
	f.id = JobID(job)

	store, err := OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	store.Save(State{Job: &job, History: []HistoryEntry{{ID: f.id, Job: job, Status: HistoryAvailable, UndoUntil: job.ImportedAt.AddDate(0, 0, UndoDays)}}})
	c := testConfig()
	c.LibraryRoots = []string{"/data/media/movies"}
	c.RestoreMounts = []MountMapping{{AppRoot: "/data", LocalRoot: f.root}}
	c.QuarantineDir = "/data/.archive-quarantine"
	f.arr = &fakeArr{current: Target{Kind: "radarr", ID: 293, Title: "Example Movie (2025)", Files: []File{{ID: 500, Path: "/data/media/movies/Example Movie (2025)/Example Movie (2025) Bluray-1080p.mkv", Size: 5}}}}
	f.r = &Runner{Config: c, Store: store, Arr: map[string]ArrAPI{"radarr": f.arr, "sonarr": &fakeArr{}}}
	return f
}

func statOf(t *testing.T, p string) (os.FileInfo, *syscall.Stat_t) {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st, st.Sys().(*syscall.Stat_t)
}

func (f *actionFixture) entry(t *testing.T) HistoryEntry {
	t.Helper()
	st, err := f.r.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return st.History[0]
}

func auditText(t *testing.T, r *Runner) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
	return string(b)
}

func TestRestoreJobShowsThePlanThenAppliesExactlyIt(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	plan, err := f.r.RestoreJob(ctx, f.now, f.id, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Applied || plan.Hash == "" || len(plan.Restore.Restore) != 1 || len(plan.Restore.Quarantine) != 2 || !strings.HasPrefix(plan.Restore.Rescan, "would POST") {
		t.Fatalf("plan incomplete: %+v", plan.Restore)
	}
	if len(plan.Sidecars) != 1 || plan.Sidecars[0].Done || plan.Sidecars[0].Path != "/data/media/movies/Example Movie (2025)/"+strings.TrimSuffix(origName, ".mkv")+".en.srt" {
		t.Fatalf("subtitle plan wrong: %+v", plan.Sidecars)
	}
	if exists(f.original) || !exists(f.recycled) || !exists(f.replacement) || f.arr.rescans != 0 || auditText(t, f.r) != "" {
		t.Fatal("the plan changed something")
	}

	if _, err := f.r.RestoreJob(ctx, f.now, f.id, true, "stale"); err == nil || exists(f.original) {
		t.Fatal("applied without the plan's hash")
	}
	done, err := f.r.RestoreJob(ctx, f.now, f.id, true, plan.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !done.Applied || !exists(f.original) || exists(f.recycled) || exists(f.replacement) || exists(f.carried) {
		t.Fatal("files not swapped back")
	}
	if !exists(filepath.Join(f.quarantine, filepath.Base(f.replacement))) || !exists(filepath.Join(f.quarantine, filepath.Base(f.carried))) {
		t.Fatal("replacement and its subtitle not quarantined")
	}
	if b, _ := os.ReadFile(f.originalSub); !strings.Contains(string(b), "Hello") || !done.Sidecars[0].Done {
		t.Fatal("original subtitle not restored")
	}
	if f.arr.rescans != 1 {
		t.Fatalf("rescans = %d, want 1", f.arr.rescans)
	}
	if h := f.entry(t); h.Status != HistoryRestored || !h.StatusAt.Equal(f.now) {
		t.Fatalf("history not marked restored: %+v", h)
	}
	audit := auditText(t, f.r)
	if !strings.Contains(audit, `"restore_intent"`) || !strings.Contains(audit, `"restore_applied"`) {
		t.Fatalf("restore not audited: %s", audit)
	}
	if _, err := f.r.RestoreJob(ctx, f.now, f.id, false, ""); err == nil || !strings.Contains(err.Error(), "already restored") {
		t.Fatalf("a restored job can be restored again: %v", err)
	}
}

// A restored title stays out of archive until the tag goes.
func TestRestoredTitleIsNotPickedAgain(t *testing.T) {
	r, a, _, now := runner(t)
	st, _ := r.Store.Load()
	st.History = []HistoryEntry{{ID: "x", Status: HistoryRestored, StatusAt: now, Job: Job{Target: target()}}}
	r.Store.Save(st)
	s := check(t, r, now)
	if a.grabs != 0 || !strings.HasPrefix(s.Report[0].Reason, "restored by the operator") {
		t.Fatalf("restored title picked again: %+v", s.Report)
	}
}

func TestRestoreJobRefusals(t *testing.T) {
	ctx := context.Background()
	for name, setup := range map[string]func(f *actionFixture) (time.Time, bool){
		"no restore mapping": func(f *actionFixture) (time.Time, bool) { f.r.Config.RestoreMounts = nil; return f.now, false },
		"dry-run apply":      func(f *actionFixture) (time.Time, bool) { f.r.DryRun = true; return f.now, true },
		"expired":            func(f *actionFixture) (time.Time, bool) { return f.now.AddDate(0, 0, 8), false },
		"original in library": func(f *actionFixture) (time.Time, bool) {
			f.arr.current.Files[0].ID = 292
			return f.now, false
		},
		"no quarantine dir": func(f *actionFixture) (time.Time, bool) { f.r.Config.QuarantineDir = ""; return f.now, false },
		"title in progress": func(f *actionFixture) (time.Time, bool) {
			st, _ := f.r.Store.Load()
			st.Job.Phase = Downloading
			f.r.Store.Save(st)
			return f.now, false
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newActionFixture(t)
			now, apply := setup(f)
			_, err := f.r.RestoreJob(ctx, now, f.id, apply, "x")
			if err == nil {
				t.Fatal("not refused")
			}
			if exists(f.original) || !exists(f.recycled) || !exists(f.replacement) || f.arr.rescans != 0 {
				t.Fatal("refusal changed something")
			}
		})
	}
	f := newActionFixture(t)
	if _, err := f.r.RestoreJob(ctx, f.now, "radarr:1:0@nope", false, ""); err != ErrUnknownJob {
		t.Fatalf("unknown job: %v", err)
	}
}

func TestPurgeNeedsTypedTitleAndDeletesOnlyTheEvidence(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	neighbour := filepath.Join(filepath.Dir(f.recycled), "other.mkv")
	os.WriteFile(neighbour, []byte("not ours"), 0o644)
	plan, err := f.r.PurgeJob(ctx, f.now, f.id, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Confirm != "Example Movie (2025)" || len(plan.Purge) != 1 || plan.Bytes != int64(len("original 2160p bytes")) || !exists(f.recycled) {
		t.Fatalf("purge plan wrong: %+v", plan)
	}
	if _, err := f.r.PurgeJob(ctx, f.now, f.id, true, plan.Hash, "example-movie"); err == nil || !exists(f.recycled) {
		t.Fatal("purged without the exact title typed")
	}
	if _, err := f.r.PurgeJob(ctx, f.now, f.id, true, "stale", plan.Confirm); err == nil || !exists(f.recycled) {
		t.Fatal("purged without the plan's hash")
	}
	done, err := f.r.PurgeJob(ctx, f.now, f.id, true, plan.Hash, plan.Confirm)
	if err != nil {
		t.Fatal(err)
	}
	if exists(f.recycled) || !exists(neighbour) || !exists(f.replacement) || !done.Purge[0].Done {
		t.Fatal("purge deleted the wrong things")
	}
	if h := f.entry(t); h.Status != HistoryPurged || h.InRecycle == nil || *h.InRecycle {
		t.Fatalf("history not marked purged: %+v", h)
	}
	audit := auditText(t, f.r)
	if !strings.Contains(audit, `"purge_intent"`) || !strings.Contains(audit, `"purge_applied"`) {
		t.Fatal("purge not audited")
	}
	if _, err := f.r.RestoreJob(ctx, f.now, f.id, false, ""); err == nil {
		t.Fatal("a purged job offers a restore")
	}
}

// Every refusal leaves the recycle bin as it was.
func TestPurgeRefusesUnsafePaths(t *testing.T) {
	for name, setup := range map[string]func(f *actionFixture, j *Job){
		"symlink": func(f *actionFixture, j *Job) {
			os.Rename(f.recycled, f.recycled+".real")
			os.Symlink(f.recycled+".real", f.recycled)
		},
		"wrong size": func(f *actionFixture, j *Job) { os.WriteFile(f.recycled, []byte("changed and longer bytes"), 0o644) },
		"replaced file": func(f *actionFixture, j *Job) {
			// Same name, size and mtime: only the inode tells.
			st, _ := os.Stat(f.recycled)
			os.WriteFile(f.recycled+".new", []byte("original 2160p bytes"), 0o644)
			os.Chtimes(f.recycled+".new", st.ModTime(), st.ModTime())
			os.Rename(f.recycled+".new", f.recycled)
		},
		"hard link": func(f *actionFixture, j *Job) { os.Link(f.recycled, f.recycled+".restored") },
		"outside the recycle bin": func(f *actionFixture, j *Job) {
			j.Recovery[0].Path = "/data/media/movies/Example Movie (2025)/" + origName
		},
		"already gone": func(f *actionFixture, j *Job) { os.Remove(f.recycled) },
		"symlinked directory": func(f *actionFixture, j *Job) {
			dir := filepath.Dir(f.recycled)
			os.Rename(dir, dir+".real")
			os.Symlink(dir+".real", dir)
		},
		"failed job": func(f *actionFixture, j *Job) { j.Phase = Failed },
	} {
		t.Run(name, func(t *testing.T) {
			f := newActionFixture(t)
			st, _ := f.r.Store.Load()
			j := st.History[0].Job
			setup(f, &j)
			files, err := PurgeOriginals(j, f.r.Config.RestoreMounts, true)
			if err == nil {
				t.Fatal("not refused")
			}
			for _, p := range files {
				if p.Done {
					t.Fatal("refusal deleted a file")
				}
			}
			if name != "already gone" {
				if _, err := os.Lstat(f.recycled); err != nil {
					t.Fatal("the recycle original was touched")
				}
			}
		})
	}
}

func TestPurgeAllSkipsRefusedAndRequiresConfirmation(t *testing.T) {
	f := newActionFixture(t)
	st, _ := f.r.Store.Load()
	for _, status := range []string{HistoryPurged, HistoryRestored} {
		h := st.History[0]
		h.ID += string(status)
		h.Status = status
		st.History = append(st.History, h)
	}
	expired := st.History[0]
	expired.ID += "expired"
	expired.UndoUntil = f.now.Add(-time.Hour)
	st.History = append(st.History, expired)
	if err := f.r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	plan, err := f.r.PurgeAll(ctx, f.now, false, "", "")
	if err != nil || len(plan.Entries) != 1 || plan.Bytes != plan.Entries[0].Bytes || plan.Hash == "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	for _, args := range [][2]string{{"stale", "purge all"}, {plan.Hash, "Purge all"}} {
		if _, err := f.r.PurgeAll(ctx, f.now, true, args[0], args[1]); err == nil || !exists(f.recycled) {
			t.Fatal("unsafe apply")
		}
	}
	done, err := f.r.PurgeAll(ctx, f.now, true, plan.Hash, "purge all")
	if err != nil || done.Purged != 1 || !done.Applied || exists(f.recycled) {
		t.Fatalf("apply: %+v %v", done, err)
	}
}

func TestPurgeAllSkipsPlanningFailure(t *testing.T) {
	f := newActionFixture(t)
	before, err := f.r.PurgeAll(context.Background(), f.now, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := f.r.Store.Load()
	// A distinct title with no recovery evidence is absent from the preview
	// and its hash, while the valid title can still purge its original.
	broken := st.History[0]
	broken.ID = "broken"
	broken.Job.Target.ID++
	broken.Job.Target.Title = "Broken"
	broken.Job.Recovery = nil
	st.History = append([]HistoryEntry{broken}, st.History...)
	if err := f.r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	plan, err := f.r.PurgeAll(context.Background(), f.now, false, "", "")
	if err != nil || len(plan.Entries) != 1 || plan.Entries[0].ID != f.id || plan.Hash != before.Hash {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	done, err := f.r.PurgeAll(context.Background(), f.now, true, plan.Hash, "purge all")
	if err != nil || done.Purged != 1 || done.Entries[0].Error != "" || exists(f.recycled) {
		t.Fatalf("partial: %+v %v", done, err)
	}
}

func TestPurgeAllContinuesAfterApplyLeaseFailure(t *testing.T) {
	f := newActionFixture(t)
	st, _ := f.r.Store.Load()
	first := st.History[0]
	first.ID = "busy"
	first.Job.Target.ID++
	st.History = append([]HistoryEntry{first}, st.History...)
	if err := f.r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	plan, err := f.r.PurgeAll(context.Background(), f.now, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.r.Acquire = func(Target) (func(), error) {
		calls++
		// The first two calls re-plan the batch. The first execution fails.
		if calls == 3 {
			return nil, fmt.Errorf("lease held")
		}
		return func() {}, nil
	}
	done, err := f.r.PurgeAll(context.Background(), f.now, true, plan.Hash, "purge all")
	if err != nil || done.Purged != 1 || done.Entries[0].Error == "" || !done.Entries[1].Applied {
		t.Fatalf("partial: %+v %v", done, err)
	}
}

func TestPurgeAllOmitsUnavailableOriginalsAndBusyTitles(t *testing.T) {
	for _, refusal := range []string{"missing original", "busy lease"} {
		t.Run(refusal, func(t *testing.T) {
			f := newActionFixture(t)
			if refusal == "missing original" {
				if err := os.Remove(f.recycled); err != nil {
					t.Fatal(err)
				}
			} else {
				f.r.Acquire = func(Target) (func(), error) { return nil, fmt.Errorf("lease held") }
			}
			plan, err := f.r.PurgeAll(context.Background(), f.now, false, "", "")
			if err != nil || len(plan.Entries) != 0 || plan.Bytes != 0 || plan.Hash != planHash([]ActionPlan{}) {
				t.Fatalf("refused entry in preview/hash: %+v %v", plan, err)
			}
			done, err := f.r.PurgeAll(context.Background(), f.now, true, plan.Hash, "purge all")
			if err != nil || len(done.Entries) != 0 || done.Purged != 0 || auditText(t, f.r) != "" {
				t.Fatalf("empty filtered set changed on apply: %+v %v", done, err)
			}
		})
	}
}

func TestPurgeAllContinuesAfterPartialDeletion(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict root")
	}
	f := newActionFixture(t)
	st, err := f.r.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	addOriginal := func(j *Job, dir, name string, id int) string {
		t.Helper()
		local := filepath.Join(filepath.Dir(f.recycled), dir, name)
		if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(local, []byte("recycle bytes"), 0644); err != nil {
			t.Fatal(err)
		}
		info, raw := statOf(t, local)
		j.Target.Files = append(j.Target.Files, File{ID: id, Path: "/data/media/movies/Example Movie (2025)/" + name, Size: info.Size()})
		j.Recovery = append(j.Recovery, Evidence{Path: "/data/" + filepath.ToSlash(strings.TrimPrefix(local, f.root+"/")), Size: info.Size(), Inode: raw.Ino, Modified: info.ModTime().UnixNano()})
		return local
	}
	second := addOriginal(&st.History[0].Job, "read-only", "second.mkv", 293)
	next := st.History[0]
	next.ID = "next"
	next.Job.Target.ID++
	next.Job.Target.Title = "Next title"
	next.Job.Target.Files, next.Job.Recovery = nil, nil
	third := addOriginal(&next.Job, "next", "third.mkv", 294)
	st.History = append(st.History, next)
	if err := f.r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Dir(second)
	if err := os.Chmod(blocked, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(blocked, 0755) })
	plan, err := f.r.PurgeAll(context.Background(), f.now, false, "", "")
	if err != nil || len(plan.Entries) != 2 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	done, err := f.r.PurgeAll(context.Background(), f.now, true, plan.Hash, "purge all")
	if err != nil || done.Purged != 1 || !done.Entries[0].Applied || done.Entries[0].Error == "" || !done.Entries[0].Purge[0].Done || done.Entries[0].Purge[1].Done || !done.Entries[1].Applied || done.Entries[1].Error != "" {
		t.Fatalf("partial deletion did not continue: %+v %v", done, err)
	}
	if exists(f.recycled) || !exists(second) || exists(third) {
		t.Fatal("unexpected recycle files after partial deletion")
	}
	saved, err := f.r.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	h := saved.History[0]
	if h.Status != HistoryPurgeFailed || !h.StatusAt.Equal(f.now) || h.Note != done.Entries[0].Error || !strings.Contains(h.Note, "delete "+plan.Entries[0].Purge[1].Path) || saved.History[1].Status != HistoryPurged {
		t.Fatalf("partial purge history: %+v", saved.History)
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(auditText(t, f.r)), "\n") {
		var a struct {
			Action string
			Detail ActionPlan
		}
		if err := json.Unmarshal([]byte(line), &a); err != nil {
			t.Fatal(err)
		}
		if a.Action == "purge_failed" {
			found = true
			if a.Detail.ID != f.id || a.Detail.Error != h.Note || len(a.Detail.Purge) != 2 || !a.Detail.Purge[0].Done || a.Detail.Purge[1].Done {
				t.Fatalf("partial purge audit: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("missing purge_failed audit")
	}
}
