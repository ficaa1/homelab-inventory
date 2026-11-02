package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestArchiveTagCreation(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"missing", "exists", "conflict race", "duplicate", "still missing", "existing duplicate", "dry run", "both missing", "untag"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				tags := []tag{{ID: 19, Label: "4k"}}
				if scenario == "exists" {
					tags = append(tags, tag{7, "ARCHIVE"})
				}
				if scenario == "existing duplicate" {
					tags = append(tags, tag{7, "archive"}, tag{8, "ARCHIVE"})
				}
				if scenario == "both missing" || scenario == "untag" {
					tags = []tag{}
				}
				created := []string{}
				edits := []map[string]any{}
				afterCreateReads := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v3/system/status":
						name := "Radarr"
						if kind == "sonarr" {
							name = "Sonarr"
						}
						json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": DefaultConfig().ReviewedVersions[kind]})
					case "/api/v3/tag":
						if r.Method == "GET" {
							if len(created) > 0 {
								afterCreateReads++
							}
							json.NewEncoder(w).Encode(tags)
							return
						}
						var body map[string]string
						if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || (body["label"] != "archive" && body["label"] != "4k") {
							t.Error("unexpected tag write")
						}
						label := body["label"]
						created = append(created, label)
						if scenario != "still missing" {
							tags = append(tags, tag{7 + len(created), strings.ToUpper(label)})
							if scenario == "duplicate" {
								tags = append(tags, tag{90, label})
							}
						}
						if scenario == "conflict race" {
							w.WriteHeader(400)
							w.Write([]byte(`[{"propertyName":"Label","errorMessage":"Tag already exists"}]`))
							return
						}
						// Ignore this ID: a racing creator's inventory is the authority.
						w.Write([]byte(`{"id":999,"label":"archive"}`))
					case "/api/v3/series/editor", "/api/v3/movie/editor":
						wantPath := "/api/v3/movie/editor"
						if kind == "sonarr" {
							wantPath = "/api/v3/series/editor"
						}
						if r.Method != "PUT" || r.URL.Path != wantPath {
							t.Error("wrong editor")
						}
						var body map[string]any
						if json.NewDecoder(r.Body).Decode(&body) != nil {
							t.Error("bad editor JSON")
						}
						edits = append(edits, body)
						w.WriteHeader(204)
					default:
						t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer srv.Close()
				c := NewClient(kind, srv.URL, "fixture", scenario != "dry run")
				err := c.SetArchiveTags(context.Background(), 8, scenario != "untag", scenario == "both missing")
				refused := scenario == "duplicate" || scenario == "still missing" || scenario == "existing duplicate" || scenario == "dry run"
				if (err != nil) != refused {
					t.Fatalf("error=%v", err)
				}
				wantCreated := []string{"archive"}
				if scenario == "both missing" {
					wantCreated = append(wantCreated, "4k")
				}
				if scenario == "exists" || scenario == "existing duplicate" || scenario == "dry run" || scenario == "untag" {
					wantCreated = []string{}
				}
				if !reflect.DeepEqual(created, wantCreated) {
					t.Fatalf("created=%v want=%v", created, wantCreated)
				}
				if refused || scenario == "untag" {
					if len(edits) != 0 {
						t.Fatalf("refused/no-op edited title: %v", edits)
					}
					return
				}
				if len(created) > 0 && afterCreateReads < len(created) {
					t.Fatal("creation not reread")
				}
				wantAdd := []any{float64(8)}
				if scenario == "exists" {
					wantAdd = []any{float64(7)}
				}
				if scenario == "both missing" {
					wantAdd = append(wantAdd, float64(9))
				}
				if len(edits) == 0 || edits[len(edits)-1]["applyTags"] != "add" || !reflect.DeepEqual(edits[len(edits)-1]["tags"], wantAdd) {
					t.Fatalf("wrong resolved IDs: %v", edits)
				}
			})
		}
	}
}

func TestMissingArchiveTagReadsAndRetry(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Error("missing-tag read wrote")
				}
				switch r.URL.Path {
				case "/api/v3/system/status":
					name := "Radarr"
					if kind == "sonarr" {
						name = "Sonarr"
					}
					json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": DefaultConfig().ReviewedVersions[kind]})
				case "/api/v3/tag":
					w.Write([]byte(`[]`))
				case "/api/v3/movie":
					w.Write([]byte(`[{"id":8,"title":"Movie","path":"/movies/a","tags":[],"movieFile":{"id":9,"size":100}}]`))
				case "/api/v3/series":
					w.Write([]byte(`[{"id":8,"title":"Series","path":"/tv/a","tags":[],"statistics":{"sizeOnDisk":100},"seasons":[]}]`))
				case "/api/v3/queue":
					json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 0, "records": []any{}})
				default:
					t.Errorf("unexpected call %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := NewClient(kind, srv.URL, "fixture", true)
			ctx := context.Background()
			rows, err := c.Library(ctx)
			if err != nil || len(rows) != 1 || rows[0].Tags.Archive {
				t.Fatalf("library: %v %v", rows, err)
			}
			targets, err := c.Targets(ctx)
			if err != nil || targets == nil || len(targets) != 0 {
				t.Fatalf("targets: %v %v", targets, err)
			}
			target := Target{Kind: kind, ID: 8}
			if opted, err := c.Opted(ctx, target); opted || err != nil {
				t.Fatalf("opted: %v %v", opted, err)
			}
			if err := c.RemoveArchiveTag(ctx, target); err != nil {
				t.Fatal(err)
			}
			if err := c.SetArchiveTags(ctx, 8, false, true); err != nil {
				t.Fatal(err)
			}
			r, _, _, now := runner(t)
			r.Arr[kind] = c
			other := "sonarr"
			if kind == "sonarr" {
				other = "radarr"
			}
			r.Arr[other] = &fakeArr{}
			if err := r.Store.Save(State{TagRemovals: map[string]Target{target.Key(): target}}); err != nil {
				t.Fatal(err)
			}
			st := check(t, r, now)
			if !Healthy(st) || len(st.Report) != 0 || st.Job != nil || st.TagRemovalError != "" || len(st.TagRemovals) != 0 {
				t.Fatalf("scan/retry unhealthy: %+v", st)
			}
		})
	}
}

func TestArchiveTagConcurrentCreators(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			posts, edits := 0, 0
			results := []TagCreateResult{}
			bothInFlight := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/system/status":
					name := "Radarr"
					if kind == "sonarr" {
						name = "Sonarr"
					}
					json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": DefaultConfig().ReviewedVersions[kind]})
				case "/api/v3/tag":
					mu.Lock()
					if r.Method == "GET" {
						ready := posts == 2
						mu.Unlock()
						if ready {
							w.Write([]byte(`[{"id":7,"label":"archive"}]`))
						} else {
							w.Write([]byte(`[]`))
						}
						return
					}
					posts++
					n := posts
					if n == 2 {
						close(bothInFlight)
					}
					mu.Unlock()
					// Neither create may return until both POSTs reach the server.
					select {
					case <-bothInFlight:
					case <-r.Context().Done():
						return
					}
					if n == 1 {
						w.WriteHeader(http.StatusCreated)
					} else {
						w.WriteHeader(http.StatusConflict)
					}
				case "/api/v3/movie/editor", "/api/v3/series/editor":
					var body struct {
						Tags []int `json:"tags"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "PUT" || !reflect.DeepEqual(body.Tags, []int{7}) {
						t.Errorf("incorrect editor write: %s %+v", r.Method, body)
					}
					mu.Lock()
					edits++
					mu.Unlock()
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			c := NewClient(kind, srv.URL, "fixture", true)
			c.TagCreateResult = func(result TagCreateResult) error {
				mu.Lock()
				defer mu.Unlock()
				results = append(results, result)
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 2)
			for range 2 {
				go func() { done <- c.SetArchiveTags(ctx, 8, true, false) }()
			}
			for range 2 {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if posts != 2 || edits != 2 || len(results) != 2 {
				t.Fatalf("posts=%d edits=%d results=%+v", posts, edits, results)
			}
			conflicts := 0
			for _, result := range results {
				if result.ID != 7 {
					t.Fatalf("wrong resolved ID: %+v", result)
				}
				if result.Error != "" {
					conflicts++
					if result.Error != kind+" HTTP 409" || result.ResolvedBy != "existing tag" {
						t.Fatalf("conflict outcome lost: %+v", result)
					}
				}
			}
			if conflicts != 1 {
				t.Fatalf("conflicts=%d", conflicts)
			}
		})
	}
}
