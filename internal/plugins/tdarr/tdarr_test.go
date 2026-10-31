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

// The page API keeps the action barriers: preview and execute need the
// operator credential, and an unreviewed plan never reaches the sidecar.
func TestPageAPIGatesExecution(t *testing.T) {
	hits := 0
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer sidecar.Close()
	p := New(testHost{t.TempDir()}).(*Integration)
	if e := p.Configure(plugin.Config{"tdarr_url": "http://service.invalid", "sidecar_url": sidecar.URL}); e != nil {
		t.Fatal(e)
	}
	if p.APIOperator("GET", "status") || p.APIOperator("GET", "batch/candidates") || !p.APIOperator("POST", "batch/preview") || !p.APIOperator("POST", "batch/execute") {
		t.Fatal("operator routes mismatch")
	}
	body := `{"plan":{"selection":[{"kind":"radarr","id":1}],"files":[],"paths":["/worker/movies/a.mkv"],"total_bytes":1,"selected_bytes":1,"withPc":false},"hash":"x","confirmation_token":"forged","confirm":true}`
	out, err := p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "batch/execute", Body: []byte(body), Actor: "operator"})
	if err != nil || out.Status != 409 || hits != 0 {
		t.Fatalf("forged confirmation: %d %s, sidecar hits %d", out.Status, out.Body, hits)
	}
	if _, err = p.API(context.Background(), plugin.APIRequest{Method: "POST", Path: "status"}); err != plugin.ErrNotFound {
		t.Fatal("unknown route admitted")
	}
}
