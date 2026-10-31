package media

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

var _ mediadata.Source = (*Media)(nil)
var duskPoster = regexp.MustCompile(`^/[A-Za-z0-9_-]+\.(jpg|png|webp)$`)
var duskDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (m *Media) tvService(id string) (service, bool) {
	for _, s := range m.services {
		if s.id == id {
			return s, true
		}
	}
	return service{}, false
}
func (m *Media) TVCapabilities() mediadata.Capabilities {
	_, o := m.tvService("overseerr")
	_, s := m.tvService("sonarr")
	_, r := m.tvService("radarr")
	_, t := m.tvService("tautulli")
	return mediadata.Capabilities{Media: s || r || t, Upcoming: s || r, Browse: s || r, Discover: o, Search: o, Requests: o}
}
func validTVTitle(kind string, id int) bool {
	return (kind == "movie" || kind == "tv") && id > 0 && id <= 1000000000
}

type duskMetadata struct {
	ID                                                                      int
	TMDBID                                                                  int
	MediaType, Title, Name, Overview, PosterPath, ReleaseDate, FirstAirDate string
	Runtime                                                                 int
	VoteAverage                                                             float64
	Seasons                                                                 []struct{ SeasonNumber, EpisodeCount int }
	CreatedBy                                                               []struct{ Name string }
	Credits                                                                 struct {
		Cast []struct{ Name string }
		Crew []struct{ Name, Job string }
	}
	MediaInfo *struct {
		Status   *int
		Seasons  []struct{ SeasonNumber, Status int }
		Requests []struct {
			Status  int
			Is4k    bool
			Seasons []struct{ SeasonNumber, Status int }
		}
	}
	Images []struct{ CoverType, RemoteURL string }
}

func duskState(status int) string {
	switch status {
	case 1, 6:
		return "not_requested"
	case 2:
		return "pending"
	case 3:
		return "requested"
	case 4:
		return "partial"
	case 5:
		return "available"
	}
	return "unknown"
}
func duskRequestState(status int) string {
	switch status {
	case 1:
		return "pending"
	case 2, 5:
		return "requested"
	case 3:
		return "declined"
	case 4:
		return "failed"
	}
	return "unknown"
}
func (m *Media) tvTitle(r duskMetadata, kind string) mediadata.Title {
	t := mediadata.Title{ID: r.ID, Kind: kind, Title: m.text(r.Title), Overview: m.text(r.Overview), Seasons: []mediadata.Season{}, State: "not_requested", Availability: "not_in_plex"}
	date := r.ReleaseDate
	if kind == "tv" {
		t.Title = m.text(r.Name)
		date = r.FirstAirDate
	}
	if duskDate.MatchString(date) {
		t.Year = date[:4]
	}
	if duskPoster.MatchString(r.PosterPath) {
		t.Poster = "https://image.tmdb.org/t/p/w300_and_h450_face" + r.PosterPath
	}
	if r.MediaInfo != nil {
		if r.MediaInfo.Status == nil {
			t.State = "unknown"
		} else {
			t.State = duskState(*r.MediaInfo.Status)
		}
	}
	if r.MediaInfo != nil {
		for _, q := range r.MediaInfo.Requests {
			if !q.Is4k && t.State != "available" && t.State != "partial" {
				t.State = duskRequestState(q.Status)
				break
			}
		}
	}
	t.Availability = t.State
	if t.State == "not_requested" || t.State == "declined" {
		t.Availability = "not_in_plex"
	}
	if t.State == "pending" {
		t.Availability = "requested"
	}
	t.CanRequest = kind == "movie" && t.State == "not_requested"
	for _, s := range r.Seasons {
		if s.SeasonNumber < 1 || s.SeasonNumber > 1000 || len(t.Seasons) >= 100 {
			continue
		}
		state := "not_requested"
		if r.MediaInfo != nil {
			for _, v := range r.MediaInfo.Seasons {
				if v.SeasonNumber == s.SeasonNumber {
					state = duskState(v.Status)
				}
			}
			for _, q := range r.MediaInfo.Requests {
				if !q.Is4k {
					for _, v := range q.Seasons {
						if v.SeasonNumber == s.SeasonNumber && state != "available" && state != "partial" {
							state = duskRequestState(q.Status)
						}
					}
				}
			}
		}
		t.Seasons = append(t.Seasons, mediadata.Season{Number: s.SeasonNumber, Episodes: s.EpisodeCount, State: state, CanRequest: state == "not_requested" && s.EpisodeCount > 0})
	}
	if kind == "movie" && r.Runtime > 0 && r.Runtime < 1000 {
		t.Runtime = r.Runtime
	}
	for _, v := range r.Credits.Crew {
		if v.Job == "Director" && len(t.Directors) < 3 {
			t.Directors = append(t.Directors, m.text(v.Name))
		}
	}
	for _, v := range r.CreatedBy {
		if len(t.Creators) < 3 {
			t.Creators = append(t.Creators, m.text(v.Name))
		}
	}
	for _, v := range r.Credits.Cast {
		if len(t.Cast) < 6 {
			t.Cast = append(t.Cast, m.text(v.Name))
		}
	}
	if r.VoteAverage >= 0 && r.VoteAverage <= 10 {
		t.Rating = &r.VoteAverage
	}
	return t
}
func (m *Media) tvPage(ctx context.Context, s service, path string, q url.Values) ([]mediadata.Title, error) {
	var p struct{ Results []duskMetadata }
	if err := m.get(ctx, s, path, q, &p, true); err != nil {
		return nil, err
	}
	if p.Results == nil || len(p.Results) > 100 {
		return nil, errors.New("incomplete discovery response")
	}
	out := []mediadata.Title{}
	seen := map[string]bool{}
	for _, r := range p.Results {
		if !validTVTitle(r.MediaType, r.ID) {
			continue
		}
		k := fmt.Sprint(r.MediaType, ":", r.ID)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, m.tvTitle(r, r.MediaType))
		if len(out) == 40 {
			break
		}
	}
	return out, nil
}
func (m *Media) TVRead(ctx context.Context, operation, kind string, id int, query string) (any, error) {
	if operation == "refresh" {
		m.cache = map[string]cached{}
		m.cacheBytes = 0
		operation = "discover"
	}
	if operation == "media" {
		return m.tvMedia(ctx), nil
	}
	if operation == "browse" {
		return m.tvBrowse(ctx)
	}
	s, ok := m.tvService("overseerr")
	if !ok {
		return nil, errors.New("metadata source is not configured")
	}
	switch operation {
	case "title":
		if !validTVTitle(kind, id) {
			return nil, errors.New("invalid title")
		}
		var r duskMetadata
		if err := m.get(ctx, s, "/api/v1/"+kind+"/"+strconv.Itoa(id), nil, &r, true); err != nil {
			return nil, err
		}
		if r.ID != id {
			return nil, errors.New("invalid title response")
		}
		return mediadata.Detail{Title: m.tvTitle(r, kind)}, nil
	case "search":
		query = strings.TrimSpace(query)
		if query == "" || len([]rune(query)) > 80 || strings.IndexFunc(query, unicode.IsControl) >= 0 {
			return nil, errors.New("invalid search")
		}
		titles, err := m.tvPage(ctx, s, "/api/v1/search", url.Values{"query": {query}, "page": {"1"}})
		return mediadata.Titles{Titles: titles, Hidden: []string{}}, err
	case "discover", "similar":
		path := "/api/v1/discover/trending"
		if operation == "similar" {
			if !validTVTitle(kind, id) {
				return nil, errors.New("invalid title")
			}
			path = "/api/v1/" + kind + "/" + strconv.Itoa(id) + "/recommendations"
		}
		titles, err := m.tvPage(ctx, s, path, url.Values{"page": {"1"}})
		if err != nil {
			return nil, err
		}
		status := "ok"
		if len(titles) == 0 {
			status = "empty"
		}
		return mediadata.Discovery{Status: status, CheckedAt: m.host.Now().UTC().Format(time.RFC3339), LibraryStatus: "unavailable", Partial: true, Titles: titles, Seeds: []mediadata.Seed{}, Recent: []mediadata.Title{}, Watchlist: []mediadata.Title{}, Hidden: []string{}}, nil
	}
	return nil, errors.New("unknown media operation")
}
func (m *Media) tvBrowse(ctx context.Context) (mediadata.Titles, error) {
	out := mediadata.Titles{Titles: []mediadata.Title{}, Hidden: []string{}}
	configured, failed := 0, 0
	for _, s := range m.services {
		kind, path := "movie", "/api/v3/movie"
		if s.id == "sonarr" {
			kind, path = "tv", "/api/v3/series"
		} else if s.id != "radarr" {
			continue
		}
		configured++
		var rows []duskMetadata
		if m.get(ctx, s, path, nil, &rows, true) != nil || rows == nil {
			failed++
			continue
		}
		for _, r := range rows {
			if r.TMDBID <= 0 {
				continue
			}
			r.ID = r.TMDBID
			if kind == "tv" {
				r.Name = r.Title
			}
			t := m.tvTitle(r, kind)
			out.Titles = append(out.Titles, t)
			if len(out.Titles) >= 100 {
				return out, nil
			}
		}
	}
	if configured == 0 || failed == configured {
		return out, errors.New("library is unavailable")
	}
	return out, nil
}
