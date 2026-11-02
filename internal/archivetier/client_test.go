package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWriteContractAndSecretErrors(t *testing.T) {
	secret := "private-key-example"
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != secret {
			t.Error("missing auth")
		}
		if r.Method == http.MethodPost {
			posts++
			var body map[string]json.RawMessage
			json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Path == "/api/v3/release" {
				if len(body) != 2 || string(body["guid"]) != `"cached-guid"` || string(body["indexerId"]) != "10" {
					t.Errorf("incorrect release body %s", body)
				}
			} else if r.URL.Path == "/api/v3/command" {
				if string(body["name"]) != `"ManualImport"` || string(body["importMode"]) != `"copy"` {
					t.Error("unsafe import mode")
				}
				w.Write([]byte(`{"id":12}`))
				return
			} else {
				t.Error("unexpected mutation")
			}
			w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(500)
		w.Write([]byte(secret))
	}))
	defer server.Close()
	c := NewClient("sonarr", server.URL, secret, false)
	ctx := context.Background()
	r := Release{Guid: "cached-guid", IndexerID: 10}
	if c.Grab(ctx, r) == nil || posts != 0 {
		t.Fatal("dry client wrote")
	}
	c.Apply = true
	if e := c.Grab(ctx, r); e != nil {
		t.Fatal(e)
	}
	if id, e := c.Import(ctx, nil); e != nil || id != 12 {
		t.Fatal(id, e)
	}
	var out any
	e := c.get(ctx, "/queue", nil, &out)
	if e == nil || strings.Contains(e.Error(), secret) {
		t.Fatal("upstream body leaked")
	}
	if c.call(ctx, http.MethodDelete, "/episodefile/1", nil, nil, nil) == nil {
		t.Fatal("DELETE permitted")
	}
}
func TestRedirectRefused(t *testing.T) {
	replayed := false
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { replayed = true }))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, dest.URL, http.StatusFound) }))
	defer src.Close()
	c := NewClient("radarr", src.URL, "secret", false)
	if c.Preflight(context.Background()) == nil || replayed {
		t.Fatal("redirect replayed credentials")
	}
}
func TestQueuePagination(t *testing.T) {
	var records []json.RawMessage
	for i := 1; i <= 101; i++ {
		records = append(records, json.RawMessage(fmt.Sprintf(`{"id":%d,"movieId":%d,"downloadId":"d%d","trackedDownloadState":"downloading"}`, 1000+i, i, i)))
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(pagedBody(t, r, records)) }))
	defer server.Close()
	c := NewClient("radarr", server.URL, "key", false)
	q, e := c.Queue(context.Background())
	if e != nil || len(q) != 101 || q[100].MovieID != 101 {
		t.Fatal("queue not completely paged", e)
	}
}

// Null or absent collections and totals are never "nothing there". The first
// two bodies are the reviewer's reproductions.
func TestNullSafetyReadsFailClosed(t *testing.T) {
	arr := []struct{ name, path, body string }{
		{"queue records null", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":0,"records":null}`},
		{"queue total null", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":null,"records":[]}`},
		{"queue total absent", "/api/v3/queue", `{"page":1,"pageSize":100,"records":[]}`},
		{"queue negative total", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":-1,"records":[]}`},
		{"queue short of total", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":2,"records":[{"id":1,"movieId":3}]}`},
		{"queue record without id", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"movieId":3}]}`},
		{"queue repeated id", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":2,"records":[{"id":1,"movieId":3},{"id":1,"movieId":4}]}`},
		{"queue unmapped title", "/api/v3/queue", `{"page":1,"pageSize":100,"totalRecords":1,"records":[{"id":1,"movieId":0}]}`},
		{"queue page not echoed", "/api/v3/queue", `{"page":2,"pageSize":100,"totalRecords":0,"records":[]}`},
	}
	for _, tc := range arr {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) }))
			defer srv.Close()
			if q, e := NewClient("radarr", srv.URL, "key", false).Queue(context.Background()); e == nil {
				t.Fatalf("accepted %s as %v", tc.body, q)
			}
		})
	}
	plex := []struct{ name, cmd, data string }{
		{"history data null", "get_history", `{"recordsFiltered":0,"data":null}`},
		{"history total null", "get_history", `{"recordsFiltered":null,"data":[]}`},
		{"history total absent", "get_history", `{"data":[]}`},
		{"history row without id", "get_history", `{"recordsFiltered":1,"data":[{"date":1791244800,"rating_key":5,"media_type":"movie"}]}`},
		{"history short of total", "get_history", `{"recordsFiltered":2,"data":[{"id":1,"date":1791244800,"rating_key":5,"media_type":"movie"}]}`},
		{"response data null", "get_history", `null`},
		{"activity sessions null", "get_activity", `{"stream_count":"0","sessions":null}`},
		{"activity sessions absent", "get_activity", `{"stream_count":"0"}`},
		{"activity count disagrees", "get_activity", `{"stream_count":"1","sessions":[]}`},
		{"activity count null", "get_activity", `{"stream_count":null,"sessions":[]}`},
	}
	for _, tc := range plex {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"response":{"result":"success","data":%s}}`, tc.data)
			}))
			defer srv.Close()
			p := NewPlex(srv.URL, "key")
			var e error
			if tc.cmd == "get_activity" {
				_, e = p.Active(context.Background())
			} else {
				_, e = p.Recent(context.Background(), time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC), 30)
			}
			if e == nil {
				t.Fatalf("accepted %s", tc.data)
			}
		})
	}
	other := []struct{ name, path, body string }{
		{"release list null", "/api/v3/release", `null`},
		{"tags null", "/api/v3/tag", `null`},
		{"titles null", "/api/v3/movie", `null`},
		{"title tags null", "/api/v3/movie", `[{"id":293,"title":"Example Movie","tags":null}]`},
		{"manual import list null", "/api/v3/manualimport", `null`},
		{"command status unknown", "/api/v3/command/42", `{"id":42,"status":""}`},
		{"command identity differs", "/api/v3/command/42", `{"id":41,"status":"completed"}`},
	}
	for _, tc := range other {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == tc.path:
					fmt.Fprint(w, tc.body)
				case r.URL.Path == "/api/v3/tag":
					fmt.Fprint(w, `[{"id":5,"label":"archive"}]`)
				default:
					fmt.Fprint(w, `[]`)
				}
			}))
			defer srv.Close()
			c := NewClient("radarr", srv.URL, "key", false)
			var e error
			switch tc.path {
			case "/api/v3/release":
				_, e = c.Search(context.Background(), movieTarget())
			case "/api/v3/tag", "/api/v3/movie":
				_, e = c.Targets(context.Background())
			case "/api/v3/manualimport":
				_, e = c.Manual(context.Background(), QueueItem{DownloadID: "x"})
			default:
				_, e = c.Command(context.Background(), 42)
			}
			if e == nil {
				t.Fatalf("accepted %s", tc.body)
			}
		})
	}
}
func TestPlexHistoryPaginationAndStoppedTime(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("after") != "2026-09-06" || q.Get("apikey") != "key" {
			t.Error("wrong history parameters")
		}
		calls++
		if q.Get("start") == "0" {
			rows := []map[string]any{}
			for i := 0; i < 100; i++ {
				rows = append(rows, map[string]any{"id": i + 1, "date": "1788739199", "stopped": "1791244800", "rating_key": "500", "grandparent_rating_key": "900", "media_type": "episode"})
			}
			json.NewEncoder(w).Encode(map[string]any{"response": map[string]any{"result": "success", "data": map[string]any{"recordsFiltered": 101, "data": rows}}})
		} else {
			w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":101,"data":[{"id":101,"date":1791244800,"stopped":1791244900,"rating_key":901,"grandparent_rating_key":"","media_type":"movie"}]}}}`))
		}
	}))
	defer server.Close()
	p := NewPlex(server.URL, "key")
	_, _, _, now := runner(t)
	m, e := p.Recent(context.Background(), now, 30)
	if e != nil || !m["900"] || !m["901"] || calls != 2 {
		t.Fatal("history identity or pagination", m, e, calls)
	}
}

func TestTrackedImportRefused(t *testing.T) {
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { posts++; w.Write([]byte(`{"id":1}`)) }))
	defer server.Close()
	c := NewClient("sonarr", server.URL, "key", true)
	if _, e := c.Import(context.Background(), []map[string]any{{"path": "/downloads/ep.mkv", "downloadId": "hash"}}); e == nil || posts != 0 {
		t.Fatal("tracked source-deleting import accepted")
	}
}

func TestRuntimeAndFourKDiscoveryAndTagRecheck(t *testing.T) {
	for _, kind := range []string{"sonarr", "radarr"} {
		t.Run(kind, func(t *testing.T) {
			fourK := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v3/tag":
					w.Write([]byte(`[{"id":1,"label":"archive"},{"id":2,"label":"4k"}]`))
				case "/api/v3/movie", "/api/v3/movie/1", "/api/v3/series", "/api/v3/series/1":
					tags := []int{1}
					if fourK {
						tags = append(tags, 2)
					}
					v := map[string]any{"id": 1, "title": "Fixture", "runtime": 55, "tags": tags}
					if kind == "radarr" {
						v["runtime"] = 126
						v["movieFile"] = map[string]any{"id": 10, "path": "/library/movie.mkv", "size": 100 * GB}
					}
					if strings.HasSuffix(r.URL.Path, "/1") {
						json.NewEncoder(w).Encode(v)
					} else {
						json.NewEncoder(w).Encode([]any{v})
					}
				case "/api/v3/episode":
					w.Write([]byte(`[{"id":101,"seriesId":1,"seasonNumber":3,"episodeFileId":10}]`))
				case "/api/v3/episodefile":
					w.Write([]byte(`[{"id":10,"path":"/library/episode.mkv","size":10000000000,"seasonNumber":3}]`))
				default:
					t.Error("unexpected route", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := NewClient(kind, server.URL, "fixture-key", false)
			ctx := context.Background()
			targets, e := c.Targets(ctx)
			runtime := 55
			if kind == "radarr" {
				runtime = 126
			}
			if e != nil || len(targets) != 1 || targets[0].Runtime != runtime || targets[0].Mode() != Mode2160p {
				t.Fatalf("discovery %+v %v", targets, e)
			}
			if opted, e := c.Opted(ctx, targets[0]); e != nil || !opted {
				t.Fatal(opted, e)
			}
			fourK = false
			if _, e := c.Opted(ctx, targets[0]); e == nil || !strings.Contains(e.Error(), "4k tag changed") {
				t.Fatal("tag removal accepted", e)
			}
			targets, e = c.Targets(ctx)
			if e != nil || targets[0].Mode() != Mode1080p {
				t.Fatal("default mode", e)
			}
			fourK = true
			if _, e := c.Opted(ctx, targets[0]); e == nil {
				t.Fatal("tag addition accepted")
			}
		})
	}
}

func TestImportAndVerificationProfiles(t *testing.T) {
	c := testConfig()
	target := target()
	target.FourK = true
	items := fixture[[]ManualItem](t, "manual-sonarr.json")
	for i := range items {
		items[i].Path = strings.Replace(items[i].Path, "1080p", "2160p.HDR10", 1)
		items[i].Quality.Quality.Resolution = 2160
		items[i].Size = 8 * GB
	}
	if _, e := ValidateImport(items, target, c, Mode2160p); e != nil {
		t.Fatal(e)
	}
	items[0].Path = strings.Replace(items[0].Path, "HDR10", "DV", 1)
	if _, e := ValidateImport(items, target, c, Mode2160p); e == nil {
		t.Fatal("DV-only import accepted")
	}
	items[0].Path = strings.Replace(items[0].Path, "DV", "HDR10", 1)
	items[0].MediaInfo = &MediaInfo{AudioChannels: 2, AudioCodec: "AAC", VideoCodec: "HEVC", VideoDynamicRangeType: "HDR10"}
	if _, e := ValidateImport(items, target, c, Mode2160p); e == nil {
		t.Fatal("stereo probe accepted")
	}
	current := target
	current.Files = append([]File(nil), target.Files...)
	current.Episodes = append([]Episode(nil), target.Episodes...)
	for i := range current.Files {
		current.Files[i].ID += 100
		current.Files[i].Size = 8 * GB
		current.Episodes[i].FileID = current.Files[i].ID
		current.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC", VideoDynamicRangeType: "HDR10"}
	}
	if !Verified(target, current, c, Mode2160p) {
		t.Fatal("4k verification rejected")
	}
	for _, tc := range []struct {
		channels          float64
		audio, hdr, video string
	}{{2, "EAC3", "HDR10", "HEVC"}, {5.1, "TrueHD", "HDR10", "HEVC"}, {5.1, "EAC3", "Dolby Vision", "HEVC"}, {5.1, "EAC3", "SDR", "HEVC"}, {5.1, "EAC3", "HDR10", "unknown"}} {
		current.Files[0].MediaInfo = &MediaInfo{AudioChannels: tc.channels, AudioCodec: tc.audio, VideoCodec: tc.video, VideoDynamicRangeType: tc.hdr}
		if Verified(target, current, c, Mode2160p) {
			t.Fatal("unsafe verification", tc)
		}
	}
	current.Files[0].MediaInfo = nil
	if Verified(target, current, c, Mode2160p) {
		t.Fatal("missing probe accepted")
	}
}

func TestRecentRecordsEpisodeSeason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response":{"result":"success","data":{"recordsFiltered":1,"data":[{"id":1,"date":1791244800,"rating_key":500,"grandparent_rating_key":900,"parent_media_index":5,"media_type":"episode"}]}}}`))
	}))
	defer server.Close()
	_, _, _, now := runner(t)
	m, e := NewPlex(server.URL, "key").Recent(context.Background(), now, 30)
	if e != nil || m["900"] || !m["900:s5"] {
		t.Fatal("episode play not recorded per season", m, e)
	}
}
