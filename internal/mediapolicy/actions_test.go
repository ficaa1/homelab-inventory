package mediapolicy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func call(t *testing.T, svc *Service, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	svc.Handler("secret").ServeHTTP(w, r)
	return w
}

func TestActionEndpointsNeedTheToken(t *testing.T) {
	svc, _, _ := supervised(t, "", time.Time{})
	for _, p := range []string{"/archive/run", "/archive/restore", "/archive/purge", "/archive/purge-all", "/archive/approve", "/archive/clear"} {
		for _, token := range []string{"", "wrong"} {
			if w := call(t, svc, "POST", p, token, `{"id":"x","apply":true}`); w.Code != http.StatusUnauthorized {
				t.Errorf("POST %s with token %q: %d, want 401", p, token, w.Code)
			}
		}
		if w := call(t, svc, "GET", p, "secret", ""); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d, want 405", p, w.Code)
		}
	}
	if w := call(t, svc, "DELETE", "/status", "secret", ""); w.Code != http.StatusMethodNotAllowed {
		t.Error("DELETE accepted")
	}
	if w := call(t, svc, "POST", "/archive/nope", "secret", "{}"); w.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d", w.Code)
	}
	if w := call(t, svc, "POST", "/archive/restore", "secret", `{"id":"x","rm":"-rf"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown request field accepted: %d", w.Code)
	}
}

// Check now refreshes discovery: at noon a normal cycle makes no lookup,
// the requested one does, and every request and outcome is audited.
func TestRunNowEndpoint(t *testing.T) {
	svc, s, fake := supervised(t, "", time.Time{})
	noon := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	svc.Tick(context.Background(), noon)
	if fake.searches != 0 {
		t.Fatal("daytime cycle searched")
	}
	if err := svc.RunNowTick(context.Background(), noon); err != nil || fake.searches != 0 {
		t.Fatal("run-now tick without a request ran")
	}
	svc.DryRun = true
	w := call(t, svc, "POST", "/archive/run", "secret", `{"requestedBy":"operator@example.org"}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "dry run") {
		t.Fatalf("run now: %d %s", w.Code, w.Body)
	}
	select {
	case <-svc.Wake():
	default:
		t.Fatal("daemon loop not woken")
	}
	if err := svc.RunNowTick(context.Background(), noon); err != nil {
		t.Fatal(err)
	}
	if fake.searches == 0 {
		t.Fatal("run now still waited for the window")
	}
	var view Status
	json.Unmarshal(call(t, svc, "GET", "/status", "secret", "").Body.Bytes(), &view)
	if view.ArchiveInfo == nil || view.ArchiveInfo.RunNow.Pending || view.ArchiveInfo.RunNow.FinishedAt.IsZero() || view.ArchiveInfo.RunNow.Result == "" {
		t.Fatalf("run now outcome not reported: %+v", view.ArchiveInfo)
	}
	open, _, _ := svc.Config.Archive.WindowBounds(time.Now().UTC())
	if view.ArchiveInfo.WindowOpen != open || view.ArchiveInfo.SearchesPerMinute != 5 {
		t.Fatalf("archive info wrong: %+v", view.ArchiveInfo)
	}
	audit, _ := os.ReadFile(filepath.Join(s.dir, "audit.jsonl"))
	if !strings.Contains(string(audit), `"run_now_requested"`) || !strings.Contains(string(audit), "operator@example.org") || !strings.Contains(string(audit), `"run_now_finished"`) {
		t.Fatalf("run now not audited: %s", audit)
	}
}

// purgeFixture files a completed job whose original sits in a real
// temporary recycle bin mapped at /data.
func purgeFixture(t *testing.T, svc *Service, s *Store) (string, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".recycle", "radarr", "Example Movie (2025)")
	os.MkdirAll(dir, 0o755)
	file := filepath.Join(dir, "Example Movie.2025.2160p.mkv")
	os.WriteFile(file, []byte("original"), 0o644)
	st, _ := os.Stat(file)
	now := time.Now().UTC()
	job := archivetier.Job{Phase: archivetier.Complete, Started: now.Add(-time.Hour), ImportedAt: now.Add(-time.Hour), RecycleRoot: "/data/.recycle/radarr",
		Target:   archivetier.Target{Kind: "radarr", ID: 293, Title: "Example Movie (2025)", Files: []archivetier.File{{ID: 292, Path: "/data/media/movies/Example Movie (2025)/Example Movie.2025.2160p.mkv", Size: 8}}},
		Recovery: []archivetier.Evidence{{Path: "/data/.recycle/radarr/Example Movie (2025)/Example Movie.2025.2160p.mkv", Size: 8, Inode: st.Sys().(*syscall.Stat_t).Ino, Modified: st.ModTime().UnixNano()}}}
	id := archivetier.JobID(job)
	if err := s.SaveArchive(archivetier.State{History: []archivetier.HistoryEntry{{ID: id, Job: job, Status: archivetier.HistoryAvailable, UndoUntil: now.AddDate(0, 0, 7)}}}); err != nil {
		t.Fatal(err)
	}
	r := svc.Archive.(*archivetier.Runner)
	r.Config.RestoreMounts = []archivetier.MountMapping{{AppRoot: "/data", LocalRoot: root}}
	r.Store.AuditDetailRecord = s.AuditDetail
	return id, file
}

func TestPurgeEndpointPlansThenNeedsTypedConfirmation(t *testing.T) {
	svc, s, _ := supervised(t, "", time.Time{})
	id, file := purgeFixture(t, svc, s)
	w := call(t, svc, "POST", "/archive/purge", "secret", `{"id":"`+id+`"}`)
	var plan archivetier.ActionPlan
	json.Unmarshal(w.Body.Bytes(), &plan)
	if w.Code != 200 || plan.Applied || plan.Confirm != "Example Movie (2025)" || plan.Hash == "" || len(plan.Purge) != 1 {
		t.Fatalf("plan: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("plan deleted the original")
	}
	body := func(confirm, hash string) string {
		b, _ := json.Marshal(map[string]any{"id": id, "apply": true, "hash": hash, "confirm": confirm, "requestedBy": "operator"})
		return string(b)
	}
	if w := call(t, svc, "POST", "/archive/purge", "secret", body("Example Movie", plan.Hash)); w.Code != http.StatusConflict {
		t.Fatalf("wrong confirmation: %d", w.Code)
	}
	if w := call(t, svc, "POST", "/archive/purge", "secret", body(plan.Confirm, "0000")); w.Code != http.StatusConflict {
		t.Fatalf("stale plan: %d", w.Code)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("refused purge deleted the original")
	}
	if w := call(t, svc, "POST", "/archive/purge", "secret", body(plan.Confirm, plan.Hash)); w.Code != 200 {
		t.Fatalf("purge: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatal("original still there")
	}
	if h := s.Snapshot().Archive.History[0]; h.Status != archivetier.HistoryPurged {
		t.Fatalf("history: %+v", h)
	}
	if svc.Status().Archive.History[0].Status != archivetier.HistoryPurged {
		t.Fatal("served status not refreshed after the action")
	}
	audit, _ := os.ReadFile(filepath.Join(s.dir, "audit.jsonl"))
	for _, a := range []string{`"purge_requested"`, `"purge_intent"`, `"purge_applied"`} {
		if !strings.Contains(string(audit), a) {
			t.Fatalf("missing %s in audit", a)
		}
	}
}

func TestActionRefusalsAndBusy(t *testing.T) {
	svc, s, _ := supervised(t, "", time.Time{})
	id, file := purgeFixture(t, svc, s)
	if w := call(t, svc, "POST", "/archive/restore", "secret", `{"id":"radarr:1:0@x"}`); w.Code != http.StatusNotFound {
		t.Fatalf("unknown job: %d", w.Code)
	}
	if w := call(t, svc, "POST", "/archive/restore", "secret", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing id: %d", w.Code)
	}
	svc.Archive.(*archivetier.Runner).DryRun = true
	if w := call(t, svc, "POST", "/archive/purge", "secret", `{"id":"`+id+`","apply":true,"confirm":"Example Movie (2025)"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "dry-run") {
		t.Fatalf("dry-run apply: %d %s", w.Code, w.Body)
	}
	svc.tick.Lock()
	svc.ActionWait = 200 * time.Millisecond
	if w := call(t, svc, "POST", "/archive/purge", "secret", `{"id":"`+id+`"}`); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("action ran beside a cycle: %d", w.Code)
	}
	svc.tick.Unlock()
	svc.Config.ArchiveEnabled = false
	if w := call(t, svc, "POST", "/archive/run", "secret", `{}`); w.Code != http.StatusConflict {
		t.Fatalf("run now with archive disabled: %d", w.Code)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("a refusal deleted the original")
	}
}

// The dashboard's refresh reads the queue (GET) at most every 15 seconds.
func TestStatusProgressRefreshIsThrottled(t *testing.T) {
	now := time.Now().UTC()
	svc, s, fake := supervised(t, archivetier.Downloading, now.Add(-time.Hour))
	fake.queue = []archivetier.QueueItem{{ID: 1, MovieID: 293, DownloadID: "selected", Size: 20 * archivetier.GB, SizeLeft: 12 * archivetier.GB, Status: "downloading"}}
	call(t, svc, "GET", "/status?refresh=progress", "secret", "")
	reads := fake.queueReads
	if p := s.Snapshot().Archive.Job.Progress; reads != 1 || p == nil || p.BytesLeft != 12*archivetier.GB {
		t.Fatalf("progress not refreshed: reads=%d %+v", reads, p)
	}
	call(t, svc, "GET", "/status?refresh=progress", "secret", "")
	call(t, svc, "GET", "/status", "secret", "")
	if fake.queueReads != reads || fake.searches+fake.grabs+fake.imports != 0 {
		t.Fatal("refresh not throttled or not read-only")
	}
	var view Status
	json.Unmarshal(call(t, svc, "GET", "/status", "secret", "").Body.Bytes(), &view)
	if view.Archive.Job.Progress == nil || view.Archive.Job.Progress.BytesTotal != 20*archivetier.GB {
		t.Fatal("served status lacks progress")
	}
}

func TestArchiveInfoReportsRollingRateAndUncappedNightlyCounts(t *testing.T) {
	svc, _, _ := supervised(t, "", time.Time{})
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	_, night := svc.Config.Archive.Window(now)
	st := archivetier.State{Night: night, SearchNight: night, SearchesUsed: 12,
		SearchTimes: []time.Time{now.Add(-time.Minute), now.Add(-59 * time.Second), now}}
	info := svc.archiveInfo(now, st)
	if info.SearchesLastMinute != 2 || info.SearchesPerMinute != 5 || info.SearchesUsed != 12 {
		t.Fatalf("archive rate/counts wrong: %+v", info)
	}
	// Old JSON fields stay present for clients during a rolling dashboard deploy.
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	json.Unmarshal(data, &fields)
	for _, key := range []string{"searchesPerMinute", "searchesLastMinute", "searchesUsed"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing status field %s", key)
		}
	}
}

// Supply a real below-threshold lookup through the normal discovery cycle,
// then approve it through the authenticated endpoint and verify the daemon wake.
type approvalArr struct {
	*arr
	release archivetier.Release
}

func (a *approvalArr) Search(ctx context.Context, _ archivetier.Target) ([]archivetier.Release, error) {
	a.searches++
	return []archivetier.Release{a.release}, ctx.Err()
}
func TestApprovalEndpointQueuesWakesAndClears(t *testing.T) {
	svc, store, fake := supervised(t, "", time.Time{})
	runner := svc.Archive.(*archivetier.Runner)
	runner.Store.AuditDetailRecord = store.AuditDetail
	runner.Store.Writable = store.WriteAllowed
	release := archivetier.Release{Title: "Example Movie.1080p.BluRay.DD5.1.x264-TayTO", Guid: "fixture-guid", IndexerID: 11, Size: 17 * archivetier.GB, Seeders: 58, Protocol: "torrent", Flags: json.RawMessage(`1`), Languages: []archivetier.Language{{Name: "English"}}}
	release.Quality.Quality.Name = "Bluray-1080p"
	release.Quality.Quality.Resolution = 1080
	runner.Arr["radarr"] = &approvalArr{arr: fake, release: release}
	runner.DryRun = true
	now := time.Now().UTC()
	st, err := runner.CheckAdmission(context.Background(), now, archivetier.Admission{NewWrites: true, CheckNow: true})
	if err != nil || len(st.Report) == 0 || len(st.Report[0].Candidates) != 1 {
		t.Fatalf("lookup: %v", err)
	}
	runner.DryRun = false
	c := st.Report[0].Candidates[0]
	in := archivetier.ApprovalRequest{Kind: "radarr", ID: 293, ReleaseID: c.ID, IndexerID: c.IndexerID}
	body := func() string {
		b, _ := json.Marshal(map[string]any{"approval": in, "requestedBy": "owner"})
		return string(b)
	}
	w := call(t, svc, "POST", "/archive/approve", "secret", body())
	if w.Code != http.StatusAccepted || len(store.Snapshot().Archive.Approvals) == 0 || fake.grabs != 0 {
		t.Fatalf("approval: %d %s", w.Code, w.Body)
	}
	select {
	case <-svc.Wake():
	default:
		t.Fatal("approval did not wake worker")
	}
	svc.publish()
	raw := call(t, svc, "GET", "/status", "secret", "").Body.String()
	if strings.Contains(raw, "fixture-guid") || !strings.Contains(raw, c.ID) {
		t.Fatal("private tracker identity leaked or candidate missing")
	}
	in.Clear = true
	w = call(t, svc, "POST", "/archive/approve", "secret", body())
	if w.Code != http.StatusOK || len(store.Snapshot().Archive.Approvals) != 0 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	searches := fake.searches
	if err := svc.RunNowTick(context.Background(), now); err != nil || fake.searches != searches {
		t.Fatal("cleared approval left a run queued")
	}
}

// The applied endpoint must refresh an auto-eligible report without a grab intent at noon.
func TestCheckNowEndpointDoesNotAutograbOutsideWindow(t *testing.T) {
	svc, store, fake := supervised(t, "", time.Time{})
	r := svc.Archive.(*archivetier.Runner)
	release := archivetier.Release{Title: "Example Movie.1080p.BluRay.DD5.1.x265-TayTO", Guid: "fixture", IndexerID: 11, Size: 6 * archivetier.GB, Seeders: 58, Protocol: "torrent", Flags: json.RawMessage(`1`), Languages: []archivetier.Language{{Name: "English"}}}
	release.Quality.Quality.Name = "Bluray-1080p"
	release.Quality.Quality.Resolution = 1080
	r.Arr["radarr"] = &approvalArr{arr: fake, release: release}
	noon := time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { return noon }
	r.DownloadFree = func() (int64, error) { return 10000 * archivetier.GB, nil }
	w := call(t, svc, "POST", "/archive/run", "secret", `{}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "automatic downloads only inside") {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	if err := svc.RunNowTick(context.Background(), noon); err != nil {
		t.Fatal(err)
	}
	st := store.Snapshot().Archive
	audit, err := os.ReadFile(filepath.Join(store.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if fake.grabs != 0 || st.Job != nil || strings.Contains(string(audit), "grab_intent") || len(st.Report) != 1 || st.Report[0].Release == nil {
		t.Fatalf("applied daytime check: grabs=%d %+v", fake.grabs, st.Report)
	}
}

func TestClearEndpointPersistsLeaseAndErrorRecovery(t *testing.T) {
	for _, scenario := range []string{"allowed", "library change", "import", "queue", "changed job", "no confirmation", "active lease"} {
		t.Run(scenario, func(t *testing.T) {
			svc, s, fake := supervised(t, "", time.Time{})
			now := time.Now().UTC()
			j := &archivetier.Job{Target: fake.original, Phase: archivetier.Failed, Started: now.Add(-time.Hour), Message: "grab failure", Release: archivetier.Release{Title: "fixture release", IndexerID: 11}}
			if scenario != "library change" {
				fake.current = fake.original
			}
			if scenario == "import" {
				j.ImportedAt = now
			}
			if scenario == "queue" {
				fake.queue = []archivetier.QueueItem{{MovieID: j.Target.ID, DownloadID: "other"}}
			}
			if err := s.SaveArchive(archivetier.State{Job: j, Error: "archive halted"}); err != nil {
				t.Fatal(err)
			}
			if scenario == "active lease" {
				release, err := s.Acquire(j.Target.Key(), "archive")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			s.Finish(now, errors.New("archive halted"))
			svc.publish()
			if _, ok := s.Snapshot().Leases[j.Target.Key()]; !ok {
				t.Fatal("failed lease missing")
			}
			id := archivetier.JobID(*j)
			if scenario == "changed job" {
				id = "old"
			}
			apply := scenario != "no confirmation"
			body, _ := json.Marshal(map[string]any{"id": id, "apply": apply, "requestedBy": "operator@example.org"})
			w := call(t, svc, "POST", "/archive/clear", "secret", string(body))
			if scenario == "allowed" {
				if w.Code != http.StatusOK {
					t.Fatalf("clear: %d %s", w.Code, w.Body)
				}
				st := s.Snapshot()
				if st.Archive.Job != nil || st.Archive.Error != "" || st.Error != "" {
					t.Fatal("errors or job retained")
				}
				if _, ok := st.Leases[j.Target.Key()]; ok {
					t.Fatal("lease retained")
				}
				b, err := os.ReadFile(filepath.Join(s.dir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				var persisted State
				json.Unmarshal(b, &persisted)
				if persisted.Archive.Job != nil || persisted.Error != "" || len(persisted.Leases) != 0 {
					t.Fatal("clear not durable")
				}
				audit, _ := os.ReadFile(filepath.Join(s.dir, "audit.jsonl"))
				for _, action := range []string{"clear_operator_requested", "clear_intent", "cleared", "operator@example.org"} {
					if !strings.Contains(string(audit), action) {
						t.Fatal("audit missing", action)
					}
				}
			} else {
				if w.Code != 400 && w.Code != 404 && w.Code != 409 {
					t.Fatalf("guard missing: %d %s", w.Code, w.Body)
				}
				if len(s.Snapshot().Archive.FailedJobs) == 0 {
					t.Fatal("refusal lost job")
				}
			}
			if fake.grabs+fake.imports+fake.searches != 0 {
				t.Fatal("clear performed external writes")
			}
		})
	}
	svc, _, _ := supervised(t, "", time.Time{})
	req := httptest.NewRequest("POST", "/archive/clear", strings.NewReader(`{"id":"x","apply":true}`))
	req.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	svc.Handler("secret").ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatal("clear lacks JSON barrier")
	}
}

func TestCleanGrabFailureIsHealthyAndUnleased(t *testing.T) {
	svc, s, fake := supervised(t, "", time.Time{})
	now := time.Now().UTC()
	j := &archivetier.Job{Target: fake.original, Phase: archivetier.Failed, NonHalting: true, Started: now.Add(-time.Hour), Message: "Radarr refused the grab (404)"}
	if err := s.SaveArchive(archivetier.State{Job: j}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Snapshot().Leases[j.Target.Key()]; ok {
		t.Fatal("clean failure retained lease")
	}
	// Outside the window, no new job replaces the failed card.
	noon := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := svc.Tick(context.Background(), noon); err != nil || !svc.Status().Healthy {
		t.Fatal("clean failure unhealthy", err)
	}
	s.Close()
	reopened, err := OpenStore(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok := reopened.Snapshot().Leases[j.Target.Key()]; ok {
		t.Fatal("clean failure lease restored at startup")
	}
}

func TestPurgeAllEndpoint(t *testing.T) {
	svc, store, _ := supervised(t, "", time.Time{})
	_, file := purgeFixture(t, svc, store)
	w := call(t, svc, "POST", "/archive/purge-all", "secret", `{}`)
	var plan archivetier.PurgeAllPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil || w.Code != 200 || len(plan.Entries) != 1 {
		t.Fatalf("plan: %d %s", w.Code, w.Body)
	}
	for _, body := range []string{`{"apply":true,"hash":"stale","confirm":"purge all"}`, `{"apply":true,"hash":"` + plan.Hash + `","confirm":"wrong"}`} {
		if w := call(t, svc, "POST", "/archive/purge-all", "secret", body); w.Code != 409 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	svc.tick.Lock()
	svc.ActionWait = time.Millisecond
	w = call(t, svc, "POST", "/archive/purge-all", "secret", `{}`)
	svc.tick.Unlock()
	if w.Code != 503 {
		t.Fatal("cycle lock", w.Code)
	}
	body := `{"apply":true,"hash":"` + plan.Hash + `","confirm":"purge all"}`
	if w := call(t, svc, "POST", "/archive/purge-all", "secret", body); w.Code != 200 || !strings.Contains(w.Body.String(), `"purged":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("original remains", err)
	}
}
