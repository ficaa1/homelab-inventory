package archivetier

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// SeedImport joins a download's import history to current file identities.
// DroppedPath is the exact source, never a basename/size-only guess.
type SeedImport struct {
	Kind      string `json:"kind"`
	ID        int    `json:"id"`
	FileID    int    `json:"fileId"`
	Source    string `json:"source"`
	Path      string `json:"path"`
	Root      string `json:"root"`
	TitlePath string `json:"titlePath"`
	Size      int64  `json:"size"`
}

// SeedLibraryReferences is independent of download history: missing history
// must never make the client's only library copy eligible for removal.
func (c *Client) SeedLibraryReferences(ctx context.Context) ([]SeedImport, error) {
	if err := c.Preflight(ctx); err != nil {
		return nil, err
	}
	endpoint := "/movie"
	if c.Kind == "sonarr" {
		endpoint = "/series"
	}
	type record struct {
		ID        int    `json:"id"`
		Path      string `json:"path"`
		Root      string `json:"rootFolderPath"`
		HasFile   *bool  `json:"hasFile"`
		MovieFile *File  `json:"movieFile"`
	}
	ts, err := list[record](ctx, c, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if err = uniqueIDs(ts, func(t record) int { return t.ID }); err != nil {
		return nil, err
	}
	out := []SeedImport{}
	for _, t := range ts {
		if t.Path == "" || t.Root == "" {
			return nil, fmt.Errorf("library ownership inventory incomplete")
		}
		t.Path, t.Root = seedDir(t.Path), seedDir(t.Root)
		out = append(out, SeedImport{Kind: c.Kind, ID: t.ID, TitlePath: t.Path, Root: t.Root})
		fs := []File{}
		if c.Kind == "sonarr" {
			fs, err = c.episodeFiles(ctx, t.ID)
		} else {
			if t.HasFile == nil || (*t.HasFile && t.MovieFile == nil) {
				return nil, fmt.Errorf("movie file ownership unknown")
			}
			if t.MovieFile != nil {
				fs = append(fs, *t.MovieFile)
			}
		}
		if err != nil {
			return nil, err
		}
		for _, f := range fs {
			if f.ID <= 0 || f.Path == "" || f.Size <= 0 {
				return nil, fmt.Errorf("library file ownership incomplete")
			}
			out = append(out, SeedImport{Kind: c.Kind, ID: t.ID, FileID: f.ID, Path: f.Path, TitlePath: t.Path, Root: t.Root, Size: f.Size})
		}
	}
	return out, nil
}

func (c *Client) SeedImports(ctx context.Context, hash string) ([]SeedImport, error) {
	if err := c.Preflight(ctx); err != nil {
		return nil, err
	}
	// Arr stores qBittorrent download IDs in upper case and matches the
	// filter case-sensitively: a lower-case hash finds no history at all.
	hs, err := paged(ctx, c, "/history", url.Values{"downloadId": {strings.ToUpper(hash)}, "sortKey": {"date"}, "sortDirection": {"descending"}}, func(h HistoryItem) int { return h.ID })
	if err != nil {
		return nil, err
	}
	out := []SeedImport{}
	seen := map[string]bool{}
	for _, h := range hs {
		if !strings.EqualFold(h.DownloadID, hash) || h.EventType != "downloadFolderImported" {
			continue
		}
		id := h.MovieID
		endpoint := "/movie/"
		if c.Kind == "sonarr" {
			id = h.SeriesID
			endpoint = "/series/"
		}
		fileID, e := strconv.Atoi(h.Data["fileId"])
		if e != nil || fileID <= 0 || id <= 0 || h.Data["droppedPath"] == "" {
			return nil, fmt.Errorf("import history lacks exact source/file identity")
		}
		var title struct {
			ID        int    `json:"id"`
			Path      string `json:"path"`
			Root      string `json:"rootFolderPath"`
			MovieFile *File  `json:"movieFile"`
		}
		if e = c.get(ctx, endpoint+strconv.Itoa(id), nil, &title); e != nil {
			return nil, e
		}
		if title.ID != id || title.Path == "" || title.Root == "" {
			return nil, fmt.Errorf("incomplete library title")
		}
		title.Path, title.Root = seedDir(title.Path), seedDir(title.Root)
		fs := []File{}
		if c.Kind == "sonarr" {
			fs, e = c.episodeFiles(ctx, id)
		} else if title.MovieFile != nil {
			fs = append(fs, *title.MovieFile)
		}
		if e != nil {
			return nil, e
		}
		for _, f := range fs {
			if f.ID != fileID {
				continue
			}
			if f.Path == "" || f.Size <= 0 {
				return nil, fmt.Errorf("incomplete library file")
			}
			key := fmt.Sprintf("%d:%d:%s", id, fileID, h.Data["droppedPath"])
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, SeedImport{c.Kind, id, fileID, h.Data["droppedPath"], f.Path, title.Root, title.Path, f.Size})
		}
	}
	return out, nil
}

// MoveSeedLibrary moves one item, with no tag/monitor/profile changes.
// The editor returns titles, not the queued BulkMove command's ID.
func (c *Client) MoveSeedLibrary(ctx context.Context, id int, root string) error {
	if err := c.Preflight(ctx); err != nil {
		return err
	}
	field, endpoint := "movieIds", "/movie/editor"
	if c.Kind == "sonarr" {
		field, endpoint = "seriesIds", "/series/editor"
	}
	return c.call(ctx, "PUT", endpoint, nil, map[string]any{field: []int{id}, "rootFolderPath": root, "moveFiles": true, "deleteFiles": true}, nil)
}

type SeedCommand struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Body   struct {
		Destination string `json:"destinationRootFolder"`
		Movies      []struct {
			ID     int    `json:"movieId"`
			Source string `json:"sourcePath"`
		} `json:"movies"`
		Series []struct {
			ID     int    `json:"seriesId"`
			Source string `json:"sourcePath"`
		} `json:"series"`
	} `json:"body"`
}

func (c *Client) SeedCommands(ctx context.Context) ([]SeedCommand, error) {
	cs, err := list[SeedCommand](ctx, c, "/command", nil)
	if err != nil {
		return nil, err
	}
	if err = uniqueIDs(cs, func(c SeedCommand) int { return c.ID }); err != nil {
		return nil, err
	}
	return cs, nil
}

// seedDir drops a trailing slash from a title or root folder. Sonarr and
// Radarr keep whatever was typed: live libraries hold "/data/media/series/Twin
// Peaks/" beside "/data/media/series", which the clean-path containment checks
// would otherwise reject, refusing every torrent.
func seedDir(p string) string {
	if len(p) > 1 {
		return strings.TrimRight(p, "/")
	}
	return p
}
