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
	"testing"
	"time"
)

type recoveringArr struct {
	*fakeArr
	grabErr    error
	historyErr error
	queueErr   error
	historyID  string
}

func (a *recoveringArr) Grab(ctx context.Context, rel Release) error {
	a.fakeArr.Grab(ctx, rel)
	return a.grabErr
}
func (a *recoveringArr) ReconcileDownload(context.Context, *Job) (string, error) {
	return a.historyID, a.historyErr
}
func (a *recoveringArr) Queue(context.Context) ([]QueueItem, error) { return a.queue, a.queueErr }

func TestGrabRefusalAndAmbiguousRecovery(t *testing.T) {
	for _, scenario := range []string{"404", "400", "found", "never seen", "conflict", "unavailable", "queue only", "different torrent name"} {
		t.Run(scenario, func(t *testing.T) {
			r, fake, _, now := runner(t)
			a := &recoveringArr{fakeArr: fake, grabErr: &HTTPError{Kind: "sonarr", Status: 503}}
			if scenario == "404" {
				a.grabErr = &HTTPError{Kind: "sonarr", Status: 404}
			}
			if scenario == "400" {
				a.grabErr = &HTTPError{Kind: "sonarr", Status: 400}
			}
			r.Arr["sonarr"] = a
			st := check(t, r, now)
			searches := a.searches
			if scenario == "404" || scenario == "400" {
				if testStateJob(st).Phase != Failed || !testStateJob(st).NonHalting || !Healthy(st) || testStateJob(st).HoldsLease() {
					t.Fatalf("refusal halted: %+v", st)
				}
				if !strings.Contains(testStateJob(st).Message, "Sonarr refused the grab") || strings.Contains(testStateJob(st).Message, RecoveryInspection) {
					t.Fatal(testStateJob(st).Message)
				}
				audit, _ := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
				if !strings.Contains(string(audit), `"grab_refused"`) {
					t.Fatal("refusal missing audit")
				}
				a.targets = nil
				st = check(t, r, now.Add(2*time.Minute))
				if !Healthy(st) || a.grabs != 1 {
					t.Fatal("next cycle halted or replayed")
				}
				next := target()
				next.ID = 2
				a.targets, a.current, a.grabErr = []Target{next}, next, nil
				r.Config.PlexKeys["sonarr:2"] = "901"
				st = check(t, r, now.Add(4*time.Minute))
				if testStateJob(st).Target.ID != 2 || testStateJob(st).Phase != Downloading || a.grabs != 2 || a.grabbed.SeriesID != 2 {
					t.Fatal("next title did not start after refusal")
				}
				return
			}
			if testStateJob(st).Phase != GrabIntent || !Healthy(st) || strings.Contains(testStateJob(st).Message, "recovery evidence") {
				t.Fatal("ambiguity not waiting")
			}
			switch scenario {
			case "found":
				a.historyID = "download"
				a.queue = []QueueItem{{SeriesID: testStateJob(st).Target.ID, Title: "torrent-name", DownloadID: "download", SizeLeft: 123}}
			case "queue only":
				a.queue = []QueueItem{{SeriesID: testStateJob(st).Target.ID, Title: testStateJob(st).Release.Title, IndexerID: testStateJob(st).Release.IndexerID, DownloadID: "download", SizeLeft: 123}}
			case "different torrent name":
				a.queue = []QueueItem{{SeriesID: testStateJob(st).Target.ID, Title: "torrent-name", DownloadID: "download", SizeLeft: 123}}
			case "conflict":
				a.historyErr = &EvidenceConflict{"selected release grabbed more than once"}
			case "unavailable":
				a.queueErr = errors.New("queue unavailable")
			}
			st, err := r.Check(context.Background(), now.Add(2*time.Minute))
			if scenario == "conflict" {
				if err == nil || testStateJob(st).Phase != Failed || testStateJob(st).NonHalting || Healthy(st) || !testStateJob(st).HoldsLease() {
					t.Fatal("conflict not halted")
				}
				if strings.Contains(testStateJob(st).Message, "after restart") || strings.Contains(testStateJob(st).Message, RecoveryInspection) {
					t.Fatal(testStateJob(st).Message)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if scenario == "found" || scenario == "queue only" {
				if testStateJob(st).Phase != Downloading || testStateJob(st).DownloadID != "download" {
					t.Fatal("not adopted")
				}
			}
			if scenario == "never seen" || scenario == "unavailable" || scenario == "different torrent name" {
				for _, mins := range []int{4, 14, 16} {
					st, err = r.Check(context.Background(), now.Add(time.Duration(mins)*time.Minute))
					if scenario == "different torrent name" && mins == 16 {
						if err == nil || testStateJob(st).Phase != Failed || testStateJob(st).NonHalting {
							t.Fatal("unmatched queue not halted")
						}
					} else if err != nil {
						t.Fatal(err)
					}
					if mins == 14 && testStateJob(st).Phase != GrabIntent {
						t.Fatal("window ended early")
					}
				}
				if scenario == "never seen" && (testStateJob(st).Phase != Failed || !testStateJob(st).NonHalting || !Healthy(st)) {
					t.Fatal("absence not clean")
				}
				if scenario == "unavailable" && (testStateJob(st).Phase != GrabIntent || testStateJob(st).GrabChecks != 0) {
					t.Fatal("failed reads established absence")
				}
			}
			if a.grabs != 1 || a.imports != 0 || a.searches != searches {
				t.Fatal("reconciliation performed writes or search")
			}
		})
	}
}

func TestClearFailedGuardsAndMessages(t *testing.T) {
	for _, scenario := range []string{"safe", "import", "library change", "queue", "history", "read error", "wrong id", "active", "too soon"} {
		t.Run(scenario, func(t *testing.T) {
			r, f, _, now := runner(t)
			a := &recoveringArr{fakeArr: f}
			r.Arr["sonarr"] = a
			j := &Job{Target: target(), Phase: Failed, Started: now.Add(-time.Hour), Message: "old grab failure", Release: Release{Title: "fixture release", IndexerID: 10}}
			switch scenario {
			case "import":
				j.ImportedAt = now
			case "library change":
				f.current = replacement()
			case "queue":
				a.queue = []QueueItem{{SeriesID: j.Target.ID, DownloadID: "other"}}
			case "history":
				a.historyID = "download"
			case "read error":
				a.queueErr = errors.New("read failed")
			case "active":
				j.Phase = GrabIntent
			case "too soon":
				j.Started = now
			}
			r.Store.Save(State{Job: j, Error: "archive halted", Night: "night"})
			id := JobID(*j)
			if scenario == "wrong id" {
				id = "other"
			}
			st, err := r.ClearFailed(context.Background(), now, id)
			if scenario == "safe" {
				if err != nil || testStateJob(st) != nil || st.Error != "" {
					t.Fatal("clear refused", err)
				}
				b, _ := os.ReadFile(filepath.Join(r.Store.Dir, "audit.jsonl"))
				if !strings.Contains(string(b), `"clear_intent"`) || !strings.Contains(string(b), `"cleared"`) {
					t.Fatal("clear audit missing")
				}
			} else if err == nil {
				t.Fatal("unsafe clear accepted")
			}
			if a.grabs != 0 || a.imports != 0 || a.searches != 0 {
				t.Fatal("clear performed external writes")
			}
		})
	}
	r, a, _, _ := runner(t)
	j := Job{Phase: GrabIntent, Target: target()}
	a.recoveryErr = errors.New("missing")
	if got := r.collectRecovery(context.Background(), &j, "one line"); got != "one line" {
		t.Fatal(got)
	}
	j.ImportedAt = time.Now()
	if got := r.collectRecovery(context.Background(), &j, "import failed"); !strings.Contains(got, RecoveryInspection) {
		t.Fatal("import checklist missing", got)
	}
}

func TestGrabFailureShape(t *testing.T) {
	var incident struct {
		Version int   `json:"version"`
		Archive State `json:"archive"`
		Leases  map[string]struct {
			Owner string `json:"owner"`
		} `json:"leases"`
		Audit []struct {
			At     time.Time `json:"at"`
			Action string    `json:"action"`
			Job    Job       `json:"job"`
			Detail struct {
				Result string `json:"result"`
			} `json:"detail"`
		} `json:"audit"`
	}
	b, err := os.ReadFile("testdata/grab-failure.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &incident); err != nil {
		t.Fatal(err)
	}
	j := testStateJob(incident.Archive)
	if incident.Version != 1 || j.Target.ID != 66 || j.Target.Season != 3 || j.Release.IndexerID != 10 || j.Release.Guid != "" || j.HasImportIntent() || incident.Leases["sonarr:66"].Owner != "archive" || len(incident.Audit) != 3 || incident.Audit[1].Detail.Result != "sonarr HTTP 404" {
		t.Fatal("incident shape changed")
	}
	pub := PublicState(incident.Archive)
	if strings.Contains(testStateJob(pub).Message, RecoveryInspection) || strings.Contains(testStateJob(pub).Message, "after restart") || strings.Contains(pub.Error, RecoveryInspection) {
		t.Fatal("legacy pre-import guidance leaked")
	}
	if testStateJob(pub).Message == j.Message {
		t.Fatal("legacy reason not shortened")
	}
	r, f, _, _ := runner(t)
	a := &recoveringArr{fakeArr: f}
	r.Arr["sonarr"] = a
	f.current = j.Target
	r.Store.Save(incident.Archive)
	st, err := r.ClearFailed(context.Background(), j.Started.Add(time.Hour), JobID(*j))
	if err != nil || testStateJob(st) != nil {
		t.Fatal("synthetic failure cannot clear", err)
	}
}

func TestGrabHTTPStatusAndTargetPayload(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, status := range []int{400, 404, 408, 500, 503} {
			t.Run(kind+http.StatusText(status), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					var body map[string]any
					json.NewDecoder(req.Body).Decode(&body)
					field := "seriesId"
					if kind == "radarr" {
						field = "movieId"
					}
					if body["guid"] != "fresh-guid" || body["indexerId"] != float64(10) || body[field] != float64(66) {
						t.Errorf("payload: %+v", body)
					}
					w.WriteHeader(status)
					w.Write([]byte(`{"message":"Couldn't find requested release in cache, try searching again", "key":"fixture-secret"}`))
				}))
				defer srv.Close()
				c := NewClient(kind, srv.URL, "fixture", true)
				err := c.Grab(context.Background(), Release{Guid: "fresh-guid", IndexerID: 10, SeriesID: 66, MovieID: 66})
				var he *HTTPError
				if !errors.As(err, &he) || he.Status != status || strings.Contains(err.Error(), "fixture-secret") {
					t.Fatal("typed status lost or body leaked", err)
				}
				if status == 404 && he.Reason != "release missing from cache" {
					t.Fatal("safe diagnostic lost")
				}
			})
		}
	}
}

func TestReconcileHistoryIndexerAndContradictions(t *testing.T) {
	b, err := os.ReadFile("testdata/grab-history-sonarr.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtureRecord HistoryItem
	if err = json.Unmarshal(b, &fixtureRecord); err != nil {
		t.Fatal(err)
	}
	if fixtureRecord.EventType != "grabbed" || fixtureRecord.Data["indexer"] == "" || fixtureRecord.DownloadID == "" {
		t.Fatal("fixtureRecord grab contract missing")
	}
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"found", "duplicate episode rows", "multiple downloads", "wrong indexer", "missing indexer", "read error", "old event", "wrong title", "wrong target"} {
			t.Run(kind+scenario, func(t *testing.T) {
				h := fixtureRecord
				h.ID, h.SeriesID, h.MovieID = 1, 66, 66
				h.Data = map[string]string{"indexer": fixtureRecord.Data["indexer"]}
				j := &Job{Target: Target{Kind: kind, ID: 66}, Started: h.Date, Release: Release{Title: h.SourceTitle, IndexerID: 10}}
				switch scenario {
				case "wrong indexer":
					h.Data["indexer"] = "different"
				case "missing indexer":
					h.Data = map[string]string{}
				case "old event":
					h.Date = j.Started.Add(-2 * time.Minute)
				case "wrong title":
					h.SourceTitle = "other"
				case "wrong target":
					h.SeriesID, h.MovieID = 67, 67
				}
				raw, _ := json.Marshal(h)
				records := []json.RawMessage{raw}
				if scenario == "duplicate episode rows" || scenario == "multiple downloads" {
					h.ID = 2
					if scenario == "multiple downloads" {
						h.DownloadID = "other"
					}
					raw, _ = json.Marshal(h)
					records = append(records, raw)
				}
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != "GET" {
						t.Error("reconciliation write")
					}
					switch r.URL.Path {
					case "/api/v3/history":
						field := "seriesIds"
						if kind == "radarr" {
							field = "movieIds"
						}
						if r.URL.Query().Get(field) != "66" || r.URL.Query().Get("sortKey") != "date" {
							t.Error("history query does not match the reviewed Arr controller")
						}
						if scenario == "read error" {
							w.WriteHeader(503)
							return
						}
						w.Write(pagedBody(t, r, records))
					case "/api/v3/indexer/10":
						json.NewEncoder(w).Encode(map[string]any{"id": 10, "name": fixtureRecord.Data["indexer"]})
					default:
						t.Errorf("unexpected path %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer srv.Close()
				c := NewClient(kind, srv.URL, "fixture", false)
				id, err := c.ReconcileDownload(context.Background(), j)
				var conflict *EvidenceConflict
				switch scenario {
				case "found", "duplicate episode rows":
					if err != nil || id != fixtureRecord.DownloadID {
						t.Fatal("history not adopted", err)
					}
				case "multiple downloads", "wrong indexer", "old event":
					if !errors.As(err, &conflict) {
						t.Fatal("contradiction missed", err)
					}
				case "read error", "missing indexer":
					if err == nil || errors.As(err, &conflict) {
						t.Fatal("unknown evidence treated as proof", err)
					}
				default:
					if err != nil || id != "" {
						t.Fatal("unrelated history adopted", id, err)
					}
				}
			})
		}
	}
}

func TestPreviouslyQueuedGrabCannotBecomeCleanAbsence(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		t.Run(strconv.FormatBool(readFailure), func(t *testing.T) {
			r, f, _, now := runner(t)
			a := &recoveringArr{fakeArr: f, grabErr: errors.New("response lost")}
			r.Arr["sonarr"] = a
			st := check(t, r, now)
			a.queue = []QueueItem{{SeriesID: st.Job.Target.ID, Title: "torrent-name", DownloadID: "download"}}
			if readFailure {
				a.historyErr = errors.New("history unavailable")
			}
			check(t, r, now.Add(2*time.Minute))
			a.queue = nil
			a.historyErr = nil
			check(t, r, now.Add(4*time.Minute))
			check(t, r, now.Add(6*time.Minute))
			st, err := r.Check(context.Background(), now.Add(16*time.Minute))
			if err == nil || testStateJob(st).CleanFailure() || !testStateJob(st).HoldsLease() {
				t.Fatal("observed download was forgotten", err)
			}
			if _, err := r.ClearFailed(context.Background(), now.Add(time.Hour), JobID(*testStateJob(st))); err == nil {
				t.Fatal("unresolved download cleared")
			}
		})
	}
}

func TestGrabRequestTimeoutIsReconciled(t *testing.T) {
	r, f, _, now := runner(t)
	a := &recoveringArr{fakeArr: f, grabErr: &HTTPError{Kind: "sonarr", Status: 408}}
	r.Arr["sonarr"] = a
	st := check(t, r, now)
	if st.Job.Phase != GrabIntent || !st.Job.HoldsLease() {
		t.Fatal("timeout assumed to be a refusal")
	}
	a.historyID = "accepted"
	st = check(t, r, now.Add(2*time.Minute))
	if st.Job.DownloadID != "accepted" || a.grabs != 1 {
		t.Fatal("accepted timeout was not adopted")
	}
}
