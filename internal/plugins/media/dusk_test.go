package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/user":
			fmt.Fprint(w, `{"results":[{"id":12,"displayName":"Household"}]}`)
		case "/api/v1/user/12":
			fmt.Fprint(w, `{"id":12,"permissions":8}`)
		case "/api/v1/settings/radarr":
			fmt.Fprint(w, `[{"id":4,"name":"Movies"}]`)
		case "/api/v1/settings/sonarr":
			fmt.Fprint(w, `[]`)
		case "/api/v1/settings/radarr/4":
			fmt.Fprint(w, `{"profiles":[{"id":6,"name":"Standard"}],"rootFolders":[{"path":"/library"}]}`)
		case "/api/v1/movie/42":
			fmt.Fprint(w, `{"id":42,"title":"Example"}`)
		case "/api/v1/request":
			posts++
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if body["userId"] != float64(12) || body["profileId"] != float64(6) {
				t.Error("policy not applied")
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":51,"status":1,"profileId":6,"serverId":4,"type":"movie","media":{"tmdbId":42}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	m := New(&testHost{client: server.Client(), now: time.Now()}).(*Media)
	m.Configure(plugin.Config{"overseerr_url": server.URL, "overseerr_key": "synthetic-secret"})
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
