package archivetier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func waitingJob(t *testing.T) (*Runner, *fakeArr, time.Time, State) {
	t.Helper()
	r, a, _, now := runner(t)
	check(t, r, now)
	a.queue = []QueueItem{{ID: 1, SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	a.opted = false
	observeQuiet(t, r, now.Add(time.Hour))
	st := check(t, r, now.Add(time.Hour))
	if st.Job.Phase != NeedsApproval {
		t.Fatal("fixture did not wait")
	}
	return r, a, now, st
}

func TestOwnerWaitNeverTimesOutAndDropRetainsDownload(t *testing.T) {
	r, a, now, st := waitingJob(t)
	id, reason := JobID(*st.Job), st.Job.ApprovalReason
	for _, at := range []time.Time{now.Add(25 * time.Hour), now.Add(30 * 24 * time.Hour)} {
		st = check(t, r, at)
		if st.Job.Phase != NeedsApproval || !st.Job.HoldsLease() || st.Job.ApprovalReason != reason || a.imports != 0 || a.grabs != 1 || len(st.History) != 0 {
			t.Fatal("owner wait ended or changed")
		}
	}
	st, err := r.DecideJob(context.Background(), now.Add(31*24*time.Hour), id, false)
	if err != nil || st.Job.Phase != Abandoned || st.Job.HoldsLease() || len(st.History) != 1 || st.History[0].Status != HistoryEnded || len(a.queue) != 1 || !SameFiles(st.Job.Target, a.current) || st.Job.Message != reason {
		t.Fatalf("drop lost evidence: %v", err)
	}
}

func TestOwnerForceBypassesOdditiesButKeepsTargetAndPostProof(t *testing.T) {
	for _, oddity := range []string{"rejection", "extra", "queue", "policy", "selection", "probe"} {
		t.Run(oddity, func(t *testing.T) {
			r, a, now, st := waitingJob(t)
			switch oddity {
			case "rejection":
				a.items[0].Rejections = []struct {
					Reason string `json:"reason"`
				}{{"Unrecognized file"}}
			case "extra":
				extra := a.items[0]
				extra.Path = "/downloads/unrelated.mkv"
				extra.Series.ID = 999
				extra.Episodes = nil
				a.items = append(a.items, extra)
			case "queue":
				a.queue = append(a.queue, QueueItem{SeriesID: 1, DownloadID: "someone-else", State: "downloading", SizeLeft: GB})
			case "policy":
				st.Job.PolicyVersion, st.Job.Mode = "old", "invalid"
				r.Store.Save(st)
			case "selection":
				st.Job.Selection.MinScore = -1
				r.Store.Save(st)
			}
			r.Probe = func(context.Context, File) (*MediaInfo, error) {
				t.Fatal("owner import probed")
				return nil, errors.New("probe unavailable")
			}
			if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
				t.Fatal(err)
			}
			observeQuiet(t, r, now.Add(25*time.Hour))
			st = check(t, r, now.Add(25*time.Hour))
			if a.imports != 1 || st.Job.Phase != Verifying || !st.Job.HasImportIntent() || len(a.imported) != len(st.Job.Target.Files) {
				t.Fatal("owner import blocked", st.Paused)
			}
			for _, f := range a.imported {
				if f["downloadId"] != nil || strings.Contains(f["path"].(string), "unrelated") {
					t.Fatal("imported unrelated or tracked source")
				}
			}
			a.current = replacement()
			a.present = false
			_, err := r.Check(context.Background(), now.Add(26*time.Hour))
			saved, _ := r.Store.Load()
			if err == nil || len(saved.FailedJobs) != 1 || len(saved.FailedJobs[0].Recovery) == 0 || !saved.FailedJobs[0].HoldsLease() {
				t.Fatal("owner bypassed post-import recovery proof")
			}
		})
	}
}

type changedAfterPresence struct{ *fakeArr }

func (a changedAfterPresence) Present(ctx context.Context, t Target) (bool, error) {
	a.current.Files[0].ID += 100
	return true, nil
}

func TestOwnerCannotOverrideFinalUnchangedLibraryProof(t *testing.T) {
	for _, scenario := range []string{"changed before", "changed during proof", "unreadable"} {
		t.Run(scenario, func(t *testing.T) {
			r, a, now, st := waitingJob(t)
			if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "changed before":
				a.current.Files[0].ID += 100
			case "changed during proof":
				r.Arr["sonarr"] = changedAfterPresence{a}
			case "unreadable":
				a.present = false
			}
			_, _ = r.Check(context.Background(), now.Add(time.Hour))
			if a.imports != 0 {
				t.Fatal("changed or unreadable originals overwritten")
			}
		})
	}
}

func TestDropRefusesChangedLibraryAndUnknownJob(t *testing.T) {
	r, a, now, st := waitingJob(t)
	if _, err := r.DecideJob(context.Background(), now, "wrong", false); !errors.Is(err, ErrUnknownJob) {
		t.Fatal("unknown job accepted")
	}
	a.current.Files[0].ID += 100
	if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), false); err == nil {
		t.Fatal("changed library dropped")
	}
	saved, _ := r.Store.Load()
	if !saved.Job.HoldsLease() || len(saved.History) != 0 {
		t.Fatal("failed drop released job")
	}
}

func TestOwnerPauseBlocksSearchGrabAndImport(t *testing.T) {
	r, a, _, now := runner(t)
	paused := true
	r.OwnerPaused = func() bool { return paused }
	check(t, r, now)
	if a.searches+a.grabs+a.imports != 0 {
		t.Fatal("paused new work")
	}
	paused = false
	check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	paused = true
	observeQuiet(t, r, now.Add(time.Hour))
	st := check(t, r, now.Add(time.Hour))
	if st.Job.Phase != Downloading || a.imports != 0 {
		t.Fatal("paused import")
	}
	paused = false
	observeQuiet(t, r, now.Add(time.Hour))
	st = check(t, r, now.Add(time.Hour))
	if st.Job.Phase != Verifying || a.imports != 1 {
		t.Fatal("resume did not import")
	}
}

type pendingOwnerCommand struct{ *fakeArr }

func (a pendingOwnerCommand) Command(context.Context, int) (string, error) { return "started", nil }
func TestOwnerApprovalGetsNewVerificationDeadline(t *testing.T) {
	r, a, now, st := waitingJob(t)
	r.Arr["sonarr"] = pendingOwnerCommand{a}
	if _, err := r.DecideJob(context.Background(), now.Add(30*24*time.Hour), JobID(*st.Job), true); err != nil {
		t.Fatal(err)
	}
	observeQuiet(t, r, now.Add(30*24*time.Hour))
	st = check(t, r, now.Add(30*24*time.Hour))
	if st.Job.Phase != Verifying {
		t.Fatal("owner import not submitted")
	}
	observeQuiet(t, r, now.Add(30*24*time.Hour+time.Minute))
	st = check(t, r, now.Add(30*24*time.Hour+time.Minute))
	if st.Job.Phase != Verifying || !st.Job.HoldsLease() {
		t.Fatal("approval wait consumed verification deadline")
	}
}
func TestReadyStreamWaitDoesNotConsumeJobTimeout(t *testing.T) {
	r, a, p, now := runner(t)
	check(t, r, now)
	a.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	p.active = true
	check(t, r, now.Add(time.Hour))
	st := check(t, r, now.Add(72*time.Hour))
	if st.Job.Phase != Downloading || a.imports != 0 {
		t.Fatal("stream wait expired")
	}
	p.active = false
	check(t, r, now.Add(73*time.Hour))
	observeQuiet(t, r, now.Add(73*time.Hour+5*time.Minute))
	st = check(t, r, now.Add(73*time.Hour+5*time.Minute))
	if st.Job.Phase != Verifying || a.imports != 1 {
		t.Fatal("stream wait did not import after quiet period", st.Paused)
	}
}

func TestOwnerMovieImportSkipsUnrelatedExtras(t *testing.T) {
	r, a, now, st := waitingJob(t)
	m := movieTarget()
	st.Job.Target = m
	r.Arr["radarr"] = a
	a.current = m
	chosen := a.items[0]
	chosen.Movie.ID = m.ID
	chosen.Path = "/downloads/movie/main.mkv"
	chosen.Size = GB
	extra := chosen
	extra.Movie.ID = 999
	extra.Path = "/downloads/movie/extra.mkv"
	a.items = []ManualItem{chosen, extra}
	a.queue = []QueueItem{{MovieID: m.ID, DownloadID: "hash", State: "importPending"}}
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
		t.Fatal(err)
	}
	observeQuiet(t, r, now.Add(time.Hour))
	check(t, r, now.Add(time.Hour))
	if a.imports != 1 || len(a.imported) != 1 || a.imported[0]["path"] != chosen.Path || a.imported[0]["movieId"] != m.ID {
		t.Fatal("movie extras imported")
	}
}

func TestOwnerApprovalReconcilesAutomaticImportWithoutManualListing(t *testing.T) {
	r, a, now, st := waitingJob(t)
	if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
		t.Fatal(err)
	}
	a.current = replacement()
	a.queue = nil
	a.items = nil
	r.OwnerPaused = func() bool { return true }
	observeQuiet(t, r, now.Add(time.Hour))
	st = check(t, r, now.Add(time.Hour))
	if st.Job.Phase != Complete || a.imports != 0 || len(st.Job.Recovery) == 0 {
		t.Fatal("owner approval lost independent import proof", st.Paused)
	}
}

func TestOwnerOverridesMissingPackCoverageButPostProofStillHalts(t *testing.T) {
	r, a, now, st := waitingJob(t)
	a.items = a.items[:len(a.items)-1]
	if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
		t.Fatal(err)
	}
	observeQuiet(t, r, now.Add(time.Hour))
	st = check(t, r, now.Add(time.Hour))
	if a.imports != 1 || len(a.imported) != len(a.items) || st.Job.Phase != Verifying {
		t.Fatal("owner pack override blocked", st.Paused)
	}
	a.current = replacement()
	a.current.Episodes = a.current.Episodes[:len(a.current.Episodes)-1]
	if _, err := r.Check(context.Background(), now.Add(2*time.Hour)); err == nil {
		t.Fatal("incomplete replacement passed post-import proof")
	}
	saved, _ := r.Store.Load()
	if len(saved.FailedJobs) != 1 || !saved.FailedJobs[0].HoldsLease() {
		t.Fatal("partial replacement lost recovery lease")
	}
}

// Simulate Arr starting its import while the owner path prepares the listing.
type importingDuringOwnerListing struct{ *fakeArr }

func (a importingDuringOwnerListing) Manual(ctx context.Context, q QueueItem) ([]ManualItem, error) {
	a.queue[0].State = "importing"
	return a.fakeArr.Manual(ctx, q)
}

func TestOwnerImportWaitsForApplicationImport(t *testing.T) {
	for _, duringListing := range []bool{false, true} {
		t.Run(fmt.Sprint(duringListing), func(t *testing.T) {
			r, a, now, st := waitingJob(t)
			if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
				t.Fatal(err)
			}
			if duringListing {
				r.Arr["sonarr"] = importingDuringOwnerListing{a}
			} else {
				a.queue[0].State = "importing"
			}
			observeQuiet(t, r, now.Add(time.Hour))
			st = check(t, r, now.Add(time.Hour))
			if a.imports != 0 || st.Job.HasImportIntent() || !strings.Contains(st.Paused, "application import") {
				t.Fatalf("duplicate import: imports=%d paused=%q", a.imports, st.Paused)
			}
			a.current, a.queue = replacement(), nil
			observeQuiet(t, r, now.Add(time.Hour+time.Minute))
			st = check(t, r, now.Add(time.Hour+time.Minute))
			if a.imports != 0 || st.Job.Phase != Complete || len(st.Job.Recovery) == 0 {
				t.Fatal("automatic import was not reconciled")
			}
		})
	}
}

func TestWaitingApprovalCollectsRecoveryAndTimesOutAfterLibraryChange(t *testing.T) {
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprint(approved), func(t *testing.T) {
			r, a, now, st := waitingJob(t)
			if approved {
				if _, err := r.DecideJob(context.Background(), now, JobID(*st.Job), true); err != nil {
					t.Fatal(err)
				}
			}
			a.current = replacement()
			a.queue[0].State = "importing"
			changed := now.Add(2 * time.Hour)
			st = check(t, r, changed)
			if !st.Job.ImportedAt.Equal(changed) || len(st.Job.Recovery) != len(st.Job.Target.Files) {
				t.Fatal("changed library waited without recovery evidence")
			}
			saved, err := r.Store.Load()
			if err != nil || len(saved.Job.Recovery) == 0 {
				t.Fatal("recovery evidence was not persisted")
			}
			// Playback cannot extend the evidence deadline after originals changed.
			r.Plex.(*fakePlex).active = true
			st, err = r.Check(context.Background(), changed.Add(25*time.Hour))
			if err == nil || len(st.FailedJobs) != 1 || len(st.FailedJobs[0].Recovery) == 0 || !st.FailedJobs[0].HoldsLease() || Healthy(st) || a.imports != 0 {
				t.Fatalf("unproven replacement did not halt: %v %+v", err, st)
			}
		})
	}
}
