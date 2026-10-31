// Package libactivity turns the Arr queues into the import progress the
// Library view shows: one state per title and per season, with no download
// identifiers, paths or tracker data.
package activity

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"time"
)

const (
	StatusOK           = "ok"
	StatusUnconfigured = "unconfigured"
	StatusUnavailable  = "unavailable"
)

// Activity is the payload the dashboard polls. Titles from an app that is not
// ok are absent; its status says why.
type Activity struct {
	CheckedAt time.Time `json:"checked_at"`
	Sonarr    string    `json:"sonarr"`
	Radarr    string    `json:"radarr"`
	Titles    []Title   `json:"titles"`
}

// Title is one series or movie with downloads in progress. For a series the
// top-level progress is the most urgent season's.
type Title struct {
	Kind       string `json:"kind"`
	ID         int    `json:"id"`
	Title      string `json:"title,omitempty"`
	State      string `json:"state"`
	Percent    *int   `json:"percent"`
	ETASeconds *int64 `json:"eta_seconds"`
	SizeBytes  *int64 `json:"size_bytes,omitempty"`
	LeftBytes  *int64 `json:"left_bytes,omitempty"`
	Imported   *int   `json:"imported"`
	Total      *int   `json:"total"`
	// QueuePosition is where the title's first download sits in the Arr
	// queue as read (1-based): the order the download client takes them.
	QueuePosition *int     `json:"queue_position,omitempty"`
	Seasons       []Season `json:"seasons,omitempty"`
}

// Season is one season of a series with downloads in progress.
type Season struct {
	Season     int    `json:"season"`
	State      string `json:"state"`
	Percent    *int   `json:"percent"`
	ETASeconds *int64 `json:"eta_seconds"`
	SizeBytes  *int64 `json:"size_bytes,omitempty"`
	LeftBytes  *int64 `json:"left_bytes,omitempty"`
	Imported   *int   `json:"imported"`
	Total      *int   `json:"total"`
	// QueuePosition is the season's first download in the Arr queue (1-based).
	QueuePosition *int `json:"queue_position,omitempty"`
}

const (
	stateFailed      = "failed"
	stateAttention   = "attention"
	stateImporting   = "importing"
	stateDownloading = "downloading"
	statePaused      = "paused"
	stateWaiting     = "waiting"
	stateQueued      = "queued"
)

// urgency orders states for a title: the first match is the one shown.
var urgency = []string{stateFailed, stateAttention, stateImporting, stateDownloading, statePaused, stateWaiting, stateQueued}

// Source is one Arr app's download activity reader.
type Source interface {
	Configured() bool
	Activity(context.Context) (*ArrActivity, error)
}

// Collect reads both apps and aggregates what it could. An app that is not
// configured or fails to answer contributes no titles.
func Collect(ctx context.Context, now time.Time, sonarr, radarr Source) Activity {
	// A slow app cannot consume the other app's entire cache-build deadline.
	sonarrFeed := make(chan Feed, 1)
	go func() { sonarrFeed <- read(ctx, sonarr) }()
	radarrFeed := read(ctx, radarr)
	return Build(now, <-sonarrFeed, radarrFeed)
}

// Feed is one app's answer: its status, and its activity when it was read.
type Feed struct {
	Status   string
	Activity *ArrActivity
}

func read(ctx context.Context, src Source) Feed {
	if !src.Configured() {
		return Feed{Status: StatusUnconfigured}
	}
	act, err := src.Activity(ctx)
	if err != nil {
		return Feed{Status: StatusUnavailable}
	}
	return Feed{Status: StatusOK, Activity: act}
}

// Build aggregates two feeds. It is pure: the same input always gives the
// same output, and nothing here reads a clock except through now.
func Build(now time.Time, sonarr, radarr Feed) Activity {
	out := Activity{CheckedAt: now.UTC(), Sonarr: sonarr.Status, Radarr: radarr.Status, Titles: []Title{}}
	if sonarr.Activity != nil {
		out.Titles = append(out.Titles, seriesTitles(sonarr.Activity)...)
	}
	if radarr.Activity != nil {
		out.Titles = append(out.Titles, movieTitles(radarr.Activity)...)
	}
	sort.SliceStable(out.Titles, func(i, j int) bool {
		if out.Titles[i].Kind != out.Titles[j].Kind {
			return out.Titles[i].Kind < out.Titles[j].Kind
		}
		return out.Titles[i].ID < out.Titles[j].ID
	})
	return out
}

// season collects one series' queue records for one season. Imported episodes
// count only when their download is still queued in this season.
type season struct {
	records   []ActivityRecord
	position  int
	queued    map[episodeImport]bool
	imported  map[episodeImport]bool
	downloads map[string]ActivityRecord
}

// Reimports in separate downloads are separate work, even for the same episode.
type episodeImport struct {
	download string
	episode  int
}

func newSeason() *season {
	return &season{queued: map[episodeImport]bool{}, imported: map[episodeImport]bool{}, downloads: map[string]ActivityRecord{}}
}

func (s *season) add(r ActivityRecord) {
	s.records = append(s.records, r)
	s.queued[episodeImport{downloadKey(r), r.EpisodeID}] = true
	if k := downloadKey(r); s.downloads[k] == (ActivityRecord{}) {
		s.downloads[k] = r
	}
}

// downloadKey groups records by their download. A record with no download ID
// has nothing to match an import against, so it stands alone.
func downloadKey(r ActivityRecord) string {
	if r.DownloadID == "" {
		return "queue:" + strconv.Itoa(r.ID)
	}
	return strings.ToUpper(r.DownloadID)
}

func seriesTitles(a *ArrActivity) []Title {
	bySeries := map[int]map[int]*season{}
	for i, r := range a.Queue {
		sn := a.Seasons[r.EpisodeID]
		if bySeries[r.SeriesID] == nil {
			bySeries[r.SeriesID] = map[int]*season{}
		}
		if bySeries[r.SeriesID][sn] == nil {
			bySeries[r.SeriesID][sn] = newSeason()
			bySeries[r.SeriesID][sn].position = i + 1
		}
		bySeries[r.SeriesID][sn].add(r)
	}
	for _, im := range a.Imports {
		sn := a.Seasons[im.EpisodeID]
		s := bySeries[im.SeriesID][sn]
		if s == nil {
			continue
		}
		if _, ok := s.downloads[strings.ToUpper(im.DownloadID)]; ok {
			s.imported[episodeImport{strings.ToUpper(im.DownloadID), im.EpisodeID}] = true
		}
	}

	titles := []Title{}
	for seriesID, seasons := range bySeries {
		numbers := make([]int, 0, len(seasons))
		for n := range seasons {
			numbers = append(numbers, n)
		}
		sort.Ints(numbers)
		t := Title{Kind: "sonarr", ID: seriesID}
		for _, n := range numbers {
			if n := recordTitle(seasons[n].records, true); n != "" {
				t.Title = n
				break
			}
		}
		best := -1
		for _, n := range numbers {
			s := seasons[n]
			m := progress(s.records)
			imported, total := s.counts(m.state)
			pos := s.position
			view := Season{Season: n, State: m.state, Percent: m.percent, ETASeconds: m.eta, SizeBytes: m.size, LeftBytes: m.left, Imported: &imported, Total: &total, QueuePosition: &pos}
			t.Seasons = append(t.Seasons, view)
			if t.QueuePosition == nil || pos < *t.QueuePosition {
				t.QueuePosition = &pos
			}
			if r := rank(m.state); best < 0 || r < best {
				best = r
				t.State, t.Percent, t.ETASeconds, t.SizeBytes, t.LeftBytes, t.Imported, t.Total = m.state, m.percent, m.eta, m.size, m.left, &imported, &total
			}
		}
		titles = append(titles, t)
	}
	return titles
}

// Union queue and history within each download so an episode imported between
// the two reads is counted once. Sum downloads, without merging repacks. When
// showing Importing, unrelated downloads still downloading do not dilute N/M.
func (s *season) counts(state string) (imported, total int) {
	active := map[string]bool{}
	for _, r := range s.records {
		if state != stateImporting || recordState(r) == stateImporting {
			active[downloadKey(r)] = true
		}
	}
	for ep := range s.queued {
		if active[ep.download] {
			total++
		}
	}
	for ep := range s.imported {
		if active[ep.download] {
			imported++
			if !s.queued[ep] {
				total++
			}
		}
	}
	return imported, total
}

func movieTitles(a *ArrActivity) []Title {
	byMovie := map[int][]ActivityRecord{}
	position := map[int]int{}
	for i, r := range a.Queue {
		if _, ok := byMovie[r.MovieID]; !ok {
			position[r.MovieID] = i + 1
		}
		byMovie[r.MovieID] = append(byMovie[r.MovieID], r)
	}
	titles := []Title{}
	for id, records := range byMovie {
		m := progress(records)
		pos := position[id]
		titles = append(titles, Title{Kind: "radarr", ID: id, Title: recordTitle(records, false), State: m.state, Percent: m.percent, ETASeconds: m.eta, SizeBytes: m.size, LeftBytes: m.left, QueuePosition: &pos})
	}
	return titles
}

// recordTitle is the first series or movie title a queue record carries.
func recordTitle(records []ActivityRecord, series bool) string {
	for _, r := range records {
		name := r.Movie
		if series {
			name = r.Series
		}
		if name != nil && strings.TrimSpace(name.Title) != "" {
			return strings.TrimSpace(name.Title)
		}
	}
	return ""
}

// measurement is the state of a set of queue records and, while downloading
// or paused, how far along it is.
type measurement struct {
	state      string
	percent    *int
	eta        *int64
	size, left *int64
}

// progress gives the state of a set of queue records. The first state in
// urgency order that any record is in wins.
func progress(records []ActivityRecord) measurement {
	seen := map[string]bool{}
	for _, r := range records {
		seen[recordState(r)] = true
	}
	state := stateWaiting
	for _, s := range urgency {
		if seen[s] {
			state = s
			break
		}
	}
	if state != stateDownloading && state != statePaused {
		return measurement{state: state}
	}
	relevant := []ActivityRecord{}
	for _, r := range records {
		if recordState(r) == state {
			relevant = append(relevant, r)
		}
	}
	m := measure(relevant)
	m.state = state
	if state == statePaused {
		m.eta = nil
	}
	return m
}

func recordState(r ActivityRecord) string {
	switch {
	case strings.EqualFold(r.Status, "failed"), strings.EqualFold(r.TrackedDownloadState, "failed"), strings.EqualFold(r.TrackedDownloadState, "failedPending"), strings.EqualFold(r.TrackedDownloadState, "downloadFailed"):
		return stateFailed
	case strings.EqualFold(r.Status, "error"), strings.EqualFold(r.Status, "warning"):
		return stateAttention
	case r.DownloadState == "failed":
		return stateFailed
	case r.Problem:
		return stateAttention
	case strings.EqualFold(r.TrackedDownloadState, "importing"), r.ImportState == "imported":
		return stateImporting
	case r.DownloadState == "completed":
		return stateWaiting
	case r.DownloadState == "paused":
		return statePaused
	case strings.EqualFold(r.Status, "queued"), strings.EqualFold(r.Status, "delay"):
		return stateQueued
	default:
		return stateDownloading
	}
}

// measure works out percent and remaining time over the distinct downloads in
// a set of records. Every record in a pack carries the same size, so the first
// record per download stands for it. ETA is nil unless every download reports one.
func measure(records []ActivityRecord) measurement {
	firsts := map[string]ActivityRecord{}
	for _, r := range records {
		if _, ok := firsts[downloadKey(r)]; !ok {
			firsts[downloadKey(r)] = r
		}
	}
	var size, left, eta int64
	etaKnown := len(firsts) > 0
	for _, r := range firsts {
		size += r.Size
		left += r.SizeLeft
		secs, ok := parseTimeLeft(r.TimeLeft)
		if !ok {
			etaKnown = false
		} else if secs > eta {
			eta = secs
		}
	}
	var m measurement
	if size > 0 {
		left = min(max(left, 0), size)
		p := int((size - left) * 100 / size)
		m.percent, m.size, m.left = &p, &size, &left
	}
	if etaKnown {
		m.eta = &eta
	}
	return m
}

// parseTimeLeft reads Arr's "hh:mm:ss" or "d.hh:mm:ss" remaining time.
func parseTimeLeft(s string) (int64, bool) {
	var days int64
	if i := strings.IndexByte(s, '.'); i >= 0 {
		d, err := strconv.ParseInt(s[:i], 10, 64)
		if err != nil || d < 0 {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	var secs [3]int64
	for i, p := range parts {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 0 {
			return 0, false
		}
		secs[i] = n
	}
	return days*86400 + secs[0]*3600 + secs[1]*60 + secs[2], true
}

func rank(state string) int {
	for i, s := range urgency {
		if s == state {
			return i
		}
	}
	return len(urgency)
}
