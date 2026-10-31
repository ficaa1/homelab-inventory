package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestDuskStructuredReads(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("read performed mutation")
		}
		if r.Header.Get("X-Api-Key") != "synthetic-secret" {
			t.Error("missing credential")
		}
		calls++
		switch r.URL.Path {
		case "/api/v1/search", "/api/v1/discover/trending":
			fmt.Fprint(w, `{"results":[{"id":42,"mediaType":"movie","title":"Example","posterPath":"/example.jpg","releaseDate":"2020-01-02","mediaInfo":{"status":5},"apiKey":"synthetic-secret","user":"private"}]}`)
		case "/api/v1/movie/42":
			fmt.Fprint(w, `{"id":42,"title":"Example","mediaInfo":{"status":5},"credits":{"cast":[{"name":"Performer"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m := New(&testHost{client: server.Client(), now: time.Now()}).(*Media)
	if err := m.Configure(plugin.Config{"overseerr_url": server.URL, "overseerr_key": "synthetic-secret"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		v, err := m.TVRead(context.Background(), "search", "", 0, "Example")
		if err != nil {
			t.Fatal(err)
		}
		rows := v.(mediadata.Titles)
		if len(rows.Titles) != 1 || rows.Titles[0].Availability != "available" {
			t.Fatalf("unexpected titles: %+v", rows)
		}
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), "synthetic-secret") || strings.Contains(string(b), "private") {
			t.Fatal("private upstream fields escaped")
		}
	}
	if calls != 1 {
		t.Fatalf("cache not shared: %d", calls)
	}
	v, err := m.TVRead(context.Background(), "title", "movie", 42, "")
	if err != nil || v.(mediadata.Detail).Title.State != "available" {
		t.Fatalf("detail %v %v", v, err)
	}
	for _, q := range []string{"", strings.Repeat("x", 81), "bad\nquery"} {
		if _, err := m.TVRead(context.Background(), "search", "", 0, q); err == nil {
			t.Fatal("invalid search accepted")
		}
	}
	if _, err := m.TVRead(context.Background(), "title", "../settings", 42, ""); err == nil {
		t.Fatal("invalid kind accepted")
	}
}
func TestDuskExplicitRequestPolicy(t *testing.T) {
	port := ""
	posts := 0
	missingPermissions := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			fmt.Fprint(w, `{"results":[{"id":12,"displayName":"Household"}]}`)
		case "/api/v1/user/12":
			if missingPermissions {
				fmt.Fprint(w, `{"id":12}`)
			} else {
				fmt.Fprint(w, `{"id":12,"permissions":32}`)
			}
		case "/api/v1/settings/radarr":
			fmt.Fprintf(w, `[{"id":4,"name":"Movies","hostname":"127.0.0.1","port":%s,"apiKey":"synthetic-secret"}]`, port)
		case "/api/v1/settings/sonarr":
			fmt.Fprint(w, `[]`)
		case "/api/v3/qualityprofile":
			fmt.Fprint(w, `[{"id":6,"name":"Standard"}]`)
		case "/api/v3/rootfolder":
			fmt.Fprint(w, `[{"path":"/library"}]`)
		case "/api/v1/movie/42":
			fmt.Fprint(w, `{"id":42,"title":"Example"}`)
		case "/api/v1/request":
			posts++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("X-API-User") != "12" || body["userId"] != nil || body["profileId"] != float64(6) {
				t.Error("policy not applied")
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":51,"status":1,"profileId":6,"serverId":4,"type":"movie","media":{"tmdbId":42}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	port = u.Port()
	m := New(&testHost{client: server.Client(), now: time.Now()}).(*Media)
	m.Configure(plugin.Config{"overseerr_url": server.URL, "overseerr_key": "synthetic-secret", "radarr_url": server.URL, "radarr_key": "synthetic-secret", "sonarr_url": server.URL, "sonarr_key": "synthetic-secret"})
	p := mediadata.RequestPolicy{Approval: "manual", RequesterID: 12, Movie: mediadata.Selection{ServerID: 4, ProfileID: 6, Root: "/library"}}
	r := mediadata.Request{Kind: "movie", ID: 42, Operation: "synthetic-operation-01", Confirmed: true}
	bad := p
	bad.Movie.ProfileID = 99
	if _, err := m.TVRequest(context.Background(), bad, r); err == nil {
		t.Fatal("unknown selection accepted")
	}
	if posts != 0 {
		t.Fatal("invalid policy mutated upstream")
	}
	result, err := m.TVRequest(context.Background(), p, r)
	if err != nil || result.State != "pending" || posts != 1 {
		t.Fatalf("result %+v error %v posts %d", result, err, posts)
	}
	p.Approval = "automatic"
	if _, err := m.TVRequest(context.Background(), p, r); err == nil {
		t.Fatal("manual requester accepted automatic policy")
	}
	if posts != 1 {
		t.Fatal("invalid approval policy mutated upstream")
	}
	missingPermissions = true
	p.Approval = "manual"
	if _, err := m.TVRequest(context.Background(), p, r); err == nil {
		t.Fatal("missing permissions accepted")
	}
	if posts != 1 {
		t.Fatal("unverified requester mutated upstream")
	}
}
func TestDuskUnavailableFeedKeepsNullCounts(t *testing.T) {
	m := New(&testHost{now: time.Now()}).(*Media)
	v := m.tvMedia(context.Background()).(map[string]any)
	px := v["plex"].(map[string]any)
	if px["streams"] != nil || px["status"] != "unconfigured" {
		t.Fatal(px)
	}
	dl := v["downloads"].(map[string]any)
	if dl["titles"] == nil {
		t.Fatal("missing empty titles")
	}
}

func TestDuskGuardRejectsCrossAuthorityRedirect(t *testing.T) {
	var escaped atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped.Add(1)
		t.Error("TV operation followed cross-authority redirect")
	}))
	defer sink.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-secret" {
			t.Error("origin missing configured credential")
		}
		http.Redirect(w, r, sink.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	var mediaInstance *Media
	registry, err := plugin.Open(t.TempDir(), []plugin.Builtin{{ID: "media", New: func(h plugin.Host) plugin.Plugin { mediaInstance = New(h).(*Media); return mediaInstance }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	on := true
	if _, err := registry.Update("media", plugin.Update{Enabled: &on, Settings: plugin.Config{"overseerr_url": origin.URL, "overseerr_key": "synthetic-secret"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := mediaInstance.TVRead(context.Background(), "title", "movie", 42, ""); err == nil {
		t.Fatal("redirect returned successful metadata")
	}
	if _, err := mediaInstance.TVOptions(context.Background()); err == nil {
		t.Fatal("redirect returned successful request choices")
	}
	if escaped.Load() != 0 {
		t.Fatal("credential-bearing TV operation reached unconfigured authority")
	}
}

func TestDuskActivityRealShapeAndMalformed(t *testing.T) {
	for _, fixture := range []struct {
		body, status string
		streams      any
	}{
		{`{"stream_count":"3","sessions":[{"transcode_decision":"direct play"},{"transcode_decision":"copy"},{"transcode_decision":"transcode"}]}`, "ok", 3},
		{`{"stream_count":"0","sessions":[]}`, "ok", 0},
		{`{}`, "unavailable", nil},
		{`{"stream_count":"1","sessions":[]}`, "unavailable", nil},
	} {
		t.Run(fixture.body, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"response":{"result":"success","data":%s}}`, fixture.body)
			}))
			defer upstream.Close()
			m := New(&testHost{now: time.Now(), client: upstream.Client()}).(*Media)
			if err := m.Configure(plugin.Config{"tautulli_url": upstream.URL, "tautulli_key": "synthetic"}); err != nil {
				t.Fatal(err)
			}
			px := m.tvMedia(context.Background()).(map[string]any)["plex"].(map[string]any)
			if px["status"] != fixture.status || px["streams"] != fixture.streams {
				t.Fatal(px)
			}
			if fixture.streams == 3 && (px["direct_play"] != 1 || px["direct_stream"] != 1 || px["transcode"] != 1) {
				t.Fatal(px)
			}
		})
	}
}

func TestDuskQueueGroupingAndStates(t *testing.T) {
	m := New(&testHost{}).(*Media)
	for _, status := range []string{"paused", "queued", "completed", "importing"} {
		rows := []queueItem{{ID: 1, SeriesID: 7, Series: &mediaTitle{ID: 7, Title: "Series"}, Size: 100, Sizeleft: 25, DownloadID: "pack", Status: status}, {ID: 2, SeriesID: 7, Series: &mediaTitle{ID: 7, Title: "Series"}, Size: 100, Sizeleft: 25, DownloadID: "pack", Status: status}}
		if status == "importing" {
			for i := range rows {
				rows[i].TrackedDownloadState = "importing"
			}
		}
		got := m.tvQueueTitles("sonarr", rows)
		want := status
		if want == "completed" {
			want = "waiting"
		}
		if len(got) != 1 || got[0].(map[string]any)["state"] != want || got[0].(map[string]any)["percent"] != 75 {
			t.Fatal(got)
		}
	}
}

func TestDuskArrBrowseProjection(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":1,"tmdbId":42,"title":"Synthetic film","year":2024,"hasFile":true,"images":[{"coverType":"poster","remoteUrl":"https://image.tmdb.org/t/p/original/example.jpg"}]},{"id":2,"tmdbId":43,"title":"Unknown","images":[{"coverType":"poster","remoteUrl":"https://secret.invalid/poster.jpg?token=private"}]}]`)
	}))
	defer upstream.Close()
	m := New(&testHost{now: time.Now(), client: upstream.Client()}).(*Media)
	if err := m.Configure(plugin.Config{"radarr_url": upstream.URL, "radarr_key": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	got, err := m.tvBrowse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Titles) != 2 || got.Titles[0].Year != "2024" || got.Titles[0].Availability != "available" || got.Titles[0].Poster != "https://image.tmdb.org/t/p/w300_and_h450_face/example.jpg" || got.Titles[1].Availability != "unknown" || got.Titles[1].Poster != "" {
		t.Fatal(got)
	}
}

func TestDuskRecentMeansReleaseDate(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"id":1,"mediaType":"movie","title":"New","releaseDate":"2026-05-01"},{"id":2,"mediaType":"movie","title":"Old","releaseDate":"2020-01-01"},{"id":3,"mediaType":"movie","title":"Future","releaseDate":"2027-01-01"}]}`)
	}))
	defer upstream.Close()
	m := New(&testHost{now: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), client: upstream.Client()}).(*Media)
	if err := m.Configure(plugin.Config{"overseerr_url": upstream.URL, "overseerr_key": "synthetic"}); err != nil {
		t.Fatal(err)
	}
	value, err := m.TVRead(context.Background(), "discover", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	report := value.(mediadata.Discovery)
	if len(report.Recent) != 1 || report.Recent[0].ID != 1 || report.Recent[0].ReleasedAt != "2026-05-01" {
		t.Fatal(report)
	}
}
