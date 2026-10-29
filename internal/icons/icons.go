// Package icons gives launcher tiles a picture, without giving up the
// dashboard's offline guarantee.
//
// # Why the page never fetches an icon itself
//
// The dashboard is one self-contained file that loads nothing from anywhere,
// and the reason is not purity: it has to work when the homelab cannot reach
// the internet, which is exactly the situation you are in when something is
// broken enough that you are looking at it. A tile whose icon comes from a CDN
// is a tile that is blank in the one moment it matters, and every page load
// would also tell that CDN which services you run.
//
// So the *server* fetches an icon once, stores it, and serves it from its own
// origin. The page still loads nothing external, it works offline after the
// first fetch, and the CDN sees one request from one machine rather than one
// per browser per visit. Fetching is opt-in and off by default, because it is
// still an outbound request to a third party and that should be a decision.
//
// # Why a slug and not a URL
//
// A user-supplied icon *URL* would make this endpoint a general-purpose
// fetcher: hand it `http://192.0.2.31/` or a cloud metadata address and the
// server retrieves it and hands back the body. That is server-side request
// forgery, and no icon is worth it. Icons are named by slug, and slugs resolve
// only against a fixed pair of known icon repositories.
//
// # Why an SVG from the internet is safe here
//
// An SVG can contain script. Served same-origin and opened directly it would
// be stored XSS. Three things stop that: the dashboard renders icons in an
// `<img>` tag, which disables scripting in SVG entirely; the handler sends a
// restrictive Content-Security-Policy and `nosniff`; and anything containing a
// script element is rejected before it is ever cached.
package icons

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StateName is the key the icon cache is stored under.
const StateName = "icons"

// sources are the only places an icon may come from. Both are public icon
// repositories for exactly this purpose. The list is fixed at compile time:
// this is what stops the endpoint being a general-purpose fetcher.
var sources = []string{
	"https://cdn.jsdelivr.net/gh/homarr-labs/dashboard-icons/svg/%s.svg",
	"https://cdn.jsdelivr.net/gh/selfhst/icons/svg/%s.svg",
}

// maxIconBytes bounds one icon. Real ones are 200 bytes to a few KB.
const maxIconBytes = 256 << 10

// Entry is one cached icon.
type Entry struct {
	ContentType string    `json:"content_type"`
	Data        string    `json:"data"` // base64
	FetchedAt   time.Time `json:"fetched_at"`
	// Missing records a slug that was looked for and not found, so a service
	// with no icon is not re-fetched on every page load forever.
	Missing bool `json:"missing,omitempty"`
}

// Book is the cache, persisted between runs.
type Book struct {
	Entries   map[string]Entry `json:"entries"`
	UpdatedAt time.Time        `json:"updated_at,omitempty"`
}

// StateStore is the slice of internal/store this package needs.
type StateStore interface {
	ReadState(name string, v any) error
	WriteState(name string, v any) error
}

// Cache fetches icons at most once each and remembers the answer.
type Cache struct {
	Store StateStore
	// Log records why a fetch failed. Without it a failure is indistinguishable
	// from "no icon exists", which is exactly the confusion that hid a missing
	// CA bundle in the FROM scratch image.
	Log func(format string, args ...any)
	// Fetch enables outbound requests. Off by default: retrieving an icon
	// tells a third party which services exist here, which is a small leak but
	// a real one, and it should be chosen rather than assumed.
	Fetch  bool
	Client *http.Client

	mu sync.Mutex
}

// NewCache returns a cache with a short timeout — an icon is decoration, and
// must never be able to hold up a page.
func NewCache(store StateStore, fetch bool) *Cache {
	return &Cache{Store: store, Fetch: fetch, Client: &http.Client{Timeout: 10 * time.Second}}
}

// Get returns a cached icon, fetching it once if fetching is enabled.
//
// The boolean is false for "no icon", which is a perfectly normal answer: the
// UI falls back to initials, and a missing icon is never an error.
func (c *Cache) Get(ctx context.Context, slug string) (contentType string, data []byte, ok bool) {
	slug = Normalise(slug)
	if slug == "" {
		return "", nil, false
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	book := c.read()
	if e, hit := book.Entries[slug]; hit {
		if e.Missing {
			return "", nil, false
		}
		raw, err := base64.StdEncoding.DecodeString(e.Data)
		if err == nil {
			return e.ContentType, raw, true
		}
		// A corrupt entry is worth re-fetching rather than serving.
	}
	if !c.Fetch {
		return "", nil, false
	}

	body, err := c.download(ctx, slug)
	if err != nil {
		if c.Log != nil {
			c.Log("icons: %s: %v", slug, err)
		}
		// Remember the miss so a service with no upstream icon is not looked
		// up again on every single page load.
		c.store(book, slug, Entry{Missing: true, FetchedAt: time.Now()})
		return "", nil, false
	}
	e := Entry{
		ContentType: "image/svg+xml",
		Data:        base64.StdEncoding.EncodeToString(body),
		FetchedAt:   time.Now(),
	}
	c.store(book, slug, e)
	return e.ContentType, body, true
}

// Cached reports whether a slug has already been looked up, so a caller can
// serve what it has without deciding whether a fresh fetch is warranted.
func (c *Cache) Cached(slug string) bool {
	slug = Normalise(slug)
	if slug == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.read().Entries[slug]
	return ok
}

func (c *Cache) read() Book {
	var book Book
	if c.Store != nil {
		_ = c.Store.ReadState(StateName, &book)
	}
	if book.Entries == nil {
		book.Entries = map[string]Entry{}
	}
	return book
}

func (c *Cache) store(book Book, slug string, e Entry) {
	book.Entries[slug] = e
	book.UpdatedAt = time.Now()
	if c.Store != nil {
		_ = c.Store.WriteState(StateName, book)
	}
}

func (c *Cache) download(ctx context.Context, slug string) ([]byte, error) {
	var lastErr error
	for _, tmpl := range sources {
		url := fmt.Sprintf(tmpl, slug)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := c.Client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxIconBytes+1))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("icons: %s returned %s", url, resp.Status)
			continue
		}
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if len(body) > maxIconBytes {
			lastErr = fmt.Errorf("icons: %s is larger than %d bytes", slug, maxIconBytes)
			continue
		}
		if err := checkSVG(body); err != nil {
			lastErr = err
			continue
		}
		return body, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("icons: no source had %q", slug)
	}
	return nil, lastErr
}

// checkSVG rejects anything that is not a plain SVG image.
//
// Rendering happens in an <img>, which already disables scripting in SVG, and
// the handler sends a restrictive CSP. This is the third layer, and it is here
// because "we fetch a file from the internet and serve it from our own origin"
// deserves more than one thing going right.
func checkSVG(body []byte) error {
	s := strings.ToLower(string(body))
	if !strings.Contains(s, "<svg") {
		return fmt.Errorf("icons: not an SVG")
	}
	for _, bad := range []string{"<script", "javascript:", "<foreignobject", "<iframe", "<embed"} {
		if strings.Contains(s, bad) {
			return fmt.Errorf("icons: refusing an SVG containing %q", bad)
		}
	}
	return nil
}

// Normalise turns anything into a safe slug, and is the only thing standing
// between a path parameter and a URL this server will fetch.
func Normalise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ' ', r == '_', r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// aliases map a display name to the slug the icon repositories use, for the
// cases where they differ. Same spirit as the service table: a small piece of
// knowledge that cannot be derived, kept as data.
var aliases = map[string]string{
	"proxmox ve":     "proxmox",
	"truenas":        "truenas",
	"adguard home":   "adguard-home",
	"calibre-web":    "calibre-web",
	"qbittorrent":    "qbittorrent",
	"home assistant": "home-assistant",
	"uptime kuma":    "uptime-kuma",
	"node exporter":  "prometheus",
	"tv shows":       "sonarr",
	"pangolin":       "traefik",
	"homelab api":    "",
	"copyparty":      "",
}

// SlugFor guesses the icon slug for a service's display name.
func SlugFor(name string) string {
	if s, ok := aliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return s
	}
	return Normalise(name)
}

// Initials is the no-network fallback: one or two letters from the name.
//
// Every tile has one of these underneath its icon, so a fleet with fetching
// switched off, or an icon the repositories do not have, still reads as a set
// of distinct things rather than a grid of blanks.
func Initials(name string) string {
	fields := strings.Fields(strings.TrimSpace(name))
	if len(fields) == 0 {
		return "?"
	}
	first := []rune(fields[0])
	if len(fields) == 1 {
		if len(first) == 1 {
			return strings.ToUpper(string(first[0]))
		}
		// The second letter keeps its own case, so camelCase brand names read
		// the way they are written: qBittorrent gives QB, not Qb.
		return strings.ToUpper(string(first[0])) + string(first[1])
	}
	second := []rune(fields[1])
	return strings.ToUpper(string(first[0])) + strings.ToUpper(string(second[0]))
}
