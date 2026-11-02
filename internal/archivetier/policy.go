// Package archivetier implements an opt-in library replacement job.
// It never deletes files directly; rejected downloads are removed through Arr
// only after their seeding retention period.
package archivetier

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const GB int64 = 1000000000

type Config struct {
	ProbeCommand      string            `json:"probeCommand,omitempty"`
	ReviewedVersions  map[string]string `json:"reviewedVersions"`
	Mounts            []MountMapping    `json:"mounts"`
	FreeSpaceMarginGB float64           `json:"freeSpaceMarginGB"`
	// DownloadMinFreeGB is the free space the download disk (the first
	// download root) must keep after a grab; below it no grab is made.
	DownloadMinFreeGB float64 `json:"downloadMinFreeGB"`
	Timezone          string  `json:"timezone"`
	Start             string  `json:"start"`
	End               string  `json:"end"`
	IdleDays          int     `json:"idleDays"`
	RequireFreeleech  bool    `json:"requireFreeleech"`
	MinSeeders        int     `json:"minSeeders"`
	// Selection is the size table and score knobs; sizes are GB per hour of
	// runtime, so they replace flat per-episode and per-movie caps.
	Selection    Selection `json:"selection"`
	MaxFraction  float64   `json:"maxFraction"`
	TimeoutHours int       `json:"timeoutHours"`
	// SearchCooldownHours is how long one title's release lookup result is
	// reused, in dry-run and applied mode alike. SearchesPerMinute caps
	// tracker lookups in a rolling minute, including the fresh lookup made
	// immediately before a grab. Unattended lookups only run in the window.
	SearchCooldownHours int `json:"searchCooldownHours"`
	SearchesPerMinute   int `json:"searchesPerMinute"`
	// Only, when set, restricts archive to these title keys ("radarr:293"),
	// for a supervised run. Titles still need the archive tag.
	Only      []string `json:"only,omitempty"`
	MaxDState int      `json:"maxDState"`
	ProcRoot  string   `json:"procRoot"`
	// Roots use app paths; Mounts maps them to local read-only inspection paths.
	DownloadRoots []string `json:"downloadRoots"`
	LibraryRoots  []string `json:"libraryRoots"`
	// SidecarMounts maps library app paths to a writable local view used
	// only to write carried-over subtitle files. Empty turns carry-over off.
	SidecarMounts []MountMapping `json:"sidecarMounts,omitempty"`
	// RestoreMounts maps app paths to a writable local view for the
	// dashboard's restore and purge (like -restore-map); it must cover the
	// library roots, the recycle bin and QuarantineDir. Empty refuses both.
	RestoreMounts []MountMapping `json:"restoreMounts,omitempty"`
	// QuarantineDir (an app path) receives replacement files a restore moves
	// aside. It must exist and share the library's filesystem.
	QuarantineDir string            `json:"quarantineDir,omitempty"`
	IndexerIDs    map[string]int    `json:"indexerIDs"`
	PlexKeys      map[string]string `json:"plexKeys"` // optional overrides: "sonarr:123" -> show key; "radarr:456" -> movie key
	Languages     []string          `json:"languages"`
}

func DefaultConfig() Config {
	return Config{ProbeCommand: "ffprobe", ReviewedVersions: map[string]string{"sonarr": "4.0.19.2979", "radarr": "6.3.0.10514"}, FreeSpaceMarginGB: 20, DownloadMinFreeGB: 20, Timezone: "UTC", Start: "01:00", End: "06:00", IdleDays: 30, MinSeeders: 10, Selection: DefaultSelection(), MaxFraction: .5, TimeoutHours: 24, SearchCooldownHours: 24, SearchesPerMinute: 5, MaxDState: 3, ProcRoot: "/proc", DownloadRoots: []string{"/data/downloads/completed"}, LibraryRoots: []string{"/data/media/movies", "/data/media/series"}, Languages: []string{"English"}}
}
func (c Config) Validate() error {
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("invalid timezone")
	}
	a, e := clockMinute(c.Start)
	if e != nil {
		return e
	}
	b, e := clockMinute(c.End)
	if e != nil {
		return e
	}
	if a == b || c.IdleDays < 1 || c.MinSeeders < 10 || c.MaxFraction <= 0 || c.MaxFraction > .5 || c.TimeoutHours < 1 || c.SearchCooldownHours < 1 || c.SearchesPerMinute < 1 || c.SearchesPerMinute > 5 || c.MaxDState < 1 || len(c.Languages) == 0 {
		return fmt.Errorf("invalid archive limits")
	}
	if math.IsNaN(c.DownloadMinFreeGB) || math.IsInf(c.DownloadMinFreeGB, 0) || c.DownloadMinFreeGB < 0 || c.DownloadMinFreeGB > 1e6 {
		return fmt.Errorf("invalid download disk free-space floor")
	}
	if e := c.Selection.Validate(); e != nil {
		return e
	}
	if len(c.DownloadRoots) == 0 || len(c.LibraryRoots) == 0 {
		return fmt.Errorf("explicit download and library roots required")
	}
	for _, roots := range [][]string{c.DownloadRoots, c.LibraryRoots} {
		for _, r := range roots {
			if !strings.HasPrefix(r, "/") || path.Clean(r) != r || r == "/" {
				return fmt.Errorf("invalid root")
			}
		}
	}
	for _, a := range c.DownloadRoots {
		for _, b := range c.LibraryRoots {
			if under(a, b) || under(b, a) {
				return fmt.Errorf("download and library roots overlap")
			}
		}
	}
	for _, m := range c.SidecarMounts {
		if !safePath(m.AppRoot, c.LibraryRoots) && !inRoots(m.AppRoot, c.LibraryRoots) || !filepath.IsAbs(m.LocalRoot) || filepath.Clean(m.LocalRoot) != m.LocalRoot || m.LocalRoot == "/" {
			return fmt.Errorf("sidecar mounts must map library roots to clean local paths")
		}
	}
	for _, m := range c.RestoreMounts {
		if !strings.HasPrefix(m.AppRoot, "/") || path.Clean(m.AppRoot) != m.AppRoot || m.AppRoot == "/" || !filepath.IsAbs(m.LocalRoot) || filepath.Clean(m.LocalRoot) != m.LocalRoot || m.LocalRoot == "/" {
			return fmt.Errorf("restore mounts must map clean app paths to clean local paths")
		}
	}
	if c.QuarantineDir != "" && (!strings.HasPrefix(c.QuarantineDir, "/") || path.Clean(c.QuarantineDir) != c.QuarantineDir || c.QuarantineDir == "/" || safePath(c.QuarantineDir, c.LibraryRoots) || inRoots(c.QuarantineDir, c.LibraryRoots)) {
		return fmt.Errorf("quarantine directory must be a clean app path outside the library roots")
	}
	for _, kind := range []string{"sonarr", "radarr"} {
		if c.IndexerIDs[kind] <= 0 {
			return fmt.Errorf("Configured indexer indexer ID required for %s", kind)
		}
	}
	return nil
}
func clockMinute(s string) (int, error) {
	t, e := time.Parse("15:04", s)
	if e != nil {
		return 0, fmt.Errorf("invalid nightly window")
	}
	return t.Hour()*60 + t.Minute(), nil
}
func (c Config) Window(now time.Time) (bool, string) {
	loc, _ := time.LoadLocation(c.Timezone)
	now = now.In(loc)
	n := now.Hour()*60 + now.Minute()
	a, _ := clockMinute(c.Start)
	b, _ := clockMinute(c.End)
	day := now
	if a > b && n < b {
		day = now.AddDate(0, 0, -1)
	}
	ok := n >= a && n < b
	if a > b {
		ok = n >= a || n < b
	}
	return ok, day.Format("2006-01-02")
}
func under(p, r string) bool { return p == r || strings.HasPrefix(p, r+"/") }
func inRoots(p string, roots []string) bool {
	for _, r := range roots {
		if p == r {
			return true
		}
	}
	return false
}
func safePath(p string, roots []string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	for _, r := range roots {
		if under(p, r) && p != r {
			return true
		}
	}
	return false
}

type Quality struct {
	Revision json.RawMessage `json:"revision,omitempty"`
	Quality  struct {
		ID         int    `json:"id"`
		Source     string `json:"source,omitempty"`
		Modifier   string `json:"modifier,omitempty"`
		Name       string `json:"name"`
		Resolution int    `json:"resolution"`
	} `json:"quality"`
}
type Language struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}
type Release struct {
	SeriesID   int             `json:"seriesId,omitempty"`
	MovieID    int             `json:"movieId,omitempty"`
	Indexer    string          `json:"indexer,omitempty"`
	Edition    string          `json:"edition"`
	Guid       string          `json:"guid"`
	IndexerID  int             `json:"indexerId"`
	Title      string          `json:"title"`
	Size       int64           `json:"size"`
	Seeders    int             `json:"seeders"`
	Flags      json.RawMessage `json:"indexerFlags"`
	Quality    Quality         `json:"quality"`
	Languages  []Language      `json:"languages"`
	Rejections []string        `json:"rejections"`
	FullSeason bool            `json:"fullSeason"`
	Season     int             `json:"seasonNumber"`
	Protocol   string          `json:"protocol"`
	Group      string          `json:"releaseGroup,omitempty"`
}

func Freeleech(raw json.RawMessage) bool {
	var bits int
	if json.Unmarshal(raw, &bits) == nil {
		return bits >= 0 && bits&1 != 0
	}
	var flags []json.RawMessage
	if json.Unmarshal(raw, &flags) == nil {
		found := false
		for _, f := range flags {
			var name string
			if string(f) == "null" || json.Unmarshal(f, &name) != nil || name == "" {
				return false
			}
			if name == "G_Freeleech" {
				found = true
			}
		}
		return found
	}
	return false
}
func resolution(q Quality) int {
	if q.Quality.Resolution > 0 {
		return q.Quality.Resolution
	}
	if strings.Contains(q.Quality.Name, "2160p") {
		return 2160
	}
	if strings.Contains(q.Quality.Name, "1080p") {
		return 1080
	}
	return 0
}

// Only known quality-ranking rejections are ignored. Unknown reasons fail closed.
// rankingOnly is an Arr rejection about its own quality profile or size
// limits, which the archive replaces with its own rules and explicit-quality
// copy import.
func rankingOnly(s string) bool {
	s = strings.ToLower(s)
	if strings.HasSuffix(s, " is not wanted in profile") || strings.Contains(s, " is larger than maximum allowed ") || strings.Contains(s, " is smaller than minimum allowed ") {
		return true
	}
	return s == "existing file meets cutoff" || strings.HasPrefix(s, "existing file meets cutoff:") || strings.HasPrefix(s, "not an upgrade for existing") || strings.HasPrefix(s, "not a quality upgrade for existing") || strings.HasPrefix(s, "existing file on disk is of equal or higher quality") || s == "quality profile does not allow upgrades" || s == "upgrades are not allowed"
}

var unwanted = regexp.MustCompile(`(?i)(^|[ ._\-])(3d|sbs|hsbs|hou|extras?|sample|trailer|dubbed|multi|french|german|italian|spanish|russian|extended|unrated|bonus|collection)([ ._\-]|$)`)

func languageOK(ls []Language, allowed []string) bool {
	if len(ls) == 0 {
		return false
	}
	for _, l := range ls {
		found := false
		for _, a := range allowed {
			if strings.EqualFold(l.Name, a) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type File struct {
	Edition string  `json:"edition"`
	ID      int     `json:"id"`
	Path    string  `json:"path"`
	Size    int64   `json:"size"`
	Season  int     `json:"seasonNumber"`
	Quality Quality `json:"quality"`
	// MediaInfo is the application's probe of the file; final verification
	// needs it for the audio and HDR rules.
	MediaInfo *MediaInfo `json:"mediaInfo,omitempty"`
}

// MediaInfo is the subset of the Arr mediaInfo record the policy reads.
type MediaInfo struct {
	AudioChannels         float64 `json:"audioChannels"`
	AudioCodec            string  `json:"audioCodec"`
	VideoCodec            string  `json:"videoCodec"`
	VideoDynamicRangeType string  `json:"videoDynamicRangeType"`
}
type Episode struct {
	ID       int    `json:"id"`
	SeriesID int    `json:"seriesId"`
	Season   int    `json:"seasonNumber"`
	Number   int    `json:"episodeNumber"`
	FileID   int    `json:"episodeFileId"`
	AirDate  string `json:"airDateUtc"`
}
type Target struct {
	TMDBID     int    `json:"tmdbId,omitempty"`
	TVDBID     int    `json:"tvdbId,omitempty"`
	IMDBID     string `json:"imdbId,omitempty"`
	PlexKey    string `json:"plexKey,omitempty"`
	PlexReason string `json:"plexReason,omitempty"`
	Unsafe     string `json:"unsafe,omitempty"`
	Kind       string `json:"kind"`
	ID         int    `json:"id"`
	Title      string `json:"title"`
	Season     int    `json:"season,omitempty"`
	// Runtime is minutes per episode (series) or of the movie; 0 unknown.
	Runtime int `json:"runtime,omitempty"`
	// FourK is set when the title also carries the 4k tag.
	FourK    bool      `json:"fourK,omitempty"`
	Episodes []Episode `json:"episodes,omitempty"`
	Files    []File    `json:"files"`
}

// TitleKey is the one spelling of a title's lease key; every leaser must use it.
func TitleKey(kind string, id int) string { return fmt.Sprintf("%s:%d", kind, id) }

func (t Target) Key() string { return TitleKey(t.Kind, t.ID) }
func (t Target) Bytes() int64 {
	var n int64
	for _, f := range t.Files {
		n += f.Size
	}
	return n
}
func (t Target) Eligible(c Config, now time.Time) string {
	if t.Unsafe != "" {
		return t.Unsafe
	}
	if len(t.Files) == 0 {
		return "no library files"
	}
	for _, f := range t.Files {
		if f.ID <= 0 || f.Size <= 0 || resolution(f.Quality) != 2160 || !safePath(f.Path, c.LibraryRoots) {
			return "requires known 2160p files in approved library roots"
		}
	}
	if t.Kind == "sonarr" {
		if t.Season <= 0 || len(t.Episodes) == 0 {
			return "specials or empty season"
		}
		ids := map[int]bool{}
		for _, f := range t.Files {
			ids[f.ID] = true
		}
		for _, ep := range t.Episodes {
			date, e := time.Parse(time.RFC3339, ep.AirDate)
			if e != nil || date.After(now) || !ids[ep.FileID] {
				return "season incomplete or still airing"
			}
		}
	}
	if t.plexKey(c) == "" {
		if t.PlexReason != "" {
			return t.PlexReason
		}
		return "not found in Plex"
	}
	if len(c.Only) > 0 {
		for _, k := range c.Only {
			if k == t.Key() {
				return ""
			}
		}
		return "not in the configured title list"
	}
	return ""
}

// ManualEligible keeps only what a copy import and recycle need: known library
// files in approved roots and, for a season, every episode with a file.
// Resolution, recent play and Plex identity are automatic-selection rules.
func (t Target) ManualEligible(c Config, now time.Time) string {
	if t.Unsafe != "" {
		return t.Unsafe
	}
	if len(t.Files) == 0 {
		return "no library files"
	}
	for _, f := range t.Files {
		if f.ID <= 0 || f.Size <= 0 || !safePath(f.Path, c.LibraryRoots) {
			return "requires known files in approved library roots"
		}
	}
	if t.Kind == "sonarr" {
		if t.Season <= 0 || len(t.Episodes) == 0 {
			return "specials or empty season"
		}
		ids := map[int]bool{}
		for _, f := range t.Files {
			ids[f.ID] = true
		}
		for _, ep := range t.Episodes {
			if !ids[ep.FileID] {
				return "season incomplete"
			}
		}
	}
	return ""
}

// manualReject is what an operator approval cannot override: the configured
// torrent indexer, a matching season pack, and an actual saving.
func manualReject(r Release, t Target, c Config) string {
	if r.Guid == "" || r.IndexerID != c.IndexerIDs[t.Kind] || !strings.EqualFold(r.Protocol, "torrent") {
		return "unapproved indexer or protocol"
	}
	if c.RequireFreeleech && !Freeleech(r.Flags) {
		return "not freeleech"
	}
	if t.Kind == "sonarr" && (!r.FullSeason || r.Season != t.Season) {
		return "requires matching full season pack"
	}
	if r.Size <= 0 || r.Size >= t.Bytes() {
		return "not smaller than the current files"
	}
	return ""
}

// Reject returns why a release cannot replace the target, or "".
func Reject(r Release, t Target, c Config) string {
	if r.Guid == "" || r.IndexerID != c.IndexerIDs[t.Kind] || !strings.EqualFold(r.Protocol, "torrent") {
		return "unapproved indexer or protocol"
	}
	if c.RequireFreeleech && !Freeleech(r.Flags) {
		return "not freeleech"
	}
	if why := profileReject(releaseMode(r, t), r.Quality, r.Title, c); why != "" {
		return why
	}
	if groupIn(releaseGroup(r), c.Selection.DeniedGroups) {
		return "denied release group"
	}
	if r.Seeders < c.MinSeeders {
		return "too few seeders"
	}
	if why := sizeReject(r, t, c); why != "" {
		return why
	}
	if float64(r.Size) >= float64(t.Bytes())*c.MaxFraction {
		return "insufficient savings"
	}
	if t.Kind == "sonarr" && (!r.FullSeason || r.Season != t.Season) {
		return "requires matching full season pack"
	}
	if (unwanted.MatchString(r.Title) || unsafeEdition(r.Title+" "+r.Edition)) || !languageOK(r.Languages, c.Languages) {
		return "language, 3D or extras"
	}
	for _, s := range r.Rejections {
		if !rankingOnly(s) {
			return "non-ranking rejection"
		}
	}
	return ""
}

type Decision struct {
	AudioUnverified  bool         `json:"audioUnverified,omitempty"`
	Candidates       []*Candidate `json:"candidates,omitempty"`
	ManuallyApproved bool         `json:"manuallyApproved,omitempty"`
	Target           Target       `json:"target"`
	Release          *Release     `json:"release,omitempty"`
	Reason           string       `json:"reason,omitempty"`
	// Skipped explains why a found release is not grabbed (budget, already
	// attempted tonight); later candidates are then considered.
	Skipped string `json:"skipped,omitempty"`
	// SearchedAt is when the release lookup behind this decision ran; a
	// cached decision keeps the original time.
	SearchedAt time.Time      `json:"searchedAt,omitempty"`
	Savings    int64          `json:"projectedSavings"`
	Rejected   map[string]int `json:"rejected,omitempty"`
	// Mode is 1080p or 2160p; Score is the winner's breakdown and RunnerUp
	// the next best release (or, with no winner, the best one below the
	// minimum score). Note says when the runtime was a fallback guess.
	Mode     string     `json:"mode,omitempty"`
	Score    *Score     `json:"score,omitempty"`
	RunnerUp *Candidate `json:"runnerUp,omitempty"`
	Note     string     `json:"note,omitempty"`
}

// NoGoodRelease is the reason when releases pass the filters but none
// reaches the minimum score.
const NoGoodRelease = "no release good enough"

// Select filters, scores and picks the best release for the target.
func Select(t Target, rs []Release, c Config) Decision {
	return selectReleases(t, rs, c, nil)
}

func selectReleases(t Target, rs []Release, c Config, bad map[string]string) Decision {
	d := Decision{Target: t, Rejected: map[string]int{}, Mode: t.Mode()}
	if _, guessed := t.Hours(); guessed {
		m := fallbackMovieMinutes
		if t.Kind == "sonarr" {
			m = fallbackEpisodeMinutes
		}
		d.Note = fmt.Sprintf("runtime unknown; assumed %d min", m)
	}
	var candidates []Release
	scores := map[string]Score{}
	for _, r := range rs {
		why := Reject(r, t, c)
		reason := bad[releaseID(r)]
		if reason == "" {
			reason = bad[releaseNameID(r)]
		}
		if reason != "" {
			why = "rejected after download: " + reason
		}
		if why != "" {
			d.Rejected[why]++
			continue
		}
		candidates = append(candidates, r)
		scores[r.Guid] = score(r, t, c)
	}
	rank(candidates, scores)
	d.Candidates = options(t, rs, candidates, scores, c)
	for i := range d.Candidates {
		o := d.Candidates[i]
		why := bad[o.ID]
		if why == "" {
			for _, rel := range rs {
				if releaseID(rel) == o.ID {
					why = bad[releaseNameID(rel)]
					break
				}
			}
		}
		if why != "" {
			o.Rejection = "rejected after download: " + why
		}
	}
	if len(candidates) == 0 {
		d.Reason = "no safe replacement"
		return d
	}
	best := candidates[0]
	if s := scores[best.Guid]; s.Total < c.Selection.MinScore {
		d.Reason = NoGoodRelease
		d.RunnerUp = candidate(best, s)
		d.Rejected[fmt.Sprintf("score below %.0f", c.Selection.MinScore)] += len(candidates)
		return d
	}
	s := scores[best.Guid]
	d.Release, d.Score = &best, &s
	d.AudioUnverified = ParseTraits(best.Title).Channels == 0
	d.Savings = t.Bytes() - best.Size
	if len(candidates) > 1 {
		d.RunnerUp = candidate(candidates[1], scores[candidates[1].Guid])
	}
	return d
}

// Edition separators are normalized consistently for search metadata and import paths.
func unsafeEdition(s string) bool {
	s = strings.ToLower(s)
	s = strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return unwanted.MatchString(s) || strings.Contains(s, "black and chrome") || strings.Contains(s, "director")
}

// fileCap is one replacement file's cap: the mode's GB/h cap, scaled by the
// file's probed codec, over the playing time of the episodes it covers.
func fileCap(t Target, eps []Episode, f File, c Config, mode string) int64 {
	return int64(c.Selection.Sizes[mode].Cap * fileFactor(c, f) * t.fileHours(eps, f.ID) * float64(GB))
}

// DownloadFree reports free bytes on the first download root, read through
// its local inspection mount.
func (c Config) DownloadFree() (int64, error) {
	if len(c.DownloadRoots) == 0 {
		return 0, fmt.Errorf("no download root configured")
	}
	root := c.DownloadRoots[0]
	for _, m := range c.Mounts {
		if under(root, m.AppRoot) {
			v, e := (LocalFilesystem{}).Volume(filepath.Join(m.LocalRoot, strings.TrimPrefix(root, m.AppRoot)))
			return v.Free, e
		}
	}
	return 0, fmt.Errorf("download root has no inspection mount")
}

// UnmarshalJSON ignores retired nightly caps while retaining strict configuration parsing.
func (c *Config) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for key := range fields {
		if strings.EqualFold(key, "maxTitles") || strings.EqualFold(key, "maxGB") {
			delete(fields, key)
		}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	type plain Config
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode((*plain)(c))
}
