package letterboxd

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

var tmdbLink = regexp.MustCompile(`themoviedb\.org/(movie|tv)/([1-9][0-9]{0,8})(?:[^0-9]|$)`)

func allowedLetterboxdURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" && (u.Hostname() == "boxd.it" || u.Hostname() == "letterboxd.com" || u.Hostname() == "www.letterboxd.com") && len(u.Path) <= 512 && u.RawQuery == "" && u.Fragment == ""
}

func pageIdentity(body []byte) (string, int, error) {
	matches := tmdbLink.FindAllStringSubmatch(string(body), -1)
	kind := ""
	id := 0
	for _, m := range matches {
		n, _ := strconv.Atoi(m[2])
		if id != 0 && (id != n || kind != m[1]) {
			return "", 0, fmt.Errorf("ambiguous film page")
		}
		kind, id = m[1], n
	}
	if id == 0 {
		return "", 0, fmt.Errorf("TMDB link unavailable")
	}
	return kind, id, nil
}

type Detailer interface {
	Details(context.Context, string, int) (tastedata.TitleDetail, error)
}

func canonicalMatch(ctx context.Context, f Film, s Searcher) *tastedata.BrowseTitle {
	d, ok := s.(Detailer)
	if !ok || strings.TrimSpace(f.URI) == "" {
		return nil
	}
	source, ok := s.(interface {
		PublicIdentity(context.Context, string) (string, int, error)
	})
	if !ok {
		return nil
	}
	kind, id, e := source.PublicIdentity(ctx, f.URI)
	if e != nil {
		return nil
	}
	title, e := d.Details(ctx, kind, id)
	if e != nil || title.ID != id || title.Kind != kind {
		return nil
	}
	return &title.BrowseTitle
}

func PageIdentity(body []byte) (string, int, error) { return pageIdentity(body) }
