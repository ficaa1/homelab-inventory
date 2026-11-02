package archivetier

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// LibraryItem is an Arr database inventory, never a filesystem scan. Series
// quality is omitted to avoid one episode-file request per title.
type LibraryItem struct {
	Kind          string          `json:"kind"`
	ID            int             `json:"id"`
	Title         string          `json:"title"`
	Year          int             `json:"year"`
	Path          string          `json:"path"`
	SizeBytes     int64           `json:"sizeBytes"`
	Runtime       int             `json:"runtime,omitempty"`
	Resolution    int             `json:"resolution,omitempty"`
	Quality       string          `json:"quality,omitempty"`
	VideoCodec    string          `json:"videoCodec,omitempty"`
	DynamicRange  string          `json:"dynamicRange,omitempty"`
	Tags          LibraryTags     `json:"tags"`
	Seasons       []LibrarySeason `json:"seasons,omitempty"`
	ArchiveStatus LibraryStatus   `json:"archiveStatus"`
}
type LibraryTags struct {
	Archive bool `json:"archive"`
	FourK   bool `json:"fourK"`
}
type LibrarySeason struct {
	Season        int           `json:"season"`
	Selected      bool          `json:"selected,omitempty"`
	SizeBytes     int64         `json:"sizeBytes"`
	ArchiveStatus LibraryStatus `json:"archiveStatus"`
}
type LibraryStatus struct {
	Status     string `json:"status"`
	Date       string `json:"date,omitempty"`
	SavedBytes int64  `json:"savedBytes,omitempty"`
	Phase      Phase  `json:"phase,omitempty"`
}

func (c *Client) Library(ctx context.Context) ([]LibraryItem, error) {
	tags, err := list[tag](ctx, c, "/tag", nil)
	if err != nil {
		return nil, err
	}
	if err := uniqueIDs(tags, func(t tag) int { return t.ID }); err != nil {
		return nil, err
	}
	labels := map[int]string{}
	for _, t := range tags {
		labels[t.ID] = t.Label
	}
	p := "/movie"
	if c.Kind == "sonarr" {
		p = "/series"
	} else if c.Kind != "radarr" {
		return nil, fmt.Errorf("invalid library kind")
	}
	type stats struct {
		Size int64 `json:"sizeOnDisk"`
	}
	type record struct {
		ID         int    `json:"id"`
		Title      string `json:"title"`
		Year       int    `json:"year"`
		Path       string `json:"path"`
		Runtime    int    `json:"runtime"`
		Size       int64  `json:"sizeOnDisk"`
		Tags       *[]int `json:"tags"`
		MovieFile  *File  `json:"movieFile"`
		Statistics stats  `json:"statistics"`
		Seasons    []struct {
			Number     int   `json:"seasonNumber"`
			Statistics stats `json:"statistics"`
		} `json:"seasons"`
	}
	titles, err := list[record](ctx, c, p, nil)
	if err != nil {
		return nil, err
	}
	if err := uniqueIDs(titles, func(t record) int { return t.ID }); err != nil {
		return nil, err
	}
	out := []LibraryItem{}
	for _, t := range titles {
		if t.Tags == nil || t.Title == "" || t.Path == "" || t.Size < 0 || t.Statistics.Size < 0 {
			return nil, fmt.Errorf("invalid %s library record", c.Kind)
		}
		item := LibraryItem{Kind: c.Kind, ID: t.ID, Title: t.Title, Year: t.Year, Path: t.Path, Runtime: t.Runtime, SizeBytes: t.Size}
		for _, id := range *t.Tags {
			item.Tags.Archive = item.Tags.Archive || strings.EqualFold(labels[id], "archive")
			item.Tags.FourK = item.Tags.FourK || strings.EqualFold(labels[id], FourKTag)
		}
		if c.Kind == "radarr" {
			if t.MovieFile == nil || t.MovieFile.ID <= 0 || t.MovieFile.Size <= 0 {
				continue
			}
			if item.SizeBytes == 0 {
				item.SizeBytes = t.MovieFile.Size
			}
			item.Resolution, item.Quality = resolution(t.MovieFile.Quality), t.MovieFile.Quality.Quality.Name
			if m := t.MovieFile.MediaInfo; m != nil {
				item.VideoCodec, item.DynamicRange = m.VideoCodec, m.VideoDynamicRangeType
			}
		} else {
			item.SizeBytes = t.Statistics.Size
			item.Seasons = []LibrarySeason{}
			seen := map[int]bool{}
			for _, season := range t.Seasons {
				if season.Number < 0 || season.Statistics.Size < 0 || seen[season.Number] {
					return nil, fmt.Errorf("invalid season statistics")
				}
				seen[season.Number] = true
				if season.Statistics.Size > 0 {
					item.Seasons = append(item.Seasons, LibrarySeason{Season: season.Number, SizeBytes: season.Statistics.Size})
				}
			}
			sort.Slice(item.Seasons, func(i, j int) bool { return item.Seasons[i].Season < item.Seasons[j].Season })
		}
		out = append(out, item)
	}
	return out, nil
}

// SetArchiveTags creates missing requested archive/4k tags before editing the
// title. Untag removes only archive; every other tag is preserved.
func (c *Client) SetArchiveTags(ctx context.Context, id int, archive, fourK bool) error {
	if !c.Apply {
		return fmt.Errorf("Changes are paused")
	}
	if id <= 0 || (c.Kind != "radarr" && c.Kind != "sonarr") {
		return fmt.Errorf("invalid title")
	}
	if !archive {
		return c.RemoveArchiveTag(ctx, Target{Kind: c.Kind, ID: id})
	}
	if err := c.Preflight(ctx); err != nil {
		return err
	}
	a, err := c.ensureArchiveTag(ctx, "archive", true)
	if err != nil {
		return err
	}
	k, err := c.ensureArchiveTag(ctx, FourKTag, fourK)
	if err != nil {
		return err
	}
	p, ids := "/movie/editor", "movieIds"
	if c.Kind == "sonarr" {
		p, ids = "/series/editor", "seriesIds"
	}
	add, remove := []int{}, []int{}
	for _, tag := range []struct {
		id int
		on bool
	}{{a, archive}, {k, fourK}} {
		if tag.id == 0 {
			continue
		}
		if tag.on {
			add = append(add, tag.id)
		} else {
			remove = append(remove, tag.id)
		}
	}
	for _, op := range []struct {
		action string
		tags   []int
	}{{"remove", remove}, {"add", add}} {
		if len(op.tags) == 0 {
			continue
		}
		if err := c.call(ctx, http.MethodPut, p, nil, map[string]any{ids: []int{id}, "tags": op.tags, "applyTags": op.action}, nil); err != nil {
			return err
		}
	}
	return nil
}

// TagCreateResult records the POST outcome separately from the title edit.
// Error strings are sanitized by call; upstream bodies never enter the audit.
type TagCreateResult struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	ID         int    `json:"id,omitempty"`
	Error      string `json:"error,omitempty"`
	ReadError  string `json:"read_error,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
}

// Re-read after one creation attempt; never trust the POST ID or retry a write.
func (c *Client) ensureArchiveTag(ctx context.Context, label string, needed bool) (int, error) {
	id, err := c.tagID(ctx, label)
	if err != nil || id != 0 || !needed {
		return id, err
	}
	createErr := c.call(ctx, http.MethodPost, "/tag", nil, map[string]string{"label": label}, nil)
	result := TagCreateResult{Kind: c.Kind, Label: label}
	var status *HTTPError
	rejected := false
	if createErr != nil {
		result.Error = createErr.Error()
		if errors.As(createErr, &status) {
			duplicate := status.Status == http.StatusConflict || status.Status == http.StatusBadRequest && status.Reason == "tag already exists"
			rejected = status.Status < 500 && !duplicate
		}
	}
	if rejected {
		err = fmt.Errorf("Tag creation rejected")
	} else {
		id, err = c.tagID(ctx, label)
		if err != nil {
			result.ReadError = err.Error()
			err = fmt.Errorf("Tags need attention")
		} else if id == 0 {
			err = fmt.Errorf("Tag creation unresolved")
		} else {
			result.ID = id
			if createErr != nil {
				result.ResolvedBy = "existing tag"
			}
		}
	}
	if c.TagCreateResult != nil {
		if auditErr := c.TagCreateResult(result); auditErr != nil {
			return 0, fmt.Errorf("Changes unavailable")
		}
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}
