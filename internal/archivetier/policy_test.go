package archivetier

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture[T any](t *testing.T, name string) T {
	t.Helper()
	b, e := os.ReadFile("testdata/" + name)
	if e != nil {
		t.Fatal(e)
	}
	var v T
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func testConfig() Config {
	c := DefaultConfig()
	c.RequireFreeleech = true
	c.DownloadMinFreeGB = 300
	c.Start, c.End = "00:00", "05:00"
	c.Selection.TrustedGroups = []string{"trusted"}
	c.LibraryRoots = []string{"/library"}
	c.DownloadRoots = []string{"/downloads"}
	c.IndexerIDs = map[string]int{"sonarr": 10, "radarr": 11}
	c.PlexKeys = map[string]string{"sonarr:1": "900", "radarr:2": "901"}
	return c
}
func qual(res int) Quality {
	q := Quality{}
	q.Quality.Resolution = res
	q.Quality.ID = 15
	q.Quality.Name = "WEBRip-1080p"
	return q
}
func target() Target {
	return Target{Kind: "sonarr", ID: 1, Title: "Example Series", Season: 3, Runtime: 60, Files: []File{{ID: 10, Path: "/library/Example Series/ep1.mkv", Size: 50 * GB, Quality: qual(2160)}, {ID: 11, Path: "/library/Example Series/ep2.mkv", Size: 51 * GB, Quality: qual(2160)}}, Episodes: []Episode{{ID: 101, Season: 3, FileID: 10, AirDate: "2025-01-01T00:00:00Z"}, {ID: 102, Season: 3, FileID: 11, AirDate: "2025-01-08T00:00:00Z"}}}
}
func movieTarget() Target {
	return Target{Kind: "radarr", ID: 2, Title: "Example Film", Files: []File{{ID: 20, Path: "/library/Example Film/movie.mkv", Size: 45800000000, Quality: qual(2160)}}}
}
func TestFreeleechShapes(t *testing.T) {
	for _, tc := range []struct {
		raw string
		yes bool
	}{{"1", true}, {"3", true}, {"2", false}, {`["G_Freeleech"]`, true}, {`["G_Freeleech","G_DoubleUpload"]`, true}, {`[]`, false}, {`["freeleech"]`, false}, {`"1"`, false}, {`null`, false}, {`["G_Freeleech",null]`, false}, {`[null,"G_Freeleech"]`, false}, {`["G_Freeleech",1]`, false}, {`["G_Freeleech",{}]`, false}, {`["G_Freeleech",""]`, false}, {`["G_Freeleech"`, false}, {`{}`, false}} {
		if got := Freeleech(json.RawMessage(tc.raw)); got != tc.yes {
			t.Errorf("%s: got %v", tc.raw, got)
		}
	}
}
func TestObservedSelection(t *testing.T) {
	c := testConfig()
	rs := fixture[[]Release](t, "sonarr.json")
	d := Select(target(), rs, c)
	if d.Release == nil || d.Release.Guid != "fixture-sonarr-44" || d.Savings != 101*GB-4800000000 {
		t.Fatalf("unexpected selection %+v", d)
	}
	// Pilot's ~15.6 GB option can pass the default movie cap; 3 GB non-freeleech cannot.
	rs = fixture[[]Release](t, "radarr.json")
	small := rs[0]
	small.Guid = "pilot-size-example"
	small.Size = 12000000000
	small.Quality = qual(1080)
	small.Title = "Example Film 2015 1080p BluRay x264 DD5.1"
	tiny := small
	tiny.Size = 3 * GB
	tiny.Flags = json.RawMessage(`[]`)
	tiny.Guid = "not-free"
	rs = append(rs, small, tiny)
	d = Select(movieTarget(), rs, c)
	if d.Release == nil || d.Release.Guid != small.Guid {
		t.Fatalf("unsafe movie selection %+v", d)
	}
	band := c.Selection.Sizes[Mode1080p]
	band.Cap = 2
	c.Selection.Sizes[Mode1080p] = band
	d = Select(movieTarget(), rs, c)
	if d.Release != nil {
		t.Fatal("must keep large original when compact options are not freeleech")
	}
}
func TestReject(t *testing.T) {
	base := fixture[[]Release](t, "sonarr.json")[2]
	for _, tc := range []struct {
		name   string
		mutate func(*Release)
	}{
		{"missing flags", func(r *Release) { r.Flags = nil }}, {"low seeders", func(r *Release) { r.Seeders = 0 }}, {"size cap", func(r *Release) { r.Size = 11 * GB }}, {"unknown rejection", func(r *Release) { r.Rejections = []string{"Release is blocklisted"} }}, {"language rejection", func(r *Release) { r.Rejections = []string{"Language is not wanted"} }}, {"unparsed release", func(r *Release) { r.Rejections = []string{"Unable to parse release"} }}, {"wrong language", func(r *Release) { r.Languages = []Language{{Name: "French"}} }}, {"unknown language", func(r *Release) { r.Languages = nil }}, {"3D", func(r *Release) { r.Title += " 3D" }}, {"extras", func(r *Release) { r.Title += " Extras" }}, {"edition", func(r *Release) { r.Title += " Extended" }}, {"wrong season", func(r *Release) { r.Season = 2 }}, {"single episode", func(r *Release) { r.FullSeason = false }}, {"wrong indexer", func(r *Release) { r.IndexerID = 99 }}, {"wrong quality", func(r *Release) { r.Quality = qual(2160) }}, {"not torrent", func(r *Release) { r.Protocol = "usenet" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if Reject(r, target(), testConfig()) == "" {
				t.Fatal("accepted unsafe release")
			}
		})
	}
	r := base
	r.Size = 50 * GB
	c := testConfig()
	band := c.Selection.Sizes[Mode1080p]
	band.Cap = 100
	c.Selection.Sizes[Mode1080p] = band
	c.MaxFraction = float64(r.Size) / float64(target().Bytes())
	if Reject(r, target(), c) != "insufficient savings" {
		t.Fatal("fraction boundary accepted")
	}
}
func TestTargetEligibility(t *testing.T) {
	now := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	c := testConfig()
	v := target()
	if why := v.Eligible(c, now); why != "" {
		t.Fatal(why)
	}
	v.Episodes[0].FileID = 0
	if v.Eligible(c, now) == "" {
		t.Fatal("missing episode accepted")
	}
	v = target()
	v.Episodes[0].AirDate = "2030-01-01T00:00:00Z"
	if v.Eligible(c, now) == "" {
		t.Fatal("airing season accepted")
	}
	v = target()
	delete(c.PlexKeys, v.Key())
	if v.Eligible(c, now) == "" {
		t.Fatal("unmapped Plex title accepted")
	}
}
func TestManualImportValidation(t *testing.T) {
	c := testConfig()
	items := fixture[[]ManualItem](t, "manual-sonarr.json")
	files, e := ValidateImport(items, target(), c, Mode1080p)
	if e != nil {
		t.Fatal(e)
	}
	if files[0]["downloadId"] != nil || files[0]["episodeFileId"] != nil {
		t.Fatal("tracked source cleanup must not be requested")
	}
	q := files[0]["quality"].(Quality)
	if q.Quality.ID != 15 || len(q.Revision) == 0 {
		t.Fatal("lost quality identity or revision")
	}
	for _, tc := range []struct {
		name   string
		mutate func([]ManualItem) []ManualItem
	}{
		{"incomplete", func(v []ManualItem) []ManualItem { return v[:1] }}, {"duplicates", func(v []ManualItem) []ManualItem { v[1] = v[0]; return v }}, {"wrong episode", func(v []ManualItem) []ManualItem { v[1].Episodes[0].ID = 999; return v }}, {"library source", func(v []ManualItem) []ManualItem { v[0].Path = "/library/Example Series/source.mkv"; return v }}, {"traversal", func(v []ManualItem) []ManualItem { v[0].Path = "/downloads/../library/source.mkv"; return v }}, {"sibling prefix", func(v []ManualItem) []ManualItem { v[0].Path = "/downloads-other/ep.mkv"; return v }}, {"unsafe rejection", func(v []ManualItem) []ManualItem { v[0].Rejections[0].Reason = "Sample"; return v }}, {"size", func(v []ManualItem) []ManualItem { v[0].Size = 20 * GB; return v }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := fixture[[]ManualItem](t, "manual-sonarr.json")
			if _, e := ValidateImport(tc.mutate(v), target(), c, Mode1080p); e == nil {
				t.Fatal("unsafe import accepted")
			}
		})
	}
}
func TestWindowAndConfig(t *testing.T) {
	c := testConfig()
	if e := c.Validate(); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		at   string
		open bool
	}{{"2026-10-06T23:59:00Z", false}, {"2026-10-07T00:00:00Z", true}, {"2026-10-07T03:59:00Z", true}, {"2026-10-07T05:00:00Z", false}, {"2026-10-26T00:00:00Z", true}, {"2026-10-26T05:00:00Z", false}} {
		v, _ := time.Parse(time.RFC3339, tc.at)
		ok, _ := c.Window(v)
		if ok != tc.open {
			t.Errorf("window %s", tc.at)
		}
	}
	c.Start = "23:00"
	c.End = "04:00"
	a, _ := time.Parse(time.RFC3339, "2026-10-06T23:00:00Z")
	b := a.Add(time.Hour)
	_, x := c.Window(a)
	_, y := c.Window(b)
	if x != y {
		t.Fatal("overnight budget date reset at midnight")
	}
	c.LibraryRoots = []string{"/downloads/library"}
	if c.Validate() == nil {
		t.Fatal("overlapping roots accepted")
	}
}
func TestVerifiedAndTransitions(t *testing.T) {
	c := testConfig()
	old := target()
	new := target()
	for i := range new.Files {
		new.Files[i].ID += 100
		new.Files[i].Size = 2 * GB
		new.Files[i].Quality = qual(1080)
		new.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		new.Episodes[i].FileID = new.Files[i].ID
	}
	if !Verified(old, new, c, Mode1080p) {
		t.Fatal("replacement not verified")
	}
	new.Episodes[0].FileID = 0
	if Verified(old, new, c, Mode1080p) {
		t.Fatal("incomplete replacement verified")
	}
	if Verified(old, old, c, Mode1080p) {
		t.Fatal("original verified")
	}
	j := Job{Phase: GrabIntent}
	for _, p := range []Phase{Downloading, ImportIntent, Verifying, Complete} {
		if e := Transition(&j, p); e != nil {
			t.Fatal(e)
		}
	}
	if Transition(&j, Downloading) == nil {
		t.Fatal("completed job restarted")
	}
}

func TestPerFileCapsAndEditionMetadata(t *testing.T) {
	c := testConfig()
	items := fixture[[]ManualItem](t, "manual-sonarr.json")
	items[0].Size = 6 * GB
	items[1].Size = GB
	if _, e := ValidateImport(items, target(), c, Mode1080p); e == nil {
		t.Fatal("uneven pack exceeded episode file cap")
	}
	items[0].Path = strings.Replace(items[0].Path, "x265", "x264", 1)
	if _, e := ValidateImport(items, target(), c, Mode1080p); e != nil {
		t.Fatal("x264 file held to the HEVC cap", e)
	}
	items = fixture[[]ManualItem](t, "manual-sonarr.json")
	items[0].Episodes = append(items[0].Episodes, items[1].Episodes...)
	items[0].Size = 7 * GB
	if _, e := ValidateImport(items[:1], target(), c, Mode1080p); e != nil {
		t.Fatal("multi-episode file incorrectly capped", e)
	}
	old := target()
	now := target()
	for i := range now.Files {
		now.Files[i].ID += 100
		now.Files[i].Quality = qual(1080)
		now.Files[i].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
		now.Episodes[i].FileID = now.Files[i].ID
	}
	now.Files[0].Size = 6 * GB
	now.Files[1].Size = GB
	if Verified(old, now, c, Mode1080p) {
		t.Fatal("final uneven pack exceeded episode file cap")
	}
	// The cap follows the probed codec: the same size is fine for x264.
	now.Files[0].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "x264"}
	if !Verified(old, now, c, Mode1080p) {
		t.Fatal("x264 file held to the HEVC cap")
	}
	now.Files[0].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
	now.Files = now.Files[:1]
	now.Files[0].Size = 7 * GB
	now.Episodes[1].FileID = now.Files[0].ID
	if !Verified(old, now, c, Mode1080p) {
		t.Fatal("multi-episode replacement rejected")
	}
	for _, edition := range []string{"Black.And.Chrome.Edition", "Black_And_Chrome_Edition", "Black-And-Chrome-Edition", "Directors.Cut", "Extended.Edition"} {
		r := fixture[[]Release](t, "sonarr.json")[2]
		r.Edition = edition
		if Reject(r, target(), c) == "" {
			t.Fatal("release edition metadata accepted", edition)
		}
		r.Edition = ""
		r.Title += " " + edition
		if Reject(r, target(), c) == "" {
			t.Fatal("release edition title accepted", edition)
		}
		it := fixture[[]ManualItem](t, "manual-sonarr.json")
		it[0].Edition = edition
		if _, e := ValidateImport(it, target(), c, Mode1080p); e == nil {
			t.Fatal("import edition metadata accepted", edition)
		}
		it[0].Edition = ""
		it[0].Movie.Edition = edition
		if _, e := ValidateImport(it, target(), c, Mode1080p); e == nil {
			t.Fatal("movie edition metadata accepted", edition)
		}
	}
}

// A per-episode floor refuses packs that save the most but are too small to
// look right (Example Series S03: a 4.7 GB season pack against 100 GB of 4K WEB-DL).
func TestQualityFloorRejectsTooSmallReplacements(t *testing.T) {
	base := fixture[[]Release](t, "sonarr.json")[2]
	c := testConfig()
	if Reject(base, target(), c) != "" {
		t.Fatal("fixture release should be acceptable without a floor")
	}
	band := c.Selection.Sizes[Mode1080p]
	h, _ := target().Hours()
	band.Floor = float64(base.Size)/float64(GB)/h + 0.01
	c.Selection.Sizes[Mode1080p] = band
	if got := Reject(base, target(), c); got != "below quality floor" {
		t.Fatalf("release under floor accepted: %q", got)
	}
}

func testMovieRelease(t *testing.T) Release {
	r := fixture[[]Release](t, "radarr.json")[0]
	r.Title = "Example Film 2015 1080p BluRay x265 DDP5.1-test"
	r.Size = 7 * GB
	r.Quality = qual(1080)
	return r
}

func TestOptionalTitleCapAndMinuteRateValidation(t *testing.T) {
	c := testConfig()
	if c.SearchesPerMinute != 5 || c.Validate() != nil {
		t.Fatal("bad unlimited defaults")
	}
	for _, n := range []int{0, 6} {
		c.SearchesPerMinute = n
		if c.Validate() == nil {
			t.Fatal("invalid rate allowed")
		}
	}
}

func TestTitleKeyFormat(t *testing.T) {
	if got := TitleKey("sonarr", 10); got != "sonarr:10" {
		t.Fatalf("TitleKey = %q, want sonarr:10", got)
	}
	if got := (Target{Kind: "radarr", ID: 293}).Key(); got != TitleKey("radarr", 293) {
		t.Fatalf("Target.Key = %q, want %q", got, TitleKey("radarr", 293))
	}
}

func TestLegacyNightlyCapsIgnored(t *testing.T) {
	c := testConfig()
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	json.Unmarshal(data, &fields)
	fields["maxTitles"], fields["maxGB"] = -1, "retired"
	data, _ = json.Marshal(fields)
	if err := json.Unmarshal(data, &c); err != nil || c.Validate() != nil {
		t.Fatalf("legacy caps rejected: %v", err)
	}
	data, _ = json.Marshal(c)
	if strings.Contains(string(data), "maxTitles") || strings.Contains(string(data), "maxGB") {
		t.Fatal("retired caps serialized")
	}
	fields["unknownSetting"] = true
	data, _ = json.Marshal(fields)
	if json.Unmarshal(data, &c) == nil {
		t.Fatal("unknown config accepted")
	}
}

func TestRequireFreeleechSetting(t *testing.T) {
	c := testConfig()
	r := fixture[[]Release](t, "sonarr.json")[2]
	r.Flags = json.RawMessage(`0`)
	for _, reject := range []func(Release, Target, Config) string{Reject, manualReject} {
		c.RequireFreeleech = true
		if why := reject(r, target(), c); why != "not freeleech" {
			t.Fatalf("required: %s", why)
		}
		c.RequireFreeleech = false
		if why := reject(r, target(), c); why != "" {
			t.Fatalf("optional: %s", why)
		}
	}
}
