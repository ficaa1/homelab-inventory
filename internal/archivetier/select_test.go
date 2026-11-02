package archivetier

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func selectionTarget(kind string) Target {
	t := movieTarget()
	t.Title = "Example Movie"
	t.Runtime = 126
	t.Files[0].Size = 77253337510
	if kind == "sonarr" {
		t = target()
		t.Runtime = 55
		t.Episodes = nil
		t.Files = nil
		for i := 1; i <= 10; i++ {
			t.Episodes = append(t.Episodes, Episode{ID: i, FileID: i, Season: 3})
			t.Files = append(t.Files, File{ID: i, Size: 10050000000})
		}
	}
	return t
}

// Balanced releases outrank undersized packs and unsafe formats.
func TestSelectionFixtures(t *testing.T) {
	for _, tc := range []struct {
		name, kind, group string
		fourK             bool
		total             float64
	}{
		{"example-series", "sonarr", "trusted", false, 73.0}, {"example-series", "sonarr", "", true, 0},
		{"example-movie", "radarr", "balanced", false, 90.3}, {"example-movie", "radarr", "balanced", true, 85.3},
	} {
		t.Run(tc.name+map[bool]string{false: "1080p", true: "4k"}[tc.fourK], func(t *testing.T) {
			target := selectionTarget(tc.kind)
			target.FourK = tc.fourK
			rs := fixture[[]Release](t, tc.name+"-releases.json")
			d := Select(target, rs, testConfig())
			b, _ := json.Marshal(d)
			t.Log(string(b))
			if tc.group == "" {
				if d.Release != nil {
					t.Fatalf("expected skip, got %+v", d.Release)
				}
				return
			}
			if d.Release == nil || !strings.EqualFold(releaseGroup(*d.Release), tc.group) {
				t.Fatalf("expected %s: %+v", tc.group, d)
			}
			if math.Abs(d.Score.Total-tc.total) > .05 {
				t.Fatalf("score %.2f, want %.1f", d.Score.Total, tc.total)
			}
			if d.RunnerUp != nil && d.Score.Total-d.RunnerUp.Score.Total < 5 {
				t.Fatalf("winner not clear of runner-up %+v", d.RunnerUp)
			}
		})
	}
	// Example Series's small packs pass the filters but must stay well below trusted.
	target := selectionTarget("sonarr")
	for _, r := range fixture[[]Release](t, "example-series-releases.json") {
		for _, g := range []string{"compact", "Lootera", "H4XO"} {
			if strings.Contains(r.Title, g) && Reject(r, target, testConfig()) == "" && score(r, target, testConfig()).Total >= 60 {
				t.Fatalf("%s reaches the minimum score", r.Title)
			}
		}
	}
}
func TestNameProfiles(t *testing.T) {
	c := testConfig()
	q := qual(1080)
	for _, tc := range []struct {
		name string
		ok   bool
	}{
		{"Title 1080p WEBRip 10Bit DDP5.1 H265-trusted", true},
		{"Title 1080p BluRay x264 DD+7.1-group", true},
		{"Title 1080p WEB-DL AV1 AAC 6ch", true},
		{"Title 1080p WEB-DL HEVC TrueHD Atmos 7.1", false},
		{"Title 1080p WEB-DL HEVC FLAC 5.1", false},
		{"Title 1080p WEB-DL HEVC DDP Atmos", true},
		{"Title 1080p WEB-DL HEVC AAC2.0", false},
		{"Title 1080p WEB-DL HEVC", true},
		{"Title 1080p BluRay HEVC LPCM 5.1", false},
		{"Title 1080p BluRay REMUX HEVC DD5.1", false},
		{"Title 1080p COMPLETE BLURAY HEVC DD5.1", false},
		{"Title 1080p HDTV HEVC DD5.1", false},
		{"Title 1080p CAM HEVC DD5.1", false},
		{"Title 1080p TS HEVC DD5.1", false},
		{"Title 1080p TC HEVC DD5.1", false},
		{"Title 1080p SCREENER HEVC DD5.1", false},
		{"Title 1080p BluRay HEVC DTS-HD MA 5.1", false},
		{"Title 1080p BluRay HEVC TrueHD 7.1 DDP5.1", true},
		{"Title 1080p WEB-DL DV P8.1 HEVC DDP2.0", false},
		{"Title 1080p WEB-DL DoVi P8 1 HEVC AAC", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := profileReject(Mode1080p, q, tc.name, c); (got == "") != tc.ok {
				t.Fatalf("%s", got)
			}
		})
	}
	sonarrRemux := q
	sonarrRemux.Quality.Name = "Bluray-1080p Remux"
	if profileReject(Mode1080p, sonarrRemux, "Title 1080p BluRay HEVC DDP5.1", c) == "" {
		t.Fatal("Sonarr remux quality accepted")
	}
	q.Quality.Resolution = 2160
	for _, n := range []string{"DV", "SDR", "DV SDR HDR10", ""} {
		if profileReject(Mode2160p, q, "Title HEVC DDP5.1 "+n, c) == "" {
			t.Fatal("unsafe HDR", n)
		}
	}
	for _, n := range []string{"HDR", "DV HDR10", "DoVi HDR10+"} {
		if why := profileReject(Mode2160p, q, "Title HEVC DDP5.1 "+n, c); why != "" {
			t.Fatal(n, why)
		}
	}
}
func TestScoreAndCodecBands(t *testing.T) {
	c := testConfig()
	target := selectionTarget("radarr")
	target.Runtime = 120
	target.Files[0].Size = 100 * GB
	r := Release{Title: "Title 1080p WEBRip 10Bit DDP5.1 x265-trusted", Size: 6 * GB, Seeders: 100}
	s := score(r, target, c)
	if math.Abs(s.Total-98.8) > .01 || math.Abs(s.SizeFit-50) > .001 {
		t.Fatalf("score %+v", s)
	}
	// Floor, target and cap all scale with the codec factor.
	for _, tc := range []struct {
		codec  string
		factor float64
	}{{"x265", 1}, {"x264", 2}, {"AV1", .85}} {
		r.Title = "Title 1080p WEBRip DDP5.1 " + tc.codec
		r.Size = int64(2*tc.factor*float64(GB)) - 1
		if sizeReject(r, target, c) != "below quality floor" {
			t.Fatal("floor", tc.codec)
		}
		r.Size = int64(6 * tc.factor * float64(GB))
		if sizeReject(r, target, c) != "" || math.Abs(score(r, target, c).SizeFit-50) > .001 {
			t.Fatal("target", tc.codec)
		}
		r.Size = int64(10 * tc.factor * float64(GB))
		if sizeReject(r, target, c) != "" {
			t.Fatal("at cap", tc.codec)
		}
		r.Size++
		if sizeReject(r, target, c) != "size cap" {
			t.Fatal("cap", tc.codec)
		}
	}
}
func TestScoreThresholdRuntimeAndGroups(t *testing.T) {
	c := testConfig()
	target := target()
	r := fixture[[]Release](t, "sonarr.json")[2]
	c.Selection.MinScore = 100
	if d := Select(target, []Release{r}, c); d.Release != nil || d.Reason != NoGoodRelease || d.RunnerUp == nil {
		t.Fatalf("threshold: %+v", d)
	}
	c.Selection.MinScore = 60
	c.Selection.DeniedGroups = []string{"compact"}
	if Reject(r, target, c) != "denied release group" {
		t.Fatal("denied group")
	}
	target.Runtime = 0
	h, fallback := target.Hours()
	if !fallback || h != 1.5 {
		t.Fatal(h, fallback)
	}
	if d := Select(target, nil, c); !strings.Contains(d.Note, "45 min") {
		t.Fatal(d.Note)
	}
	m := movieTarget()
	h, fallback = m.Hours()
	if !fallback || h != 2 {
		t.Fatal(h, fallback)
	}
}

func TestRankingTiesUseEveryTieBreak(t *testing.T) {
	a := Release{Guid: "a", Size: 10, Seeders: 10}
	b := Release{Guid: "b", Size: 10, Seeders: 10}
	for _, tc := range []struct {
		name   string
		mutate func(*Release, *Release, map[string]Score)
		winner string
	}{
		{"identity", func(*Release, *Release, map[string]Score) {}, "a"},
		{"seeders", func(a, b *Release, s map[string]Score) { b.Seeders = 11 }, "b"},
		{"smaller", func(a, b *Release, s map[string]Score) { b.Size = 9 }, "b"},
		{"size fit", func(a, b *Release, s map[string]Score) { s["b"] = Score{Total: 70, SizeFit: 41} }, "b"},
		{"unrounded total", func(a, b *Release, s map[string]Score) { s["b"] = Score{Total: 70.001, SizeFit: 0} }, "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x, y := a, b
			s := map[string]Score{"a": {Total: 70, SizeFit: 40}, "b": {Total: 70, SizeFit: 40}}
			tc.mutate(&x, &y, s)
			rs := []Release{y, x}
			rank(rs, s)
			if rs[0].Guid != tc.winner {
				t.Fatal(rs)
			}
		})
	}
}

func TestUnsafeSelectionConfigRefused(t *testing.T) {
	for _, mutate := range []func(*Config){
		func(c *Config) { c.DownloadMinFreeGB = -1 }, func(c *Config) { c.DownloadMinFreeGB = math.NaN() },
		func(c *Config) { c.DownloadMinFreeGB = math.Inf(1) },
		func(c *Config) { c.MinSeeders = 9 }, func(c *Config) { c.MaxFraction = .6 },
		func(c *Config) { c.Selection.CodecFactors["hevc"] = 0 }, func(c *Config) { c.Selection.MinScore = math.NaN() },
		func(c *Config) { c.Selection.CodecFactors["av1"] = math.NaN() },
		func(c *Config) { c.Selection.Sizes[Mode1080p] = SizeBand{Floor: 1, Target: 2, Cap: math.Inf(1)} },
		func(c *Config) { c.SidecarMounts = []MountMapping{{AppRoot: "/downloads", LocalRoot: "/writable"}} },
		func(c *Config) {
			c.SidecarMounts = []MountMapping{{AppRoot: "/library", LocalRoot: "/writable/../escape"}}
		},
	} {
		c := testConfig()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatal("unsafe config accepted")
		}
	}
}

func TestFourKReleaseForTenEightyTarget(t *testing.T) {
	c := testConfig()
	target := selectionTarget("radarr")
	target.Runtime = 120
	target.Files[0].Size = 60 * GB
	q := qual(2160)
	q.Quality.Name = "Bluray-2160p"
	hdr := Release{Guid: "4k", IndexerID: 11, Protocol: "torrent", Flags: json.RawMessage(`1`), Title: "Title 2160p BluRay DDP5.1 HDR x265-grp", Size: 16 * GB, Seeders: 100, Quality: q, Languages: []Language{{Name: "English"}}}
	if releaseMode(hdr, target) != Mode2160p || Reject(hdr, target, c) != "" {
		t.Fatalf("4K HDR release refused for a 1080p target: %s", Reject(hdr, target, c))
	}
	sdr := hdr
	sdr.Title = "Title 2160p BluRay DDP5.1 x265-grp"
	if Reject(sdr, target, c) == "" {
		t.Fatal("4K without HDR10 accepted for a 1080p target")
	}
	big := hdr
	big.Size = 29 * GB // under half, but over the 4K cap of 14 GB/h * 2 h
	if Reject(big, target, c) != "size cap" {
		t.Fatal("4K size band not applied")
	}
	ten := hdr
	ten.Title, ten.Quality = "Title 1080p BluRay DDP5.1 x265-grp", qual(1080)
	ten.Size = 16 * GB / 8 * 3 // same size fit at the 1080p target
	if score(hdr, target, c).Total <= score(ten, target, c).Total {
		t.Fatal("equally good 4K release should outscore 1080p")
	}
}

func TestArrProfileOpinionsAreRankingOnly(t *testing.T) {
	for _, s := range []string{"Bluray-2160p is not wanted in profile", "19.4 GB is larger than maximum allowed 15.6 GB (for Obsession)", "1.2 GB is smaller than minimum allowed 2.0 GB (for X)", "Existing file meets cutoff: WEB 1080p (WEBDL-1080p, WEBRip-1080p) []"} {
		if !rankingOnly(s) {
			t.Error(s)
		}
	}
	for _, s := range []string{"Unable to parse release", "Unknown Movie", "Sample"} {
		if rankingOnly(s) {
			t.Error("accepted", s)
		}
	}
}
