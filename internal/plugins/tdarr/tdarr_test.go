package tdarr

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/tdarr/engine"
	"net/http"
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
