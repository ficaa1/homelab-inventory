package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
)

type duskHTTPTest struct {
	t    *testing.T
	s    *server
	http *httptest.Server
}

func (h *duskHTTPTest) call(method, path, token string, body any, want int) map[string]any {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			h.t.Fatal(e)
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequest(method, h.http.URL+path, reader)
	if e != nil {
		h.t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, e := h.http.Client().Do(req)
	if e != nil {
		h.t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s got %d want %d: %s", method, path, resp.StatusCode, want, b)
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}
func (h *duskHTTPTest) profile(name string) string {
	h.t.Helper()
	return h.call("POST", "/dusk/profiles", h.s.token, map[string]any{"name": name}, 201)["id"].(string)
}
func (h *duskHTTPTest) pair(profile string, scopes []string) (string, string) {
	h.t.Helper()
	c := h.call("POST", "/dusk/codes", h.s.token, map[string]any{"profile_id": profile, "scopes": scopes}, 201)
	out := h.call("POST", "/tv/v1/pair", "", map[string]any{"code": strings.ToLower(c["code"].(string)), "device_name": "Synthetic television"}, 201)
	h.call("POST", "/tv/v1/pair", "", map[string]any{"code": c["code"], "device_name": "Replay"}, 400)
	return out["token"].(string), out["device_id"].(string)
}
func (h *duskHTTPTest) features(token string) map[string]any {
	h.t.Helper()
	return h.call("GET", "/tv/v1/capabilities", token, nil, 200)["features"].(map[string]any)
}
func TestDuskHTTPPairingPoliciesIsolationAndRestart(t *testing.T) {
	var posts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "synthetic-upstream-secret" {
			t.Error("upstream credential missing")
		}
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
			fmt.Fprint(w, `{"profiles":[{"id":6,"name":"Standard"}],"rootFolders":[{"path":"/synthetic/library"}]}`)
		case "/api/v1/movie/42":
			fmt.Fprint(w, `{"id":42,"title":"Synthetic film"}`)
		case "/api/v1/search", "/api/v1/discover/trending", "/api/v1/movie/42/recommendations":
			fmt.Fprint(w, `{"results":[{"id":42,"mediaType":"movie","title":"Synthetic film","releaseDate":"2020-01-01"},{"id":43,"mediaType":"movie","title":"Another film"}]}`)
		case "/api/v1/request":
			if r.Method != "POST" {
				t.Error("incorrect write method")
			}
			posts.Add(1)
			w.WriteHeader(201)
			fmt.Fprint(w, `{"id":51,"status":1,"profileId":6,"serverId":4,"type":"movie","media":{"tmdbId":42}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	s := authServer(t)
	var err error
	s.plugins, err = openPlugins(s.store.Dir(), s.log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.plugins.Close() }()
	h := &duskHTTPTest{t: t, s: s, http: httptest.NewServer(s.routes())}
	defer func() { h.http.Close() }()
	h.call("POST", "/tv/v1/pair", "", map[string]any{"code": "BAD-CODE", "device_name": "TV"}, 503)
	h.call("PUT", "/plugins/dusk", s.token, map[string]any{"enabled": true, "settings": map[string]any{"server_name": "Synthetic household", "discovery": true, "taste": true, "requests": true}}, 200)
	profile := h.profile("First household profile")
	other := h.profile("Second household profile")
	scopes := []string{"media", "upcoming", "discover", "search", "requests", "preferences", "refresh", "taste"}
	token, device := h.pair(profile, scopes)
	second, _ := h.pair(profile, scopes)
	different, _ := h.pair(other, scopes)
	if h.features(token)["discover"] != false {
		t.Fatal("missing Media advertised discovery")
	}
	h.call("GET", "/tv/v1/discover", token, nil, 403)
	h.call("PUT", "/plugins/media", s.token, map[string]any{"enabled": true, "settings": map[string]any{"overseerr_url": upstream.URL, "overseerr_key": "synthetic-upstream-secret"}}, 200)
	if h.features(token)["discover"] != true || h.features(token)["requests"] != false {
		t.Fatal("configured capability policy incorrect")
	}
	for _, path := range []string{"/dusk/admin", "/plugins", "/hosts", "/settings"} {
		h.call("GET", path, token, nil, 401)
	}
	h.call("GET", "/tv/v1/capabilities", s.token, nil, 401)
	h.call("GET", "/tv/v1/capabilities", s.agentToken, nil, 401)
	readOnly, _ := h.pair(profile, []string{"search"})
	h.call("POST", "/tv/v1/preference", readOnly, map[string]any{"kind": "movie", "id": 42, "hidden": true}, 403)
	policy := map[string]any{"label": "Standard", "approval": "manual", "requester_id": 12, "movie": map[string]any{"server_id": 4, "profile_id": 6, "root": "/synthetic/library"}}
	h.call("PUT", "/dusk/policies/"+profile, s.token, policy, 200)
	if h.features(token)["requests"] != true || h.features(different)["requests"] != false {
		t.Fatal("request policy crossed profile")
	}
	h.call("GET", "/tv/v1/title/movie/42", token, nil, 200)
	hidden := map[string]any{"kind": "movie", "id": 42, "hidden": true}
	h.call("POST", "/tv/v1/preference", token, hidden, 200)
	for _, tc := range []struct {
		token  string
		hidden bool
	}{{second, true}, {different, false}} {
		out := h.call("GET", "/tv/v1/search?q=film", tc.token, nil, 200)
		values := out["hidden"].([]any)
		found := false
		for _, v := range values {
			found = found || v == "movie:42"
		}
		if found != tc.hidden {
			t.Fatal("profile preferences not isolated")
		}
	}
	h.call("POST", "/tv/v1/taste/verdict", token, map[string]any{"kind": "movie", "id": 42, "verdict": "liked", "title": "Synthetic film", "year": "2020"}, 200)
	h.call("GET", "/tv/v1/taste/picks?kind=movie", token, nil, 200)
	h.call("POST", "/tv/v1/discover/refresh", token, map[string]any{}, 200)
	request := map[string]any{"kind": "movie", "id": 42, "season": 0, "operation": "synthetic-operation-01", "confirmed": true}
	out := h.call("POST", "/tv/v1/request", token, request, 200)
	if out["state"] != "pending" {
		t.Fatalf("request result %v", out)
	}
	h.call("POST", "/tv/v1/request", second, request, 200)
	if posts.Load() != 1 {
		t.Fatal("duplicate device request wrote twice")
	}
	// Opening fresh registry and server instances against the same private state
	// checks credentials, scoped preferences, policy and durable receipt reload.
	h.http.Close()
	s.plugins.Close()
	fresh := authServer(t)
	fresh.store = s.store
	fresh.plugins, err = openPlugins(s.store.Dir(), s.log)
	if err != nil {
		t.Fatal(err)
	}
	s = fresh
	h.s = s
	h.http = httptest.NewServer(s.routes())
	if h.features(token)["requests"] != true {
		t.Fatal("policy or credential lost on restart")
	}
	h.call("POST", "/tv/v1/request", second, request, 200)
	if posts.Load() != 1 {
		t.Fatal("restart lost durable duplicate protection")
	}
	h.call("DELETE", "/dusk/devices/"+device, s.token, nil, 200)
	h.call("GET", "/tv/v1/capabilities", token, nil, 401)
	h.call("GET", "/tv/v1/capabilities", second, nil, 200)
	h.call("PUT", "/plugins/media", s.token, map[string]any{"enabled": false}, 200)
	if h.features(second)["discover"] != false {
		t.Fatal("disabled source remains advertised")
	}
	h.call("GET", "/tv/v1/discover", second, nil, 403)
	h.call("PUT", "/plugins/dusk", s.token, map[string]any{"enabled": false}, 200)
	h.call("GET", "/tv/v1/capabilities", second, nil, 503)
}

func TestDuskAdminBrowserCSRF(t *testing.T) {
	s := authServer(t)
	ts := httptest.NewServer(s.routes())
	defer ts.Close()
	cookie := &http.Cookie{Name: auth.SessionCookie, Value: s.browserAuth.Issue("synthetic@example.org", time.Now())}
	for _, tc := range []struct {
		csrf string
		want int
	}{{"", 403}, {"homelab-inventory", 201}} {
		req, _ := http.NewRequest("POST", ts.URL+"/dusk/profiles", strings.NewReader(`{"name":"Synthetic household"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Requested-By", tc.csrf)
		req.AddCookie(cookie)
		res, e := ts.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Fatalf("csrf %q got %d want %d", tc.csrf, res.StatusCode, tc.want)
		}
	}
}
