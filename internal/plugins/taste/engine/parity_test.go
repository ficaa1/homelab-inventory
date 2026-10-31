package engine

import (
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
	"testing"
	"time"
)

func TestVerdictOverridesAndIgnoreRetractsSignal(t *testing.T) {
	films := []tasteFilm{{title: tastedata.BrowseTitle{Kind: "movie", ID: 1, Title: "Imported love"}, signal: 1}, {title: tastedata.BrowseTitle{Kind: "tv", ID: 2, Title: "Shared playback"}, signal: .8}}
	got := applyVerdicts(films, map[string]Verdict{"movie:1": {Verdict: "disliked", Title: "Imported love"}, "tv:2": {Verdict: "ignore", Title: "Shared playback"}, "movie:3": {Verdict: "seen", Title: "Known without opinion"}})
	if len(got) != 2 || got[0].signal != -1 || got[0].source != "you" || !got[1].unknown {
		t.Fatalf("explicit owner judgement lost: %#v", got)
	}
}
func TestRatingScaleAndWatchedWithoutOpinion(t *testing.T) {
	for stars, want := range map[float64]float64{5: 1, 4.5: .8, 4: .55, 3.5: .3, 3: 0, 2: -.6} {
		if got := ratingSignal(stars); got != want {
			t.Fatalf("rating %.1f got %v want %v", stars, got, want)
		}
	}
	p := &letterboxd.Profile{Films: []letterboxd.Film{{Watched: true, Match: &tastedata.BrowseTitle{ID: 1, Kind: "movie"}}}}
	if got := signals(p); len(got) != 0 {
		t.Fatal("watched with no opinion taught taste", got)
	}
	if floorFor(7, "tv") != 7.5 || bayes(10, 1, "2020-01-01", time.Now()) > 7 {
		t.Fatal("quality prior changed")
	}
}
