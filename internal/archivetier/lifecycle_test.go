package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// pagedBody serves records the way Sonarr/Radarr v3 page them.
func pagedBody(t *testing.T, r *http.Request, records []json.RawMessage) []byte {
	t.Helper()
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	size, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if page < 1 || size < 1 {
		t.Fatalf("unpaged request %s", r.URL)
	}
	start := (page - 1) * size
	end := min(start+size, len(records))
	start = min(start, end)
	b, _ := json.Marshal(map[string]any{"page": page, "pageSize": size, "sortKey": "id", "sortDirection": "descending", "totalRecords": len(records), "records": records[start:end]})
	return b
}

// fixtureHistory loads a synthetic import-history resource.
func fixtureHistory(t *testing.T, kind string) (map[string]any, HistoryItem) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "history-"+kind+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Records []map[string]any `json:"records"`
	}
	if err = json.Unmarshal(b, &page); err != nil || len(page.Records) != 1 {
		t.Fatal("bad history fixture", err)
	}
	raw := page.Records[0]
	var h HistoryItem
	b, _ = json.Marshal(raw)
	if err = json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if h.EventType != "downloadFolderImported" || h.Data["fileId"] == "" || h.Data["importedPath"] == "" {
		t.Fatalf("history contract changed: %+v", h)
	}
	return raw, h
}

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// lifecycleCase builds an original 2160p target, the replacement as the
// application now reports it, and history records shaped exactly like the
// fixture resources, one per replacement file (and per episode for Sonarr).
func lifecycleCase(t *testing.T, kind string) (*Job, Target, []map[string]any) {
	t.Helper()
	raw, h := fixtureHistory(t, kind)
	fileID, _ := strconv.Atoi(h.Data["fileId"])
	j := &Job{DownloadID: h.DownloadID, CommandID: 1}
	var current Target
	var records []map[string]any
	if kind == "radarr" {
		j.Target = Target{Kind: "radarr", ID: h.MovieID, Files: []File{{ID: fileID - 1, Path: "/data/media/movies/old-2160p.mkv", Size: 40 * GB, Quality: qual(2160)}}}
		current = Target{Kind: "radarr", ID: h.MovieID, Files: []File{{ID: fileID, Path: h.Data["importedPath"], Size: 15 * GB, Quality: qual(1080)}}}
		records = append(records, raw)
		return j, current, records
	}
	// Two episodes: one record and a second one shaped the same way.
	second := clone(raw)
	second["id"] = h.ID - 1
	second["episodeId"] = h.EpisodeID - 1
	data := second["data"].(map[string]any)
	data["fileId"] = strconv.Itoa(fileID - 1)
	data["importedPath"] = filepath.Dir(h.Data["importedPath"]) + "/second.mkv"
	j.Target = Target{Kind: "sonarr", ID: h.SeriesID, Season: 5, Episodes: []Episode{{ID: h.EpisodeID, SeriesID: h.SeriesID, Season: 5, FileID: 1}, {ID: h.EpisodeID - 1, SeriesID: h.SeriesID, Season: 5, FileID: 2}}, Files: []File{{ID: 1, Path: "/data/media/series/a.mkv", Size: 6 * GB}, {ID: 2, Path: "/data/media/series/b.mkv", Size: 6 * GB}}}
	current = Target{Kind: "sonarr", ID: h.SeriesID, Season: 5,
		Episodes: []Episode{{ID: h.EpisodeID, SeriesID: h.SeriesID, Season: 5, FileID: fileID}, {ID: h.EpisodeID - 1, SeriesID: h.SeriesID, Season: 5, FileID: fileID - 1}},
		Files:    []File{{ID: fileID, Path: h.Data["importedPath"], Size: 2 * GB, Quality: qual(1080)}, {ID: fileID - 1, Path: data["importedPath"].(string), Size: 2 * GB, Quality: qual(1080)}}}
	return j, current, []map[string]any{raw, second}
}

func lifecycleServer(t *testing.T, history []map[string]any, queue []json.RawMessage) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatal("lifecycle mutated upstream")
		}
		switch r.URL.Path {
		case "/api/v3/history":
			rs := []json.RawMessage{}
			for _, h := range history {
				b, _ := json.Marshal(h)
				rs = append(rs, b)
			}
			w.Write(pagedBody(t, r, rs))
		case "/api/v3/queue":
			w.Write(pagedBody(t, r, queue))
		default:
			t.Error("unexpected lifecycle endpoint", r.URL.Path)
		}
	}))
}

func TestLifecycleRequiresFileIDOnEveryReplacement(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		for _, scenario := range []string{"success", "fileId missing", "fileId wrong", "fileId not a number", "data null", "replacement without import", "path differs", "other download", "episode on another file", "queue keeps download", "queue keeps title"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				j, current, history := lifecycleCase(t, kind)
				data := history[0]["data"].(map[string]any)
				queue := []json.RawMessage{}
				switch scenario {
				case "fileId missing":
					delete(data, "fileId")
				case "fileId wrong":
					data["fileId"] = "999999"
				case "fileId not a number":
					data["fileId"] = ""
				case "data null":
					history[0]["data"] = nil
				case "replacement without import":
					current.Files = append(current.Files, File{ID: 777, Path: "/data/extra.mkv", Size: GB, Quality: qual(1080)})
				case "path differs":
					data["importedPath"] = "/data/elsewhere.mkv"
				case "other download":
					history[0]["downloadId"] = "OTHER"
				case "episode on another file":
					if kind == "radarr" {
						t.Skip("movies have no episodes")
					}
					history[0]["episodeId"] = current.Episodes[1].ID
				case "queue keeps download":
					queue = append(queue, json.RawMessage(`{"id":5,"movieId":1,"seriesId":1,"downloadId":"`+j.DownloadID+`"}`))
				case "queue keeps title":
					queue = append(queue, json.RawMessage(`{"id":5,"movieId":`+strconv.Itoa(current.ID)+`,"seriesId":`+strconv.Itoa(current.ID)+`,"downloadId":"x"}`))
				}
				srv := lifecycleServer(t, history, queue)
				defer srv.Close()
				e := NewClient(kind, srv.URL, "fixture-key", false).Lifecycle(context.Background(), j, current)
				if (e == nil) != (scenario == "success") {
					t.Fatalf("unexpected lifecycle outcome: %v", e)
				}
			})
		}
	}
}

func TestLifecyclePaginationFailsClosed(t *testing.T) {
	for _, scenario := range []string{"paged success", "truncated", "duplicated", "changing total", "null records", "null total", "missing page"} {
		t.Run(scenario, func(t *testing.T) {
			j, current, imports := lifecycleCase(t, "sonarr")
			var records []json.RawMessage
			for i := 0; i < 100; i++ {
				records = append(records, json.RawMessage(`{"id":`+strconv.Itoa(100000+i)+`,"seriesId":1,"episodeId":1,"downloadId":"unrelated","eventType":"grabbed","data":{}}`))
			}
			for _, h := range imports {
				b, _ := json.Marshal(h)
				records = append(records, b)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v3/queue" {
					w.Write(pagedBody(t, r, []json.RawMessage{}))
					return
				}
				var body map[string]any
				json.Unmarshal(pagedBody(t, r, records), &body)
				page := r.URL.Query().Get("page")
				switch {
				case scenario == "truncated" && page == "1":
					body["records"] = body["records"].([]any)[:99]
				case scenario == "duplicated" && page == "2":
					body["records"].([]any)[0].(map[string]any)["id"] = 100000
				case scenario == "changing total" && page == "2":
					body["totalRecords"] = 103
				case scenario == "null records" && page == "2":
					body["records"] = nil
				case scenario == "null total":
					body["totalRecords"] = nil
				case scenario == "missing page":
					delete(body, "page")
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()
			e := NewClient("sonarr", srv.URL, "fixture", false).Lifecycle(context.Background(), j, current)
			if (e == nil) != (scenario == "paged success") {
				t.Fatal(e)
			}
		})
	}
}

// replacement is target() after a successful 1080p import.
func replacement() Target {
	t := target()
	for i := range t.Files {
		t.Files[i].ID += 100
		t.Files[i].Size = 2 * GB
		t.Files[i].Quality = qual(1080)
		t.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		t.Episodes[i].FileID = t.Files[i].ID
	}
	return t
}

func TestLifecycleIgnoresRetainedAbandonedDownloads(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		j, current, history := lifecycleCase(t, kind)
		j.ignoredDownloads = map[string]bool{"abandoned": true}
		b, _ := json.Marshal(QueueItem{ID: 5, MovieID: current.ID, SeriesID: current.ID, DownloadID: "abandoned"})
		srv := lifecycleServer(t, history, []json.RawMessage{b})
		c := NewClient(kind, srv.URL, "key", false)
		if err := c.Lifecycle(context.Background(), j, current); err != nil {
			t.Error(err)
		}
		j.ignoredDownloads = nil
		if err := c.Lifecycle(context.Background(), j, current); err == nil {
			t.Fatal("unrelated queue item ignored")
		}
		srv.Close()
	}
}
