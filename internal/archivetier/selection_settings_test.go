package archivetier

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }

func TestSelectionOverridesLayerAndValidate(t *testing.T) {
	base := DefaultSelection()
	base.TrustedGroups = []string{"trusted"}
	o := SelectionOverrides{Sizes: map[string]SizeOverrides{Mode1080p: {Target: ptr(2.5)}}, CodecFactors: map[string]*float64{"hevc": ptr(1.2)}, TrustedGroups: ptr([]string{}), MinScore: ptr(0.0)}
	out, err := o.Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if out.MinScore != 0 || len(out.TrustedGroups) != 0 || out.Sizes[Mode1080p] != (SizeBand{1, 2.5, 5}) || out.Sizes[Mode2160p] != base.Sizes[Mode2160p] || out.CodecFactors["avc"] != 2 || out.CodecFactors["hevc"] != 1.2 {
		t.Fatalf("bad layering: %+v", out)
	}
	out.CodecFactors["avc"] = 7
	if base.CodecFactors["avc"] != 2 || base.MinScore != 60 || len(base.TrustedGroups) != 1 {
		t.Fatal("mutated file defaults")
	}
	for _, bad := range []SelectionOverrides{
		{MinScore: ptr(-1.0)}, {MinScore: ptr(101.0)}, {MinScore: ptr(math.NaN())},
		{CodecFactors: map[string]*float64{"avc": ptr(0.0)}}, {CodecFactors: map[string]*float64{"hevc": ptr(-1.0)}},
		{CodecFactors: map[string]*float64{"av1": ptr(math.Inf(1))}},
		{CodecFactors: map[string]*float64{"hevc": ptr(1e308)}}, {CodecFactors: map[string]*float64{"vp9": ptr(1.0)}},
		{Sizes: map[string]SizeOverrides{Mode1080p: {Floor: ptr(4.0)}}},
		{Sizes: map[string]SizeOverrides{Mode2160p: {Target: ptr(14.0)}}},
		{Sizes: map[string]SizeOverrides{"720p": {Target: ptr(2.0)}}},
	} {
		if _, err := bad.Apply(base); err == nil {
			t.Fatalf("accepted invalid overrides: %+v", bad)
		}
	}
	reset, err := (SelectionOverrides{}).Apply(base)
	if err != nil || !reflect.DeepEqual(reset, base) {
		t.Fatalf("reset: %+v %v", reset, err)
	}
}

func TestCachedRescorePreservesSearchAndJobState(t *testing.T) {
	c := testConfig()
	target := selectionTarget("sonarr")
	releases := fixture[[]Release](t, "example-series-releases.json")
	at := time.Now().Add(-time.Hour)
	d := Select(target, releases, c)
	d.SearchedAt = at
	st := State{Report: []Decision{sanitized(d)}, Lookups: map[string]Lookup{targetKey(target): cachedLookup(target, d, releases, at)}, SearchTimes: []time.Time{at}, SearchesUsed: 4, Job: &Job{Target: target, Phase: Downloading, Score: d.Score, Selection: &c.Selection}}
	next := c
	next.Selection = DefaultSelection()
	next.Selection.MinScore = 100
	jobJSON, _ := json.Marshal(st.Job)
	result := RescoreCached(&st, c, next)
	if len(result.Titles) != 1 || result.CurrentAutoPicks != 1 || result.AutoPicks != 0 || result.Titles[0].Proposed != nil {
		t.Fatalf("preview: %+v", result)
	}
	if st.Report[0].Release != nil || st.Report[0].Candidates[0].Score.MinScore != 100 || st.Lookups[targetKey(target)].Decision.Release != nil {
		t.Fatal("lookup/report not rescored")
	}
	afterJSON, _ := json.Marshal(st.Job)
	if string(jobJSON) != string(afterJSON) || !st.Lookups[targetKey(target)].At.Equal(at) || st.SearchesUsed != 4 || !reflect.DeepEqual(st.SearchTimes, []time.Time{at}) {
		t.Fatal("changed job or search accounting")
	}
	// Bad release exclusions survive settings edits, including their chip.
	id := st.Report[0].Candidates[0].ID
	st.BadReleases = map[string]map[string]string{targetKey(target): {id: "bad audio"}}
	RescoreCached(&st, next, c)
	for _, candidate := range st.Report[0].Candidates {
		if candidate.ID == id && candidate.Rejection != "rejected after download: bad audio" {
			t.Fatal("lost bad release")
		}
	}
	// Eligibility refusals remain even when selection would pass.
	st.Report[0].Reason = "watched within idle window"
	st.Report[0].Release = nil
	RescoreCached(&st, c, c)
	if st.Report[0].Release != nil || st.Report[0].Reason != "watched within idle window" {
		t.Fatal("cleared eligibility refusal")
	}
}

func TestScoreStoresScaledContextAndFeatureNames(t *testing.T) {
	c := testConfig()
	target := selectionTarget("radarr")
	releases := fixture[[]Release](t, "example-movie-releases.json")
	d := Select(target, releases, c)
	if d.Score.Mode != Mode2160p || d.Score.Target != 8 || d.Score.Floor != 4 || d.Score.Cap != 14 || d.Score.MinScore != 60 || d.Score.CodecFactor != 1 {
		t.Fatalf("missing context: %+v", d.Score)
	}
	if !reflect.DeepEqual(d.Score.FeatureNames, []string{"DD+ 5.1", "Atmos/7.1", "4K bonus"}) {
		t.Fatalf("features: %+v", d.Score.FeatureNames)
	}
	r := *d.Release
	r.Title = "Example Movie.1080p.BluRay.x264.DD5.1-unknown"
	r.Quality.Quality.Resolution = 1080
	s := score(r, target, c)
	if s.Target != 6 || s.Cap != 10 || s.Floor != 2 || s.Trusted || len(s.FeatureNames) != 0 {
		t.Fatalf("codec scale: %+v", s)
	}
	r.Title = "Example Movie.1080p.BluRay.unknown-unknown"
	s = score(r, target, c)
	if _, err := json.Marshal(s); err != nil {
		t.Fatalf("unscorable release must remain JSON: %v", err)
	}
}

func TestCachedRescoreSkipsLegacyCacheWithoutReleases(t *testing.T) {
	c := testConfig()
	target := selectionTarget("sonarr")
	d := Select(target, fixture[[]Release](t, "example-series-releases.json"), c)
	d.Reason = NoGoodRelease
	d.Release = nil
	l := cachedLookup(target, d, nil, time.Now())
	st := State{Report: []Decision{d}, Lookups: map[string]Lookup{targetKey(target): l}}
	before, _ := json.Marshal(st.Lookups)
	result := RescoreCached(&st, c, c)
	after, _ := json.Marshal(st.Lookups)
	if len(result.Titles) != 0 || string(before) != string(after) {
		t.Fatal("changed legacy cache's refresh path")
	}
}

func TestSelectionGroupNamesNormalizeAndDeny(t *testing.T) {
	base := DefaultSelection()
	out, err := (SelectionOverrides{TrustedGroups: ptr([]string{" NewGroup ", "newgroup"}), DeniedGroups: ptr([]string{" trusted ", "TRUSTED"})}).Apply(base)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.TrustedGroups, []string{"NewGroup"}) || !reflect.DeepEqual(out.DeniedGroups, []string{"trusted"}) {
		t.Fatalf("groups not trimmed/deduped: %+v", out)
	}
	c := testConfig()
	c.Selection = out
	target := selectionTarget("sonarr")
	found := false
	for _, r := range fixture[[]Release](t, "example-series-releases.json") {
		if strings.EqualFold(releaseGroup(r), "trusted") {
			found = true
			if why := Reject(r, target, c); why != "denied release group" {
				t.Fatalf("spaced group not denied: %s", why)
			}
		}
	}
	if !found {
		t.Fatal("fixture has no trusted release")
	}
}

func TestSelectionGroupNamesRejectInvalid(t *testing.T) {
	tooMany := make([]string, 51)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("group%d", i)
	}
	cases := map[string][]string{
		"empty": {""}, "whitespace": {"   "}, "control": {"trusted\x00"},
		"newline": {"trusted\n"}, "carriage return": {"\rtrusted"}, "tab": {"d3\tg"},
		"unicode newline": {"trusted\u2028"}, "long": {strings.Repeat("g", 65)},
		"too many": tooMany,
	}
	for name, groups := range cases {
		for _, kind := range []string{"trusted", "denied"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				o := SelectionOverrides{}
				if kind == "trusted" {
					o.TrustedGroups = &groups
				} else {
					o.DeniedGroups = &groups
				}
				if _, err := o.Apply(DefaultSelection()); err == nil {
					t.Fatal("accepted invalid group names")
				}
			})
		}
	}
	t.Run("both lists", func(t *testing.T) {
		o := SelectionOverrides{TrustedGroups: ptr([]string{" trusted "}), DeniedGroups: ptr([]string{"TRUSTED"})}
		if _, err := o.Apply(DefaultSelection()); err == nil {
			t.Fatal("accepted group in both lists")
		}
	})
}

func TestSelectionSizeAndCodecBounds(t *testing.T) {
	for name, band := range map[string]SizeBand{
		"low floor": {0.099, 3, 5}, "huge floor": {1e12, 2e12, 3e12},
		"huge target": {1, 1e12, 2e12}, "huge cap": {1, 3, 1e12},
	} {
		t.Run(name, func(t *testing.T) {
			o := SelectionOverrides{Sizes: map[string]SizeOverrides{Mode1080p: {Floor: &band.Floor, Target: &band.Target, Cap: &band.Cap}}}
			if _, err := o.Apply(DefaultSelection()); err == nil {
				t.Fatal("accepted unbounded sizes")
			}
		})
	}
	for _, factor := range []float64{0.099, 10.001, 1e12} {
		t.Run(fmt.Sprint(factor), func(t *testing.T) {
			o := SelectionOverrides{CodecFactors: map[string]*float64{"hevc": &factor}}
			if _, err := o.Apply(DefaultSelection()); err == nil {
				t.Fatal("accepted unbounded codec factor")
			}
		})
	}
	o := SelectionOverrides{Sizes: map[string]SizeOverrides{Mode1080p: {Floor: ptr(0.1), Cap: ptr(100.0)}}, CodecFactors: map[string]*float64{"hevc": ptr(0.1), "avc": ptr(10.0)}}
	if _, err := o.Apply(DefaultSelection()); err != nil {
		t.Fatalf("valid boundary refused: %v", err)
	}
}

func TestCapBytesRefusesOverflow(t *testing.T) {
	c := testConfig()
	target := selectionTarget("radarr")
	target.Runtime = 120
	if got := capBytes(target, c, Mode1080p, 2); got != 20*GB {
		t.Fatalf("normal cap: %d", got)
	}
	for _, cap := range []float64{1e12, math.Inf(1), math.NaN(), -1} {
		c.Selection.Sizes[Mode1080p] = SizeBand{1, 3, cap}
		if got := capBytes(target, c, Mode1080p, 1); got != 0 {
			t.Fatalf("unsafe cap %v converted to %d", cap, got)
		}
	}
	c = testConfig()
	// Even valid settings cannot make an untrusted runtime overflow bytes.
	target.Runtime = 1e12
	if got := capBytes(target, c, Mode1080p, 1); got != 0 {
		t.Fatalf("runtime overflow: %d", got)
	}
}
