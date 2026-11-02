package mediapolicy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func TestPausePersistsAndRemainsAvailableDuringMove(t *testing.T) {
	f := seedFake(t)
	f.svc.tick.Lock()
	w := f.post("/archive/pause", OwnerControlRequest{Paused: ptrBool(true), Apply: true, RequestedBy: "operator"})
	f.svc.tick.Unlock()
	if w.Code != 200 || !f.svc.Store.Snapshot().ArchivePaused {
		t.Fatalf("pause blocked by running operation: %d %s", w.Code, w.Body)
	}
	if p, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil || p.PlanHash != "" {
		t.Fatal("pause admitted a move")
	}
	f.queueState(SeedQueueEntry{Hash: seedTestHash, PlanHash: "p"})
	if !f.svc.seedQueueStep(context.Background()) || len(f.writes) != 0 || len(f.svc.Store.Snapshot().SeedQueue) != 1 {
		t.Fatal("pause advanced Link all")
	}
	data, err := os.ReadFile(filepath.Join(f.svc.Store.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved State
	if json.Unmarshal(data, &saved) != nil || !saved.ArchivePaused {
		t.Fatal("pause not persisted")
	}
	if !f.svc.Status().ArchivePaused {
		t.Fatal("pause absent from status")
	}
}
func ptrBool(v bool) *bool { return &v }
func ptrInt(v int) *int    { return &v }

func TestQueueReorderPersistsAllQueuesAndAuditsActor(t *testing.T) {
	for _, path := range []string{"/seeding/queue/reorder", "/archive/search/reorder", "/archive/approvals/reorder"} {
		t.Run(path, func(t *testing.T) {
			f := seedFake(t)
			first := archivetier.ApprovalRequest{Kind: "sonarr", ID: 1, Season: 1}
			second := archivetier.ApprovalRequest{Kind: "radarr", ID: 2}
			f.svc.Store.UpdateSeeding(func(st *State) error {
				st.SeedQueue = []SeedQueueEntry{{Hash: seedTestHash}, {Hash: seedOtherHash}}
				st.ArchiveSearches = []ArchiveSearch{{Title: first, State: "queued"}, {Title: archivetier.ApprovalRequest{Kind: "radarr", ID: 3}, State: "searching"}, {Title: second, State: "queued"}}
				st.Archive.Approvals = []archivetier.Approval{{ApprovalRequest: first}, {ApprovalRequest: second}}
				return nil
			})
			in := OwnerControlRequest{Apply: true, Position: ptrInt(0), RequestedBy: "operator"}
			if path == "/seeding/queue/reorder" {
				in.Hash = seedOtherHash
			} else {
				in.Key = "radarr:2:0"
			}
			w := f.post(path, in)
			if w.Code != 200 {
				t.Fatalf("reorder: %d %s", w.Code, w.Body)
			}
			f.svc.Store.Close()
			reopened, err := OpenStore(f.svc.Store.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			st := reopened.Snapshot()
			switch path {
			case "/seeding/queue/reorder":
				if st.SeedQueue[0].Hash != seedOtherHash {
					t.Fatal("seed order lost")
				}
			case "/archive/search/reorder":
				if st.ArchiveSearches[0].Title != second || st.ArchiveSearches[2].Title != first {
					t.Fatal("search order lost")
				}
			case "/archive/approvals/reorder":
				if st.Archive.Approvals[0].ID != 2 {
					t.Fatal("approval order lost")
				}
			}
			audit, _ := os.ReadFile(filepath.Join(f.svc.Store.dir, "audit.jsonl"))
			if !strings.Contains(string(audit), "operator") || !strings.Contains(string(audit), "owner_control_applied") {
				t.Fatal("decision unaudited")
			}
		})
	}
}

func TestOwnerControlValidationAndAuthentication(t *testing.T) {
	f := seedFake(t)
	for _, body := range []string{`{}`, `{"paused":true,"apply":false}`, `{"paused":true,"apply":true,"position":0}`, `{"paused":true,"apply":true,"unknown":1}`, `{"paused":true,"apply":true} {}`} {
		r := httptest.NewRequest("POST", "/archive/pause", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer fixture-token")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.svc.Handler("fixture-token").ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("invalid control accepted: %s %d", body, w.Code)
		}
	}
	for _, tc := range []struct {
		auth, ct string
		code     int
	}{{"", "application/json", 401}, {"Bearer fixture-token", "text/plain", 415}} {
		r := httptest.NewRequest("POST", "/archive/pause", strings.NewReader(`{"paused":true,"apply":true}`))
		r.Header.Set("Authorization", tc.auth)
		r.Header.Set("Content-Type", tc.ct)
		w := httptest.NewRecorder()
		f.svc.Handler("fixture-token").ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatal("missing barrier", w.Code)
		}
	}
}

func TestSeedingStopAtSafePoints(t *testing.T) {
	for _, point := range []string{"queued", "library", "stop", "setDownloadPath", "setLocation", "recheck", "start"} {
		t.Run(point, func(t *testing.T) {
			f := seedFake(t)
			if point == "library" {
				f.root = "/data/downloads/Movies"
				f.library = "/data/downloads/Movies/Movie/video.mkv"
				f.put(f.library)
				f.stopLibrary = true
			}
			f.downloadPath = "/data/downloads/qbittorrent/incomplete"
			p := f.plan()
			j := &SeedJob{Plan: p, Phase: "queued", Started: time.Now().UTC()}
			f.svc.Store.SaveSeeding(j)
			if point == "queued" {
				if w := f.post("/seeding/stop", OwnerControlRequest{Hash: seedTestHash, Apply: true}); w.Code != 200 {
					t.Fatal(w.Code)
				}
			} else if point != "library" {
				f.stopAction = "torrents/" + point
			}
			f.svc.runSeed(j)
			if !j.StopRequested {
				t.Fatal("stop request lost to worker save")
			}
			switch point {
			case "queued", "library", "stop", "setDownloadPath":
				if j.Phase != "stopped" || j.Error != "Stopped by owner" || j.holds() || f.save != p.OldPath || f.state != "stalledUP" {
					t.Fatalf("original path stop unsafe: %+v", j)
				}
				if _, err := os.Stat(f.local(filepath.Join(p.OldPath, f.relative))); err != nil {
					t.Fatal("original data lost")
				}
			case "setLocation":
				if j.Phase != "halted" || j.Error != "Stopped by owner" || !j.holds() || f.state != "stoppedUP" || f.save != p.Destination || f.checks != 0 {
					t.Fatalf("unverified NAS stop unsafe: %+v", j)
				}
			case "recheck", "start":
				if j.Phase != "complete" || !j.NASVerified || j.holds() || f.save != p.Destination || f.state != "stalledUP" {
					t.Fatalf("verified NAS stop unsafe: %+v", j)
				}
				if _, err := os.Stat(f.local(filepath.Join(p.OldPath, f.relative))); !os.IsNotExist(err) {
					t.Fatal("verified cleanup unfinished")
				}
			}
		})
	}
}

func TestReorderDuringOperationSurvivesWorkerSnapshotSave(t *testing.T) {
	f := seedFake(t)
	first := archivetier.ApprovalRequest{Kind: "radarr", ID: 1}
	second := archivetier.ApprovalRequest{Kind: "radarr", ID: 2}
	f.svc.Store.UpdateSeeding(func(st *State) error {
		st.Archive.Approvals = []archivetier.Approval{{ApprovalRequest: first}, {ApprovalRequest: second}}
		st.ArchiveSearches = []ArchiveSearch{{Title: first, State: "queued"}, {Title: second, State: "queued"}}
		st.SeedQueue = []SeedQueueEntry{{Hash: seedTestHash}, {Hash: seedOtherHash}}
		return nil
	})
	old := f.svc.Store.Snapshot()
	f.svc.tick.Lock()
	defer f.svc.tick.Unlock()
	for _, path := range []string{"/archive/approvals/reorder", "/archive/search/reorder", "/seeding/queue/reorder"} {
		in := OwnerControlRequest{Key: "radarr:2:0", Position: ptrInt(0), Apply: true}
		if strings.HasPrefix(path, "/seeding/") {
			in.Key = ""
			in.Hash = seedOtherHash
		}
		if w := f.post(path, in); w.Code != 200 {
			t.Fatalf("running operation blocked reorder: %d %s", w.Code, w.Body)
		}
	}
	if err := f.svc.Store.SaveArchive(old.Archive); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Store.saveArchiveSearches(old.ArchiveSearches); err != nil {
		t.Fatal(err)
	}
	saved := f.svc.Store.Snapshot()
	if saved.Archive.Approvals[0].ID != 2 || saved.ArchiveSearches[0].Title.ID != 2 || saved.SeedQueue[0].Hash != seedOtherHash {
		t.Fatal("worker reverted owner order")
	}
}

func TestNeedsApprovalLeaseSurvivesRestartAndOwnerDropIsAudited(t *testing.T) {
	svc, store, a := supervised(t, archivetier.NeedsApproval, time.Now().Add(-30*24*time.Hour))
	st := store.Snapshot().Archive
	st.Job.CommandID = 0
	st.Job.ApprovalReason = "Import rejected"
	st.Job.Message = st.Job.ApprovalReason
	a.current = a.original
	a.queue = []archivetier.QueueItem{{MovieID: 293, DownloadID: "selected", State: "importPending"}}
	if err := store.SaveArchive(st); err != nil {
		t.Fatal(err)
	}
	dir := store.dir
	store.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if !reopened.Blocked(a.original.Key(), "seeding") || reopened.Snapshot().Archive.Job.Phase != archivetier.NeedsApproval {
		t.Fatal("owner wait lost lease on restart")
	}
	svc.Store = reopened
	runner := svc.Archive.(*archivetier.Runner)
	runner.Store.LoadState = reopened.LoadArchive
	runner.Store.SaveState = reopened.SaveArchive
	runner.Store.AuditDetailRecord = reopened.AuditDetail
	runner.Store.AuditRecord = reopened.Audit
	runner.Store.Writable = reopened.WriteAllowed
	id := archivetier.JobID(*st.Job)
	body, _ := json.Marshal(OwnerControlRequest{ID: id, Apply: true, RequestedBy: "operator"})
	req := httptest.NewRequest("POST", "/archive/job/drop", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer fixture-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	svc.Handler("fixture-token").ServeHTTP(w, req)
	if w.Code != 200 || reopened.Blocked(a.original.Key(), "seeding") || reopened.Snapshot().Archive.Job.Phase != archivetier.Abandoned || len(a.queue) != 1 {
		t.Fatalf("drop did not release safely: %d %s", w.Code, w.Body)
	}
	audit, _ := os.ReadFile(filepath.Join(dir, "audit.jsonl"))
	if !strings.Contains(string(audit), "operator") || !strings.Contains(string(audit), "import_owner_dropped") || !strings.Contains(string(audit), "Import rejected") {
		t.Fatal("drop lacks actor and reason")
	}
}
