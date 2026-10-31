package taste

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

type host struct {
	dir    string
	client *http.Client
}

func (h host) HTTP() *http.Client  { return h.client }
func (h host) CacheDir() string    { return h.dir }
func (h host) Now() time.Time      { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
func (h host) Logf(string, ...any) {}

type source struct{ cancelled chan bool }

func (source) WatchHistory(context.Context, int) ([]tastedata.WatchTitle, error) { return nil, nil }
func (source) WatchIdentity(context.Context, tastedata.WatchTitle) (tastedata.BrowseTitle, error) {
	return tastedata.BrowseTitle{}, nil
}
func (source) Recommendations(context.Context, string, int) ([]tastedata.SuggestedTitle, error) {
	return nil, nil
}
func (source) RecommendationsPage(context.Context, string, int, int) ([]tastedata.SuggestedTitle, error) {
	return nil, nil
}
func (source) RecommendationAvailability(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s source) TitleFacts(ctx context.Context, _ string, id int) (tastedata.TitleFacts, error) {
	if s.cancelled != nil {
		<-ctx.Done()
		s.cancelled <- true
		return tastedata.TitleFacts{}, ctx.Err()
	}
	return tastedata.TitleFacts{SuggestedTitle: tastedata.SuggestedTitle{BrowseTitle: tastedata.BrowseTitle{ID: id, Kind: "movie", Title: "Fixture"}}}, nil
}
func (source) PersonWork(context.Context, int) ([]tastedata.SuggestedTitle, error) { return nil, nil }
func (source) Search(context.Context, string) ([]tastedata.BrowseTitle, error)     { return nil, nil }
func (source) Details(context.Context, string, int) (tastedata.TitleDetail, error) {
	return tastedata.TitleDetail{}, nil
}
func TestLegacyBytesOwnerAndOpaqueArtifact(t *testing.T) {
	b := []byte("{\n \"owner\": 9, \"titles\": {\"movie:5\":{\"verdict\":\"loved\",\"at\":\"2022-01-01T00:00:00Z\"}}, \"future\": true\n}")
	files := map[string][]byte{"taste-feedback.json": b, "tastetwins.gob": {0xff, 0, 1}}
	out, e := ConvertLegacy(files, "household-one", 9)
	if e != nil || !bytes.Equal(out["plugins/taste/profiles/household-one/taste-feedback.json"], b) {
		t.Fatalf("byte preservation: %v", e)
	}
	if _, e = ConvertLegacy(files, "../bad", 9); e == nil {
		t.Fatal("unsafe profile accepted")
	}
	if _, e = ConvertLegacy(files, "good", 8); e == nil {
		t.Fatal("wrong owner accepted")
	}
}
func TestProfileIsolationAndFeedbackPersistence(t *testing.T) {
	h := host{dir: t.TempDir(), client: &http.Client{}}
	p := New(h).(*Integration)
	p.SetRecommendationSource(source{})
	if e := p.Configure(plugin.Config{"bindings": `{"first":{"owner":11},"second":{"owner":22}}`}); e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if e := p.Feedback(context.Background(), "first", "movie", 4, "loved", "Fixture", "2024"); e != nil {
		t.Fatal(e)
	}
	if e := p.Feedback(context.Background(), "missing", "movie", 4, "liked", "Fixture", "2024"); e == nil {
		t.Fatal("unbound profile accepted")
	}
	first, e := p.profiles["first"].feedback.Load()
	if e != nil || first["movie:4"].Verdict != "loved" {
		t.Fatal(first, e)
	}
	second, e := p.profiles["second"].feedback.Load()
	if e != nil || len(second) != 0 {
		t.Fatal("cross-profile feedback", second, e)
	}
	v, e := p.View(context.Background(), "known", url.Values{"profile": {"first"}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = plugin.ValidateView(v, p.Manifest()); e != nil {
		t.Fatal(e)
	}
	if e = p.Configure(plugin.Config{"bindings": `{"first":{"owner":22}}`}); e != nil {
		t.Fatal(e)
	}
	if _, e = p.household("first"); e == nil {
		t.Fatal("existing state rebound to another owner")
	}
}
func TestCloseCancelsAndDrainsModel(t *testing.T) {
	h := host{dir: t.TempDir(), client: &http.Client{}}
	dir := filepath.Join(h.dir, "profiles", "one")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	stars := 5.0
	if e := letterboxd.Save(filepath.Join(dir, "letterboxd.json"), letterboxd.Profile{Owner: 1, Version: 1, ImportedAt: h.Now(), Films: []letterboxd.Film{{Title: "Film", Year: "2024", Rating: &stars, Match: &tastedata.BrowseTitle{ID: 1, Kind: "movie", Title: "Film", Year: "2024"}}}}); e != nil {
		t.Fatal(e)
	}
	p := New(h).(*Integration)
	cancelled := make(chan bool, 2)
	p.SetRecommendationSource(source{cancelled: cancelled})
	if e := p.Configure(plugin.Config{"bindings": `{"one":{"owner":1}}`}); e != nil {
		t.Fatal(e)
	}
	if _, e := p.household("one"); e != nil {
		t.Fatal(e)
	}
	done := make(chan bool)
	go func() { p.Close(); done <- true }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not drain workers")
	}
}
func TestExplicitVerdictOverridesImportedRating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedback.json")
	f := engine.Feedback{Path: path, Owner: 3}
	now := time.Now()
	if e := f.Set("movie", 2, "ignore", "Example", "2023", now); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(b), `"ignore"`) {
		t.Fatal(string(b), e)
	}
}

func TestLocalValidationIsInertAndRejectsForeignProfile(t *testing.T) {
	h := host{dir: t.TempDir(), client: &http.Client{}}
	p := New(h).(*Integration)
	p.SetRecommendationSource(source{})
	if e := p.Configure(plugin.Config{"bindings": `{"one":{"owner":1}}`}); e != nil {
		t.Fatal(e)
	}
	if e := p.ValidateLocal(); e != nil {
		t.Fatal(e)
	}
	if p.cancel != nil {
		t.Fatal("validation started workers")
	}
	if _, e := os.Stat(filepath.Join(h.dir, "profiles")); !os.IsNotExist(e) {
		t.Fatal("validation created state")
	}
	dir := filepath.Join(h.dir, "profiles", "one")
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(dir, "taste-model.json"), []byte(`{"owner":2,"picks":[]}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e := p.ValidateLocal(); e == nil {
		t.Fatal("foreign model accepted")
	}
}
