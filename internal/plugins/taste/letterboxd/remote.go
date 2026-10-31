package letterboxd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,39}$`)
var filmPathPattern = regexp.MustCompile(`^/film/[A-Za-z0-9_-]+/$`)

type remoteError struct{ state string }

func (e remoteError) Error() string { return "Letterboxd " + e.state }
func remoteState(e error) string {
	if v, ok := e.(remoteError); ok {
		return v.state
	}
	return "unavailable"
}

type Observation struct {
	Film                   Film
	Kind                   string
	TMDB                   int
	GUID, Fingerprint      string
	Published              time.Time
	RatingKnown, LikeKnown bool
}
type Page struct {
	Rows        []Observation
	Total, Next int
}
type Scan struct {
	Rows     []Observation
	Total    int
	Complete bool
	Err      error
}
type PublicSource interface {
	Feed(context.Context) ([]Observation, error)
	Collection(context.Context, string) Scan
}
type Remote struct {
	Username string
	Endpoint string
	client   *http.Client
	fetch    func(context.Context, string) ([]byte, error)
}

// NewRemote uses only the host-provided guarded client and configured authority.
func NewRemote(username, endpoint string, client *http.Client) *Remote {
	return &Remote{Username: username, Endpoint: strings.TrimRight(endpoint, "/"), client: client}
}
func (r *Remote) get(ctx context.Context, path string) ([]byte, error) {
	if !usernamePattern.MatchString(r.Username) || !strings.HasPrefix(path, "/"+r.Username+"/") || strings.ContainsAny(path, "?#") {
		return nil, remoteError{"invalid"}
	}
	if r.fetch != nil {
		return r.fetch(ctx, path)
	}
	req, e := http.NewRequestWithContext(ctx, "GET", r.Endpoint+path, nil)
	if e != nil {
		return nil, remoteError{"invalid"}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	res, e := r.client.Do(req)
	if e != nil {
		return nil, remoteError{"unavailable"}
	}
	defer res.Body.Close()
	if res.StatusCode == 403 || res.StatusCode == 429 {
		return nil, remoteError{"blocked"}
	}
	if res.StatusCode != 200 {
		return nil, remoteError{"unavailable"}
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (3<<20)+1))
	if e != nil || len(b) > 3<<20 {
		return nil, remoteError{"invalid"}
	}
	return b, nil
}
func (r *Remote) Feed(ctx context.Context) ([]Observation, error) {
	b, e := r.get(ctx, "/"+r.Username+"/rss/")
	if e != nil {
		return nil, e
	}
	return parseFeed(b, r.Username)
}
func parseFeed(b []byte, user string) ([]Observation, error) {
	var raw struct {
		Channel struct {
			Links []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:"link"`
			Items []struct {
				Title   string  `xml:"https://letterboxd.com filmTitle"`
				Year    string  `xml:"https://letterboxd.com filmYear"`
				Rating  *string `xml:"https://letterboxd.com memberRating"`
				Like    *string `xml:"https://letterboxd.com memberLike"`
				Watched string  `xml:"https://letterboxd.com watchedDate"`
				Movie   int     `xml:"https://themoviedb.org movieId"`
				TV      int     `xml:"https://themoviedb.org tvShowId"`
				Link    string  `xml:"link"`
				GUID    string  `xml:"guid"`
				Date    string  `xml:"pubDate"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if xml.Unmarshal(b, &raw) != nil {
		return nil, remoteError{"invalid"}
	}
	channelLink := ""
	for _, link := range raw.Channel.Links {
		if link.XMLName.Space == "" {
			channelLink = link.Value
		}
	}
	if channelLink != "https://letterboxd.com/"+user+"/" || len(raw.Channel.Items) > 200 {
		return nil, remoteError{"invalid"}
	}
	out := []Observation{}
	for _, v := range raw.Channel.Items {
		if v.Title == "" && v.Year == "" {
			continue
		}
		year, e := strconv.Atoi(v.Year)
		published, pe := time.Parse(time.RFC1123Z, v.Date)
		if pe != nil {
			published, pe = time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", v.Date)
		}
		if e != nil || year < 1870 || year > 2100 || pe != nil || v.GUID == "" || len(v.GUID) > 200 || !strings.HasPrefix(v.Link, "https://letterboxd.com/"+user+"/") {
			return nil, remoteError{"invalid"}
		}
		o := Observation{Film: Film{Title: v.Title, Year: v.Year, Date: v.Watched, Watched: true, URI: v.Link}, GUID: v.GUID, Published: published, TMDB: v.Movie, Kind: "movie"}
		if v.TV > 0 {
			o.Kind = "tv"
			o.TMDB = v.TV
		}
		if o.TMDB < 0 || o.TMDB >= 1000000000 {
			return nil, remoteError{"invalid"}
		}
		rating, like := "", ""
		if v.Rating != nil {
			rating = *v.Rating
			n, e := strconv.ParseFloat(rating, 64)
			if e != nil || n < 0.5 || n > 5 || n*2 != float64(int(n*2)) {
				return nil, remoteError{"invalid"}
			}
			o.Film.Rating = &n
			o.RatingKnown = true
		}
		if v.Like != nil {
			like = *v.Like
			switch strings.ToLower(like) {
			case "yes":
				o.Film.Liked = true
			case "no":
			default:
				return nil, remoteError{"invalid"}
			}
			o.LikeKnown = true
		}
		digest := sha256.Sum256([]byte(v.Title + "\n" + v.Year + "\n" + rating + "\n" + like + "\n" + v.Watched))
		o.Fingerprint = hex.EncodeToString(digest[:])
		out = append(out, o)
	}
	return out, nil
}

// Scope parsing to the actual poster grid. A login/challenge/template page is
// never a valid empty collection. Counter and pagination must prove completeness.
var bodyTag = regexp.MustCompile(`(?is)<body\b[^>]*>`)
var attrPattern = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9_-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
var gridPattern = regexp.MustCompile(`(?is)<ul\b[^>]*class=["'][^"']*\bgrid\b[^"']*["'][^>]*>(.*?)</ul>`)
var cardPattern = regexp.MustCompile(`(?is)<li\b[^>]*>.*?</li>`)
var posterPattern = regexp.MustCompile(`(?is)<div\b[^>]*data-item-name=["'][^>]*>`)
var viewingPattern = regexp.MustCompile(`(?is)<p\b[^>]*class=["'][^"']*poster-viewingdata[^"']*["'][^>]*>(.*?)</p>`)
var ratingPattern = regexp.MustCompile(`\brated-([1-9]|10)\b`)
var countPattern = regexp.MustCompile(`(?is)(?:data-num-entries=["']([0-9,]+)["']|title=["']([0-9,]+)(?:&nbsp;|\s|\x{00a0})+(?:films?|likes?)["'])`)
var headingCount = regexp.MustCompile(`(?is)<span[^>]*class=["'][^"']*js-watchlist-count[^"']*["'][^>]*>([0-9,]+)(?:&nbsp;|\s|\x{00a0})+films?</span>`)
var nextPattern = regexp.MustCompile(`(?is)<a\b[^>]*class=["'][^"']*\bnext\b[^"']*["'][^>]*>`)
var displayName = regexp.MustCompile(`^(.*) \(([0-9]{4})\)$`)

func attrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrPattern.FindAllStringSubmatch(s, -1) {
		v := m[2]
		if v == "" {
			v = m[3]
		}
		out[strings.ToLower(m[1])] = html.UnescapeString(v)
	}
	return out
}
func parsePage(b []byte, user, kind string, page int) (Page, error) {
	out := Page{Rows: []Observation{}, Total: -1}
	s := string(b)
	body := attrs(bodyTag.FindString(s))
	expected := map[string]string{"films": "films-watched", "watchlist": "watchlist", "likes": "films-liked"}[kind]
	if body["data-owner"] != user || !containsWord(body["class"], expected) {
		return out, remoteError{"invalid"}
	}
	for _, m := range countPattern.FindAllStringSubmatch(s, -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		n, e := strconv.Atoi(strings.ReplaceAll(v, ",", ""))
		if e == nil {
			if out.Total >= 0 && out.Total != n {
				return out, remoteError{"invalid"}
			}
			out.Total = n
		}
	}
	if m := headingCount.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
		if out.Total >= 0 && out.Total != n {
			return out, remoteError{"invalid"}
		}
		out.Total = n
	}
	if out.Total < 0 || out.Total > 10000 {
		return out, remoteError{"invalid"}
	}
	grid := gridPattern.FindStringSubmatch(s)
	if grid == nil {
		if out.Total == 0 && strings.Contains(s, "section-heading") {
			return out, nil
		}
		return out, remoteError{"invalid"}
	}
	seen := map[string]bool{}
	for _, card := range cardPattern.FindAllString(grid[1], -1) {
		tag := posterPattern.FindString(card)
		if tag == "" {
			return out, remoteError{"invalid"}
		}
		a := attrs(tag)
		name := displayName.FindStringSubmatch(a["data-item-name"])
		path := a["data-item-link"]
		if name == nil || !filmPathPattern.MatchString(path) || seen[path] {
			return out, remoteError{"invalid"}
		}
		seen[path] = true
		f := Film{Title: name[1], Year: name[2], URI: "https://letterboxd.com" + path, Slug: strings.Trim(strings.TrimPrefix(path, "/film/"), "/")}
		o := Observation{Film: f}
		switch kind {
		case "watchlist":
			o.Film.Watchlist = true
		case "likes":
			o.Film.Liked = true
			o.LikeKnown = true
		case "films":
			data := viewingPattern.FindStringSubmatch(card)
			if data == nil {
				return out, remoteError{"invalid"}
			}
			o.Film.Watched = true
			o.RatingKnown = true
			o.LikeKnown = true
			o.Film.Liked = strings.Contains(data[1], "icon-liked") || strings.Contains(data[1], "liked-micro")
			if strings.Contains(data[1], "rated-") && ratingPattern.FindStringSubmatch(data[1]) == nil {
				return out, remoteError{"invalid"}
			}
			if m := ratingPattern.FindStringSubmatch(data[1]); m != nil {
				n, _ := strconv.Atoi(m[1])
				v := float64(n) / 2
				o.Film.Rating = &v
			}
		}
		out.Rows = append(out.Rows, o)
	}
	if len(out.Rows) > 100 || len(out.Rows) > out.Total {
		return out, remoteError{"invalid"}
	}
	if tag := nextPattern.FindString(s); tag != "" {
		href := attrs(tag)["href"]
		route := kind
		if kind == "likes" {
			route = "likes/films"
		}
		want := "/" + user + "/" + route + "/page/" + strconv.Itoa(page+1) + "/"
		if href != want {
			return out, remoteError{"invalid"}
		}
		out.Next = page + 1
	}
	return out, nil
}
func containsWord(s, w string) bool {
	for _, v := range strings.Fields(s) {
		if v == w && w != "" {
			return true
		}
	}
	return false
}
func (r *Remote) Collection(ctx context.Context, kind string) Scan {
	result := Scan{Rows: []Observation{}, Total: -1}
	if kind != "films" && kind != "watchlist" && kind != "likes" {
		result.Err = remoteError{"invalid"}
		return result
	}
	seen := map[string]bool{}
	for page := 1; page <= 150; page++ {
		route := kind
		if kind == "likes" {
			route = "likes/films"
		}
		path := "/" + r.Username + "/" + route + "/"
		if page > 1 {
			path += "page/" + strconv.Itoa(page) + "/"
			select {
			case <-ctx.Done():
				result.Err = remoteError{"unavailable"}
				return result
			case <-time.After(750 * time.Millisecond):
			}
		}
		b, e := r.get(ctx, path)
		if e != nil {
			result.Err = e
			return result
		}
		p, e := parsePage(b, r.Username, kind, page)
		if e != nil {
			result.Err = e
			return result
		}
		if result.Total >= 0 && result.Total != p.Total {
			result.Err = remoteError{"changed"}
			return result
		}
		result.Total = p.Total
		for _, o := range p.Rows {
			if seen[o.Film.Slug] {
				result.Err = remoteError{"invalid"}
				return result
			}
			seen[o.Film.Slug] = true
			result.Rows = append(result.Rows, o)
		}
		if len(result.Rows) > 10000 {
			result.Err = remoteError{"invalid"}
			return result
		}
		if p.Next == 0 {
			if len(result.Rows) != result.Total {
				result.Err = remoteError{"incomplete"}
				return result
			}
			result.Complete = true
			return result
		}
	}
	result.Err = remoteError{"incomplete"}
	return result
}
