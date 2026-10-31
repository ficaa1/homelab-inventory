package archive

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type host struct{ client *http.Client }

func (h host) HTTP() *http.Client { return h.client }
func (host) CacheDir() string     { return "" }
func (host) Logf(string, ...any)  {}
func (host) Now() time.Time       { return time.Now() }
func TestFixedRouteNoRetryAndRedaction(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/seeding/move" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("unexpected request")
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"planHash":"reviewed"`) {
			t.Error("missing reviewed plan")
		}
		w.WriteHeader(503)
	}))
	defer s.Close()
	p := New(host{s.Client()}).(*Integration)
	if e := p.Configure(plugin.Config{"policy_url": s.URL, "policy_token": "synthetic-key"}); e != nil {
		t.Fatal(e)
	}
	_, e := p.Action(context.Background(), "seed-move", plugin.Config{"hash": "abcdef", "plan_hash": "reviewed"})
	if e == nil || calls != 1 {
		t.Fatalf("write repeated or failure hidden: %d %v", calls, e)
	}
	_, e = p.Action(context.Background(), "../../anything", plugin.Config{})
	if e != plugin.ErrNotFound || calls != 1 {
		t.Fatal("unknown action reached upstream")
	}
	v := p.render("fixture", map[string]any{"token": "synthetic-key", "job": map[string]any{"title": "synthetic-key <title>", "phase": "waiting"}})
	b, e := plugin.ValidateView(v, p.Manifest())
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "synthetic-key") {
		t.Fatal("secret exposed")
	}
}

func TestFixedActionRefusesRedirectReplay(t *testing.T) {
	replayed := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/archive/run" {
			w.Header().Set("Location", "/unexpected-write")
			w.WriteHeader(307)
			return
		}
		replayed++
		w.Write([]byte(`{}`))
	}))
	defer upstream.Close()
	p := New(host{upstream.Client()}).(*Integration)
	p.Configure(plugin.Config{"policy_url": upstream.URL})
	if _, e := p.Action(context.Background(), "run", plugin.Config{}); e == nil || replayed != 0 {
		t.Fatal("fixed action followed redirect")
	}
}

// The page API maps each route to one fixed upstream path, sets the actor
// itself, refuses shapes the private routes refused, and strips the recycle
// baseline from status.
func TestPageAPIFixedPathsAndBarriers(t *testing.T) {
	var got []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		if r.URL.Path == "/status" {
			w.Write([]byte(`{"archiveEnabled":true,"archive":{"job":{"phase":"downloading","recycleBaseline":["/media/x"]},"failedJobs":[{"recycleBaseline":["/media/y"]}]}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer s.Close()
	p := New(host{s.Client()}).(*Integration)
	if e := p.Configure(plugin.Config{"policy_url": s.URL}); e != nil {
		t.Fatal(e)
	}
	call := func(method, path, body string) plugin.APIResponse {
		r, e := p.API(context.Background(), plugin.APIRequest{Method: method, Path: path, Body: []byte(body), Actor: "owner@example.org"})
		if e != nil {
			t.Fatalf("%s %s: %v", method, path, e)
		}
		return r
	}
	r := call("GET", "status", "")
	if strings.Contains(string(r.Body), "recycleBaseline") || !strings.Contains(string(r.Body), `"reachable":true`) {
		t.Fatalf("status not shaped: %s", r.Body)
	}
	call("POST", "seeding/queue/reorder", `{"hash":"`+strings.Repeat("a", 40)+`","position":1,"apply":true}`)
	call("POST", "approve", `{"approval":{"kind":"radarr","id":7,"releaseId":"r1","indexerId":2}}`)
	want := []string{
		"GET /status?refresh=progress ",
		`POST /seeding/queue/reorder {"hash":"` + strings.Repeat("a", 40) + `","position":1,"apply":true,"requestedBy":"owner@example.org"}`,
		`POST /archive/approve {"approval":{"kind":"radarr","id":7,"releaseId":"r1","indexerId":2},"requestedBy":"owner@example.org"}`,
	}
	for _, refused := range [][2]string{
		{"pause", `{"paused":true,"apply":true,"requestedBy":"someone-else"}`},
		{"seeding/stop", `{"hash":"../../x","apply":true}`},
		{"seeding/preview", `{"hash":"` + strings.Repeat("b", 40) + `","apply":true}`},
		{"approve", `{"approval":{"kind":"radarr","id":7},"url":"http://192.0.2.1/"}`},
	} {
		if r := call("POST", refused[0], refused[1]); r.Status != 400 {
			t.Errorf("%s accepted %s", refused[0], refused[1])
		}
	}
	if _, e := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "anything/else"}); e != plugin.ErrNotFound {
		t.Error("unknown route reached upstream")
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("upstream calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
