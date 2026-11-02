package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSetArchiveTags(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		t.Run(kind, func(t *testing.T) {
			writes := []map[string]any{}
			tags := `[{"id":7,"label":"archive"},{"id":19,"label":"4k"},{"id":99,"label":"owner"}]`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/system/status":
					name := "Radarr"
					if kind == "sonarr" {
						name = "Sonarr"
					}
					json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": DefaultConfig().ReviewedVersions[kind]})
				case "/api/v3/tag":
					w.Write([]byte(tags))
				default:
					p := "/api/v3/movie/editor"
					if kind == "sonarr" {
						p = "/api/v3/series/editor"
					}
					if r.Method != "PUT" || r.URL.Path != p {
						t.Error("unexpected write")
					}
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					writes = append(writes, body)
					w.WriteHeader(204)
				}
			}))
			defer srv.Close()
			c := NewClient(kind, srv.URL, "fixture", true)
			key := "movieIds"
			if kind == "sonarr" {
				key = "seriesIds"
			}
			for _, flags := range []struct{ a, k bool }{{true, false}, {true, true}, {false, true}, {false, false}} {
				writes = nil
				if err := c.SetArchiveTags(context.Background(), 8, flags.a, flags.k); err != nil {
					t.Fatal(err)
				}
				add, remove := []any{}, []any{}
				for _, v := range []struct {
					id float64
					on bool
				}{{7, flags.a}, {19, flags.k}} {
					if !flags.a && v.id == 19 {
						continue
					}
					if v.on {
						add = append(add, v.id)
					} else {
						remove = append(remove, v.id)
					}
				}
				for _, op := range writes {
					expected := remove
					if op["applyTags"] == "add" {
						expected = add
					}
					if len(op) != 3 || !reflect.DeepEqual(op[key], []any{float64(8)}) || !reflect.DeepEqual(op["tags"], expected) {
						t.Errorf("editor contract: %v", op)
					}
				}
				expectedCalls := 1
				if len(add) > 0 && len(remove) > 0 {
					expectedCalls = 2
				}
				if len(writes) != expectedCalls {
					t.Errorf("writes=%d", len(writes))
				}
			}
			writes = nil
			c.Apply = false
			if err := c.SetArchiveTags(context.Background(), 8, true, true); err == nil || len(writes) != 0 {
				t.Fatal("dry-run wrote")
			}
			c.Apply = true
			for _, p := range []string{"/series/editor/extra", "/movie/editor/extra", "/tag", "/series/1", "/movie/1"} {
				if c.call(context.Background(), http.MethodPut, p, nil, nil, nil) == nil {
					t.Errorf("allowed %s", p)
				}
			}
		})
	}
}

func TestLibraryArrInventory(t *testing.T) {
	for _, kind := range []string{"radarr", "sonarr"} {
		t.Run(kind, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Fatal("inventory wrote")
				}
				switch r.URL.Path {
				case "/api/v3/tag":
					w.Write([]byte(`[{"id":7,"label":"archive"},{"id":19,"label":"4k"}]`))
				case "/api/v3/movie":
					w.Write([]byte(`[{"id":8,"title":"Movie","year":2025,"path":"/movies/Movie","runtime":120,"tags":[7,19],"sizeOnDisk":123,"movieFile":{"id":9,"size":123,"quality":{"quality":{"name":"Bluray-2160p","resolution":2160}},"mediaInfo":{"videoCodec":"HEVC","videoDynamicRangeType":"HDR10"}}},{"id":10,"title":"Missing","path":"/movies/Missing","tags":[]}]`))
				case "/api/v3/series":
					w.Write([]byte(`[{"id":8,"title":"Series","year":2023,"path":"/tv/Series","tags":[7],"statistics":{"sizeOnDisk":300},"seasons":[{"seasonNumber":2,"statistics":{"sizeOnDisk":200}},{"seasonNumber":1,"statistics":{"sizeOnDisk":100}},{"seasonNumber":3,"statistics":{"sizeOnDisk":0}}]}]`))
				default:
					t.Errorf("unexpected inventory request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			c := NewClient(kind, srv.URL, "fixture", false)
			items, err := c.Library(context.Background())
			if err != nil || len(items) != 1 {
				t.Fatalf("inventory: %v %v", items, err)
			}
			item := items[0]
			if !item.Tags.Archive || item.ID != 8 || item.Year == 0 {
				t.Fatalf("metadata missing: %+v", item)
			}
			if kind == "radarr" && (item.Resolution != 2160 || item.VideoCodec != "HEVC" || item.DynamicRange != "HDR10" || !item.Tags.FourK || item.Runtime != 120) {
				t.Fatalf("movie metadata: %+v", item)
			}
			if kind == "sonarr" && (item.SizeBytes != 300 || len(item.Seasons) != 2 || item.Seasons[0].Season != 1 || item.Seasons[1].SizeBytes != 200 || item.Quality != "") {
				t.Fatalf("season metadata: %+v", item)
			}
		})
	}
}
