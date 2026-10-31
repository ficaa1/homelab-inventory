// Package letterboxd imports only movie preferences from a private CSV export.
// Profile, reviews, comments and following data are deliberately never read.
package letterboxd

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

type Film struct {
	Slug           string                 `json:"slug,omitempty"`
	URI            string                 `json:"-"`
	IdentitySource string                 `json:"identity_source,omitempty"`
	Title          string                 `json:"title"`
	Year           string                 `json:"year"`
	Date           string                 `json:"date"`
	Rating         *float64               `json:"rating,omitempty"`
	Liked          bool                   `json:"liked"`
	Watched        bool                   `json:"watched"`
	Watchlist      bool                   `json:"watchlist"`
	Match          *tastedata.BrowseTitle `json:"match,omitempty"`
}
type Profile struct {
	SyncUser   string            `json:"sync_user,omitempty"`
	Sync       *SyncStatus       `json:"sync,omitempty"`
	RSSSeen    map[string]string `json:"rss_seen,omitempty"`
	Owner      int               `json:"owner"`
	Version    int               `json:"version"`
	ImportedAt time.Time         `json:"imported_at"`
	Ratings    int               `json:"ratings"`
	Watched    int               `json:"watched"`
	Watchlist  int               `json:"watchlist"`
	Likes      int               `json:"likes"`
	Unmatched  int               `json:"unmatched"`
	Films      []Film            `json:"films"`
}

func Normalize(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}
func Parse(data []byte, now time.Time) (Profile, error) {
	p := Profile{Version: 1, ImportedAt: now.UTC(), Films: []Film{}}
	if len(data) > 16<<20 {
		return p, fmt.Errorf("export exceeds 16 MiB")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return p, fmt.Errorf("invalid export ZIP")
	}
	allowed := map[string]bool{"ratings.csv": true, "watched.csv": true, "watchlist.csv": true, "likes/films.csv": true}
	seen := map[string]bool{}
	films := map[string]*Film{}
	for _, file := range z.File {
		if !allowed[file.Name] {
			continue
		}
		if seen[file.Name] || file.UncompressedSize64 > 4<<20 {
			return p, fmt.Errorf("duplicate or oversized preference file")
		}
		seen[file.Name] = true
		f, e := file.Open()
		if e != nil {
			return p, fmt.Errorf("unreadable export")
		}
		body, e := io.ReadAll(io.LimitReader(f, (4<<20)+1))
		f.Close()
		if e != nil || len(body) > 4<<20 {
			return p, fmt.Errorf("oversized preference file")
		}
		rows, e := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(body), "\ufeff"))).ReadAll()
		if e != nil || len(rows) == 0 || len(rows) > 10001 {
			return p, fmt.Errorf("invalid preference CSV")
		}
		columns := map[string]int{}
		for i, c := range rows[0] {
			columns[c] = i
		}
		for _, c := range []string{"Name", "Year", "Date"} {
			if _, ok := columns[c]; !ok {
				return p, fmt.Errorf("missing CSV column")
			}
		}
		if file.Name == "ratings.csv" {
			if _, ok := columns["Rating"]; !ok {
				return p, fmt.Errorf("missing rating column")
			}
		}
		for _, row := range rows[1:] {
			name, year, date := strings.TrimSpace(row[columns["Name"]]), row[columns["Year"]], row[columns["Date"]]
			y, e := strconv.Atoi(year)
			if name == "" || len([]rune(name)) > 200 || e != nil || y < 1870 || y > 2100 {
				return p, fmt.Errorf("invalid film identity")
			}
			if _, e = time.Parse("2006-01-02", date); e != nil {
				return p, fmt.Errorf("invalid preference date")
			}
			k := Normalize(name) + ":" + year
			v := films[k]
			if v == nil {
				v = &Film{Title: name, Year: year, Date: date}
				films[k] = v
			}
			if col, ok := columns["Letterboxd URI"]; ok {
				v.URI = row[col]
			}
			if date > v.Date {
				v.Date = date
			}
			switch file.Name {
			case "ratings.csv":
				rating, e := strconv.ParseFloat(row[columns["Rating"]], 64)
				if e != nil || rating < 0.5 || rating > 5 || rating*2 != float64(int(rating*2)) {
					return p, fmt.Errorf("invalid star rating")
				}
				v.Rating = &rating
				v.Watched = true
			case "watched.csv":
				v.Watched = true
			case "watchlist.csv":
				v.Watchlist = true
			case "likes/films.csv":
				v.Liked = true
			}
		}
	}
	if !seen["ratings.csv"] || !seen["watched.csv"] || !seen["watchlist.csv"] {
		return p, fmt.Errorf("export is missing ratings, watched or watchlist CSV")
	}
	for _, v := range films {
		if v.Rating != nil {
			p.Ratings++
		}
		if v.Watched {
			p.Watched++
		}
		if v.Watchlist {
			p.Watchlist++
		}
		if v.Liked {
			p.Likes++
		}
		p.Films = append(p.Films, *v)
	}
	sort.Slice(p.Films, func(i, j int) bool {
		if p.Films[i].Date == p.Films[j].Date {
			return p.Films[i].Title < p.Films[j].Title
		}
		return p.Films[i].Date > p.Films[j].Date
	})
	return p, nil
}

type Searcher interface {
	Search(context.Context, string) ([]tastedata.BrowseTitle, error)
}

// Match requires an exact title (including public original-title aliases) and
// year, and exactly one catalog identity. Ambiguous/remade films stay unresolved.
func Match(f Film, rows []tastedata.BrowseTitle) *tastedata.BrowseTitle {
	movies := map[int]tastedata.BrowseTitle{}
	shows := map[int]tastedata.BrowseTitle{}
	for _, r := range rows {
		if r.Year != f.Year || r.ID <= 0 {
			continue
		}
		names := append([]string{r.Title}, r.MatchNames...)
		for _, name := range names {
			if Normalize(name) == Normalize(f.Title) {
				if r.Kind == "movie" {
					movies[r.ID] = r
				} else if r.Kind == "tv" {
					shows[r.ID] = r
				}
				break
			}
		}
	}
	if len(movies) > 0 {
		if len(movies) != 1 {
			return nil
		}
		for _, r := range movies {
			return &r
		}
	}
	if len(shows) == 1 {
		for _, r := range shows {
			return &r
		}
	}
	return nil
}
func Resolve(ctx context.Context, p Profile, s Searcher) (Profile, error) {
	out := append([]Film{}, p.Films...)
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	failures := 0
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			call, cancel := context.WithTimeout(ctx, 16*time.Second)
			defer cancel()
			rows, e := s.Search(call, out[i].Title)
			if e != nil {
				mu.Lock()
				failures++
				mu.Unlock()
				return
			}
			out[i].Match = Match(out[i], rows)
			if out[i].Match != nil {
				out[i].IdentitySource = "title_year"
			} else {
				out[i].Match = canonicalMatch(ctx, out[i], s)
				if out[i].Match != nil {
					out[i].IdentitySource = "letterboxd_tmdb"
				}
			}
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil || failures > 0 {
		return p, fmt.Errorf("catalog lookup failed; previous import was kept")
	}
	p.Films = out
	p.Unmatched = 0
	for _, f := range out {
		if f.Match == nil {
			p.Unmatched++
		}
	}
	return p, nil
}
func writeProfile(path string, p Profile) error {
	b, e := json.MarshalIndent(p, "", "  ")
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(path, b, 0600)
}
func Load(path string) (*Profile, error) {
	if path == "" {
		return nil, nil
	}
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, fmt.Errorf("Letterboxd import unavailable")
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if e != nil || len(b) > 16<<20 {
		return nil, fmt.Errorf("invalid Letterboxd import")
	}
	return decodeProfile(b)
}
func decodeProfile(b []byte) (*Profile, error) {
	if len(b) > 16<<20 {
		return nil, fmt.Errorf("invalid Letterboxd import")
	}
	var p Profile
	if json.Unmarshal(b, &p) != nil || p.Version != 1 || p.Owner <= 0 || p.ImportedAt.IsZero() || len(p.Films) > 10000 {
		return nil, fmt.Errorf("invalid Letterboxd import")
	}
	for _, f := range p.Films {
		y, e := strconv.Atoi(f.Year)
		if f.Title == "" || e != nil || y < 1870 || y > 2100 {
			return nil, fmt.Errorf("invalid Letterboxd identity")
		}
		if f.Rating != nil && (*f.Rating < 0.5 || *f.Rating > 5) {
			return nil, fmt.Errorf("invalid Letterboxd rating")
		}
		if f.Match != nil && (f.Match.ID <= 0 || (f.Match.Kind != "movie" && f.Match.Kind != "tv") || (f.Match.Year != f.Year && f.IdentitySource != "letterboxd_tmdb")) {
			return nil, fmt.Errorf("invalid Letterboxd match")
		}
	}
	return &p, nil
}
