package mediapolicy

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func selectionCall(s *Service, method, path, body, token, ct string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", ct)
	w := httptest.NewRecorder()
	s.Handler("secret").ServeHTTP(w, r)
	return w
}

type selectionSpy struct {
	archivetier.ArrAPI
	searches, grabs, imports int
}

func (a *selectionSpy) Search(context.Context, archivetier.Target) ([]archivetier.Release, error) {
	a.searches++
	return nil, nil
}
func (a *selectionSpy) Grab(context.Context, archivetier.Release) error { a.grabs++; return nil }
func (a *selectionSpy) Import(context.Context, []map[string]any) (int, error) {
	a.imports++
	return 1, nil
}

func TestSelectionPreviewSaveResetCachedOnly(t *testing.T) {
	svc, store, _ := supervised(t, "", time.Time{})
	target := archivetier.Target{Kind: "radarr", ID: 293, Title: "Example Movie", Runtime: 120, Files: []archivetier.File{{ID: 1, Size: 50 * archivetier.GB}}}
	// Fingerprint has the same form as persisted lookup state, with no service
	// call used to seed it. A best cached release starts below the score minimum.
	release := archivetier.Release{Guid: "private-tracker-guid", Title: "Example Movie.1080p.WEB-DL.x265.10bit.DDP5.1-trusted", Group: "trusted", Size: 2600000000, Seeders: 100, IndexerID: 11, Protocol: "torrent", FullSeason: true}
	release.Languages = []archivetier.Language{{Name: "English"}}
	release.Flags = json.RawMessage(`1`)
	release.Quality.Quality.Name = "WEBDL-1080p"
	release.Quality.Quality.Resolution = 1080
	decision := archivetier.Select(target, []archivetier.Release{release}, svc.Config.Archive)
	if len(decision.Candidates) == 0 {
		t.Fatalf("fixture rejected: %+v", decision)
	}
	decision.SearchedAt = time.Now().Add(-time.Hour)
	st := archivetier.State{Report: []archivetier.Decision{decision}, Lookups: map[string]archivetier.Lookup{"radarr:293:0": {Decision: decision, Releases: []archivetier.Release{release}, At: decision.SearchedAt, Files: "1080p:120:2:1:50000000000"}}, SearchesUsed: 3, SearchTimes: []time.Time{decision.SearchedAt}}
	if err := store.SaveArchive(st); err != nil {
		t.Fatal(err)
	}
	spy := &selectionSpy{}
	svc.Archive.(*archivetier.Runner).Arr = map[string]archivetier.ArrAPI{"radarr": spy}
	body := `{"overrides":{"sizes":{"1080p":{"target":1.4}},"minScore":0,"trustedGroups":[]},"requestedBy":"operator@example.org"}`
	before := store.Snapshot()
	preview := selectionCall(svc, "POST", "/archive/selection/preview", body, "secret", "application/json")
	if preview.Code != 200 {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body)
	}
	var p archivetier.SelectionPreview
	json.Unmarshal(preview.Body.Bytes(), &p)
	if p.AutoPicks != 1 || p.CurrentAutoPicks != 0 || len(p.Titles) != 1 || p.Titles[0].Current != nil || p.Titles[0].Proposed == nil {
		t.Fatalf("preview: %+v", p)
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("preview changed state")
	}
	if _, err := os.Stat(filepath.Join(store.dir, "audit.jsonl")); !os.IsNotExist(err) {
		t.Fatal("preview wrote audit")
	}
	for _, at := range []time.Time{time.Date(2026, 10, 11, 0, 30, 0, 0, time.UTC), time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)} {
		// Saving inside or outside the configured window never runs a cycle or grabs.
		svc.status.Checked = at
		saved := selectionCall(svc, "PUT", "/archive/selection", body, "secret", "application/json")
		if saved.Code != 200 {
			t.Fatalf("save: %d %s", saved.Code, saved.Body)
		}
		snap := store.Snapshot()
		if snap.Archive.Report[0].Release == nil || snap.Archive.Report[0].Candidates[0].Score.MinScore != 0 || snap.Archive.Lookups["radarr:293:0"].Decision.Score.Target != 1.4 || snap.Archive.SearchesUsed != 3 {
			t.Fatalf("rescore missing: %+v", snap.Archive)
		}
		select {
		case <-svc.Wake():
			t.Fatal("save woke a cycle")
		default:
		}
	}
	if spy.searches+spy.grabs+spy.imports != 0 {
		t.Fatalf("settings called Arr: %+v", spy)
	}
	audit, err := os.ReadFile(filepath.Join(store.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"selection_settings_saved"`) || !strings.Contains(string(audit), `"actor":"operator@example.org"`) || !strings.Contains(string(audit), `"before":`) || !strings.Contains(string(audit), `"after":`) {
		t.Fatalf("missing audit: %s", audit)
	}
	// Reload from the actual durable state file.
	dir := store.dir
	store.Close()
	store.lock = nil
	reopened, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	svc.Store = reopened
	settings, err := svc.selectionSettings()
	if err != nil || settings.Effective.MinScore != 0 || settings.Effective.Sizes["1080p"].Target != 1.4 || len(settings.Effective.TrustedGroups) != 0 {
		t.Fatalf("restart lost overrides: %+v %v", settings, err)
	}
	reset := selectionCall(svc, "PUT", "/archive/selection", `{"reset":true,"requestedBy":"operator@example.org"}`, "secret", "application/json")
	if reset.Code != 200 {
		t.Fatalf("reset: %d %s", reset.Code, reset.Body)
	}
	settings, _ = svc.selectionSettings()
	if !reflect.DeepEqual(settings.Effective, settings.File) || reopened.Snapshot().Archive.Report[0].Release != nil {
		t.Fatal("reset failed to restore file policy and rescore")
	}
	if mode, _ := os.Stat(filepath.Join(dir, "state.json")); mode.Mode().Perm() != 0600 {
		t.Fatal("state permissions")
	}
}

func TestSelectionChangesAllowedDuringActiveJobKeepDecision(t *testing.T) {
	now := time.Now()
	svc, store, fake := supervised(t, archivetier.Verifying, now.Add(-time.Hour))
	before := store.Snapshot().Archive.Job
	w := selectionCall(svc, "PUT", "/archive/selection", `{"overrides":{"sizes":{"1080p":{"cap":3.1}},"codecFactors":{"hevc":0.1},"minScore":100}}`, "secret", "application/json")
	if w.Code != 200 {
		t.Fatalf("active job refused settings: %d %s", w.Code, w.Body)
	}
	j := store.Snapshot().Archive.Job
	if j.Phase != before.Phase || j.Selection == nil || !reflect.DeepEqual(*j.Selection, svc.Config.Archive.Selection) {
		t.Fatal("changed active decision")
	}
	// New caps would reject the 7 GB fixture. The already verifying job still
	// uses the persisted cap and completes outside the nightly window.
	if err := svc.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if j = store.Snapshot().Archive.Job; j.Phase != archivetier.Complete {
		t.Fatalf("changed job's cap: %+v", j)
	}
	if fake.searches+fake.grabs+fake.imports != 0 {
		t.Fatal("new writes from settings or verification")
	}
}

func TestSelectionAuthValidationAndPersistenceFailure(t *testing.T) {
	svc, store, _ := supervised(t, "", time.Time{})
	for _, route := range []struct{ method, path string }{{"GET", "/archive/selection"}, {"PUT", "/archive/selection"}, {"POST", "/archive/selection/preview"}} {
		if w := selectionCall(svc, route.method, route.path, `{"overrides":{}}`, "wrong", "application/json"); w.Code != 401 {
			t.Fatalf("auth %s: %d", route.path, w.Code)
		}
		if route.method != "GET" {
			if w := selectionCall(svc, route.method, route.path, `{"overrides":{}}`, "secret", "text/plain"); w.Code != 415 {
				t.Fatal("CSRF accepted")
			}
		}
	}
	before := store.Snapshot()
	for _, body := range []string{`{}`, `{"overrides":{"minScore":101}}`, `{"overrides":{"minScore":-1}}`, `{"overrides":{"sizes":{"1080p":{"floor":4}}}}`, `{"overrides":{"codecFactors":{"hevc":0}}}`, `{"overrides":{"minScore":0},"reset":true}`, `{"overrides":{},"unexpected":1}`, `{"overrides":{}} {}`, `{"overrides":{"trustedGroups":"group"}}`} {
		if w := selectionCall(svc, "PUT", "/archive/selection", body, "secret", "application/json"); w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body)
		}
	}
	if !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("invalid save changed state")
	}
	// Fail the durable audit before the state write. No override is installed.
	if err := os.Mkdir(filepath.Join(store.dir, "audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if w := selectionCall(svc, "PUT", "/archive/selection", `{"overrides":{"minScore":0}}`, "secret", "application/json"); w.Code != 409 {
		t.Fatal("failed audit accepted")
	}
	if store.Snapshot().SelectionOverrides.MinScore != nil {
		t.Fatal("installed unaudited override")
	}
}

func TestSelectionAtomicStateWriteFailureKeepsOldSettings(t *testing.T) {
	svc, store, _ := supervised(t, "", time.Time{})
	before := store.Snapshot()
	if err := os.Mkdir(filepath.Join(store.dir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	w := selectionCall(svc, "PUT", "/archive/selection", `{"overrides":{"minScore":0}}`, "secret", "application/json")
	if w.Code != 409 || !reflect.DeepEqual(before, store.Snapshot()) {
		t.Fatal("failed atomic write installed override")
	}
	if store.WriteAllowed() == nil {
		t.Fatal("failed write did not close admission")
	}
}

func TestInvalidLayeredSelectionStillReconcilesJobAndCanReset(t *testing.T) {
	svc, store, fake := supervised(t, archivetier.Verifying, time.Now().Add(-time.Hour))
	st := store.Snapshot().Archive
	selection := svc.Config.Archive.Selection
	st.Job.Selection = &selection
	// A file-policy change can invalidate an old override. Reconciliation and
	// reset must remain available using the valid file policy.
	min := 101.0
	if err := store.saveSelection(archivetier.SelectionOverrides{MinScore: &min}, st); err != nil {
		t.Fatal(err)
	}
	if err := svc.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Archive.Job.Phase != archivetier.Complete || !svc.Status().Healthy || svc.Status().SelectionWarning == "" {
		t.Fatal("reconciliation blocked or settings warning hidden")
	}
	if fake.searches+fake.grabs+fake.imports != 0 {
		t.Fatal("invalid settings admitted writes")
	}
	if w := selectionCall(svc, "PUT", "/archive/selection", `{"reset":true}`, "secret", "application/json"); w.Code != 200 {
		t.Fatalf("reset refused invalid overrides: %d %s", w.Code, w.Body)
	}
}

func TestResetInvalidOverridesPinsLegacyJobToFilePolicy(t *testing.T) {
	now := time.Now()
	svc, store, fake := supervised(t, archivetier.Verifying, now.Add(-time.Hour))
	st := store.Snapshot().Archive
	if st.Job.Selection != nil {
		t.Fatal("fixture must be a legacy job without a policy snapshot")
	}
	min := 101.0
	if err := store.saveSelection(archivetier.SelectionOverrides{MinScore: &min}, st); err != nil {
		t.Fatal(err)
	}
	w := selectionCall(svc, "PUT", "/archive/selection", `{"reset":true}`, "secret", "application/json")
	if w.Code != 200 {
		t.Fatalf("reset: %d %s", w.Code, w.Body)
	}
	// Exercise the next normal tick before inspecting the snapshot: the original
	// bug failed verification and retained the title's durable lease.
	if err := svc.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	snap := store.Snapshot()
	if snap.Archive.Job.Phase != archivetier.Complete || snap.Archive.Job.Selection == nil || !reflect.DeepEqual(*snap.Archive.Job.Selection, svc.Config.Archive.Selection) {
		t.Fatalf("legacy verification changed policy: %+v", snap.Archive.Job)
	}
	if _, held := snap.Leases[st.Job.Target.Key()]; held {
		t.Fatal("verified job kept its lease")
	}
	if fake.searches+fake.grabs+fake.imports != 0 {
		t.Fatal("reset or verification made new writes")
	}
}

func TestCorruptSelectionOverridesSurviveStartupAndRemainResettable(t *testing.T) {
	for _, raw := range []string{`{"minScore":"broken"}`, `{"minScore":101}`} {
		for _, repair := range []string{`{"reset":true}`, `{"overrides":{"minScore":0}}`} {
			t.Run(raw+repair, func(t *testing.T) {
				now := time.Now()
				svc, store, fake := supervised(t, archivetier.Verifying, now.Add(-time.Hour))
				// Write the actual persisted document, then use startup's OpenStore
				// path. A typed override error must not reject the whole document.
				doc, err := json.Marshal(store.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(doc, &fields); err != nil {
					t.Fatal(err)
				}
				fields["selectionOverrides"] = json.RawMessage(raw)
				doc, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(store.dir, "state.json"), doc, 0600); err != nil {
					t.Fatal(err)
				}
				store.Close()
				store.lock = nil
				reopened, err := OpenStore(store.dir)
				if err != nil {
					t.Fatalf("startup rejected isolated overrides: %v", err)
				}
				defer reopened.Close()
				svc.Store = reopened
				runner := svc.Archive.(*archivetier.Runner)
				runner.Store.LoadState, runner.Store.SaveState = reopened.LoadArchive, reopened.SaveArchive
				runner.Store.AuditRecord = reopened.Audit
				if reopened.WriteAllowed() != nil || reopened.Snapshot().Archive.Job.Phase != archivetier.Verifying || reopened.Snapshot().Leases["radarr:293"].Owner != "archive" {
					t.Fatal("bad overrides lost job, lease, or admission")
				}
				w := selectionCall(svc, "GET", "/archive/selection", "", "secret", "")
				var settings SelectionSettings
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &settings) != nil || settings.Warning == "" || !reflect.DeepEqual(settings.Effective, svc.Config.Archive.Selection) || string(settings.InvalidOverrides) != raw {
					t.Fatalf("settings cannot recover: %d %s", w.Code, w.Body)
				}
				if err := svc.Tick(context.Background(), now); err != nil {
					t.Fatal(err)
				}
				if reopened.Snapshot().Archive.Job.Phase != archivetier.Complete || !svc.Status().Healthy || !reflect.DeepEqual(runner.Config.Selection, svc.Config.Archive.Selection) {
					t.Fatal("startup did not run safely on the file policy")
				}
				w = selectionCall(svc, "GET", "/status", "", "secret", "")
				var status Status
				if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.SelectionWarning == "" {
					t.Fatalf("missing status warning: %s", w.Body)
				}
				// The tick's normal state write must retain the corrupt raw value.
				doc, err = os.ReadFile(filepath.Join(reopened.dir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(doc, &fields); err != nil {
					t.Fatal(err)
				}
				// JSON indentation may change, but the entire bad value must survive.
				b, _ := json.Marshal(json.RawMessage(fields["selectionOverrides"]))
				if string(b) != raw {
					t.Fatalf("bad raw overrides lost: %s", b)
				}
				w = selectionCall(svc, "PUT", "/archive/selection", repair, "secret", "application/json")
				if w.Code != 200 || svc.Status().SelectionWarning != "" {
					t.Fatalf("repair refused: %d %s", w.Code, w.Body)
				}
				settings, err = svc.selectionSettings()
				if err != nil || len(settings.InvalidOverrides) != 0 {
					t.Fatal("repair kept bad overrides")
				}
				if fake.searches+fake.grabs+fake.imports != 0 {
					t.Fatal("settings or reconciliation made new writes")
				}
			})
		}
	}
}

func TestSavedSelectionChangesNextNormalCycleEligibility(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 30, 0, 0, time.UTC)
	svc, store, fake := supervised(t, "", time.Time{})
	runner := svc.Archive.(*archivetier.Runner)
	svc.DryRun, runner.DryRun = true, true
	fake.original.Runtime = 120
	target := fake.original
	release := archivetier.Release{Guid: "fixture-cycle-release", Title: "Example Movie.1080p.WEB-DL.x265.DDP5.1-other", Size: 6 * archivetier.GB, Seeders: 100, IndexerID: 11, Protocol: "torrent", Flags: json.RawMessage(`1`), Languages: []archivetier.Language{{Name: "English"}}}
	release.Quality.Quality.Resolution = 1080
	release.Quality.Quality.Name = "WEBDL-1080p"
	decision := archivetier.Select(target, []archivetier.Release{release}, svc.Config.Archive)
	if decision.Release == nil || decision.Score.Total >= 100 {
		t.Fatalf("fixture needs a winner below 100: %+v", decision)
	}
	at := now.Add(-time.Hour)
	st := archivetier.State{Lookups: map[string]archivetier.Lookup{"radarr:293:0": {Decision: decision, Releases: []archivetier.Release{release}, At: at, Files: "1080p:120:2:292:77253337510"}}}
	if err := store.SaveArchive(st); err != nil {
		t.Fatal(err)
	}
	if err := svc.Tick(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if report := store.Snapshot().Archive.Report; len(report) != 1 || report[0].Release == nil {
		t.Fatalf("normal cycle did not select the initial winner: %+v", report)
	}
	w := selectionCall(svc, "PUT", "/archive/selection", `{"overrides":{"minScore":100}}`, "secret", "application/json")
	if w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	// A save rescores cached state immediately. The next cycle rebuilds Report
	// through the real runner and must keep the new eligibility decision.
	if err := svc.Tick(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	report := store.Snapshot().Archive.Report
	if len(report) != 1 || report[0].Release != nil || len(report[0].Candidates) != 1 || report[0].Candidates[0].Score.MinScore != 100 {
		t.Fatalf("next cycle ignored saved selection: %+v", report)
	}
	if fake.searches+fake.grabs+fake.imports != 0 {
		t.Fatal("cached cycle unexpectedly searched or wrote")
	}
}
