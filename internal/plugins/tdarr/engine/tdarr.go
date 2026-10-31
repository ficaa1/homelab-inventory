package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tdarr is a read-only client for Tdarr's local REST API (the haveagitgat
// image serves it unauthenticated on port 8266, reachable only on the LAN).
// It reads configured libraries and estimates which files in the media
// libraries are worth compressing, and roughly how much space each would
// give back.
//
// The underlying call is technically a POST (/api/v2/cruddb with mode
// "getAll") — that is how Tdarr's API spells a database read — but nothing
// it does mutates Tdarr state.
type Tdarr struct {
	base        string
	prefixes    []string
	http        *http.Client
	statusCache struct {
		mu      sync.Mutex
		expires time.Time
		result  []byte
		refresh chan struct{}
	}
}

// NewTdarr builds a client for the Tdarr server at baseURL (for example
// a user-configured service URL).
func NewTdarr(baseURL string, client *http.Client, prefixes []string) *Tdarr {
	return &Tdarr{base: strings.TrimRight(baseURL, "/"), http: client, prefixes: prefixes}
}

// Configured reports whether the client has an endpoint to talk to.
func (t *Tdarr) Configured() bool { return t != nil && t.base != "" }

// ShrinkCandidate is one media file worth compressing, with the numbers the
// operator needs to decide.
type ShrinkCandidate struct {
	Path       string  `json:"path"`
	SizeGB     float64 `json:"size_gb"`
	Codec      string  `json:"codec"`
	Resolution string  `json:"resolution,omitempty"`
	// Kind is what the shrink would do to this file: "video" (full
	// HEVC re-encode, the common case), "bloat" (already HEVC but
	// pathologically oversized), or "audio-only" (the flow refuses the
	// video — HDR/DV or already-fine HEVC — but drops/transcodes the
	// lossless audio track).
	Kind string `json:"kind"`
	// EstSaveGB is the estimated space recovered. Video estimates are
	// estimated at 62%; bloat and audio-only estimates are rougher and
	// carry lower confidence.
	EstSaveGB float64 `json:"est_save_gb"`
	Reason    string  `json:"reason"`
}

// The candidate rules are the ones charted for the weekly shrink (map #16):
// a file qualifies when the flow would actually change it meaningfully.
const (
	// minFatBitrate is the "fat enough to bother" floor for a non-HEVC file,
	// in bits per second.
	minFatBitrate = 8_000_000
	// bloatBitrate is the already-HEVC bloat class threshold (>30 Mbps).
	bloatBitrate = 30_000_000
	// minSizeMiB keeps tiny files out of the list entirely.
	minSizeMiB = 1024 // 1 GiB
	// pilotSaving is a conservative estimated reduction; actual savings vary.
	pilotSaving = 0.62
	// bloatSaving is a conservative guess for re-encoding an over-bitrate
	// HEVC file at CQ23; unverified against this library.
	bloatSaving = 0.30
)

var (
	// hdrRe flags files whose video the flow will refuse (tonemapping was
	// ruled out). Tdarr's stored ffProbeData carries no colour-transfer
	// fields, so this is a filename heuristic — release naming conventions
	// mark HDR/DV reliably enough for a candidates list, and the flow's own
	// checkHdr node remains the authority.
	hdrRe = regexp.MustCompile(`(?i)\b(hdr|hdr10\+?|dolby[ .]?vision|dovi|dv\b)`)
	// losslessAudio matches the codecs hlb1 drops or transcodes;
	// codec_name "dts" covers DTS-HD MA too, same as ffprobe reports it.
	losslessAudio = map[string]bool{"truehd": true, "dts": true}
)

// shrinkKind classifies one file against the flow's actual behaviour, in
// the flow's order: the audio step runs on everything, then checkHdr
// refuses HDR/DV video unconditionally (a recorded decision, issue #36),
// then the codec/bitrate rules decide the video path.
func shrinkKind(f tdarrFile) (kind, reason string, ok bool) {
	codec := strings.ToLower(f.VideoCodecName)
	isHEVC := codec == "hevc" || codec == "h265"

	// The flow never touches HDR/DV video, whatever its codec — so such a
	// file can only ever be an audio-only win.
	if hdrRe.MatchString(f.File) {
		if hasLosslessAudio(f) {
			return "audio-only",
				"HDR/DV video stays pristine (recorded decision); TrueHD/DTS track dropped or transcoded",
				true
		}
		return "", "", false
	}
	if !isHEVC && f.BitRate >= minFatBitrate {
		return "video",
			fmt.Sprintf("%s at %.0f Mbps — NVENC CQ21 re-encode saves ~%d%%",
				strings.ToUpper(codec), float64(f.BitRate)/1e6, int(pilotSaving*100)),
			true
	}
	if isHEVC && f.BitRate > bloatBitrate {
		return "bloat",
			fmt.Sprintf("already HEVC but %.0f Mbps (>30 bloat class) — CQ23 re-encode",
				float64(f.BitRate)/1e6),
			true
	}
	if isHEVC && hasLosslessAudio(f) {
		return "audio-only",
			"already HEVC at a sane bitrate; TrueHD/DTS track dropped or transcoded",
			true
	}
	return "", "", false
}

// hasLosslessAudio reports whether any audio stream is TrueHD or DTS.
func hasLosslessAudio(f tdarrFile) bool {
	for _, s := range f.FFProbeData.Streams {
		if s.CodecType == "audio" && losslessAudio[strings.ToLower(s.CodecName)] {
			return true
		}
	}
	return false
}

// audioOnlySaveGB estimates the space recovered from dropping or
// transcoding the lossless audio tracks of one file. It subtracts 640 kbps
// where the transcode branch would keep an EAC3, so the number is
// conservative for remuxes that get the full drop instead.
func audioOnlySaveGB(f tdarrFile) float64 {
	dur := f.Duration
	if dur <= 0 {
		return 0
	}
	var bits float64
	for _, s := range f.FFProbeData.Streams {
		if s.CodecType != "audio" || !losslessAudio[strings.ToLower(s.CodecName)] {
			continue
		}
		br := 0.0
		// Tdarr stores stream bit_rate as a JSON string or number depending
		// on the ffprobe variant.
		if err := json.Unmarshal(s.BitRate, &br); err != nil || br < 0 {
			br = 0
		}
		if br == 0 {
			br = 3_000_000 // typical TrueHD/DTS-HD MA 5.1 when unstated
		}
		bits += (br - 640_000) * dur
	}
	if bits < 0 {
		return 0
	}
	return round1(bits / 8 / (1 << 30))
}

// ShrinkCandidates returns the files in the two shrink libraries that the
// "Weekly shrink" flow would meaningfully change, largest estimated saving
// first. limit caps the list (0 means no cap).
func (t *Tdarr) ShrinkCandidates(ctx context.Context, limit int) ([]ShrinkCandidate, error) {
	if !t.Configured() {
		return nil, ErrNotConfigured
	}
	var files []tdarrFile
	if err := t.statusRead(ctx, "/api/v2/cruddb", `{"data":{"collection":"FileJSONDB","mode":"getAll"}}`, &files); err != nil {
		return nil, err
	}
	if files == nil {
		return nil, fmt.Errorf("tdarr: missing file inventory")
	}

	out := []ShrinkCandidate{}
	for _, f := range files {
		inLibrary := false
		for _, p := range t.prefixes {
			if strings.HasPrefix(f.File, p) {
				inLibrary = true
				break
			}
		}
		if !inLibrary || f.FileSize < minSizeMiB {
			continue
		}
		kind, reason, ok := shrinkKind(f)
		if !ok {
			continue
		}
		sizeGB := f.FileSize / 1024
		c := ShrinkCandidate{
			Path:       f.File,
			SizeGB:     round1(sizeGB),
			Codec:      strings.ToUpper(f.VideoCodecName),
			Resolution: f.VideoResolution,
			Kind:       kind,
			Reason:     reason,
		}
		switch kind {
		case "video":
			c.EstSaveGB = round1(sizeGB * pilotSaving)
		case "bloat":
			c.EstSaveGB = round1(sizeGB * bloatSaving)
		case "audio-only":
			c.EstSaveGB = audioOnlySaveGB(f)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EstSaveGB > out[j].EstSaveGB })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// tdarrFile mirrors just the fields FileJSONDB carries that the candidate
// rules need.
type tdarrFile struct {
	File            string     `json:"file"`
	FileSize        float64    `json:"file_size"` // MiB
	VideoCodecName  string     `json:"video_codec_name"`
	VideoResolution string     `json:"video_resolution"`
	BitRate         int64      `json:"bit_rate"` // total, bits per second
	Duration        float64    `json:"duration"` // seconds
	FFProbeData     tdarrProbe `json:"ffProbeData"`
}

type tdarrProbe struct {
	Streams []tdarrStream `json:"streams"`
}

type tdarrStream struct {
	CodecType string          `json:"codec_type"`
	CodecName string          `json:"codec_name"`
	BitRate   json.RawMessage `json:"bit_rate"`
}
