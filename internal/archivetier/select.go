package archivetier

import (
	"crypto/sha256"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// PolicyVersion names the selection rules a job was chosen under. A job
// persisted under another version is never imported automatically.
const PolicyVersion = "2"

// Modes: every archive title is replaced by a 1080p release, unless it also
// carries the 4k tag, in which case only 2160p HDR releases qualify.
const (
	Mode1080p = "1080p"
	Mode2160p = "2160p"
	FourKTag  = "4k"
)

// Runtime fallbacks when the application has no runtime for a title.
const (
	fallbackEpisodeMinutes = 45
	fallbackMovieMinutes   = 120
)

// SizeBand is in GB per hour of HEVC video. Floor, target and cap all scale
// with the codec factor, so an x264 encode is judged against x264 sizes.
type SizeBand struct {
	Floor  float64 `json:"floor"`
	Target float64 `json:"target"`
	Cap    float64 `json:"cap"`
}

// Selection is the release scoring table. Codec factors are relative to
// HEVC (1.0); a codec missing from the table is not accepted.
type Selection struct {
	Sizes         map[string]SizeBand `json:"sizes"`
	CodecFactors  map[string]float64  `json:"codecFactors"`
	TrustedGroups []string            `json:"trustedGroups"`
	DeniedGroups  []string            `json:"deniedGroups"`
	MinScore      float64             `json:"minScore"`
}

func DefaultSelection() Selection {
	return Selection{
		Sizes:         map[string]SizeBand{Mode1080p: {Floor: 1, Target: 3, Cap: 5}, Mode2160p: {Floor: 4, Target: 8, Cap: 14}},
		CodecFactors:  map[string]float64{"hevc": 1, "avc": 2, "av1": 0.85},
		TrustedGroups: []string{},
		DeniedGroups:  []string{},
		MinScore:      60,
	}
}

func (s Selection) Validate() error {
	for _, m := range []string{Mode1080p, Mode2160p} {
		b, ok := s.Sizes[m]
		if !ok || b.Floor < 0.1 || b.Target <= b.Floor || b.Cap <= b.Target || b.Cap > 100 || math.IsInf(b.Cap, 0) || math.IsNaN(b.Floor+b.Target+b.Cap) {
			return fmt.Errorf("selection sizes need 0.1 <= floor < target < cap <= 100 GB/h for %s", m)
		}
	}
	for k, f := range s.CodecFactors {
		if (k != "hevc" && k != "avc" && k != "av1") || f < 0.1 || f > 10 || math.IsInf(f, 0) || math.IsNaN(f) {
			return fmt.Errorf("invalid codec factor %q", k)
		}
		for _, band := range s.Sizes {
			if math.IsInf(band.Cap*f, 0) || band.Floor*f <= 0 {
				return fmt.Errorf("invalid scaled codec factor %q", k)
			}
		}
	}
	if s.CodecFactors["hevc"] <= 0 {
		return fmt.Errorf("hevc factor is required")
	}
	if s.MinScore < 0 || s.MinScore > 100 || math.IsNaN(s.MinScore) {
		return fmt.Errorf("selection minScore must be 0..100")
	}
	_, _, err := normalizeGroups(s.TrustedGroups, s.DeniedGroups)
	return err
}

// Mode is the replacement resolution the target was opted into.
func (t Target) Mode() string {
	if t.FourK {
		return Mode2160p
	}
	return Mode1080p
}

// Hours is the target's playing time and whether it is a fallback guess.
func (t Target) Hours() (float64, bool) {
	minutes, guessed := t.Runtime, t.Runtime <= 0
	if t.Kind == "sonarr" {
		if guessed {
			minutes = fallbackEpisodeMinutes
		}
		return float64(minutes) * float64(len(episodeIDs(t.Episodes, 0))) / 60, guessed
	}
	if guessed {
		minutes = fallbackMovieMinutes
	}
	return float64(minutes) / 60, guessed
}

// fileHours is the playing time of one library file: its episodes, or the movie.
func (t Target) fileHours(eps []Episode, fileID int) float64 {
	if t.Kind != "sonarr" {
		h, _ := t.Hours()
		return h
	}
	minutes := t.Runtime
	if minutes <= 0 {
		minutes = fallbackEpisodeMinutes
	}
	return float64(minutes) * float64(len(episodeIDs(eps, fileID))) / 60
}

// episodeIDs is the distinct positive episode IDs, of one file when fileID > 0.
func episodeIDs(eps []Episode, fileID int) map[int]bool {
	ids := map[int]bool{}
	for _, ep := range eps {
		if ep.ID > 0 && (fileID == 0 || ep.FileID == fileID) {
			ids[ep.ID] = true
		}
	}
	return ids
}

// capBytes is the largest acceptable replacement: the mode's GB/h cap times
// the codec factor over the target's runtime. An unknown codec has no room.
func capBytes(t Target, c Config, mode string, factor float64) int64 {
	h, _ := t.Hours()
	bytes := c.Selection.Sizes[mode].Cap * factor * h * float64(GB)
	// float64(MaxInt64) rounds up to 2^63. Refuse that boundary as well as
	// non-finite/negative inputs before converting; zero admits no files.
	if bytes <= 0 || math.IsNaN(bytes) || bytes >= float64(math.MaxInt64) {
		return 0
	}
	return int64(bytes)
}

// codecFactor is the configured factor for a name or probe codec string, 0
// when the codec is unknown or not allowed.
func codecFactor(c Config, s string) float64 {
	return c.Selection.CodecFactors[ParseTraits(s).Codec]
}

// fileFactor is the codec factor of an imported file's media probe.
func fileFactor(c Config, f File) float64 {
	if f.MediaInfo == nil {
		return 0
	}
	return codecFactor(c, f.MediaInfo.VideoCodec)
}

// releaseMode is the profile a release is judged by. A 1080p target also
// takes 2160p releases, under the stricter 4K rules (HDR10, 4K size band).
func releaseMode(r Release, t Target) string {
	if t.Mode() == Mode1080p && resolution(r.Quality) == 2160 {
		return Mode2160p
	}
	return t.Mode()
}

func modeResolution(mode string) int {
	if mode == Mode2160p {
		return 2160
	}
	return 1080
}

// mediaReject applies the audio and HDR rules to an imported file's probe.
// Without a probe nothing is proven, so the file is refused.
func mediaReject(f File, mode string) string {
	m := f.MediaInfo
	if m == nil {
		return "media info missing"
	}
	if m.AudioChannels <= 2 && m.AudioChannels > 0 {
		return "stereo audio"
	}
	if m.AudioChannels < 5.05 {
		return "audio below 5.1"
	}
	if ParseTraits(m.VideoCodec).Codec == "" {
		return "video codec unknown"
	}
	audio := ParseTraits(m.AudioCodec)
	if audio.Lossless && audio.Lossy == "" {
		return "lossless audio"
	}
	if audio.Lossy == "" {
		return "audio codec unknown"
	}
	if mode == Mode2160p && !strings.Contains(strings.ToUpper(m.VideoDynamicRangeType), "HDR10") {
		return "not HDR10"
	}
	return ""
}

// Traits is what a release name says about the video and audio. Anything
// the name does not state stays unknown.
type Traits struct {
	Codec    string  `json:"codec,omitempty"` // hevc, avc, av1
	TenBit   bool    `json:"tenBit,omitempty"`
	HDR10    bool    `json:"hdr10,omitempty"` // HDR, HDR10 or HDR10+
	DV       bool    `json:"dv,omitempty"`
	SDR      bool    `json:"sdr,omitempty"`
	Channels float64 `json:"channels,omitempty"` // 5.1, 7.1; 0 unknown
	Stereo   bool    `json:"stereo,omitempty"`
	Lossless bool    `json:"lossless,omitempty"` // TrueHD, FLAC, DTS-HD/X, PCM
	Lossy    string  `json:"lossy,omitempty"`    // DDP, DD, AAC, Opus, DTS
	Atmos    bool    `json:"atmos,omitempty"`
	BadSrc   string  `json:"badSource,omitempty"`
}

var (
	nameSeparators = strings.NewReplacer(".", " ", "_", " ", "-", " ", "(", " ", ")", " ", "[", " ", "]", " ", ",", " ", ":", " ")
	reHEVC         = regexp.MustCompile(`(^| )([xh] ?265|hevc)( |$)`)
	reAVC          = regexp.MustCompile(`(^| )([xh] ?264|avc)( |$)`)
	reAV1          = regexp.MustCompile(`(^| )av1( |$)`)
	reTenBit       = regexp.MustCompile(`(^| )10 ?bit( |$)`)
	reHDR10        = regexp.MustCompile(`(^| )(hdr|hdr10|hdr10\+|hdr10p|hdr10plus)( |$)`)
	reDV           = regexp.MustCompile(`(^| )(dv|dovi|dolby vision)( |$)`)
	reSDR          = regexp.MustCompile(`(^| )sdr( |$)`)
	// "5 1" after dots became spaces; a digit before or after means it is
	// part of another number (x265 10bit, 2025 1080p). Only 5.x-7.x are
	// channel layouts: "DV P8.1" is a Dolby Vision profile, not audio.
	reChannels = regexp.MustCompile(`(^|[^0-9])([5-7]) ([01])($|[^0-9])`)
	reChCount  = regexp.MustCompile(`(^| )([2-8])ch( |$)`)
	reStereo   = regexp.MustCompile(`(^|[^0-9])2 0($|[^0-9])|(^| )stereo( |$)`)
	reLossless = regexp.MustCompile(`(^| )(truehd|flac|l?pcm|dts ?hd|dts ?x|dts ma)`)
	reLossy    = regexp.MustCompile(`(^| )(ddpa?|dd\+|dd|e ?ac ?3|ac ?3|aac|opus|dts)($| |\+|[0-9]|atmos)`)
	reAtmos    = regexp.MustCompile(`atmos`)
	reBadSrc   = regexp.MustCompile(`(^| )(remux|iso|bdmv|brdisk|br ?disk|br ?25|br ?50|bd ?25|bd ?50|bd ?66|bd ?100|complete (uhd )?blu ?ray|hdtv|pdtv|cam|camrip|hdcam|ts|hdts|telesync|tc|telecine|scr|screener|dvdscr|workprint)( |$)`)
)

// ParseTraits reads a release name. Matching is deliberately literal.
func ParseTraits(name string) Traits {
	s := " " + strings.Join(strings.Fields(nameSeparators.Replace(strings.ToLower(name))), " ") + " "
	var t Traits
	hevc, avc, av1 := reHEVC.MatchString(s), reAVC.MatchString(s), reAV1.MatchString(s)
	switch {
	case hevc && !avc && !av1:
		t.Codec = "hevc"
	case avc && !hevc && !av1:
		t.Codec = "avc"
	case av1 && !hevc && !avc:
		t.Codec = "av1"
	}
	t.TenBit = reTenBit.MatchString(s)
	t.HDR10 = reHDR10.MatchString(s)
	t.DV = reDV.MatchString(s)
	t.SDR = reSDR.MatchString(s)
	for _, m := range reChannels.FindAllStringSubmatch(s, -1) {
		v := float64(m[2][0]-'0') + float64(m[3][0]-'0')/10
		t.Channels = math.Max(t.Channels, v)
	}
	for _, m := range reChCount.FindAllStringSubmatch(s, -1) {
		switch m[2] {
		case "6":
			t.Channels = math.Max(t.Channels, 5.1)
		case "8":
			t.Channels = math.Max(t.Channels, 7.1)
		case "2":
			t.Stereo = true
		}
	}
	t.Stereo = t.Stereo || reStereo.MatchString(s)
	t.Lossless = reLossless.MatchString(s)
	for _, m := range reLossy.FindAllStringSubmatch(s, -1) {
		if m[2] == "dts" && t.Lossless {
			continue // the core of a DTS-HD track
		}
		t.Lossy = lossyName(m[2])
		break
	}
	t.Atmos = reAtmos.MatchString(s)
	if m := reBadSrc.FindStringSubmatch(s); m != nil {
		t.BadSrc = m[2]
	}
	return t
}

func lossyName(s string) string {
	switch strings.ReplaceAll(s, " ", "") {
	case "ddp", "ddpa", "dd+", "eac3":
		return "DDP"
	case "dd", "ac3":
		return "DD"
	case "aac":
		return "AAC"
	case "opus":
		return "Opus"
	}
	return "DTS"
}

// Audio is the short label used in summaries, e.g. "DDP5.1 Atmos".
func (t Traits) Audio() string {
	s := t.Lossy
	if t.Channels > 0 {
		s += fmt.Sprintf("%.1f", t.Channels)
	}
	if t.Atmos {
		s += " Atmos"
	}
	return strings.TrimSpace(s)
}

// sourceOK accepts WEB-DL, WEBRip and BluRay encodes, by quality and name.
// Sonarr has no modifier; its remux qualities are named "Bluray-1080p Remux".
func sourceOK(q Quality, tr Traits) bool {
	n := strings.ToLower(q.Quality.Name)
	if strings.Contains(n, "remux") {
		return false
	}
	if !strings.HasPrefix(n, "webdl-") && !strings.HasPrefix(n, "webrip-") && !strings.HasPrefix(n, "bluray-") {
		return false
	}
	m := strings.ToLower(q.Quality.Modifier)
	return tr.BadSrc == "" && m != "remux" && m != "brdisk" && m != "rawhd"
}

// profileReject applies the mode's resolution, HDR, source, codec and audio
// rules to one release name and quality.
func profileReject(mode string, q Quality, name string, c Config) string {
	tr := ParseTraits(name)
	if resolution(q) != modeResolution(mode) {
		return "not " + mode
	}
	if mode == Mode2160p && (!tr.HDR10 || tr.SDR) {
		return "4k needs HDR10 (Dolby Vision only with HDR10 fallback)"
	}
	if !sourceOK(q, tr) {
		return "not a WEB-DL, WEBRip or BluRay encode"
	}
	if _, ok := c.Selection.CodecFactors[tr.Codec]; !ok {
		return "video codec unknown or not allowed"
	}
	if tr.Lossless && tr.Lossy == "" {
		return "lossless-only audio (Plex transcodes it)"
	}
	if tr.Channels < 5.1 {
		if tr.Stereo {
			return "stereo audio"
		}
		if tr.Channels > 0 {
			return "audio below 5.1"
		}
	}
	return ""
}

func groupIn(g string, list []string) bool {
	for _, v := range list {
		if g != "" && strings.EqualFold(strings.TrimSpace(g), strings.TrimSpace(v)) {
			return true
		}
	}
	return false
}

// releaseGroup prefers the application's parsed group, then the name's suffix.
func releaseGroup(r Release) string {
	if r.Group != "" {
		return r.Group
	}
	if i := strings.LastIndex(r.Title, "-"); i >= 0 && i < len(r.Title)-1 && !strings.ContainsAny(r.Title[i+1:], " .") {
		return r.Title[i+1:]
	}
	return ""
}

// Score is one release's explainable score; every part is stored.
type Score struct {
	Total     float64 `json:"total"`
	SizeFit   float64 `json:"sizeFit"`
	Savings   float64 `json:"savings"`
	Seeders   float64 `json:"seeders"`
	Group     float64 `json:"group"`
	Features  float64 `json:"features"`
	GBPerHour float64 `json:"gbPerHour"`
	Codec     string  `json:"codec"`

	AudioPenalty float64  `json:"audioPenalty,omitempty"`
	Mode         string   `json:"mode"`
	Floor        float64  `json:"floor"`
	Target       float64  `json:"target"`
	Cap          float64  `json:"cap"`
	MinScore     float64  `json:"minScore"`
	CodecFactor  float64  `json:"codecFactor"`
	Trusted      bool     `json:"trusted"`
	FeatureNames []string `json:"featureNames"`
}

// Candidate exposes an opaque release identity; tracker URLs stay in worker state.
type Candidate struct {
	AudioUnverified bool `json:"audioUnverified,omitempty"`

	ID        string `json:"id,omitempty"`
	IndexerID int    `json:"indexerId,omitempty"`
	Title     string `json:"title"`
	Group     string `json:"group,omitempty"`
	Size      int64  `json:"size"`
	Seeders   int    `json:"seeders"`
	Score     Score  `json:"score"`
	// Rejection is why automatic selection would not grab it; an operator
	// may still approve it. Empty for the releases selection ranks.
	Rejection string `json:"rejection,omitempty"`
}

// score rates a release that passed every hard filter.
func score(r Release, t Target, c Config) Score {
	tr := ParseTraits(r.Title)
	mode := releaseMode(r, t)
	band := c.Selection.Sizes[mode]
	h, _ := t.Hours()
	factor := c.Selection.CodecFactors[tr.Codec]
	s := Score{Codec: tr.Codec, Mode: mode, Floor: band.Floor * factor, Target: band.Target * factor, Cap: band.Cap * factor, CodecFactor: factor, MinScore: c.Selection.MinScore, FeatureNames: []string{}}
	if h > 0 {
		s.GBPerHour = float64(r.Size) / float64(GB) / h
	}
	if s.GBPerHour > 0 && s.Target > 0 {
		s.SizeFit = 50 * math.Max(0, 1-math.Abs(math.Log2(s.GBPerHour/s.Target)))
	}
	s.Savings = 20 * (1 - float64(r.Size)/float64(t.Bytes()))
	s.Seeders = 15 * math.Min(1, math.Max(0, math.Log10(float64(r.Seeders)/10)))
	s.Group = 5
	if groupIn(releaseGroup(r), c.Selection.TrustedGroups) {
		s.Group = 10
		s.Trusted = true
	}
	if tr.TenBit {
		s.Features += 3
		s.FeatureNames = append(s.FeatureNames, "10-bit")
	}
	if tr.Lossy == "DDP" && tr.Channels >= 5.1 {
		s.Features += 2
		s.FeatureNames = append(s.FeatureNames, "DD+ 5.1")
	}
	if tr.Atmos || tr.Channels >= 7.1 {
		s.Features++
		s.FeatureNames = append(s.FeatureNames, "Atmos/7.1")
	}
	// A 4K HDR release beats an equally good 1080p one for a 1080p target.
	if mode != t.Mode() {
		s.Features += 5
		s.FeatureNames = append(s.FeatureNames, "4K bonus")
	}
	if tr.Channels == 0 {
		s.AudioPenalty = -5
	}
	s.Total = s.SizeFit + s.Savings + s.Seeders + s.Group + s.Features + s.AudioPenalty
	return s
}

// sizeReject applies the runtime-normalized, codec-scaled floor and cap.
func sizeReject(r Release, t Target, c Config) string {
	mode := releaseMode(r, t)
	band := c.Selection.Sizes[mode]
	h, _ := t.Hours()
	factor := codecFactor(c, r.Title)
	if r.Size <= 0 || r.Size > capBytes(t, c, mode, factor) {
		return "size cap"
	}
	if float64(r.Size) < band.Floor*factor*h*float64(GB) {
		return "below quality floor"
	}
	return ""
}

// rank orders scored candidates: total, then size fit, then smaller, then
// more seeders, then stable identity.
func rank(cs []Release, ss map[string]Score) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		x, y := ss[a.Guid], ss[b.Guid]
		if x.Total != y.Total {
			return x.Total > y.Total
		}
		if x.SizeFit != y.SizeFit {
			return x.SizeFit > y.SizeFit
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		if a.Seeders != b.Seeders {
			return a.Seeders > b.Seeders
		}
		return a.Guid < b.Guid
	})
}

func candidate(r Release, s Score) *Candidate {
	return &Candidate{AudioUnverified: ParseTraits(r.Title).Channels == 0, ID: releaseID(r), IndexerID: r.IndexerID, Title: r.Title, Group: releaseGroup(r), Size: r.Size, Seeders: r.Seeders, Score: s}
}

// options lists every release an operator may approve: ranked passing
// releases first, then the rest by score with the rule they break.
func options(t Target, rs, ranked []Release, scores map[string]Score, c Config) []*Candidate {
	out := []*Candidate{}
	for _, r := range ranked {
		o := candidate(r, scores[r.Guid])
		if o.Score.Total < c.Selection.MinScore {
			o.Rejection = fmt.Sprintf("score below %.0f", c.Selection.MinScore)
		}
		out = append(out, o)
	}
	var rest []*Candidate
	for _, r := range rs {
		if _, ok := scores[r.Guid]; ok || manualReject(r, t, c) != "" {
			continue
		}
		o := candidate(r, score(r, t, c))
		o.Rejection = Reject(r, t, c)
		rest = append(rest, o)
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Score.Total > rest[j].Score.Total })
	out = append(out, rest...)
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func releaseID(r Release) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%s", r.IndexerID, r.Guid))))
}

// releaseNameID identifies a legacy release whose private GUID was not retained.
func releaseNameID(r Release) string {
	return fmt.Sprintf("name:%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%d", r.IndexerID, r.Title, r.Size))))
}
