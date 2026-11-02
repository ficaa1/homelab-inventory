package mediapolicy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func TestTagCreationWriteBarrier(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				b, _ := io.ReadAll(r.Body)
				audit, err := os.ReadFile(filepath.Join(store.dir, "audit.jsonl"))
				if err != nil || !strings.Contains(string(audit), "archive_tag_create_intent") || !strings.Contains(string(audit), kind) || !archivetier.TagCreateAllowed(b) {
					t.Error("tag reached upstream without valid body and durable intent")
				}
				w.WriteHeader(201)
			}))
			defer upstream.Close()
			tr := &Transport{Apply: true, Store: store}
			if kind == "sonarr" {
				tr.SonarrOrigin = upstream.URL
			} else {
				tr.RadarrOrigin = upstream.URL
			}
			client := SharedHTTP(tr)
			for _, tc := range []struct {
				method, path, body, ct string
				allowed                bool
			}{
				{"POST", "/api/v3/tag", `{"label":"archive"}`, "application/json", true},
				{"POST", "/api/v3/tag", `{"label":"4k"}`, "application/json", true},
				{"POST", "/api/v3/tag", `{"label":"owner"}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"label":"ARCHIVE"}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"label":"archive","id":4}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"label":"archive","label":"4k"}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"Label":"archive"}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"label":null}`, "application/json", false},
				{"POST", "/api/v3/tag", `{"label":"archive"} {}`, "application/json", false},
				{"POST", "/api/v3/tag", `[{"label":"archive"}]`, "application/json", false},
				{"POST", "/api/v3/tag", "{", "application/json", false},
				{"POST", "/api/v3/tag", `{"label":"archive"}`, "text/plain", false},
				{"POST", "/api/v3/tag", `{"label":"archive"}` + strings.Repeat(" ", 1024), "application/json", false},
				{"POST", "/api/v3/tag/", `{"label":"archive"}`, "application/json", false},
				{"POST", "/api/v3/tag/1", `{"label":"archive"}`, "application/json", false},
				{"POST", "/api/v3/%74ag", `{"label":"archive"}`, "application/json", false},
				{"POST", "/api/v3/tag?foo=1", `{"label":"archive"}`, "application/json", false},
				{"POST", "/api/v3/tag?", `{"label":"archive"}`, "application/json", false},
				{"PUT", "/api/v3/tag", `{"label":"archive"}`, "application/json", false},
				{"DELETE", "/api/v3/tag", `{"label":"archive"}`, "application/json", false},
			} {
				req, _ := http.NewRequest(tc.method, upstream.URL+tc.path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", tc.ct)
				resp, err := client.Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				if (err == nil) != tc.allowed {
					t.Errorf("%s %s %s: %v", tc.method, tc.path, tc.body, err)
				}
			}
			// Verify Body is authoritative if GetBody is stale.
			req, _ := http.NewRequest("POST", upstream.URL+"/api/v3/tag", strings.NewReader(`{"label":"archive"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Body = io.NopCloser(strings.NewReader(`{"label":"owner"}`))
			if _, err := client.Do(req); err == nil {
				t.Fatal("stale GetBody accepted")
			}
			for _, scenario := range []string{"dry run", "wrong origin", "no store", "poisoned"} {
				copy := *tr
				switch scenario {
				case "dry run":
					copy.Apply = false
				case "wrong origin":
					copy.SonarrOrigin = "http://other.invalid"
					copy.RadarrOrigin = "http://other.invalid"
				case "no store":
					copy.Store = nil
				case "poisoned":
					store.poison = io.ErrClosedPipe
				}
				req, _ := http.NewRequest("POST", upstream.URL+"/api/v3/tag", strings.NewReader(`{"label":"archive"}`))
				req.Header.Set("Content-Type", "application/json")
				if _, err := SharedHTTP(&copy).Do(req); err == nil {
					t.Fatalf("accepted %s", scenario)
				}
			}
			if calls != 2 {
				t.Fatalf("upstream writes=%d", calls)
			}
		})
	}
}

func TestTagCreateIntentFailureRefusesHTTP(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := os.Mkdir(filepath.Join(store.dir, "audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer up.Close()
	req, _ := http.NewRequest("POST", up.URL+"/api/v3/tag", strings.NewReader(`{"label":"archive"}`))
	req.Header.Set("Content-Type", "application/json")
	_, err = SharedHTTP(&Transport{Apply: true, Store: store, SonarrOrigin: up.URL}).Do(req)
	if err == nil || calls != 0 || store.WriteAllowed() == nil {
		t.Fatal("intent failure did not refuse/poison writes")
	}
}

func TestDashboardMissingTagsAndEarlyRefusals(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"create", "exists", "dry run", "disabled", "active", "failed", "approval", "duplicate", "missing", "untag"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				s, _ := libraryService(t)
				writes, reads := 0, 0
				tags := []map[string]any{}
				if scenario == "exists" {
					tags = append(tags, map[string]any{"id": 7, "label": "archive"}, map[string]any{"id": 19, "label": "4k"})
				}
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "GET" {
						reads++
					} else {
						writes++
					}
					switch r.URL.Path {
					case "/api/v3/system/status":
						name := "Radarr"
						if kind == "sonarr" {
							name = "Sonarr"
						}
						json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": archivetier.DefaultConfig().ReviewedVersions[kind]})
					case "/api/v3/tag":
						if r.Method == "POST" {
							var body map[string]string
							json.NewDecoder(r.Body).Decode(&body)
							if scenario != "missing" {
								tags = append(tags, map[string]any{"id": 7 + len(tags), "label": body["label"]})
							}
							if scenario == "duplicate" {
								tags = append(tags, map[string]any{"id": 90, "label": "ARCHIVE"})
							}
							w.WriteHeader(201)
						} else {
							json.NewEncoder(w).Encode(tags)
						}
					case "/api/v3/movie/editor", "/api/v3/series/editor":
						w.WriteHeader(204)
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
					}
				}))
				defer upstream.Close()
				c := archivetier.NewClient(kind, upstream.URL, "fixture", true)
				tr := &Transport{Apply: true, Store: s.Store}
				if kind == "sonarr" {
					tr.SonarrOrigin = upstream.URL
				} else {
					tr.RadarrOrigin = upstream.URL
				}
				c.HTTP = SharedHTTP(tr)
				s.LibraryClients[kind] = c
				early := false
				switch scenario {
				case "dry run":
					s.DryRun = true
					early = true
				case "disabled":
					s.Config.ArchiveEnabled = false
					early = true
				case "active", "failed":
					phase := archivetier.Downloading
					if scenario == "failed" {
						phase = archivetier.Failed
					}
					if err := s.Store.SaveArchive(archivetier.State{Job: &archivetier.Job{Target: archivetier.Target{Kind: kind, ID: 8}, Phase: phase}}); err != nil {
						t.Fatal(err)
					}
					early = true
				case "approval":
					if err := s.Store.SaveArchive(archivetier.State{Approvals: []archivetier.Approval{{ApprovalRequest: archivetier.ApprovalRequest{Kind: kind, ID: 8}}}}); err != nil {
						t.Fatal(err)
					}
					early = true
				}
				body := `{"kind":"` + kind + `","id":8,"archive":true,"fourK":true}`
				if scenario == "untag" {
					body = `{"kind":"` + kind + `","id":8,"archive":false,"fourK":true}`
				}
				rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture")
				refused := early || scenario == "duplicate" || scenario == "missing"
				if refused {
					var out map[string]string
					json.Unmarshal(rec.Body.Bytes(), &out)
					words := len(strings.Fields(out["error"]))
					if rec.Code != 409 || words < 2 || words > 5 || strings.ContainsAny(out["error"], ";—") {
						t.Fatalf("refusal: %d %s", rec.Code, rec.Body)
					}
				} else if rec.Code != 200 {
					t.Fatalf("tag: %d %s", rec.Code, rec.Body)
				}
				if early && (writes != 0 || reads != 0) {
					t.Fatalf("early refusal reached Arr: reads=%d writes=%d", reads, writes)
				}
				wantWrites := 3 // two tag creates, one editor add
				switch scenario {
				case "exists":
					wantWrites = 1
				case "duplicate", "missing":
					wantWrites = 1
				case "untag":
					wantWrites = 0
				}
				if early {
					wantWrites = 0
				}
				if writes != wantWrites {
					t.Fatalf("writes=%d want=%d", writes, wantWrites)
				}
			})
		}
	}
}

func TestDashboardTagCreateErrors(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, tc := range []struct {
			name        string
			status      int
			matches     int
			transport   bool
			getFails    bool
			duplicate   bool
			wantSuccess bool
		}{
			{name: "5xx then found", status: 503, matches: 1, wantSuccess: true},
			{name: "5xx then not found", status: 503},
			{name: "5xx then several matches", status: 503, matches: 2},
			{name: "transport error then found", transport: true, matches: 1, wantSuccess: true},
			{name: "transport error then GET fails", transport: true, getFails: true},
			{name: "plain 4xx rejection", status: 400, matches: 1},
			{name: "forbidden", status: 403, matches: 1},
			{name: "duplicate validation", status: 400, matches: 1, duplicate: true, wantSuccess: true},
			{name: "conflict", status: 409, matches: 1, wantSuccess: true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				s, _ := libraryService(t)
				posts, edits, readsAfter := 0, 0, 0
				tags := []map[string]any{}
				var outcomes []archivetier.TagCreateResult
				readAudit := func() ([]archivetier.TagCreateResult, map[string]json.RawMessage) {
					t.Helper()
					b, err := os.ReadFile(filepath.Join(s.Store.dir, "audit.jsonl"))
					if err != nil {
						t.Fatal(err)
					}
					results := []archivetier.TagCreateResult{}
					var final map[string]json.RawMessage
					for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
						var entry struct {
							Action string          `json:"action"`
							Detail json.RawMessage `json:"detail"`
						}
						if err := json.Unmarshal([]byte(line), &entry); err != nil {
							t.Fatal(err)
						}
						if entry.Action == "archive_tag_create_result" {
							var result archivetier.TagCreateResult
							if err := json.Unmarshal(entry.Detail, &result); err != nil {
								t.Fatal(err)
							}
							results = append(results, result)
						}
						if entry.Action == "archive_tag_result" {
							json.Unmarshal(entry.Detail, &final)
						}
					}
					return results, final
				}
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v3/system/status":
						name := "Radarr"
						if kind == "sonarr" {
							name = "Sonarr"
						}
						json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": archivetier.DefaultConfig().ReviewedVersions[kind]})
					case "/api/v3/tag":
						if r.Method == "GET" {
							if posts > 0 {
								readsAfter++
								if tc.getFails {
									w.WriteHeader(502)
									return
								}
							}
							json.NewEncoder(w).Encode(tags)
							return
						}
						posts++
						var body map[string]string
						json.NewDecoder(r.Body).Decode(&body)
						if posts > 1 {
							outcomes, _ = readAudit()
							if !tc.wantSuccess || len(outcomes) != 1 || outcomes[0].Error == "" || outcomes[0].ResolvedBy != "existing tag" {
								t.Error("second create preceded durable reconciliation")
							}
							tags = append(tags, map[string]any{"id": 19, "label": body["label"]})
							w.WriteHeader(201)
							return
						}
						if body["label"] != "archive" {
							t.Error("unexpected first label")
						}
						for i := 0; i < tc.matches; i++ {
							tags = append(tags, map[string]any{"id": 7 + i, "label": "archive"})
						}
						if tc.transport {
							conn, _, err := w.(http.Hijacker).Hijack()
							if err != nil {
								t.Error(err)
								return
							}
							conn.Close() // POST reached Arr; its response is lost.
							return
						}
						w.WriteHeader(tc.status)
						if tc.duplicate {
							w.Write([]byte(`[{"propertyName":"Label","errorMessage":"Tag already exists"}]`))
						} else {
							w.Write([]byte(`[{"propertyName":"Label","errorMessage":"upstream-private-details"}]`))
						}
					case "/api/v3/movie/editor", "/api/v3/series/editor":
						edits++
						outcomes, _ = readAudit()
						if !tc.wantSuccess || len(outcomes) != 2 || outcomes[0].Error == "" || outcomes[0].ResolvedBy != "existing tag" {
							t.Error("editor preceded durable reconciliation")
						}
						w.WriteHeader(204)
					default:
						t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
					}
				}))
				defer up.Close()
				c := archivetier.NewClient(kind, up.URL, "fixture", true)
				tr := &Transport{Apply: true, Store: s.Store}
				if kind == "sonarr" {
					tr.SonarrOrigin = up.URL
				} else {
					tr.RadarrOrigin = up.URL
				}
				c.HTTP = SharedHTTP(tr)
				s.LibraryClients[kind] = c
				rec := libraryRequest(s, "POST", "/archive/tag", `{"kind":"`+kind+`","id":8,"archive":true,"fourK":true}`, "fixture")
				wantPosts, wantEdits, wantCode := 1, 0, 409
				if tc.wantSuccess {
					wantPosts, wantEdits, wantCode = 2, 1, 200
				}
				if posts != wantPosts || edits != wantEdits || rec.Code != wantCode {
					t.Fatalf("posts=%d edits=%d response=%d %s", posts, edits, rec.Code, rec.Body)
				}
				if !tc.wantSuccess {
					var response map[string]string
					json.Unmarshal(rec.Body.Bytes(), &response)
					if words := len(strings.Fields(response["error"])); words < 2 || words > 5 {
						t.Fatalf("long refusal: %s", rec.Body)
					}
				}
				if (tc.status >= 500 || tc.transport || tc.duplicate || tc.status == 409) && readsAfter == 0 {
					t.Fatal("create outcome not reread")
				}
				outcomes, final := readAudit()
				if len(outcomes) != wantPosts {
					t.Fatalf("create outcomes missing: %+v", outcomes)
				}
				first := outcomes[0]
				wantError := fmt.Sprintf("%s HTTP %d", kind, tc.status)
				if tc.transport {
					wantError = kind + " transport failed"
				}
				if first.Kind != kind || first.Label != "archive" || first.Error != wantError {
					t.Fatalf("create error lost: %+v", first)
				}
				if tc.wantSuccess && (first.ResolvedBy != "existing tag" || first.ID != 7) || !tc.wantSuccess && (first.ResolvedBy != "" || first.ID != 0) {
					t.Fatalf("incorrect resolution: %+v", first)
				}
				if tc.getFails && first.ReadError != kind+" HTTP 502" || tc.matches == 2 && first.ReadError != "Tags need attention" {
					t.Fatalf("read error lost: %+v", first)
				}
				var applied bool
				json.Unmarshal(final["applied"], &applied)
				var finalCreates []archivetier.TagCreateResult
				json.Unmarshal(final["creates"], &finalCreates)
				if applied != tc.wantSuccess || !reflect.DeepEqual(finalCreates, outcomes) {
					t.Fatalf("final audit lost outcome: %+v", final)
				}
				if tc.wantSuccess {
					var response struct {
						Creates []archivetier.TagCreateResult `json:"creates"`
					}
					json.Unmarshal(rec.Body.Bytes(), &response)
					if !reflect.DeepEqual(response.Creates, outcomes) {
						t.Fatalf("response lost outcome: %s", rec.Body)
					}
				}
				b, _ := os.ReadFile(filepath.Join(s.Store.dir, "audit.jsonl"))
				if strings.Contains(string(b), "upstream-private-details") {
					t.Fatal("upstream body leaked to audit")
				}
			})
		}
	}
}
