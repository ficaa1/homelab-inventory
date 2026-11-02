package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSeedSonarrCurrentFileMatchAndEditor(t *testing.T) {
	writes := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/system/status":
			json.NewEncoder(w).Encode(map[string]string{"appName": "Sonarr", "version": "4.0.19.2979"})
		case "/api/v3/history":
			json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 1, "records": []HistoryItem{{ID: 1, SeriesID: 7, EpisodeID: 8, DownloadID: "ABCD", EventType: "downloadFolderImported", Data: map[string]string{"fileId": "9", "droppedPath": "/data/downloads/completed/show/ep.mkv"}}}})
		case "/api/v3/series/7":
			json.NewEncoder(w).Encode(map[string]any{"id": 7, "path": "/data/downloads/Shows/Show", "rootFolderPath": "/data/downloads/Shows"})
		case "/api/v3/episodefile":
			json.NewEncoder(w).Encode([]File{{ID: 9, Path: "/data/downloads/Shows/Show/ep.mkv", Size: 123}})
		case "/api/v3/series/editor":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			ids, _ := b["seriesIds"].([]any)
			if r.Method != "PUT" || len(b) != 4 || len(ids) != 1 || ids[0] != float64(7) || b["rootFolderPath"] != "/data/media/series" || b["moveFiles"] != true || b["deleteFiles"] != true {
				t.Error("wrong editor request")
			}
			writes++
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer up.Close()
	c := NewClient("sonarr", up.URL, "fixture-key", true)
	items, e := c.SeedImports(context.Background(), "abcd")
	if e != nil || len(items) != 1 || items[0].FileID != 9 || items[0].Root != "/data/downloads/Shows" {
		t.Fatal(items, e)
	}
	if e = c.MoveSeedLibrary(context.Background(), 7, "/data/media/series"); e != nil || writes != 1 {
		t.Fatal(e, writes)
	}
	c.Apply = false
	if e = c.MoveSeedLibrary(context.Background(), 7, "/data/media/series"); e == nil || writes != 1 {
		t.Fatal("dry-run moved library")
	}
}
