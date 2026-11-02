package mediapolicy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

type queuedSearcher struct {
	store   *Store
	calls   int
	err     error
	started chan struct{}
	finish  chan struct{}
}

func (a *queuedSearcher) CheckAdmission(context.Context, time.Time, archivetier.Admission) (archivetier.State, error) {
	return archivetier.State{}, nil
}
func (a *queuedSearcher) SetSeasons(key string, seasons []int) error {
	st := a.store.Snapshot().Archive
	if st.Seasons == nil {
		st.Seasons = map[string][]int{}
	}
	st.Seasons[key] = seasons
	return a.store.SaveArchive(st)
}
func (a *queuedSearcher) SearchTitleCached(context.Context, time.Time, archivetier.ApprovalRequest) (archivetier.State, error) {
	a.calls++
	if a.started != nil {
		close(a.started)
		<-a.finish
	}
	return a.store.Snapshot().Archive, a.err
}
func TestArchiveTagQueuesWithoutSearchAndPersists(t *testing.T) {
	s, _ := libraryService(t)
	a := &queuedSearcher{store: s.Store}
	s.Archive = a
	body := `{"kind":"sonarr","id":2,"archive":true,"fourK":false,"seasons":[1,2]}`
	rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"searchesQueued":true`) || a.calls != 0 {
		t.Fatalf("tag blocked: %d %s calls=%d", rec.Code, rec.Body, a.calls)
	}
	if len(s.Status().ArchiveSearches) != 2 {
		t.Fatal("queue not visible")
	}
	// Deduplicate a repeat request while both rows are pending.
	rec = libraryRequest(s, "POST", "/archive/tag", body, "fixture")
	if rec.Code != 200 || len(s.Store.Snapshot().ArchiveSearches) != 2 {
		t.Fatal("duplicate queue")
	}
	dir := s.Store.dir
	s.Store.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	a.store = reopened
	if len(reopened.Snapshot().ArchiveSearches) != 2 {
		t.Fatal("queue lost on restart")
	}
	a.err = archivetier.ErrSearchRateLimit
	s.drainArchiveSearches(context.Background())
	if reopened.Snapshot().ArchiveSearches[0].State != "queued" {
		t.Fatal("rate-limited lookup consumed")
	}
	a.err = nil
	a.started = make(chan struct{})
	a.finish = make(chan struct{})
	done := make(chan struct{})
	go func() { s.drainArchiveSearches(context.Background()); close(done) }()
	<-a.started
	if s.Status().ArchiveSearches[0].State != "searching" {
		t.Fatal("running lookup not visible")
	}
	close(a.finish)
	<-done
	a.started = nil
	rows := reopened.Snapshot().ArchiveSearches
	if rows[0].State != "done" || rows[1].State != "queued" {
		t.Fatal("cycle drained more than one lookup")
	}
	a.err = errors.New("fixture failure")
	s.drainArchiveSearches(context.Background())
	if row := reopened.Snapshot().ArchiveSearches[1]; row.State != "queued" || !row.Finished.IsZero() {
		t.Fatal("transient failure consumed lookup")
	}
}
func TestArchiveSearchQueueWriteBarriersAndWithdrawal(t *testing.T) {
	s, _ := libraryService(t)
	a := &queuedSearcher{store: s.Store}
	s.Archive = a
	body := `{"kind":"sonarr","id":2,"archive":true,"fourK":false,"seasons":[1,2]}`
	req := httptest.NewRequest("POST", "/archive/tag", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer fixture")
	rec := httptest.NewRecorder()
	s.Handler("fixture").ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatal("missing JSON barrier")
	}
	for _, disabled := range []bool{false, true} {
		s.DryRun = !disabled
		s.Config.ArchiveEnabled = !disabled
		if libraryRequest(s, "POST", "/archive/tag", body, "fixture").Code != 409 || len(s.Store.Snapshot().ArchiveSearches) != 0 {
			t.Fatal("paused queue admitted")
		}
	}
	s.DryRun = false
	s.Config.ArchiveEnabled = true
	if libraryRequest(s, "POST", "/archive/tag", body, "fixture").Code != 200 {
		t.Fatal("tag failed")
	}
	if libraryRequest(s, "POST", "/archive/tag", `{"kind":"sonarr","id":2,"archive":false,"fourK":false}`, "fixture").Code != 200 || len(s.Store.Snapshot().ArchiveSearches) != 0 {
		t.Fatal("withdrawn lookup retained")
	}
}

func TestInterruptedArchiveLookupIsNotReplayed(t *testing.T) {
	s, _ := libraryService(t)
	row := ArchiveSearch{Title: archivetier.ApprovalRequest{Kind: "sonarr", ID: 2, Season: 1}, Name: "A series", State: "searching"}
	if err := s.Store.saveArchiveSearches([]ArchiveSearch{row}); err != nil {
		t.Fatal(err)
	}
	dir := s.Store.dir
	s.Store.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	a := &queuedSearcher{store: reopened}
	s.Archive = a
	s.drainArchiveSearches(context.Background())
	rows := s.Status().ArchiveSearches
	if a.calls != 0 || rows[0].State != "failed" || rows[0].Finished.IsZero() {
		t.Fatal("interrupted lookup replayed")
	}
}

// Use the runner's actual pre-search paths, not string matching on errors.
type searchReadFailure struct {
	archivetier.ArrAPI
	err error
}

func (a *searchReadFailure) Targets(ctx context.Context) ([]archivetier.Target, error) {
	if a.err != nil {
		return nil, a.err
	}
	return a.ArrAPI.Targets(ctx)
}
func TestArchiveSearchQueueRetriesTransientFailures(t *testing.T) {
	for _, name := range []string{"network", "5xx", "upstream rate limit", "rate limit full", "worker busy", "untagged", "disabled"} {
		t.Run(name, func(t *testing.T) {
			s, store, fake := supervised(t, "", time.Time{})
			r := s.Archive.(*archivetier.Runner)
			r.Store.AuditDetailRecord = store.AuditDetail
			read := &searchReadFailure{ArrAPI: fake}
			r.Arr["radarr"] = read
			now := time.Now().UTC()
			switch name {
			case "network":
				read.err = errors.New("connection reset")
			case "5xx":
				read.err = &archivetier.HTTPError{Kind: "radarr", Status: http.StatusServiceUnavailable}
			case "upstream rate limit":
				read.err = &archivetier.HTTPError{Kind: "radarr", Status: http.StatusTooManyRequests}
			case "rate limit full":
				st := store.Snapshot().Archive
				for i := 0; i < r.Config.SearchesPerMinute; i++ {
					st.SearchTimes = append(st.SearchTimes, now)
				}
				if err := store.SaveArchive(st); err != nil {
					t.Fatal(err)
				}
			case "worker busy":
				r.Acquire = func(archivetier.Target) (func(), error) { return nil, errors.New("worker busy") }
			case "untagged":
				fake.original = archivetier.Target{}
			case "disabled":
				r.NoSearch = true
			}
			rows := []ArchiveSearch{{Title: archivetier.ApprovalRequest{Kind: "radarr", ID: 293}, State: "queued"}}
			if err := store.saveArchiveSearches(rows); err != nil {
				t.Fatal(err)
			}
			s.drainArchiveSearches(context.Background())
			row := store.Snapshot().ArchiveSearches[0]
			if fake.searches != 0 {
				t.Fatal("pre-search failure reached tracker")
			}
			if name == "untagged" || name == "disabled" {
				if row.State != "failed" || row.Finished.IsZero() {
					t.Fatalf("definitive refusal not terminal: %+v", row)
				}
				return
			}
			if row.State != "queued" || !row.Finished.IsZero() {
				t.Fatalf("transient failure consumed lookup: %+v", row)
			}
			read.err = nil
			r.Acquire = nil
			st := store.Snapshot().Archive
			st.SearchTimes = nil
			if err := store.SaveArchive(st); err != nil {
				t.Fatal(err)
			}
			s.drainArchiveSearches(context.Background())
			if row = store.Snapshot().ArchiveSearches[0]; row.State != "done" || fake.searches != 1 {
				t.Fatalf("later cycle did not retry: %+v searches=%d", row, fake.searches)
			}
		})
	}
}

func TestArchiveSearchQueueRetryDoesNotStarveOtherApps(t *testing.T) {
	s, store, radarr := supervised(t, "", time.Time{})
	r := s.Archive.(*archivetier.Runner)
	r.Store.AuditDetailRecord = store.AuditDetail
	r.Arr["sonarr"] = &searchReadFailure{err: errors.New("Sonarr unavailable")}
	sonarrTitle := archivetier.ApprovalRequest{Kind: "sonarr", ID: 2, Season: 1}
	radarrTitle := archivetier.ApprovalRequest{Kind: "radarr", ID: 293}
	if err := store.saveArchiveSearches([]ArchiveSearch{
		{Title: sonarrTitle, State: "queued"},
		{Title: radarrTitle, State: "queued"},
	}); err != nil {
		t.Fatal(err)
	}
	s.drainArchiveSearches(context.Background())
	if radarr.searches != 0 {
		t.Fatal("cycle attempted another lookup after the retryable failure")
	}
	// Reopen the state to prove the next title's turn survives a restart.
	dir := store.dir
	store.Close()
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	r.Store.LoadState, r.Store.SaveState = reopened.LoadArchive, reopened.SaveArchive
	r.Store.AuditRecord, r.Store.AuditDetailRecord = reopened.Audit, reopened.AuditDetail
	r.Acquire = func(t archivetier.Target) (func(), error) { return reopened.Acquire(t.Key(), "archive") }
	rows := reopened.Snapshot().ArchiveSearches
	if len(rows) != 2 || rows[0].Title != radarrTitle || rows[1].Title != sonarrTitle || rows[1].State != "queued" || !rows[1].Finished.IsZero() {
		t.Fatalf("retry did not persist at the back of the queue: %+v", rows)
	}
	// Rotation must not bypass the runner's persisted rolling-minute limit.
	now := time.Now().UTC()
	r.Now = func() time.Time { return now }
	st := reopened.Snapshot().Archive
	for i := 0; i < r.Config.SearchesPerMinute; i++ {
		st.SearchTimes = append(st.SearchTimes, now)
	}
	if err := reopened.SaveArchive(st); err != nil {
		t.Fatal(err)
	}
	s.drainArchiveSearches(context.Background())
	rows = reopened.Snapshot().ArchiveSearches
	if radarr.searches != 0 || rows[0].Title != sonarrTitle || rows[1].Title != radarrTitle || rows[1].State != "queued" {
		t.Fatalf("rate-limited lookup was consumed or not rotated: %+v searches=%d", rows, radarr.searches)
	}
	s.drainArchiveSearches(context.Background()) // Sonarr is still unavailable.
	now = now.Add(time.Minute)
	s.drainArchiveSearches(context.Background())
	rows = reopened.Snapshot().ArchiveSearches
	if radarr.searches != 1 || rows[0].Title != radarrTitle || rows[0].State != "done" || rows[1].Title != sonarrTitle || rows[1].State != "queued" || !rows[1].Finished.IsZero() {
		t.Fatalf("unavailable Sonarr starved healthy Radarr: %+v searches=%d", rows, radarr.searches)
	}
}

type cycleSearcher struct {
	*queuedSearcher
	reconciled           bool
	searchAfterReconcile bool
	checked              int
	rateFull             bool
}

func (a *cycleSearcher) CheckAdmission(ctx context.Context, _ time.Time, _ archivetier.Admission) (archivetier.State, error) {
	a.checked++
	a.reconciled = ctx.Err() == nil
	return a.store.Snapshot().Archive, ctx.Err()
}
func (a *cycleSearcher) SearchTitleCached(ctx context.Context, _ time.Time, _ archivetier.ApprovalRequest) (archivetier.State, error) {
	a.calls++
	a.searchAfterReconcile = a.reconciled
	if a.rateFull {
		return archivetier.State{}, archivetier.ErrSearchRateLimit
	}
	<-ctx.Done() // Slow lookup consumes all remaining cycle time.
	return archivetier.State{}, ctx.Err()
}
func TestArchiveReconcilesBeforeBoundedSearchDrain(t *testing.T) {
	for _, rateFull := range []bool{false, true} {
		t.Run(fmt.Sprintf("rateFull=%v", rateFull), func(t *testing.T) {
			s, store, _ := supervised(t, "", time.Time{})
			a := &cycleSearcher{queuedSearcher: &queuedSearcher{store: store}, rateFull: rateFull}
			s.Archive = a
			s.Config.Archive.SearchesPerMinute = 1
			rows := []ArchiveSearch{{Title: archivetier.ApprovalRequest{Kind: "radarr", ID: 293}, State: "queued"}, {Title: archivetier.ApprovalRequest{Kind: "radarr", ID: 294}, State: "queued"}}
			if err := store.saveArchiveSearches(rows); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := s.Tick(ctx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			if !a.reconciled || !a.searchAfterReconcile || a.checked != 1 || a.calls != 1 {
				t.Fatalf("reconciliation starved or queue unbounded: reconciled=%v checked=%d searches=%d", a.reconciled, a.checked, a.calls)
			}
			for _, row := range store.Snapshot().ArchiveSearches {
				if row.State != "queued" || !row.Finished.IsZero() {
					t.Fatalf("lookup lost: %+v", row)
				}
			}
		})
	}
}
