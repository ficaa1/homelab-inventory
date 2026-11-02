package mediapolicy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func TestLeaseSurvivesArchiveRestartAndFailedJob(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	release, e := s.Acquire("sonarr:44", "archive")
	if e != nil {
		t.Fatal(e)
	}
	if !s.Blocked("sonarr:44", "seeding") {
		t.Fatal("mover admitted during archive search")
	}
	if _, e = s.Acquire("sonarr:44", "seeding"); e == nil {
		t.Fatal("conflicting lease acquired")
	}
	job := &archivetier.Job{Target: archivetier.Target{Kind: "sonarr", ID: 44}, Phase: archivetier.Failed, Started: time.Now()}
	if e = s.SaveArchive(archivetier.State{Job: job}); e != nil {
		t.Fatal(e)
	}
	release()
	s.Close()
	s, e = OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if !s.Blocked("sonarr:44", "seeding") {
		t.Fatal("failed replacement lost exclusion after restart")
	}
	if e = s.SaveArchive(archivetier.State{}); e != nil {
		t.Fatal(e)
	}
	if s.Blocked("sonarr:44", "seeding") {
		t.Fatal("operator reconciled job remains leased")
	}
}
func TestLeaseRefusalSaysWhetherToWait(t *testing.T) {
	s, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	const key = "radarr:7"
	wait := "try again shortly"
	inspect := "Needs attention"
	check := func(name, owner, want string) {
		t.Helper()
		_, e := s.Acquire(key, owner)
		if e == nil || !strings.Contains(e.Error(), want) {
			t.Fatalf("%s: got %v, want %q", name, e, want)
		}
		if want == inspect && strings.Contains(e.Error(), "try again") {
			t.Fatalf("%s: retained work told to retry: %v", name, e)
		}
	}
	plan := SeedPlan{Name: "Fixture", Titles: []string{key}}
	if e = s.SaveSeeding(&SeedJob{Plan: plan, Phase: "moving"}); e != nil {
		t.Fatal(e)
	}
	check("moving move", "tdarr", wait)
	if e = s.SaveSeeding(&SeedJob{Plan: plan, Phase: "halted"}); e != nil {
		t.Fatal(e)
	}
	check("halted move", "tdarr", inspect)
	if e = s.SaveSeeding(&SeedJob{Plan: plan, Phase: "rolled_back"}); e != nil {
		t.Fatal(e)
	}
	job := &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 7}, Phase: archivetier.Failed, Started: time.Now()}
	if e = s.SaveArchive(archivetier.State{Job: job}); e != nil {
		t.Fatal(e)
	}
	check("failed archive", "seeding", inspect)
	job.Phase = archivetier.Downloading
	if e = s.SaveArchive(archivetier.State{Job: job}); e != nil {
		t.Fatal(e)
	}
	check("downloading archive", "seeding", wait)
}

func TestStateFailureStopsAllWrites(t *testing.T) {
	dir := t.TempDir()
	s, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if e = os.Mkdir(filepath.Join(dir, "state.json"), 0700); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveArchive(archivetier.State{}); e == nil {
		t.Fatal("expected rename failure")
	}
	if s.WriteAllowed() == nil {
		t.Fatal("persistence failure did not latch")
	}
	if e = os.Remove(filepath.Join(dir, "state.json")); e != nil {
		t.Fatal(e)
	}
	if e = s.SaveArchive(archivetier.State{}); e == nil {
		t.Fatal("write admission recovered without restart")
	}
}
func TestDryHTTPBarrierAndNoTrackerSearch(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer up.Close()
	c := SharedHTTP(&Transport{NoSearch: true})
	for _, v := range []struct{ method, path string }{{"POST", "/api/v3/command"}, {"PUT", "/api/v3/series/1"}, {"DELETE", "/api/v3/queue/1"}, {"GET", "/api/v3/release"}} {
		r, _ := http.NewRequest(v.method, up.URL+v.path, nil)
		if _, e := c.Do(r); e == nil {
			t.Fatalf("request allowed: %+v", v)
		}
	}
	if calls != 0 {
		t.Fatal("barrier contacted upstream")
	}
}

func TestCorruptStateNeverSeedsFreshPolicies(t *testing.T) {
	for _, raw := range []string{"null", "{}", `{"version":1,"leases":null}`, `{"version":1,"leases":{},"archive":{"job":{"target":{"kind":"sonarr","id":44},"phase":"unknown"}}}`} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if s, err := OpenStore(dir); err == nil {
			s.Close()
			t.Fatalf("accepted invalid state %s", raw)
		}
	}
}

func TestDisablingArchiveCannotHidePersistedFailure(t *testing.T) {
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SaveArchive(archivetier.State{Job: &archivetier.Job{Target: archivetier.Target{Kind: "sonarr", ID: 44}, Phase: archivetier.Failed, Message: "missing original recycle file"}}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: s, Config: Config{ArchiveEnabled: false}}
	if svc.Tick(context.Background(), time.Now()) == nil || svc.Status().Healthy {
		t.Fatal("disabled archive hid failed job")
	}
}

func TestAbandonedJobReleasesDurableLeaseAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := &archivetier.Job{Phase: archivetier.Downloading, Target: archivetier.Target{Kind: "radarr", ID: 7}}
	if err = s.SaveArchive(archivetier.State{Job: j}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Snapshot().Leases[j.Target.Key()]; !ok {
		t.Fatal("active job not leased")
	}
	j.Phase = archivetier.Abandoned
	if err = s.SaveArchive(archivetier.State{Job: j}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Snapshot().Leases[j.Target.Key()]; ok {
		t.Fatal("abandoned title remains leased")
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Snapshot().Leases[j.Target.Key()]; ok {
		t.Fatal("abandoned lease restored at restart")
	}
}

func TestSearchTitleRetainedLeaseNeedsAttention(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	svc, store, fake := supervised(t, "", now)
	const key = "radarr:293"
	if err := store.SaveSeeding(&SeedJob{Plan: SeedPlan{Titles: []string{key}}, Phase: "halted"}); err != nil {
		t.Fatal(err)
	}
	runner := svc.Archive.(*archivetier.Runner)
	_, err := runner.SearchTitle(context.Background(), now, archivetier.ApprovalRequest{Kind: "radarr", ID: 293})
	if err == nil || !strings.Contains(err.Error(), "Needs attention") || strings.Contains(err.Error(), "try again") {
		t.Fatalf("retained lease refusal: %v", err)
	}
	if fake.searches != 0 || fake.grabs != 0 || fake.imports != 0 {
		t.Fatal("retained lease allowed work")
	}
	if lease, ok := store.Snapshot().Leases[key]; !ok || lease.Owner != "seeding" {
		t.Fatal("retained lease was released")
	}
}

func TestFailedListRetainsOnlyItsTitlesAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	j := &archivetier.Job{Target: archivetier.Target{Kind: "sonarr", ID: 44}, Phase: archivetier.Failed, ImportedAt: time.Now(), Recovery: []archivetier.Evidence{{Path: "/old", Size: 4}}}
	if err := s.SaveArchive(archivetier.State{Job: j}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	st := s.Snapshot()
	if st.Archive.Job != nil || len(st.Archive.FailedJobs) != 1 || len(st.Archive.FailedJobs[0].Recovery) != 1 || !s.Blocked("sonarr:44", "seeding") {
		t.Fatal("failed lease or recovery lost", st)
	}
	release, err := s.Acquire("radarr:7", "archive")
	if err != nil {
		t.Fatal("other title blocked", err)
	}
	release()
	if !s.Blocked("sonarr:44", "seeding") {
		t.Fatal("another title released failed lease")
	}
}
