package archivetier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type automaticArr struct {
	*fakeArr
	client *Client
	t      *testing.T
}

func (a *automaticArr) Lifecycle(ctx context.Context, j *Job, current Target) error {
	return a.client.Lifecycle(ctx, j, current)
}
func (a *automaticArr) Command(context.Context, int) (string, error) {
	a.t.Fatal("automatic import requested a ManualImport command status")
	return "", nil
}

// Reuse the synthetic Arr import-history shape, including data.fileId and
// importedPath, with the runner fixture's selected download and episode IDs.
func automaticHistory(t *testing.T, current Target, at time.Time) []map[string]any {
	t.Helper()
	raw, _ := fixtureHistory(t, current.Kind)
	var out []map[string]any
	for i, f := range current.Files {
		h := clone(raw)
		h["id"], h["seriesId"], h["movieId"] = 1000+i, current.ID, current.ID
		h["downloadId"], h["date"] = "hash", at.Format(time.RFC3339Nano)
		if current.Kind == "sonarr" {
			h["episodeId"] = current.Episodes[i].ID
		}
		d := h["data"].(map[string]any)
		d["fileId"], d["importedPath"] = strconv.Itoa(f.ID), f.Path
		out = append(out, h)
	}
	return out
}

func importingQueue(current Target) []json.RawMessage {
	b, _ := json.Marshal(QueueItem{ID: 1, SeriesID: current.ID, MovieID: current.ID, DownloadID: "hash", Status: "completed", State: "importing"})
	return []json.RawMessage{b}
}

func TestAutomaticImportCompletesThroughExistingProof(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"success", "approved", "invalid replacement", "missing physical file", "missing recovery", "other download", "missing grab", "changed grab", "missing date", "old date", "future episode date", "partial season", "queue changes during probe"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				if kind == "radarr" && scenario == "partial season" {
					t.Skip("Sonarr season coverage")
				}
				r, fake, _, now := runner(t)
				st := check(t, r, now)
				if kind == "radarr" {
					old := testStateJob(st).Target
					old.Kind, old.Season, old.Episodes = "radarr", 0, nil
					old.Files = old.Files[:1]
					testStateJob(st).Target, st.Report[0].Target = old, old
					if err := r.Store.Save(st); err != nil {
						t.Fatal(err)
					}
				}
				current := replacement()
				if kind == "radarr" {
					current.Kind, current.Season, current.Episodes = "radarr", 0, nil
					current.Files = current.Files[:1]
				}
				history := automaticHistory(t, current, now.Add(time.Minute))
				switch scenario {
				case "approved":
					testStateJob(st).ManuallyApproved = true
					if err := r.Store.Save(st); err != nil {
						t.Fatal(err)
					}
				case "invalid replacement":
					current.Files[0].Size = 50 * GB
				case "missing physical file":
					fake.present = false
				case "missing recovery":
					fake.recoveryErr = errors.New("missing original")
				case "other download":
					history[0]["downloadId"] = "unrelated"
				case "missing grab":
					fake.noGrabHistory = true
				case "missing date":
					delete(history[0], "date")
				case "old date":
					history[0]["date"] = now.Add(-time.Hour).Format(time.RFC3339)
				case "future episode date":
					history[len(history)-1]["date"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
				case "partial season":
					current.Files[0] = testStateJob(st).Target.Files[0]
				case "changed grab":
					testStateJob(st).DownloadID = "different"
					if err := r.Store.Save(st); err != nil {
						t.Fatal(err)
					}
				}
				var probing atomic.Bool
				srv := lifecycleServer(t, history, []json.RawMessage{})
				if scenario == "queue changes during probe" {
					handler := srv.Config.Handler
					srv.Close()
					srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
						if req.URL.Path == "/api/v3/queue" && probing.Load() {
							b, _ := json.Marshal(QueueItem{ID: 2, SeriesID: current.ID, MovieID: current.ID, DownloadID: "other", SizeLeft: GB})
							w.Write(pagedBody(t, req, []json.RawMessage{b}))
							return
						}
						handler.ServeHTTP(w, req)
					}))
					probe := r.Probe
					r.Probe = func(ctx context.Context, f File) (*MediaInfo, error) {
						probing.Store(true)
						return probe(ctx, f)
					}
				}
				defer srv.Close()
				fake.current = current
				r.Arr[kind] = &automaticArr{fakeArr: fake, client: NewClient(kind, srv.URL, "fixture", false), t: t}
				st, err := r.Check(context.Background(), now.Add(2*time.Minute))
				good := scenario == "success" || scenario == "approved"
				if good {
					if err != nil || testStateJob(st).Phase != Complete || testStateJob(st).CommandID != 0 || testStateJob(st).HoldsLease() || testStateJob(st).ActualBytes != current.Bytes() || len(testStateJob(st).Recovery) != len(testStateJob(st).Target.Files) || len(st.History) != 1 || st.CompletedFiles[targetKey(current)] != filesKey(current) {
						t.Fatalf("completion missing: %+v err=%v", testStateJob(st), err)
					}
					if !testStateJob(st).ImportedAt.Equal(now.Add(time.Minute)) || !st.History[0].UndoUntil.Equal(now.Add(time.Minute).AddDate(0, 0, UndoDays)) {
						t.Fatal("undo window lost the observed import date")
					}
					if st.Report[0].Reason != "already archived" || st.ProjectedSavings != 0 {
						t.Fatal("stale report")
					}
				} else if err == nil || testStateJob(st).Phase != Failed || !testStateJob(st).HoldsLease() {
					t.Fatalf("unproven replacement accepted: %+v err=%v", testStateJob(st), err)
				}
				if scenario == "queue changes during probe" {
					if !probing.Load() || !strings.Contains(testStateJob(st).Message, "remains in queue") || !testStateJob(st).Completed.IsZero() || len(st.CompletedFiles) != 0 || len(st.TagRemovals) != 0 {
						t.Fatalf("late queue conflict recorded success: %+v", testStateJob(st))
					}
					for _, h := range st.History {
						if h.Job.Phase == Complete {
							t.Fatal("late queue conflict recorded completed history")
						}
					}
				}
				if fake.grabs != 1 || fake.imports != 0 {
					t.Fatal("automatic import issued another grab/import")
				}
			})
		}
	}
}

func TestAutomaticImportWaitsForSeasonAndHistory(t *testing.T) {
	for _, scenario := range []string{"partial season", "history lags", "snapshot lags", "empty library", "queue lingers", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			r, fake, _, now := runner(t)
			st := check(t, r, now)
			current := replacement()
			history := automaticHistory(t, current, now.Add(time.Minute))
			queue := importingQueue(current)
			if scenario == "partial season" || scenario == "timeout" {
				current.Files[1], current.Episodes[1] = testStateJob(st).Target.Files[1], testStateJob(st).Target.Episodes[1]
				history = history[:1]
			} else if scenario == "history lags" {
				history = nil
			} else if scenario == "snapshot lags" {
				current.Files[1], current.Episodes[1] = testStateJob(st).Target.Files[1], testStateJob(st).Target.Episodes[1]
			} else if scenario == "empty library" {
				current.Files, history = nil, nil
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" {
					t.Error("automatic proof mutated Arr")
					w.WriteHeader(400)
					return
				}
				if req.URL.Path == "/api/v3/queue" {
					w.Write(pagedBody(t, req, queue))
					return
				}
				if req.URL.Path != "/api/v3/history" {
					t.Error("unexpected path")
					w.WriteHeader(400)
					return
				}
				records := []json.RawMessage{}
				for _, h := range history {
					b, _ := json.Marshal(h)
					records = append(records, b)
				}
				w.Write(pagedBody(t, req, records))
			}))
			defer srv.Close()
			fake.current = current
			for _, raw := range queue {
				var q QueueItem
				if err := json.Unmarshal(raw, &q); err != nil {
					t.Fatal(err)
				}
				fake.queue = append(fake.queue, q)
			}
			r.Arr["sonarr"] = &automaticArr{fakeArr: fake, client: NewClient("sonarr", srv.URL, "fixture", false), t: t}
			st = check(t, r, now.Add(2*time.Minute))
			if testStateJob(st).Phase == Complete || testStateJob(st).Phase == Failed || !testStateJob(st).HoldsLease() || st.Paused == "" || st.Error != "" {
				t.Fatalf("in-flight import failed: %+v", st)
			}
			if scenario == "timeout" {
				st, err := r.Check(context.Background(), now.Add(25*time.Hour))
				if err == nil || testStateJob(st).Phase != Failed || len(testStateJob(st).Recovery) == 0 {
					t.Fatalf("timeout lost recovery: %+v %v", testStateJob(st), err)
				}
				return
			}
			fake.current = replacement()
			history = automaticHistory(t, fake.current, now.Add(time.Minute))
			queue, fake.queue = []json.RawMessage{}, []QueueItem{}
			st = check(t, r, now.Add(3*time.Minute))
			if testStateJob(st).Phase != Complete || fake.grabs != 1 || fake.imports != 0 {
				t.Fatalf("import not adopted: %+v", testStateJob(st))
			}
			audit, err := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
			if err != nil || strings.Count(string(audit), `"action":"automatic_import_observed"`) != 1 || !strings.Contains(string(audit), `"action":"verified"`) {
				t.Fatalf("audit lost: %s %v", audit, err)
			}
		})
	}
}

func TestAutomaticImportDoesNotClearExistingStickyFailure(t *testing.T) {
	r, fake, _, now := runner(t)
	st := check(t, r, now)
	testStateJob(st).Phase, testStateJob(st).Message = Failed, "library changed during download; automatic replacement refused"
	if err := r.Store.Save(st); err != nil {
		t.Fatal(err)
	}
	current := replacement()
	srv := lifecycleServer(t, automaticHistory(t, current, now.Add(time.Minute)), []json.RawMessage{})
	defer srv.Close()
	fake.current = current
	r.Arr["sonarr"] = &automaticArr{fakeArr: fake, client: NewClient("sonarr", srv.URL, "fixture", false), t: t}
	st, err := r.Check(context.Background(), now.Add(2*time.Minute))
	if err != nil || testStateJob(st).Phase != Failed || testStateJob(st).CommandID > 0 || !testStateJob(st).HoldsLease() || fake.grabs != 1 || fake.imports != 0 {
		t.Fatalf("sticky failure was replayed: %+v %v", testStateJob(st), err)
	}
}

func TestRemovedImportStateFieldsRemainReadable(t *testing.T) {
	for _, phase := range []Phase{Downloading, Verifying, Failed} {
		t.Run(string(phase), func(t *testing.T) {
			r, fake, _, now := runner(t)
			st := check(t, r, now)
			testStateJob(st).Phase = phase
			b, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			var old map[string]any
			if err := json.Unmarshal(b, &old); err != nil {
				t.Fatal(err)
			}
			job := old["job"].(map[string]any)
			job["automaticImport"] = true
			job["libraryChangedAt"] = now.Add(time.Minute)
			job["importEvidenceWaitStarted"] = now
			b, _ = json.Marshal(old)
			if err := os.WriteFile(filepath.Join(r.Store.Dir, "state.json"), b, 0600); err != nil {
				t.Fatal(err)
			}
			saved, err := r.Store.Load()
			if err != nil || testStateJob(saved).ImportedAt.IsZero() || !testStateJob(saved).HasImportIntent() {
				t.Fatal("old recovery evidence lost", err)
			}
			if phase == Downloading {
				fake.queue = []QueueItem{{SeriesID: 1, DownloadID: "hash", State: "importPending"}}
				waiting := check(t, r, now.Add(time.Minute))
				if waiting.Job.Phase != Downloading || fake.imports != 0 || !waiting.Job.HoldsLease() {
					t.Fatal("recorded library change permitted another import")
				}
				fake.queue = []QueueItem{}
			}
			fake.current = replacement()
			srv := lifecycleServer(t, automaticHistory(t, fake.current, now.Add(time.Minute)), []json.RawMessage{})
			defer srv.Close()
			r.Arr["sonarr"] = &automaticArr{fakeArr: fake, client: NewClient("sonarr", srv.URL, "fixture", false), t: t}
			saved, err = r.Check(context.Background(), now.Add(2*time.Minute))
			if phase == Failed {
				if err != nil || testStateJob(saved).Phase != Failed || !testStateJob(saved).HoldsLease() {
					t.Fatal("old failure cleared")
				}
			} else if err != nil || testStateJob(saved).Phase != Complete || fake.imports != 0 {
				t.Fatal("old job not verified", err)
			}
		})
	}
}

type queueChangesDuringPreflight struct {
	*fakeArr
	next []QueueItem
}

func (a *queueChangesDuringPreflight) TargetPreflight(ctx context.Context, old Target, items []ManualItem, size int64) (string, []Evidence, error) {
	if len(items) > 0 {
		a.queue = a.next
	}
	return a.fakeArr.TargetPreflight(ctx, old, items, size)
}

func TestFinalImportCheckWaitsForEveryQueueRow(t *testing.T) {
	for _, state := range []string{"importing", "downloading", "drained"} {
		t.Run(state, func(t *testing.T) {
			r, fake, _, now := runner(t)
			check(t, r, now)
			pending := QueueItem{ID: 1, SeriesID: 1, DownloadID: "hash", State: "importPending"}
			fake.queue = []QueueItem{pending}
			other := pending
			other.ID, other.State = 2, state
			if state == "downloading" {
				other.SizeLeft = GB
			}
			a := &queueChangesDuringPreflight{fakeArr: fake, next: []QueueItem{pending, other}}
			if state == "drained" {
				a.next = []QueueItem{}
			}
			r.Arr["sonarr"] = a
			st := check(t, r, now.Add(time.Minute))
			if fake.imports != 0 || st.Job.Phase != Downloading || st.Paused == "" {
				t.Fatal("stale readiness submitted an import")
			}
		})
	}
}
