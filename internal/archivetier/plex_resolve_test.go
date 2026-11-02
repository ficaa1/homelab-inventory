package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func resolverServer(t *testing.T, duplicate bool) (*Plex, *int, *bool) {
	t.Helper()
	calls := 0
	broken := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("non-GET: %s", r.Method)
		}
		calls++
		var data any
		switch r.URL.Query().Get("cmd") {
		case "get_library_names":
			data = []any{map[string]any{"section_id": 1, "section_type": "movie"}, map[string]any{"section_id": "2", "section_type": "show"}}
		case "get_library_media_info":
			if r.URL.Query().Get("refresh") != "true" {
				t.Error("first page must refresh Tautulli stale listing cache")
			}
			if broken {
				data = map[string]any{"recordsFiltered": "2", "data": []any{}}
				break
			}
			rows := []any{map[string]any{"rating_key": "15850", "media_type": "movie"}}
			if r.URL.Query().Get("section_id") == "2" {
				rows = []any{map[string]any{"rating_key": 17343, "media_type": "show"}}
			} else if duplicate {
				rows = append(rows, map[string]any{"rating_key": "15851", "media_type": "movie"})
			}
			data = map[string]any{"recordsFiltered": len(rows), "recordsTotal": len(rows), "data": rows}
		case "get_metadata":
			key := r.URL.Query().Get("rating_key")
			typ, guids := "movie", []string{"tmdb://858024", "imdb://tt14905854"}
			if key == "17343" {
				typ, guids = "show", []string{"tvdb://403245", "imdb://tt14688458"}
			}
			data = map[string]any{"rating_key": key, "media_type": typ, "guids": guids}
		case "get_activity":
			data = map[string]any{"stream_count": "0", "sessions": []any{}}
		case "get_history":
			data = map[string]any{"recordsFiltered": 1, "data": []any{map[string]any{"id": 1, "date": time.Now().Unix(), "rating_key": "15850", "media_type": "movie"}}}
		default:
			t.Errorf("unexpected command %s", r.URL.Query().Get("cmd"))
		}
		json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "success", "data": data}})
	}))
	t.Cleanup(srv.Close)
	return NewPlex(srv.URL, "test-key"), &calls, &broken
}

func TestPlexExternalIdentityCacheAndRefresh(t *testing.T) {
	p, calls, broken := resolverServer(t, false)
	now := time.Now().UTC()
	targets := []Target{{Kind: "radarr", ID: 293, Title: "different localized name", TMDBID: 858024}, {Kind: "sonarr", ID: 92, TVDBID: 403245}, {Kind: "radarr", ID: 294, Title: "Example Movie", TMDBID: 1}}
	st := State{}
	if err := p.ResolveTargets(context.Background(), targets, &st, now); err != nil {
		t.Fatal(err)
	}
	if st.PlexIdentities["radarr:293"].Key != "15850" || st.PlexIdentities["sonarr:92"].Key != "17343" || st.PlexIdentities["radarr:294"].Reason != "not found in Plex" {
		t.Fatalf("wrong external-ID resolution: %+v", st.PlexIdentities)
	}
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Save(st); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	before := *calls
	if err = p.ResolveTargets(context.Background(), targets, &persisted, now.Add(6*24*time.Hour)); err != nil || *calls != before {
		t.Fatalf("persisted cache not reused: err=%v calls=%d/%d", err, *calls, before)
	}
	// Changing an Arr external ID must not reuse the old per-title match.
	targets[0].TMDBID = 2
	if err = p.ResolveTargets(context.Background(), targets, &persisted, now.Add(time.Hour)); err != nil || persisted.PlexIdentities["radarr:293"].Key != "" {
		t.Fatal("changed identity retained old key", err)
	}
	*broken = true
	if err = p.ResolveTargets(context.Background(), targets, &persisted, now.Add(7*24*time.Hour)); err == nil {
		t.Fatal("expired cache accepted incomplete refresh")
	}
	if persisted.PlexIdentities["radarr:293"].Key != "" {
		t.Fatal("stale identity used after failed refresh")
	}
}

func TestPlexAmbiguousAndOverride(t *testing.T) {
	p, _, _ := resolverServer(t, true)
	r := Runner{Plex: p, Config: DefaultConfig()}
	st := State{}
	targets := []Target{{Kind: "radarr", ID: 293, TMDBID: 858024}}
	r.resolvePlex(context.Background(), targets, &st, time.Now())
	if targets[0].PlexKey != "" || targets[0].PlexReason != "ambiguous Plex identity (more than one match)" {
		t.Fatalf("ambiguity accepted: %+v", targets)
	}
	r.Config.PlexKeys = map[string]string{"radarr:293": "override"}
	r.resolvePlex(context.Background(), targets, &st, time.Now())
	if targets[0].plexKey(r.Config) != "override" || targets[0].PlexReason != "" {
		t.Fatal("explicit mapping did not override ambiguity")
	}
}

func TestRunnerResolvedPlexGuardsWatchedWithoutSearch(t *testing.T) {
	r, a, _, now := runner(t)
	p, _, _ := resolverServer(t, false)
	r.Plex = p
	r.DryRun = true
	r.NoSearch = true
	// The fake Arr's title is a movie here; its name is irrelevant.
	a.targets = []Target{movieTarget()}
	a.targets[0].TMDBID = 858024
	r.Arr = map[string]ArrAPI{"sonarr": &fakeArr{}, "radarr": a}
	r.Config.PlexKeys = nil
	st := check(t, r, now)
	if len(st.Report) != 1 || st.Report[0].Reason != "watched within idle window" || st.Report[0].Target.PlexKey != "15850" {
		t.Fatalf("resolved identity did not guard playback: %+v", st.Report)
	}
	if a.searches != 0 || a.grabs != 0 {
		t.Fatal("read-only resolution triggered release activity")
	}
}

func TestArrExternalIDsReachArchiveTargets(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatal("unexpected write")
				}
				switch r.URL.Path {
				case "/api/v3/tag":
					w.Write([]byte(`[{"id":1,"label":"archive"}]`))
				case "/api/v3/movie", "/api/v3/series":
					w.Write([]byte(`[{"id":293,"title":"localized","tmdbId":858024,"tvdbId":403245,"imdbId":"tt14905854","tags":[1]}]`))
				case "/api/v3/episode":
					w.Write([]byte(`[{"id":1,"seriesId":293,"seasonNumber":1,"episodeFileId":1}]`))
				case "/api/v3/episodefile":
					w.Write([]byte(`[{"id":1,"seasonNumber":1,"path":"/data/file.mkv","size":10}]`))
				default:
					t.Errorf("unexpected read %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			targets, err := NewClient(kind, srv.URL, "test-key", false).Targets(context.Background())
			if err != nil || len(targets) != 1 || targets[0].TMDBID != 858024 || targets[0].TVDBID != 403245 || targets[0].IMDBID != "tt14905854" {
				t.Fatalf("external IDs lost: %+v %v", targets, err)
			}
		})
	}
}

func TestPlexLibraryPaginationAndIncompleteMetadata(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "missing metadata"}[broken], func(t *testing.T) {
			pages := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var data any
				switch r.URL.Query().Get("cmd") {
				case "get_library_names":
					data = []any{map[string]any{"section_id": "1", "section_type": "movie"}}
				case "get_library_media_info":
					pages++
					start := 0
					if r.URL.Query().Get("start") == "100" {
						start = 100
						if r.URL.Query().Has("refresh") {
							t.Error("refresh repeated during pagination")
						}
					} else if r.URL.Query().Get("refresh") != "true" {
						t.Error("missing initial refresh")
					}
					rows := []any{}
					for i := start; i < start+100 && i < 101; i++ {
						rows = append(rows, map[string]any{"rating_key": i + 1, "media_type": "movie"})
					}
					data = map[string]any{"recordsFiltered": "101", "data": rows}
				case "get_metadata":
					key := r.URL.Query().Get("rating_key")
					data = map[string]any{"rating_key": key, "media_type": "movie", "guids": []string{"tmdb://" + key}}
					if broken && key == "101" {
						data = map[string]any{}
					}
				default:
					t.Error("unexpected API")
				}
				json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "success", "data": data}})
			}))
			defer srv.Close()
			st := State{}
			err := NewPlex(srv.URL, "test-key").ResolveTargets(context.Background(), []Target{{Kind: "radarr", ID: 1, TMDBID: 101}}, &st, time.Now())
			if pages != 2 {
				t.Fatalf("expected two listing pages, got %d", pages)
			}
			if broken {
				if err == nil || st.PlexIdentities["radarr:1"].Key != "" || len(st.PlexLibraries) != 0 {
					t.Fatal("partial metadata scan trusted", err)
				}
			} else if err != nil || st.PlexIdentities["radarr:1"].Key != "101" {
				t.Fatal("last page title unresolved", err)
			}
		})
	}
}

func TestResumedImportDoesNotResolvePlexIdentity(t *testing.T) {
	r, a, _, now := runner(t)
	a.targets[0].TVDBID = 403245
	started := check(t, r, now)
	if started.Job == nil || started.Job.Phase != Downloading {
		t.Fatal("fixture did not start download")
	}
	a.queue = []QueueItem{{SeriesID: 1, Title: started.Job.Release.Title, DownloadID: "hash", State: "importPending"}}
	p, _, _ := resolverServer(t, false)
	r.Plex = p
	r.Config.PlexKeys = nil
	r.Now = func() time.Time { return now.Add(time.Minute) }
	resumed := check(t, r, now.Add(time.Minute))
	if resumed.Job.Phase != Verifying || resumed.Job.Target.PlexKey != started.Job.Target.PlexKey || a.imports != 1 {
		t.Fatalf("resumed import did not use resolved identity: phase=%s key=%s imports=%d pause=%s", resumed.Job.Phase, resumed.Job.Target.PlexKey, a.imports, resumed.Paused)
	}
}
