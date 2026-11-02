package archivetier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

// Client restricts library writes to policy operations.
// Writes are restricted to grab, ManualImport and the two archive tags through
// exact Arr editor paths, plus on-demand creation of those two tags.
type Client struct {
	Kind, Base, Key string
	HTTP            *http.Client
	Apply           bool
	Safety          Config
	FS              Filesystem
	// TagCreateResult is called before any later write; an audit failure stops the edit.
	TagCreateResult func(TagCreateResult) error
}

func NewClient(kind, base, key string, apply bool) *Client {
	return &Client{Kind: kind, Base: strings.TrimRight(base, "/"), Key: key, Apply: apply, Safety: DefaultConfig(), HTTP: &http.Client{Timeout: 90 * time.Second, CheckRedirect: httpx.RefuseRedirects()}}
}
func validBase(s string) bool {
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func (c *Client) call(ctx context.Context, method, p string, q url.Values, body, out any) error {
	if !validBase(c.Base) || c.Key == "" || c.HTTP == nil {
		return fmt.Errorf("%s not configured", c.Kind)
	}
	var b bytes.Buffer
	if body != nil {
		if e := json.NewEncoder(&b).Encode(body); e != nil {
			return fmt.Errorf("request encoding failed")
		}
	}
	allowed := method == http.MethodPost && (p == "/release" || p == "/command") || method == http.MethodPut && (c.Kind == "radarr" && p == "/movie/editor" || c.Kind == "sonarr" && p == "/series/editor")
	allowed = allowed || method == http.MethodPost && p == "/tag" && len(q) == 0 && (c.Kind == "radarr" || c.Kind == "sonarr") && TagCreateAllowed(b.Bytes())
	allowed = allowed || method == http.MethodDelete && (c.Kind == "radarr" || c.Kind == "sonarr") && QueueDeleteAllowed(p, q) && body == nil
	if method != http.MethodGet && (!c.Apply || !allowed) {
		return fmt.Errorf("write refused")
	}
	endpoint := c.Base + "/api/v3" + p
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, &b)
	if e != nil {
		return fmt.Errorf("%s request failed", c.Kind)
	}
	req.Header.Set("X-Api-Key", c.Key)
	req.Header.Set("Content-Type", "application/json")
	transport := *c.HTTP
	transport.CheckRedirect = httpx.RefuseRedirects()
	resp, e := transport.Do(req)
	if e != nil {
		return fmt.Errorf("%s transport failed", c.Kind)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := ""
		if method == http.MethodPost && p == "/release" && resp.StatusCode == 404 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
			for text, label := range map[string]string{"Couldn't find requested release in cache": "release missing from cache", "Unable to parse episodes in the release": "release episodes could not be parsed", "Unable to find matching series and episodes": "release could not be mapped to series"} {
				if bytes.Contains(b, []byte(text)) {
					reason = label
					break
				}
			}
		}
		if method == http.MethodPost && p == "/tag" && resp.StatusCode == http.StatusBadRequest {
			var validation []struct {
				PropertyName string `json:"propertyName"`
				ErrorMessage string `json:"errorMessage"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&validation) == nil {
				for _, v := range validation {
					if strings.EqualFold(v.PropertyName, "label") && strings.EqualFold(v.ErrorMessage, "Tag already exists") {
						reason = "tag already exists"
					}
				}
			}
		}
		return &HTTPError{Kind: c.Kind, Status: resp.StatusCode, Reason: reason}
	}
	if out != nil {
		if e := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); e != nil {
			return fmt.Errorf("%s response decoding failed", c.Kind)
		}
	}
	return nil
}
func (c *Client) get(ctx context.Context, p string, q url.Values, out any) error {
	return c.call(ctx, http.MethodGet, p, q, nil, out)
}
func (c *Client) Search(ctx context.Context, t Target) ([]Release, error) {
	q := url.Values{}
	if c.Kind == "sonarr" {
		q.Set("seriesId", strconv.Itoa(t.ID))
		q.Set("seasonNumber", strconv.Itoa(t.Season))
	} else {
		q.Set("movieId", strconv.Itoa(t.ID))
	}
	return list[Release](ctx, c, "/release", q)
}

// HTTPError preserves status without exposing upstream bodies or credentials.
type HTTPError struct {
	Kind   string
	Status int
	Reason string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("%s HTTP %d", e.Kind, e.Status) }

func (c *Client) Grab(ctx context.Context, r Release) error {
	if r.Guid == "" || r.IndexerID <= 0 {
		return &HTTPError{Kind: c.Kind, Status: 400}
	}
	body := map[string]any{"guid": r.Guid, "indexerId": r.IndexerID}
	// Sonarr needs the series identity when the cached release could not be mapped.
	if r.SeriesID > 0 {
		body["seriesId"] = r.SeriesID
	}
	if r.MovieID > 0 {
		body["movieId"] = r.MovieID
	}
	return c.call(ctx, http.MethodPost, "/release", nil, body, nil)
}

type Title struct {
	TMDBID    int    `json:"tmdbId"`
	TVDBID    int    `json:"tvdbId"`
	IMDBID    string `json:"imdbId"`
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Runtime   int    `json:"runtime"`
	Tags      *[]int `json:"tags"`
	MovieFile *File  `json:"movieFile"`
}

type tag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
}

// archiveTag returns the archive tag ID, 0 when no such tag exists.
func (c *Client) archiveTag(ctx context.Context) (int, error) { return c.tagID(ctx, "archive") }
func (c *Client) tagID(ctx context.Context, label string) (int, error) {
	tags, e := list[tag](ctx, c, "/tag", nil)
	if e != nil {
		return 0, e
	}
	if e = uniqueIDs(tags, func(t tag) int { return t.ID }); e != nil {
		return 0, fmt.Errorf("%s tags: %v", c.Kind, e)
	}
	id := 0
	for _, v := range tags {
		if strings.EqualFold(v.Label, label) {
			if id != 0 {
				return 0, fmt.Errorf("Tags need attention")
			}
			id = v.ID
		}
	}
	return id, nil
}

// files is the title's current library files; a movie without a file has none.
func (v Title) files() []File {
	if v.MovieFile == nil {
		return []File{}
	}
	return []File{*v.MovieFile}
}
func hasTag(v Title, id int) bool {
	if id <= 0 || v.Tags == nil {
		return false
	}
	for _, x := range *v.Tags {
		if x == id {
			return true
		}
	}
	return false
}
func validTitle(v Title) error {
	if v.ID <= 0 || v.Tags == nil || (v.MovieFile != nil && (v.MovieFile.ID <= 0 || v.MovieFile.Path == "" || v.MovieFile.Size <= 0)) {
		return fmt.Errorf("invalid title record")
	}
	return nil
}
func (c *Client) episodes(ctx context.Context, id int) ([]Episode, error) {
	eps, e := list[Episode](ctx, c, "/episode", url.Values{"seriesId": {strconv.Itoa(id)}})
	if e != nil {
		return nil, e
	}
	for _, ep := range eps {
		if ep.SeriesID != id {
			return nil, fmt.Errorf("episode belongs to another series")
		}
	}
	return eps, uniqueIDs(eps, func(ep Episode) int { return ep.ID })
}
func (c *Client) episodeFiles(ctx context.Context, id int) ([]File, error) {
	fs, e := list[File](ctx, c, "/episodefile", url.Values{"seriesId": {strconv.Itoa(id)}})
	if e != nil {
		return nil, e
	}
	for _, f := range fs {
		if f.Path == "" || f.Size <= 0 {
			return nil, fmt.Errorf("invalid episode file record")
		}
	}
	return fs, uniqueIDs(fs, func(f File) int { return f.ID })
}

func (c *Client) Targets(ctx context.Context) ([]Target, error) {
	tag, e := c.archiveTag(ctx)
	if e != nil {
		return nil, e
	}
	out := []Target{}
	if tag == 0 {
		return out, nil
	}
	fourK, e := c.tagID(ctx, FourKTag)
	if e != nil {
		return nil, e
	}
	p := "/movie"
	if c.Kind == "sonarr" {
		p = "/series"
	}
	titles, e := list[Title](ctx, c, p, nil)
	if e != nil {
		return nil, e
	}
	if e = uniqueIDs(titles, func(v Title) int { return v.ID }); e != nil {
		return nil, e
	}
	for _, v := range titles {
		if e := validTitle(v); e != nil {
			return nil, e
		}
		found := false
		for _, id := range *v.Tags {
			if id == tag {
				found = true
			}
		}
		if !found {
			continue
		}
		if c.Kind == "radarr" {
			out = append(out, Target{Kind: c.Kind, ID: v.ID, TMDBID: v.TMDBID, TVDBID: v.TVDBID, IMDBID: v.IMDBID, Title: v.Title, Files: v.files(), Runtime: v.Runtime, FourK: hasTag(v, fourK)})
			continue
		}
		eps, e := c.episodes(ctx, v.ID)
		if e != nil {
			return nil, e
		}
		files, e := c.episodeFiles(ctx, v.ID)
		if e != nil {
			return nil, e
		}
		seasons := map[int]bool{}
		for _, f := range files {
			seasons[f.Season] = true
		}
		for s := range seasons {
			t := Target{Kind: c.Kind, ID: v.ID, TMDBID: v.TMDBID, TVDBID: v.TVDBID, IMDBID: v.IMDBID, Title: v.Title, Season: s, Runtime: v.Runtime, FourK: hasTag(v, fourK), Files: []File{}, Episodes: []Episode{}}
			for _, ep := range eps {
				if ep.Season == s {
					t.Episodes = append(t.Episodes, ep)
				}
			}
			for _, f := range files {
				if f.Season == s {
					t.Files = append(t.Files, f)
				}
			}
			for _, ep := range eps {
				if ep.Season == s {
					continue
				}
				for _, f := range t.Files {
					if ep.FileID == f.ID {
						t.Unsafe = "library file spans multiple seasons"
					}
				}
			}
			out = append(out, t)
		}
	}
	return out, nil
}
func (c *Client) Current(ctx context.Context, t Target) (Target, error) {
	out := t
	out.Files = []File{}
	if c.Kind == "radarr" {
		var v Title
		if e := c.get(ctx, "/movie/"+strconv.Itoa(t.ID), nil, &v); e != nil {
			return out, e
		}
		if e := validTitle(v); e != nil || v.ID != t.ID {
			return out, fmt.Errorf("invalid current movie record")
		}
		out.Files = v.files()
		return out, nil
	}
	fs, e := c.episodeFiles(ctx, t.ID)
	if e != nil {
		return out, e
	}
	for _, f := range fs {
		if f.Season == t.Season {
			out.Files = append(out.Files, f)
		}
	}
	eps, e := c.episodes(ctx, t.ID)
	if e != nil {
		return out, e
	}
	out.Episodes = nil
	for _, ep := range eps {
		if ep.Season == t.Season {
			out.Episodes = append(out.Episodes, ep)
		}
	}
	return out, nil
}

type QueueItem struct {
	IndexerID  int    `json:"indexerId,omitempty"`
	Indexer    string `json:"indexer,omitempty"`
	ID         int    `json:"id"`
	SeriesID   int    `json:"seriesId"`
	EpisodeID  int    `json:"episodeId"`
	MovieID    int    `json:"movieId"`
	Title      string `json:"title"`
	DownloadID string `json:"downloadId"`
	SizeLeft   int64  `json:"sizeleft"`
	Size       int64  `json:"size"`
	TimeLeft   string `json:"timeleft,omitempty"`
	// EstimatedCompletion is the download client's ETA, when it has one.
	EstimatedCompletion *time.Time `json:"estimatedCompletionTime,omitempty"`
	State               string     `json:"trackedDownloadState"`
	Status              string     `json:"status"`
	OutputPath          string     `json:"outputPath"`
}

func (q QueueItem) Matches(t Target) bool {
	if t.Kind == "sonarr" {
		return q.SeriesID == t.ID
	}
	return q.MovieID == t.ID
}

// extraPackRow identifies a known unrelated episode of the selected Sonarr
// download. Unknown episode identities still require the normal queue proof.
func (q QueueItem) extraPackRow(j *Job) bool {
	return j.Target.Kind == "sonarr" && q.SeriesID == j.Target.ID &&
		q.DownloadID == j.DownloadID && q.EpisodeID > 0 && !wantedEpisode(j.Target, q.EpisodeID)
}

func wantedEpisode(t Target, id int) bool {
	for _, ep := range t.Episodes {
		if ep.ID == id {
			return true
		}
	}
	return false
}

// targetImportItems leaves unrelated Sonarr files out before any file checks.
// Mixed wanted/unwanted mappings remain subject to strict coverage validation.
func targetImportItems(items []ManualItem, t Target) []ManualItem {
	if t.Kind != "sonarr" {
		return items
	}
	out := make([]ManualItem, 0, len(items))
	for _, it := range items {
		for _, ep := range it.Episodes {
			if ep.Season == t.Season && wantedEpisode(t, ep.ID) {
				out = append(out, it)
				break
			}
		}
	}
	return out
}

// Queue reads the whole queue strictly: a null or short page is an error,
// never an empty queue.
func (c *Client) Queue(ctx context.Context) ([]QueueItem, error) {
	out, e := paged(ctx, c, "/queue", url.Values{"includeUnknownSeriesItems": {"true"}, "includeUnknownMovieItems": {"true"}}, func(q QueueItem) int { return q.ID })
	if e != nil {
		return nil, e
	}
	for _, it := range out {
		if c.Kind == "sonarr" && it.SeriesID <= 0 || c.Kind == "radarr" && it.MovieID <= 0 {
			return nil, fmt.Errorf("queue contains unmapped title; pause")
		}
	}
	return out, nil
}
func (c *Client) Preflight(ctx context.Context) error {
	var st struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if e := c.get(ctx, "/system/status", nil, &st); e != nil {
		return e
	}
	reviewed := c.Safety.ReviewedVersions[c.Kind]
	ok := reviewed != "" && st.Version == reviewed && (c.Kind == "sonarr" && st.AppName == "Sonarr" || c.Kind == "radarr" && st.AppName == "Radarr")
	if !ok {
		return fmt.Errorf("unsupported %s API version", c.Kind)
	}
	return nil
}

// ManualItem preserves the app's quality and language models for the command.
type ManualItem struct {
	Edition    string `json:"edition"`
	Path       string `json:"path"`
	FolderName string `json:"folderName"`
	Size       int64  `json:"size"`
	Series     struct {
		ID int `json:"id"`
	} `json:"series"`
	Movie struct {
		ID      int    `json:"id"`
		Edition string `json:"edition"`
	} `json:"movie"`
	Episodes     []Episode  `json:"episodes"`
	Quality      Quality    `json:"quality"`
	Languages    []Language `json:"languages"`
	MediaInfo    *MediaInfo `json:"mediaInfo,omitempty"`
	ReleaseGroup string     `json:"releaseGroup"`
	Flags        int        `json:"indexerFlags"`
	ReleaseType  string     `json:"releaseType"`
	Rejections   []struct {
		Reason string `json:"reason"`
	} `json:"rejections"`
}

func (c *Client) Manual(ctx context.Context, q QueueItem) ([]ManualItem, error) {
	return list[ManualItem](ctx, c, "/manualimport", url.Values{"downloadId": {q.DownloadID}, "filterExistingFiles": {"false"}})
}

// ValidateImport checks the downloaded files against the job's mode and
// caps. The runner supplies the real file probe for automatic jobs; when
// present it establishes audio, HDR and codec instead of the filename.
func ValidateImport(items []ManualItem, t Target, c Config, mode string) ([]map[string]any, error) {
	return validateImport(items, t, c, mode, false)
}

func validateImport(items []ManualItem, t Target, c Config, mode string, force bool) ([]map[string]any, error) {
	items = targetImportItems(items, t)
	if len(items) == 0 {
		return nil, fmt.Errorf("no importable files")
	}
	covered := map[int]bool{}
	wanted := map[int]bool{}
	for _, ep := range t.Episodes {
		wanted[ep.ID] = true
	}
	var sum int64
	files := []map[string]any{}
	paths := map[string]bool{}
	maxFactor := 0.0
	for _, it := range items {
		factor := codecFactor(c, filepath.Base(it.Path))
		if it.MediaInfo != nil {
			factor = codecFactor(c, it.MediaInfo.VideoCodec)
		}
		maxFactor = math.Max(maxFactor, factor)
		if !safePath(it.Path, c.DownloadRoots) || paths[it.Path] || it.Size <= 0 {
			return nil, fmt.Errorf("unsafe downloaded file")
		}
		if mode != ModeAny && (resolution(it.Quality) != modeResolution(mode) || !sourceOK(it.Quality, ParseTraits(filepath.Base(it.Path))) || !languageOK(it.Languages, c.Languages) || unsafeEdition(it.Path+" "+it.FolderName+" "+it.Edition+" "+it.Movie.Edition)) {
			return nil, fmt.Errorf("unsafe downloaded file")
		}
		if why := profileReject(mode, it.Quality, filepath.Base(it.Path), c); mode != ModeAny && it.MediaInfo == nil && why != "" {
			return nil, fmt.Errorf("download profile: %s", why)
		}
		if it.MediaInfo != nil && mode != ModeAny {
			if why := mediaReject(File{MediaInfo: it.MediaInfo}, mode); why != "" {
				return nil, fmt.Errorf("download probe: %s", why)
			}
		}
		paths[it.Path] = true
		for _, r := range it.Rejections {
			if !force && !rankingOnly(r.Reason) {
				return nil, fmt.Errorf("download has non-ranking import rejection")
			}
		}
		f := map[string]any{"path": it.Path, "folderName": it.FolderName, "quality": it.Quality, "languages": it.Languages, "releaseGroup": it.ReleaseGroup, "indexerFlags": 1}
		// Deliberately omit downloadId: tracked ManualImport may delete source folders.
		if t.Kind == "sonarr" {
			if it.Series.ID != t.ID || len(it.Episodes) == 0 {
				return nil, fmt.Errorf("wrong series")
			}
			var ids []int
			for _, ep := range it.Episodes {
				if ep.Season != t.Season || !wanted[ep.ID] || covered[ep.ID] {
					return nil, fmt.Errorf("incorrect season coverage")
				}
				covered[ep.ID] = true
				ids = append(ids, ep.ID)
			}
			perFile := t
			perFile.Episodes = nil
			for _, id := range ids {
				perFile.Episodes = append(perFile.Episodes, Episode{ID: id})
			}
			if mode != ModeAny && it.Size > capBytes(perFile, c, mode, factor) {
				return nil, fmt.Errorf("per-file episode size cap")
			}
			f["seriesId"] = t.ID
			f["episodeIds"] = ids
			f["releaseType"] = it.ReleaseType
		} else {
			if len(items) != 1 || it.Movie.ID != t.ID {
				return nil, fmt.Errorf("wrong movie or extra files")
			}
			if mode != ModeAny && it.Size > capBytes(t, c, mode, factor) {
				return nil, fmt.Errorf("per-file movie size cap")
			}
			f["movieId"] = t.ID
		}
		sum += it.Size
		files = append(files, f)
	}
	if !force && t.Kind == "sonarr" && len(covered) != len(wanted) {
		return nil, fmt.Errorf("incomplete season pack")
	}
	if !force && (mode == ModeAny && sum >= t.Bytes() || mode != ModeAny && (sum > capBytes(t, c, mode, maxFactor) || float64(sum) >= float64(t.Bytes())*c.MaxFraction)) {
		return nil, fmt.Errorf("actual downloaded size fails savings cap")
	}
	return files, nil
}
func (c *Client) Import(ctx context.Context, files []map[string]any) (int, error) {
	for _, f := range files {
		if _, tracked := f["downloadId"]; tracked {
			return 0, fmt.Errorf("tracked ManualImport refused: source cleanup risk")
		}
	}
	var v struct {
		ID int `json:"id"`
	}
	e := c.call(ctx, http.MethodPost, "/command", nil, map[string]any{"name": "ManualImport", "importMode": "copy", "files": files}, &v)
	return v.ID, e
}
func (c *Client) Command(ctx context.Context, id int) (string, error) {
	var v struct {
		ID     int    `json:"id"`
		Status string `json:"status"`
	}
	if e := c.get(ctx, "/command/"+strconv.Itoa(id), nil, &v); e != nil {
		return "", e
	}
	switch v.Status {
	case "queued", "started", "completed", "failed", "aborted", "cancelled", "orphaned":
	default:
		return "", fmt.Errorf("unknown command status")
	}
	if v.ID != id {
		return "", fmt.Errorf("command identity mismatch")
	}
	return v.Status, nil
}

// Present asks the app to stat each library path. Database records alone are not
// proof that an NFS write succeeded. File-mode manualimport GET never imports.
func (c *Client) Present(ctx context.Context, t Target) (bool, error) {
	for _, f := range t.Files {
		items, e := list[ManualItem](ctx, c, "/manualimport", url.Values{"folder": {f.Path}, "filterExistingFiles": {"false"}})
		if e != nil {
			return false, e
		}
		if len(items) != 1 || items[0].Path != f.Path || items[0].Size != f.Size || f.Size <= 0 {
			return false, nil
		}
	}
	return len(t.Files) > 0, nil
}
func (c *Client) Opted(ctx context.Context, t Target) (bool, error) {
	tag, e := c.archiveTag(ctx)
	if e != nil {
		return false, e
	}
	if tag == 0 {
		return false, nil
	}
	p := "/movie/"
	if t.Kind == "sonarr" {
		p = "/series/"
	}
	var v Title
	if e := c.get(ctx, p+strconv.Itoa(t.ID), nil, &v); e != nil {
		return false, e
	}
	if e := validTitle(v); e != nil || v.ID != t.ID {
		return false, fmt.Errorf("invalid title record")
	}
	fourK, e := c.tagID(ctx, FourKTag)
	if e != nil {
		return false, e
	}
	if hasTag(v, fourK) != t.FourK {
		return false, fmt.Errorf("4k tag changed; replacement refused")
	}
	return hasTag(v, tag), nil
}
