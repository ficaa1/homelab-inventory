package tdarr

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/tdarr/engine"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type testHost struct{ dir string }

func (h testHost) HTTP() *http.Client { return http.DefaultClient }
func (h testHost) CacheDir() string   { return h.dir }
func (testHost) Logf(string, ...any)  {}
func (testHost) Now() time.Time       { return time.Now() }
func TestConfiguredPathsAndProtectedIdentities(t *testing.T) {
	p := New(testHost{t.TempDir()}).(*Integration)
	c := plugin.Config{"tdarr_url": "http://service.invalid", "radarr_root": "/arr/films", "tdarr_movie_root": "/worker/movies", "protected_titles": `[{"kind":"radarr","id":7,"title":"Protected example","path":"/arr/films/keep"}]`}
	if e := p.Configure(c); e != nil {
		t.Fatal(e)
	}
	for _, file := range []string{"/arr/films/../escape.mkv", "/arr/films/example/../../escape.mkv", "/arr/films/example/readme.txt", "/arr/filmsevil/a.mkv"} {
		if _, e := p.mapPath(file, "radarr"); e == nil {
			t.Errorf("unsafe mapping %s", file)
		}
	}
	mapped, e := p.mapPath("/arr/films/example/movie.mkv", "radarr")
	if e != nil || mapped != "/worker/movies/example/movie.mkv" {
		t.Fatal("valid mapping refused")
	}
	if !p.blocked(engine.LibraryTitle{Kind: "radarr", ID: 7}) || p.blocked(engine.LibraryTitle{Kind: "sonarr", ID: 7}) || !p.blocked(engine.LibraryTitle{Path: "/arr/films/keep/file.mkv"}) {
		t.Fatal("protection identity mismatch")
	}
	if _, e = p.Action(context.Background(), "arbitrary", plugin.Config{}); e != plugin.ErrNotFound {
		t.Fatal("unknown action admitted")
	}
}

// The page API keeps the action barriers: an unreviewed plan never reaches the sidecar.
func TestPageAPIGatesExecution(t *testing.T) {
	hits := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer sidecar.Close()
	p := New(testHost{t.TempDir()}).(*Integration)
	if e := p.Configure(plugin.Config{"tdarr_url": "http://service.invalid", "sidecar_url": sidecar.URL}); e != nil {
		t.Fatal(e)
	}
	body := `{"plan":{"selection":[{"kind":"radarr","id":1}],"files":[],"paths":["/worker/movies/a.mkv"],"total_bytes":1,"selected_bytes":1,"withPc":false},"hash":"x","confirmation_token":"forged","confirm":true}`
	out, err := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "batch/execute", Body: []byte(body), Actor: "bearer"})
	if err != nil || out.Status != 409 || hits != 0 {
		t.Fatalf("forged confirmation: %d %s, sidecar hits %d", out.Status, out.Body, hits)
	}
	if _, err = p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "status"}); err != plugin.ErrNotFound {
		t.Fatal("unknown route admitted")
	}
}

func TestDirectShrink(t *testing.T) {
	validated, shrinks := 0, 0
	validation := `{"paths":["/worker/movies/Example/movie.mkv"]}`
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/cruddb" {
			w.Write([]byte(`[]`))
			return
		}
		if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer fixture" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("bad sidecar request")
		}
		switch r.URL.Path {
		case "/validate":
			validated++
			w.Write([]byte(validation))
		case "/shrink":
			shrinks++
			w.WriteHeader(202)
			w.Write([]byte(`{"exit_code":0,"stdout":"queued"}`))
		default:
			t.Error("unexpected route")
		}
	}))
	defer sidecar.Close()
	p := New(testHost{t.TempDir()}).(*Integration)
	if err := p.Configure(plugin.Config{"tdarr_url": sidecar.URL, "sidecar_url": sidecar.URL, "sidecar_key": "fixture", "radarr_root": "/arr/films", "tdarr_movie_root": "/worker/movies", "protected_titles": `[{"path":"/arr/films/Keep"}]`}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"paths":[]}`, 400}, {`{"paths":["/arr/films/Keep/movie.mkv"]}`, 403},
		{`{"paths":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q"]}`, 400},
		{`{"paths":["/arr/films/Example"],"extra":true}`, 400},
		{`{"paths":["/arr/films/Example"],"withPc":true}`, 202},
	} {
		out, err := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "shrink", Body: []byte(tc.body)})
		if err != nil || out.Status != tc.status {
			t.Fatal(out.Status, string(out.Body), err)
		}
	}
	if validated != 1 || shrinks != 1 {
		t.Fatal(validated, shrinks)
	}
	for _, bad := range []string{`{"paths":[]}`, `{"paths":["/etc/a.mkv"]}`, `{"paths":["/worker/movies/../a.mkv"]}`, `{"paths":["/worker/movies/Keep/a.mkv"]}`} {
		validation = bad
		out, err := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "shrink", Body: []byte(`{"paths":["/arr/films/Example"]}`)})
		if err != nil || out.Status < 400 || shrinks != 1 {
			t.Fatal("bad validation forwarded", out.Status, err)
		}
	}
	for _, limit := range []string{"", "0"} {
		out, err := p.API(context.Background(), plugin.APIRequest{Method: "GET", Path: "shrink-candidates", Query: map[string][]string{"limit": {limit}}})
		want := 200
		if limit == "0" {
			want = 400
		}
		if err != nil || out.Status != want || limit == "" && string(out.Body) != "[]" {
			t.Fatal(out.Status, string(out.Body), err)
		}
	}
	if p.APITimeout("POST", "shrink") != 6*time.Minute {
		t.Fatal("missing shrink deadline")
	}
}
