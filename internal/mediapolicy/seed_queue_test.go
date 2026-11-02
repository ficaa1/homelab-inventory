package mediapolicy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const seedOtherHash = "fedcba9876543210fedcba9876543210fedcba98"

func (f *seedFixture) post(path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	f.svc.Handler("fixture-token").ServeHTTP(w, r)
	return w
}

// queueState persists a queue directly, as a restart or a long wait would find it.
func (f *seedFixture) queueState(entries ...SeedQueueEntry) {
	f.t.Helper()
	if err := f.svc.Store.UpdateSeeding(func(st *State) error { st.SeedQueue = entries; return nil }); err != nil {
		f.t.Fatal(err)
	}
}

func (f *seedFixture) results() []string {
	out := []string{}
	for _, r := range f.svc.Store.Snapshot().SeedQueueResults {
		out = append(out, r.Hash[:4]+":"+r.Result+":"+r.Reason)
	}
	return out
}

func (f *seedFixture) audits() []string {
	file, err := os.Open(filepath.Join(f.svc.Store.dir, "audit.jsonl"))
	if err != nil {
		return nil
	}
	defer file.Close()
	out := []string{}
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var a struct{ Action string }
		json.Unmarshal(sc.Bytes(), &a)
		out = append(out, a.Action)
	}
	return out
}

func TestSeedQueueValidation(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	for _, tc := range []struct {
		name string
		body any
		code int
		want string
	}{
		{"empty", SeedQueueRequest{Apply: true}, 400, "1 to 50"},
		{"duplicate", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}, {p.Hash, p.PlanHash}}, Apply: true}, 400, "repeated"},
		{"bad hash", SeedQueueRequest{Items: []SeedQueueItem{{"nothex", p.PlanHash}}, Apply: true}, 400, "invalid"},
		{"no confirmation", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}}, 409, "confirmation"},
		{"stale plan", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, "stale"}}, Apply: true}, 409, "Fixture Movie: Plan changed"},
		{"gone", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}, {seedOtherHash, "x"}}, Apply: true}, 409, "no longer on the download disk"},
	} {
		w := f.post("/seeding/queue", tc.body)
		if w.Code != tc.code || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s: %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	items := make([]SeedQueueItem, SeedQueueMax+1)
	for i := range items {
		items[i] = SeedQueueItem{strings.Repeat("0", 39) + string("0123456789abcdef"[i%16]), "x"}
	}
	if w := f.post("/seeding/queue", SeedQueueRequest{Items: items, Apply: true}); w.Code != 400 {
		t.Fatal("over the limit accepted", w.Code)
	}
	b, _ := json.Marshal(SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true})
	r := httptest.NewRequest("POST", "/seeding/queue", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	f.svc.Handler("fixture-token").ServeHTTP(w, r)
	if w.Code != 415 {
		t.Fatal("CSRF barrier", w.Code)
	}
	if len(f.writes) != 0 || len(f.svc.Store.Snapshot().SeedQueue) != 0 {
		t.Fatal("refused queue wrote")
	}
}

func TestSeedQueueRefusesMoveMode(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	if p.Mode != "move" {
		t.Fatal(p.Mode)
	}
	w := f.post("/seeding/queue", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true})
	if w.Code != 409 || !strings.Contains(w.Body.String(), "Not linkable") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSeedQueueRefusesWhileJobOrQueueActive(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	req := SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true}
	f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "moving", Started: time.Now()})
	if w := f.post("/seeding/queue", req); w.Code != 409 || !strings.Contains(w.Body.String(), "Another move in progress") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "halted", Started: time.Now()})
	if w := f.post("/seeding/queue", req); w.Code != 409 || !strings.Contains(w.Body.String(), "needs attention") {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "complete", Started: time.Now()})
	f.queueState(SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Other"})
	if w := f.post("/seeding/queue", req); w.Code != 409 || !strings.Contains(w.Body.String(), "Queue already running") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := f.post("/seeding/move", SeedRequest{Hash: p.Hash, PlanHash: p.PlanHash, Apply: true}); w.Code != 409 || !strings.Contains(w.Body.String(), "Queue in progress") {
		t.Fatal("single move beside a queue", w.Code, w.Body.String())
	}
	if len(f.writes) != 0 {
		t.Fatal("refusal wrote")
	}
}

func TestSeedQueueLinksAndRecordsDone(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	w := f.post("/seeding/queue", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true})
	if w.Code != 202 || !strings.Contains(w.Body.String(), `"queued":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.WaitSeeding()
	st := f.svc.Store.Snapshot()
	if st.Seeding == nil || st.Seeding.Phase != "complete" || len(st.SeedQueue) != 0 || len(st.Leases) != 0 {
		t.Fatalf("queue did not link: %+v", st.Seeding)
	}
	if got := strings.Join(f.results(), ","); got != "0123:done:" {
		t.Fatal(got)
	}
	a := strings.Join(f.audits(), ",")
	if !strings.Contains(a, "seeding_queue_queued") || !strings.Contains(a, "seeding_queued") || !strings.Contains(a, "seeding_queue_done") {
		t.Fatal(a)
	}
	if _, e := os.Stat(f.local(p.Files[0].Source)); !os.IsNotExist(e) {
		t.Fatal("Download not freed")
	}
}

func TestSeedQueueRunsInOrderAndSkipsChangedPlans(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.queueState(
		SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Gone"},
		SeedQueueEntry{Hash: p.Hash, PlanHash: "reviewed-earlier", Name: p.Name},
	)
	if !f.svc.ResumeSeedQueue() {
		t.Fatal("queue not started")
	}
	f.svc.WaitSeeding()
	if got := strings.Join(f.results(), ","); got != "fedc:skipped:torrent is no longer on the download disk,0123:skipped:Plan changed" {
		t.Fatal(got)
	}
	if len(f.writes) != 0 || f.svc.Store.Snapshot().Seeding != nil {
		t.Fatal("changed plan moved")
	}
	// A second queue runs its entries in the stored order too.
	f.queueState(
		SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name},
		SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Gone"},
	)
	f.svc.ResumeSeedQueue()
	f.svc.WaitSeeding()
	got := f.results()
	if len(got) != 4 || got[2] != "0123:done:" || got[3] != "fedc:skipped:torrent is no longer on the download disk" {
		t.Fatal(got)
	}
}

func TestSeedQueueContinuesAfterRollback(t *testing.T) {
	f := seedFake(t)
	f.failCheck = true
	p := f.plan()
	f.queueState(
		SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name},
		SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Next"},
	)
	f.svc.ResumeSeedQueue()
	f.svc.WaitSeeding()
	st := f.svc.Store.Snapshot()
	if st.Seeding.Phase != "rolled_back" || len(st.SeedQueue) != 0 {
		t.Fatal(st.Seeding.Phase, st.SeedQueue)
	}
	if got := strings.Join(f.results(), ","); got != "0123:failed:Rolled back,fedc:skipped:torrent is no longer on the download disk" {
		t.Fatal(got)
	}
	if strings.Contains(strings.Join(f.audits(), ","), "seeding_queue_stopped") {
		t.Fatal("rollback stopped the queue")
	}
}

func TestSeedQueueWaitsForPlexAndCancels(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name})
	f.mu.Lock()
	f.active = true
	f.mu.Unlock()
	f.svc.ResumeSeedQueue()
	time.Sleep(20 * time.Millisecond)
	if len(f.svc.Store.Snapshot().SeedQueue) != 1 {
		t.Fatal("entry left the queue while Plex was in use")
	}
	if w := f.post("/seeding/queue/cancel", SeedQueueRequest{}); w.Code != 400 {
		t.Fatal("cancel without confirmation", w.Code)
	}
	w := f.post("/seeding/queue/cancel", SeedQueueRequest{Apply: true})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"cancelled":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.WaitSeeding()
	if len(f.writes) != 0 || f.svc.Store.Snapshot().Seeding != nil || len(f.svc.Store.Snapshot().SeedQueue) != 0 {
		t.Fatal("cancelled entry ran")
	}
	if !strings.Contains(strings.Join(f.audits(), ","), "seeding_queue_cancelled") {
		t.Fatal("cancel not audited")
	}
	if w := f.post("/seeding/queue/cancel", SeedQueueRequest{Apply: true}); w.Code != 409 {
		t.Fatal("cancel of nothing", w.Code)
	}
}

func TestSeedQueueCancelKeepsRunningMove(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "moving", Started: time.Now()})
	f.queueState(SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Next"})
	if w := f.post("/seeding/queue/cancel", SeedQueueRequest{Apply: true}); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if j := f.svc.Store.Snapshot().Seeding; j == nil || j.Phase != "moving" {
		t.Fatal("cancel touched the running move")
	}
}

func TestSeedQueueRestartResumesOrDropsAfterHalt(t *testing.T) {
	for _, halted := range []bool{false, true} {
		f := seedFake(t)
		p := f.plan()
		f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name})
		if halted {
			f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "moving", Intent: "setLocation", Started: time.Now()})
		}
		old := f.svc.Store
		old.Close()
		old.lock = nil
		st, err := OpenStore(old.dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(st.Close)
		f.svc.Store = st
		if len(st.Snapshot().SeedQueue) != 1 {
			t.Fatal("queue not persisted")
		}
		started := f.svc.ResumeSeedQueue()
		f.svc.WaitSeeding()
		snap := st.Snapshot()
		if halted {
			if started || len(f.writes) != 0 || snap.Seeding.Phase != "halted" || len(snap.SeedQueue) != 0 {
				t.Fatal("halted move did not drop the queue")
			}
			if got := strings.Join(f.results(), ","); got != "0123:skipped:Queue stopped" {
				t.Fatal(got)
			}
			if a := strings.Join(f.audits(), ","); !strings.Contains(a, "seeding_queue_stopped") {
				t.Fatal(a)
			}
			// The operator can start a new manual move once the halt is cleared.
			st.SaveSeeding(&SeedJob{Plan: p, Phase: "rolled_back", Started: time.Now()})
			if w := f.post("/seeding/move", SeedRequest{Hash: p.Hash, PlanHash: p.PlanHash, Apply: true}); strings.Contains(w.Body.String(), "Queue in progress") {
				t.Fatal("dropped queue still refuses moves")
			}
			f.svc.WaitSeeding()
			continue
		}
		if !started || snap.Seeding.Phase != "complete" || len(snap.SeedQueue) != 0 {
			t.Fatal("queue not resumed")
		}
	}
}

func TestSeedQueueNeverRunsInDryRun(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.svc.DryRun = true
	if w := f.post("/seeding/queue", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true}); w.Code != 409 {
		t.Fatal(w.Code)
	}
	f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name})
	if f.svc.ResumeSeedQueue() {
		t.Fatal("dry-run resumed a queue")
	}
	f.svc.startSeedQueue()
	f.svc.WaitSeeding()
	if len(f.writes) != 0 || f.svc.Store.Snapshot().Seeding != nil || len(f.svc.Store.Snapshot().SeedQueue) != 1 {
		t.Fatal("dry-run processed the queue")
	}
}

func TestSeedQueueListedInStatusAndSeeding(t *testing.T) {
	f := seedFake(t)
	f.queueState(SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Next", Size: 7})
	f.svc.Store.UpdateSeeding(func(st *State) error {
		addSeedResults(st, SeedQueueResult{Hash: seedTestHash, Name: "Fixture Movie", Result: "skipped", Reason: "Plan changed"})
		return nil
	})
	if s := f.svc.Status(); len(s.SeedingQueue) != 1 || s.SeedingQueue[0].Name != "Next" || len(s.SeedingResults) != 1 {
		t.Fatalf("%+v", s)
	}
	w := httptest.NewRecorder()
	f.svc.handleSeeding(w, httptest.NewRequest("GET", "/seeding", nil))
	if !strings.Contains(w.Body.String(), `"queue":[{"hash":"`+seedOtherHash) || !strings.Contains(w.Body.String(), `"queueResults":[{`) {
		t.Fatal(w.Body.String())
	}
	if b, _ := json.Marshal(f.svc.Status()); !strings.Contains(string(b), `"seedingQueue":[`) {
		t.Fatal(string(b))
	}
}

func TestSeedQueueResultsKeepLast50(t *testing.T) {
	st := &State{}
	for i := 0; i < SeedQueueMax+5; i++ {
		addSeedResults(st, SeedQueueResult{Name: string(rune('a' + i%26))})
	}
	if len(st.SeedQueueResults) != SeedQueueMax || st.SeedQueueResults[0].Name != "f" {
		t.Fatal(len(st.SeedQueueResults), st.SeedQueueResults[0].Name)
	}
}

func TestSeedQueueAuditFailureSavesNoJob(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name})
	if err := os.Mkdir(filepath.Join(f.svc.Store.dir, "audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.svc.beginSeed(p, true); err == nil {
		t.Fatal("began without an audit")
	}
	st := f.svc.Store.Snapshot()
	if st.Seeding != nil || len(st.SeedQueue) != 1 || len(st.Leases) != 0 || len(f.writes) != 0 {
		t.Fatalf("failed audit saved a job: %+v %d", st.Seeding, len(st.SeedQueue))
	}
}

func TestSeedQueueHeadPlanHashMustMatch(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: "older", Name: p.Name})
	if _, _, err := f.svc.beginSeed(p, true); err == nil || err.Error() != "Queue changed" {
		t.Fatal(err)
	}
	if st := f.svc.Store.Snapshot(); st.Seeding != nil || len(st.SeedQueue) != 1 || len(st.Leases) != 0 {
		t.Fatal("mismatched head began")
	}
}

// Cancel when the drainer checks shutdown after runSeed persists rollback.
// This makes the orderly-shutdown window deterministic without timing sleeps.
type seedRollbackShutdownContext struct {
	context.Context
	store  *Store
	cancel context.CancelFunc
}

func (c seedRollbackShutdownContext) Err() error {
	if j := c.store.Snapshot().Seeding; j != nil && j.Phase == "rolled_back" {
		c.cancel()
	}
	return c.Context.Err()
}

func TestSeedQueueRestartContinuesAfterRollback(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "crash"
		if shutdown {
			name = "orderly shutdown"
		}
		t.Run(name, func(t *testing.T) {
			f := seedFake(t)
			p := f.plan()
			queued := time.Now().UTC()
			next := SeedQueueEntry{Hash: seedOtherHash, PlanHash: "x", Name: "Next", Queued: queued}
			if shutdown {
				f.failCheck = true
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.svc.Seeding.Context = seedRollbackShutdownContext{ctx, f.svc.Store, cancel}
				f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: p.PlanHash, Name: p.Name, Queued: queued}, next)
				f.svc.ResumeSeedQueue()
				f.svc.WaitSeeding()
			} else {
				// Crash after the terminal job write, before its queue result is recorded.
				f.queueState(next)
				if err := f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "rolled_back", Started: queued.Add(time.Second)}); err != nil {
					t.Fatal(err)
				}
			}
			old := f.svc.Store
			before := old.Snapshot()
			if before.Seeding.Phase != "rolled_back" || len(before.SeedQueue) != 1 {
				t.Fatalf("wrong restart state: %+v", before)
			}
			old.Close()
			old.lock = nil
			store, err := OpenStore(old.dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			f.svc.Store = store
			f.svc.Seeding.Context = context.Background()
			writes := len(f.writes)
			if !f.svc.ResumeSeedQueue() {
				t.Fatal("rollback blocked restart")
			}
			f.svc.WaitSeeding()
			if len(f.writes) != writes || len(store.Snapshot().SeedQueue) != 0 {
				t.Fatal("rollback did not drain the queue")
			}
			if got := strings.Join(f.results(), ","); !strings.Contains(got, "0123:failed:Rolled back") || !strings.HasSuffix(got, "fedc:skipped:torrent is no longer on the download disk") {
				t.Fatal(got)
			}
			if strings.Contains(strings.Join(f.audits(), ","), "seeding_queue_stopped") {
				t.Fatal("rollback stopped queue")
			}
			store.Close()
			store.lock = nil
			reopened, err := OpenStore(store.dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(reopened.Close)
			if len(reopened.Snapshot().SeedQueue) != 0 {
				t.Fatal("queue drop not persisted")
			}
		})
	}
}

func TestSeedQueueRestartRunsAfterOlderRollback(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	if err := f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "rolled_back", Started: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// A prior move can finish while this request is still planning.
	active := f.svc.Seeding.Active
	f.svc.Seeding.Active = func(context.Context) (bool, error) {
		err := f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "rolled_back", Started: time.Now().UTC()})
		return false, err
	}
	// Admit the fresh queue with the worker shutting down, then restart it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.svc.Seeding.Context = ctx
	if w := f.post("/seeding/queue", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true}); w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.WaitSeeding()
	f.svc.Seeding.Active = active
	if f.svc.Store.Snapshot().SeedQueue[0].Queued.IsZero() {
		t.Fatal("queue creation time missing")
	}
	old := f.svc.Store
	old.Close()
	old.lock = nil
	store, err := OpenStore(old.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	f.svc.Store = store
	f.svc.Seeding.Context = context.Background()
	if !f.svc.ResumeSeedQueue() {
		t.Fatal("old rollback blocked a fresh queue")
	}
	f.svc.WaitSeeding()
	if st := store.Snapshot(); st.Seeding.Phase != "complete" || len(st.SeedQueue) != 0 {
		t.Fatal("fresh queue did not complete")
	}
}

func TestSeedQueueAllowsMoveAndPersistsPermission(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	// Simulate shutdown after admission so the saved entry can be inspected.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.svc.Seeding.Context = ctx
	w := f.post("/seeding/queue", SeedQueueRequest{Items: []SeedQueueItem{{p.Hash, p.PlanHash}}, Apply: true, AllowMove: true})
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	f.svc.WaitSeeding()
	st := f.svc.Store.Snapshot()
	if len(st.SeedQueue) != 1 || !st.SeedQueue[0].AllowMove || st.SeedQueue[0].Mode != "move" || st.SeedQueue[0].NASBytesCost != p.NASBytesCost {
		t.Fatalf("permission missing: %+v", st.SeedQueue)
	}
	saved, err := os.ReadFile(filepath.Join(f.svc.Store.dir, "state.json"))
	var persisted State
	if err == nil {
		err = json.Unmarshal(saved, &persisted)
	}
	if err != nil || len(persisted.SeedQueue) != 1 || !persisted.SeedQueue[0].AllowMove {
		t.Fatal("permission not persisted", err)
	}
	f.svc.Seeding.Context = context.Background()
	f.svc.ResumeSeedQueue()
	f.svc.WaitSeeding()
	st = f.svc.Store.Snapshot()
	if st.Seeding == nil || st.Seeding.Phase != "complete" || len(st.SeedQueueResults) != 1 || st.SeedQueueResults[0].Mode != "move" {
		t.Fatalf("move failed: %+v %+v", st.Seeding, st.SeedQueueResults)
	}
}

func TestSeedQueueOldEntriesRemainLinkOnly(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	var old SeedQueueEntry
	b, _ := json.Marshal(map[string]any{"hash": p.Hash, "planHash": p.PlanHash, "name": p.Name})
	if err := json.Unmarshal(b, &old); err != nil || old.AllowMove {
		t.Fatal("old permission", err)
	}
	f.queueState(old)
	f.svc.seedQueueStep(context.Background())
	if got := strings.Join(f.results(), ","); got != "0123:skipped:Not linkable" || len(f.writes) != 0 {
		t.Fatal(got)
	}
}

func TestSeedMoveQueueStillRefusesChangedAndUnsafePlans(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	f.queueState(SeedQueueEntry{Hash: p.Hash, PlanHash: "stale", AllowMove: true})
	f.svc.seedQueueStep(context.Background())
	if got := strings.Join(f.results(), ","); got != "0123:skipped:Plan changed" || len(f.writes) != 0 {
		t.Fatal(got)
	}
	p.Refusal = "NAS floor"
	if seedQueueEligible(p, true) {
		t.Fatal("unsafe plan accepted")
	}
}

func TestSeedQueueSkipsPlansThatFlipModes(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		linkBefore, allowMove bool
		reason                string
	}{
		{"Link all becomes move", true, false, "Not linkable"},
		{"Move all link becomes move", true, true, "Plan changed"},
		{"Move all move becomes link", false, true, "Plan changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedFake(t)
			f.matched = tc.linkBefore
			before := f.plan()
			f.queueState(SeedQueueEntry{Hash: before.Hash, PlanHash: before.PlanHash, Name: before.Name, Mode: before.Mode, AllowMove: tc.allowMove})
			// Arr's actual file match changes after the operator queues the plan.
			f.mu.Lock()
			f.matched = !tc.linkBefore
			f.mu.Unlock()
			after := f.plan()
			if before.Mode == after.Mode || before.PlanHash == after.PlanHash || after.Refusal != "" {
				t.Fatalf("not a real mode change: %+v -> %+v", before, after)
			}
			if !f.svc.ResumeSeedQueue() {
				t.Fatal("queue did not resume")
			}
			f.svc.WaitSeeding()
			st := f.svc.Store.Snapshot()
			if got := strings.Join(f.results(), ","); got != "0123:skipped:"+tc.reason {
				t.Fatal(got)
			}
			if len(f.writes) != 0 || st.Seeding != nil || len(st.SeedQueue) != 0 || len(st.Leases) != 0 {
				t.Fatal("changed mode executed")
			}
			if _, err := os.Stat(f.local(before.Files[0].Source)); err != nil {
				t.Fatal("source changed", err)
			}
		})
	}
}
