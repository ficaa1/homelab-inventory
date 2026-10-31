package media

import (
	"context"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func discoveryText(s string, limit int) string {
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit])
	}
	return s
}

var posterPath = regexp.MustCompile(`^/[A-Za-z0-9_-]+\.(jpg|png|webp)$`)
var releaseYear = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (m *Media) tasteRead(ctx context.Context, path string, out any) error {
	s, ok := m.tvService("overseerr")
	if !ok {
		return fmt.Errorf("metadata source unavailable")
	}
	u, e := url.Parse(path)
	if e != nil {
		return fmt.Errorf("invalid metadata path")
	}
	return m.get(ctx, s, "/api/v1"+u.Path, u.Query(), out, false)
}
func (m *Media) tasteCall(ctx context.Context, command string, q url.Values, out any) error {
	s, ok := m.tvService("tautulli")
	if !ok {
		return fmt.Errorf("history source unavailable")
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("cmd", command)
	return m.get(ctx, s, "/api/v2", q, out, true)
}
func (t *Media) WatchHistory(ctx context.Context, owner int) ([]tastedata.WatchTitle, error) {
	if owner <= 0 {
		return nil, fmt.Errorf("watch owner not configured")
	}
	out := []tastedata.WatchTitle{}
	for start := 0; start < 10000; start += 1000 {
		var page struct {
			Data *[]struct {
				UserID  number `json:"user_id"`
				Kind    string `json:"media_type"`
				Title   string `json:"title"`
				Show    string `json:"grandparent_title"`
				Key     number `json:"rating_key"`
				ShowKey number `json:"grandparent_rating_key"`
				Season  number `json:"parent_media_index"`
				Episode number `json:"media_index"`
				Year    number `json:"year"`
				Percent number `json:"percent_complete"`
				Seconds number `json:"play_duration"`
				Date    number `json:"date"`
			} `json:"data"`
		}
		q := url.Values{"user_id": {strconv.Itoa(owner)}, "length": {"1000"}, "start": {strconv.Itoa(start)}, "order_column": {"date"}, "order_dir": {"desc"}, "grouping": {"0"}}
		if err := t.tasteCall(ctx, "get_history", q, &page); err != nil {
			return nil, err
		}
		if page.Data == nil || len(*page.Data) > 1000 {
			return nil, fmt.Errorf("invalid watch history")
		}
		for _, r := range *page.Data {
			// Verify the owner even when upstream ignores the query parameter.
			if int(r.UserID) != owner {
				continue
			}
			v := tastedata.WatchTitle{Kind: r.Kind, Title: r.Title, Key: strconv.Itoa(int(r.Key)), Year: int(r.Year), Percent: int(r.Percent), Seconds: int(r.Seconds), At: time.Unix(int64(r.Date), 0)}
			if r.Kind == "episode" {
				v.Kind = "tv"
				v.Title = r.Show
				v.Key = strconv.Itoa(int(r.ShowKey))
				v.Year = 0
				v.Season, v.Episode = int(r.Season), int(r.Episode)
			}
			if (v.Kind == "tv" || v.Kind == "movie") && v.Title != "" && v.Key != "0" {
				out = append(out, v)
			}
		}
		if len(*page.Data) < 1000 {
			break
		}
	}
	return out, nil
}

// WatchIdentity uses Plex's TMDB GUID, never a fuzzy title search.
func (t *Media) WatchIdentity(ctx context.Context, v tastedata.WatchTitle) (tastedata.BrowseTitle, error) {
	key, err := strconv.Atoi(v.Key)
	if err != nil || key <= 0 {
		return tastedata.BrowseTitle{}, fmt.Errorf("invalid Plex key")
	}
	var r struct {
		Kind  string   `json:"media_type"`
		Title string   `json:"title"`
		Year  number   `json:"year"`
		GUIDs []string `json:"guids"`
	}
	if err = t.tasteCall(ctx, "get_metadata", url.Values{"rating_key": {strconv.Itoa(key)}}, &r); err != nil {
		return tastedata.BrowseTitle{}, err
	}
	kind := r.Kind
	if kind == "show" {
		kind = "tv"
	}
	if kind != v.Kind || strings.TrimSpace(r.Title) == "" {
		return tastedata.BrowseTitle{}, fmt.Errorf("watch identity unavailable")
	}
	for _, guid := range r.GUIDs {
		if !strings.HasPrefix(guid, "tmdb://") {
			continue
		}
		id, e := strconv.Atoi(strings.TrimPrefix(guid, "tmdb://"))
		if e == nil && id > 0 && id < 1000000000 {
			return tastedata.BrowseTitle{ID: id, Kind: kind, Title: discoveryText(r.Title, 160), Year: strconv.Itoa(int(r.Year))}, nil
		}
	}
	return tastedata.BrowseTitle{}, fmt.Errorf("TMDB identity unavailable")
}

func (o *Media) Recommendations(ctx context.Context, kind string, id int) ([]tastedata.SuggestedTitle, error) {
	return o.RecommendationsPage(ctx, kind, id, 1)
}

// RecommendationsPage reads one page (1-3) of TMDB recommendations for a title.
func (o *Media) RecommendationsPage(ctx context.Context, kind string, id, page int) ([]tastedata.SuggestedTitle, error) {
	if (kind != "movie" && kind != "tv") || id <= 0 || id >= 1000000000 || page < 1 || page > 3 {
		return nil, fmt.Errorf("invalid recommendation seed")
	}
	var rows struct{ Results *[]suggestedRow }
	if err := o.tasteRead(ctx, "/"+kind+"/"+strconv.Itoa(id)+"/recommendations?page="+strconv.Itoa(page), &rows); err != nil {
		return nil, err
	}
	if rows.Results == nil || len(*rows.Results) > 100 {
		return nil, fmt.Errorf("invalid recommendations")
	}
	out := []tastedata.SuggestedTitle{}
	for _, r := range *rows.Results {
		if r.MediaType == "" {
			r.MediaType = kind
		}
		if v, ok := r.suggested(kind); ok {
			out = append(out, v)
		}
		if len(out) == 20 {
			break
		}
	}
	return out, nil
}

// suggestedRow is TMDB's list-row shape, shared by recommendations and credits.
type suggestedRow struct {
	ID                                                                      int
	MediaType, Title, Name, Overview, PosterPath, ReleaseDate, FirstAirDate string
	VoteAverage                                                             float64
	VoteCount                                                               int
	Adult                                                                   bool
	MediaInfo                                                               *struct{ Status int }
}

func (r suggestedRow) suggested(kind string) (tastedata.SuggestedTitle, bool) {
	if r.MediaType != kind || r.ID <= 0 || r.ID >= 1000000000 {
		return tastedata.SuggestedTitle{}, false
	}
	title, date := r.Title, r.ReleaseDate
	if kind == "tv" {
		title, date = r.Name, r.FirstAirDate
	}
	if strings.TrimSpace(title) == "" {
		return tastedata.SuggestedTitle{}, false
	}
	v := tastedata.SuggestedTitle{BrowseTitle: tastedata.BrowseTitle{ID: r.ID, Kind: kind, Title: title, Overview: r.Overview}, Rating: r.VoteAverage, Votes: r.VoteCount, Adult: r.Adult, Date: date, Availability: "not_in_plex"}
	v.Title, v.Overview = discoveryText(v.Title, 160), discoveryText(v.Overview, 1600)
	if releaseYear.MatchString(date) {
		v.Year = date[:4]
	}
	if posterPath.MatchString(r.PosterPath) {
		v.Poster = "https://image.tmdb.org/t/p/w300_and_h450_face" + r.PosterPath
	}
	if r.MediaInfo != nil {
		switch r.MediaInfo.Status {
		case 5:
			v.Availability = "available"
		case 4:
			v.Availability = "partial"
		case 2, 3:
			v.Availability = "requested"
		}
	}
	return v, true
}

// RecommendationAvailability reads Overseerr's scanned standard-media catalog
// once per feed build. TMDB recommendation responses omit mediaInfo entirely.
// Incomplete pagination is an error, never evidence a title is absent in Plex.
func (o *Media) RecommendationAvailability(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	seen := map[int]bool{}
	total := -1
	for skip := 0; skip < 10000; skip += 100 {
		var page struct {
			PageInfo struct{ Results *int }
			Results  *[]struct {
				ID, TMDBID, Status int
				MediaType          string
			}
		}
		if err := o.tasteRead(ctx, "/media?take=100&skip="+strconv.Itoa(skip), &page); err != nil {
			return nil, err
		}
		if page.PageInfo.Results == nil || page.Results == nil || len(*page.Results) > 100 || *page.PageInfo.Results < 0 || (total >= 0 && total != *page.PageInfo.Results) {
			return nil, fmt.Errorf("incomplete library catalog")
		}
		total = *page.PageInfo.Results
		for _, r := range *page.Results {
			if r.ID <= 0 || seen[r.ID] || r.TMDBID <= 0 || (r.MediaType != "movie" && r.MediaType != "tv") {
				return nil, fmt.Errorf("invalid library identity")
			}
			seen[r.ID] = true
			state := "unknown"
			switch r.Status {
			case 1, 6:
				state = "not_in_plex"
			case 2, 3:
				state = "requested"
			case 4:
				state = "partial"
			case 5:
				state = "available"
			}
			out[fmt.Sprintf("%s:%d", r.MediaType, r.TMDBID)] = state
		}
		if len(seen) == total {
			return out, nil
		}
		if len(*page.Results) == 0 {
			return nil, fmt.Errorf("incomplete library catalog")
		}
	}
	return nil, fmt.Errorf("library catalog exceeds limit")
}
func (o *Media) TitleFacts(ctx context.Context, kind string, id int) (tastedata.TitleFacts, error) {
	if (kind != "movie" && kind != "tv") || id <= 0 || id >= 1000000000 {
		return tastedata.TitleFacts{}, fmt.Errorf("invalid title")
	}
	type named struct {
		ID   int
		Name string
	}
	var r struct {
		suggestedRow
		Credits struct {
			Crew []struct {
				ID        int
				Name, Job string
			}
			Cast []struct {
				ID, Order int
				Name      string
			}
		}
		CreatedBy           []named
		Keywords            []named
		Genres              []named
		ProductionCountries []struct {
			ISO string `json:"iso_3166_1"`
		}
		OriginalLanguage string
		Seasons          []struct{ SeasonNumber, EpisodeCount int }
		LastEpisodeToAir *struct{ SeasonNumber, EpisodeNumber int }
	}
	if err := o.tasteRead(ctx, "/"+kind+"/"+strconv.Itoa(id), &r); err != nil {
		return tastedata.TitleFacts{}, err
	}
	r.MediaType = kind
	t, ok := r.suggested(kind)
	if !ok || t.ID != id {
		return tastedata.TitleFacts{}, fmt.Errorf("invalid title response")
	}
	f := tastedata.TitleFacts{SuggestedTitle: t, People: []tastedata.TastePerson{}, Keywords: []string{}, Genres: []string{}, Countries: []string{}, Language: discoveryText(r.OriginalLanguage, 8)}
	person := func(id int, name, role string) {
		if id > 0 && name != "" && len(f.People) < 60 {
			f.People = append(f.People, tastedata.TastePerson{ID: id, Name: discoveryText(name, 120), Role: role})
		}
	}
	for _, c := range r.CreatedBy {
		person(c.ID, c.Name, "dir")
	}
	for _, c := range r.Credits.Crew {
		switch c.Job {
		case "Director":
			person(c.ID, c.Name, "dir")
		case "Screenplay", "Writer", "Story", "Novel":
			person(c.ID, c.Name, "wri")
		case "Director of Photography":
			person(c.ID, c.Name, "dop")
		case "Original Music Composer":
			person(c.ID, c.Name, "mus")
		}
	}
	for _, c := range r.Credits.Cast {
		if c.Order < 5 {
			person(c.ID, c.Name, "cast")
		}
	}
	for _, k := range r.Keywords[:min(len(r.Keywords), 40)] {
		f.Keywords = append(f.Keywords, discoveryText(k.Name, 80))
	}
	for _, g := range r.Genres {
		f.Genres = append(f.Genres, discoveryText(g.Name, 40))
	}
	for _, c := range r.ProductionCountries {
		f.Countries = append(f.Countries, discoveryText(c.ISO, 4))
	}
	if last := r.LastEpisodeToAir; kind == "tv" && last != nil {
		for _, se := range r.Seasons {
			switch {
			case se.SeasonNumber < 1:
			case se.SeasonNumber < last.SeasonNumber:
				f.Episodes += se.EpisodeCount
			case se.SeasonNumber == last.SeasonNumber:
				f.Episodes += last.EpisodeNumber
			}
		}
	}
	return f, nil
}

// PersonWork lists the titles a person directed, wrote, shot or created.
func (o *Media) PersonWork(ctx context.Context, person int) ([]tastedata.SuggestedTitle, error) {
	if person <= 0 || person >= 1000000000 {
		return nil, fmt.Errorf("invalid person")
	}
	var r struct {
		Crew *[]struct {
			suggestedRow
			Job string
		}
	}
	if err := o.tasteRead(ctx, "/person/"+strconv.Itoa(person)+"/combined_credits", &r); err != nil {
		return nil, err
	}
	if r.Crew == nil {
		return nil, fmt.Errorf("invalid person credits")
	}
	out := []tastedata.SuggestedTitle{}
	seen := map[string]bool{}
	for _, c := range *r.Crew {
		switch c.Job {
		case "Director", "Screenplay", "Writer", "Director of Photography", "Creator":
		default:
			continue
		}
		v, ok := c.suggested(c.MediaType)
		k := v.Kind + ":" + strconv.Itoa(v.ID)
		if ok && !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
		if len(out) == 300 {
			break
		}
	}
	return out, nil
}

func (m *Media) Search(ctx context.Context, q string) ([]tastedata.BrowseTitle, error) {
	v, e := m.TVRead(ctx, "search", "", 0, q)
	if e != nil {
		return nil, e
	}
	rows := v.(mediadata.Titles)
	out := []tastedata.BrowseTitle{}
	for _, t := range rows.Titles {
		out = append(out, tastedata.BrowseTitle{ID: t.ID, Kind: t.Kind, Title: t.Title, Year: t.Year, Overview: t.Overview, Poster: t.Poster})
	}
	return out, nil
}
func (m *Media) Details(ctx context.Context, kind string, id int) (tastedata.TitleDetail, error) {
	f, e := m.TitleFacts(ctx, kind, id)
	return tastedata.TitleDetail{BrowseTitle: f.BrowseTitle}, e
}
