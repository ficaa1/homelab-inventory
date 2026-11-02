package archivetier

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// probe reads the real file through the same read-only mounts as preflight.
func (r *Runner) probe(ctx context.Context, f File) (*MediaInfo, error) {
	if r.Probe != nil {
		return r.Probe(ctx, f)
	}
	if !safePath(f.Path, r.Config.DownloadRoots) && !safePath(f.Path, r.Config.LibraryRoots) {
		return nil, fmt.Errorf("file outside inspection roots")
	}
	c := &Client{Safety: r.Config}
	st, _, err := c.inspect(f.Path)
	if err != nil || st.Directory || st.Size != f.Size || f.Size <= 0 {
		return nil, fmt.Errorf("file inspection failed")
	}
	local, _, err := c.mapped(f.Path)
	if err != nil {
		return nil, err
	}
	command := r.Config.ProbeCommand
	if command == "" {
		command = "ffprobe"
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, command, "-v", "error", "-show_streams", "-of", "json", local).Output()
	if err != nil {
		// Do not expose stderr, which may contain private paths or metadata.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("ffprobe timed out or cancelled")
		}
		return nil, fmt.Errorf("ffprobe missing or failed; check archive.probeCommand and file access")
	}
	return parseProbe(b)
}

// parseProbe selects a lossy surround track, even when the primary track is
// lossless. DTS-HD/X is not DTS core; a separate core track is required.
func parseProbe(b []byte) (*MediaInfo, error) {
	var v struct {
		Streams *[]struct {
			Type        string `json:"codec_type"`
			Codec       string `json:"codec_name"`
			Profile     string `json:"profile"`
			Channels    int    `json:"channels"`
			Transfer    string `json:"color_transfer"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(b, &v); err != nil || v.Streams == nil || len(*v.Streams) == 0 {
		return nil, fmt.Errorf("ffprobe returned no valid streams")
	}
	m := &MediaInfo{}
	var fallback *MediaInfo
	video, badVideo := false, false
	for _, s := range *v.Streams {
		if s.Type == "" || (s.Type == "video" || s.Type == "audio") && s.Codec == "" {
			return nil, fmt.Errorf("ffprobe stream metadata missing")
		}
		if s.Type == "audio" && s.Channels <= 0 {
			return nil, fmt.Errorf("ffprobe audio channels missing")
		}
		switch s.Type {
		case "video":
			if s.Disposition.AttachedPic != 0 {
				continue
			}
			// Require each actual video stream to have a supported codec;
			// every stream must have PQ before calling the file HDR10.
			codec := map[string]string{"hevc": "HEVC", "h264": "AVC", "av1": "AV1"}[s.Codec]
			if codec == "" {
				badVideo = true
			}
			if !video {
				m.VideoCodec = codec
				m.VideoDynamicRangeType = "HDR10"
			}
			video = true
			if s.Transfer != "smpte2084" {
				m.VideoDynamicRangeType = "SDR"
			}
		case "audio":
			codec := map[string]string{"eac3": "EAC3", "ac3": "AC3", "aac": "AAC", "opus": "Opus", "dts": "DTS"}[s.Codec]
			profile := strings.ToUpper(s.Profile)
			if s.Codec == "dts" && (strings.Contains(profile, "HD") || strings.Contains(profile, "MA") || strings.Contains(profile, "X")) {
				codec = ""
			}
			if fallback == nil || codec != "" && fallback.AudioCodec == "" {
				fallback = &MediaInfo{AudioCodec: codec, AudioChannels: float64(s.Channels)}
				if codec == "" {
					switch {
					case s.Codec == "dts":
						fallback.AudioCodec = "DTS-HD"
					case strings.HasPrefix(s.Codec, "pcm_"):
						fallback.AudioCodec = "PCM"
					default:
						fallback.AudioCodec = s.Codec
					}
				}
			}
			if codec != "" && s.Channels >= 6 && float64(s.Channels) > m.AudioChannels {
				m.AudioCodec, m.AudioChannels = codec, float64(s.Channels)
			}
		}
	}
	if m.AudioCodec == "" && fallback != nil {
		m.AudioCodec, m.AudioChannels = fallback.AudioCodec, fallback.AudioChannels
	}
	if badVideo {
		m.VideoCodec = ""
	}
	return m, nil
}

// downloadedVideos includes video files Arr did not offer for import (e.g.
// an episode it could not identify), so an unprobed episode cannot slip through.
// Explicitly unrelated Sonarr manual items are excluded from the probe list.
func (r *Runner) downloadedVideos(q QueueItem, items []ManualItem, t Target) ([]File, error) {
	ignored := map[string]bool{}
	for _, it := range items {
		ignored[it.Path] = true
	}
	items = targetImportItems(items, t)
	for _, it := range items {
		delete(ignored, it.Path)
	}
	files := []File{}
	if r.Probe != nil {
		for _, it := range items {
			files = append(files, File{Path: it.Path, Size: it.Size, MediaInfo: it.MediaInfo})
		}
		return files, nil
	}
	if !safePath(q.OutputPath, r.Config.DownloadRoots) {
		return nil, fmt.Errorf("download output path outside inspection roots")
	}
	c := &Client{Safety: r.Config}
	root, _, err := c.mapped(q.OutputPath)
	if err != nil {
		return nil, err
	}
	st, _, err := c.inspect(q.OutputPath)
	if err != nil {
		return nil, err
	}
	entries := []Evidence{st}
	if st.Directory {
		entries, err = c.filesystem().Walk(root)
		if err != nil {
			return nil, err
		}
	}
	found := map[string]bool{}
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Path)) {
		case ".mkv", ".mp4", ".m4v", ".avi", ".mov", ".webm", ".ts", ".m2ts", ".mts", ".m2t", ".mpg", ".mpeg", ".m2v", ".vob", ".wmv", ".ogm", ".ogv", ".divx", ".xvid":
			app := q.OutputPath
			if st.Directory {
				rel, err := filepath.Rel(root, e.Path)
				if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
					return nil, fmt.Errorf("video outside download")
				}
				app = filepath.Join(q.OutputPath, rel)
			}
			if ignored[app] {
				continue
			}
			files = append(files, File{Path: app, Size: e.Size})
			found[app] = true
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no downloaded video files found")
	}
	for _, it := range items {
		if !found[it.Path] {
			return nil, fmt.Errorf("import file absent from downloaded videos")
		}
	}
	return files, nil
}
