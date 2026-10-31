package engine

import (
	"errors"
	"strconv"
)

var ErrNotConfigured = errors.New("source not configured")

func TitleKey(kind string, id int) string { return kind + ":" + strconv.Itoa(id) }

// LibraryFile carries current Arr identity, never a caller-supplied path.
type LibraryFile struct {
	ID        int    `json:"id"`
	SeriesID  int    `json:"seriesId"`
	Season    int    `json:"seasonNumber"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	MediaInfo struct {
		VideoCodec string `json:"videoCodec"`
	} `json:"mediaInfo"`
	Quality struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality"`
}
type LibraryTitle struct {
	Kind      string      `json:"-"`
	ID        int         `json:"id"`
	Title     string      `json:"title"`
	Path      string      `json:"path"`
	TMDBID    int         `json:"tmdbId"`
	IMDBID    string      `json:"imdbId"`
	MovieFile LibraryFile `json:"movieFile"`
}

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
	Phase      string `json:"phase,omitempty"`
}
