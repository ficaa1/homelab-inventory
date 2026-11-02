package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"
)

type digestTautulli struct {
	media   *Media
	service service
}

func (t *digestTautulli) call(ctx context.Context, command string, q url.Values, out any) error {
	q.Set("cmd", command)
	return t.media.get(ctx, t.service, "/api/v2", q, out, true)
}

type Session struct {
	Title             string `json:"title"`
	GrandparentTitle  string `json:"grandparent_title"`
	User              string `json:"friendly_name"`
	MediaType         string `json:"media_type"`
	TranscodeDecision string `json:"transcode_decision"`
	VideoResolution   string `json:"video_resolution"`
	Progress          int    `json:"progress"`
	Duration          int    `json:"duration"`
	ViewOffset        *int   `json:"view_offset"`
	State             string `json:"state"`
	Quality           string `json:"quality_profile"`
}

type Activity struct {
	StreamCount          int       `json:"stream_count"`
	TranscodeCount       int       `json:"transcode_count"`
	TotalBandwidth       int       `json:"total_bandwidth"`
	DirectPlayCount      int       `json:"direct_play_count"`
	DirectStreamCount    int       `json:"direct_stream_count"`
	TranscodeStreamCount int       `json:"transcode_stream_count"`
	Sessions             []Session `json:"sessions"`
}

type Play struct {
	Title             string    `json:"title"`
	GrandparentTitle  string    `json:"grandparent_title"`
	ParentTitle       string    `json:"parent_title"`
	MediaType         string    `json:"media_type"`
	User              string    `json:"user"`
	Platform          string    `json:"platform"`
	TranscodeDecision string    `json:"transcode_decision"`
	WatchedStatus     string    `json:"watched_status"`
	Started           time.Time `json:"started"`
	PlayDuration      int       `json:"play_duration"`
}

type History struct {
	TotalPlays    int    `json:"total_plays"`
	TotalDuration int    `json:"total_duration"` // seconds of actual playback across all plays
	DistinctUsers int    `json:"distinct_users"`
	Truncated     bool   `json:"truncated"`
	Plays         []Play `json:"plays"` // newest first
}

type Library struct {
	SectionID  int    `json:"section_id"`
	Name       string `json:"section_name"`
	Type       string `json:"section_type"`
	Count      int    `json:"count"`
	TotalBytes int64  `json:"total_bytes"`
}

type tauInt int

func (n *tauInt) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return nil
	}
	var v string
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
	} else {
		v = string(b)
	}
	if v == "" {
		return nil
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("invalid Tautulli integer")
	}
	*n = tauInt(i)
	return nil
}
func (s *Session) UnmarshalJSON(b []byte) error {
	type plain Session
	var raw struct {
		*plain
		Progress   tauInt  `json:"progress"`
		Duration   tauInt  `json:"duration"`
		ViewOffset *tauInt `json:"view_offset"`
	}
	raw.plain = (*plain)(s)
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	s.Progress = int(raw.Progress)
	s.Duration = int(raw.Duration)
	if raw.ViewOffset != nil {
		v := int(*raw.ViewOffset)
		s.ViewOffset = &v
	} else {
		s.ViewOffset = nil
	}
	return nil
}
func (t *digestTautulli) Activity(ctx context.Context) (*Activity, error) {
	var a struct {
		StreamCount *tauInt   `json:"stream_count"`
		Direct      tauInt    `json:"stream_count_direct_play"`
		Copy        tauInt    `json:"stream_count_direct_stream"`
		Transcode   tauInt    `json:"stream_count_transcode"`
		Bandwidth   tauInt    `json:"total_bandwidth"`
		Sessions    []Session `json:"sessions"`
	}
	if err := t.call(ctx, "get_activity", url.Values{}, &a); err != nil {
		return nil, err
	}
	if a.StreamCount == nil {
		return nil, fmt.Errorf("tautulli: get_activity: missing stream count")
	}
	if a.Sessions == nil {
		a.Sessions = []Session{}
	}
	return &Activity{StreamCount: int(*a.StreamCount), TranscodeCount: int(a.Transcode), TotalBandwidth: int(a.Bandwidth), DirectPlayCount: int(a.Direct), DirectStreamCount: int(a.Copy), TranscodeStreamCount: int(a.Transcode), Sessions: a.Sessions}, nil
}

func (t *digestTautulli) History(ctx context.Context, since time.Time) (*History, error) {
	params := url.Values{}
	params.Set("after", fmt.Sprintf("%d", since.Unix()))
	params.Set("length", "200")
	var raw struct {
		Data []struct {
			Date              json.RawMessage `json:"date"`
			Title             string          `json:"title"`
			GrandparentTitle  string          `json:"grandparent_title"`
			ParentTitle       string          `json:"parent_title"`
			MediaType         string          `json:"media_type"`
			User              string          `json:"user"`
			Platform          string          `json:"platform"`
			TranscodeDecision string          `json:"transcode_decision"`
			WatchedStatus     json.RawMessage `json:"watched_status"`
			PlayDuration      json.RawMessage `json:"play_duration"`
		} `json:"data"`
	}
	if err := t.call(ctx, "get_history", params, &raw); err != nil {
		return nil, err
	}
	h := &History{}
	users := map[string]bool{}
	for _, r := range raw.Data {
		started, err := time.Parse(time.RFC3339, rawString(r.Date))
		if err != nil {
			if ts := rawInt(r.Date); ts > 0 {
				started = time.Unix(int64(ts), 0)
			}
		}
		dur := rawInt(r.PlayDuration)
		if dur > 0 {
			h.TotalDuration += dur
		}
		h.TotalPlays++
		if r.User != "" {
			users[r.User] = true
		}
		h.Plays = append(h.Plays, Play{
			Title:             r.Title,
			GrandparentTitle:  r.GrandparentTitle,
			ParentTitle:       r.ParentTitle,
			MediaType:         r.MediaType,
			User:              r.User,
			Platform:          r.Platform,
			TranscodeDecision: r.TranscodeDecision,
			WatchedStatus:     rawString(r.WatchedStatus),
			Started:           started,
			PlayDuration:      dur,
		})
	}
	h.DistinctUsers = len(users)
	h.Truncated = len(raw.Data) >= 200
	sort.SliceStable(h.Plays, func(i, j int) bool { return h.Plays[i].Started.After(h.Plays[j].Started) })
	return h, nil
}

func (t *digestTautulli) Libraries(ctx context.Context) ([]Library, error) {
	var names []struct {
		SectionID int    `json:"section_id"`
		Name      string `json:"section_name"`
		Type      string `json:"section_type"`
	}
	if err := t.call(ctx, "get_library_names", url.Values{}, &names); err != nil {
		return nil, err
	}
	out := make([]Library, 0, len(names))
	for _, n := range names {
		params := url.Values{}
		params.Set("section_id", fmt.Sprintf("%d", n.SectionID))
		params.Set("length", "1")
		var info struct {
			RecordsTotal  json.RawMessage `json:"recordsTotal"`
			TotalFileSize json.RawMessage `json:"total_file_size"`
		}
		if err := t.call(ctx, "get_library_media_info", params, &info); err != nil {
			return nil, err
		}
		out = append(out, Library{
			SectionID:  n.SectionID,
			Name:       n.Name,
			Type:       n.Type,
			Count:      rawInt(info.RecordsTotal),
			TotalBytes: int64(rawInt(info.TotalFileSize)),
		})
	}
	return out, nil
}
func atoi(s string) int {
	var n int
	if s == "" {
		return 0
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func rawInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return atoi(s)
		}
		return 0
	}
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	return 0
}

func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	return string(raw)
}
