// Package discovery builds a private-owner, explainable TV recommendation feed.
package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

type History interface {
	WatchHistory(context.Context, int) ([]tastedata.WatchTitle, error)
	WatchIdentity(context.Context, tastedata.WatchTitle) (tastedata.BrowseTitle, error)
}
type Catalog interface {
	Recommendations(context.Context, string, int) ([]tastedata.SuggestedTitle, error)
	RecommendationAvailability(context.Context) (map[string]string, error)
}
type Pick struct {
	ReleasedAt string `json:"released_at,omitempty"`
	Reason     string `json:"reason,omitempty"`
	tastedata.BrowseTitle
	Because      []string `json:"because"`
	Seeds        []string `json:"seeds"`
	Availability string   `json:"availability"`
	Rating       float64  `json:"rating"`
}

// Lane is one strand of the owner's taste for Dusk: a short label for the
// chip, a heading, and its own picks, which may reach beyond Titles' 100.
type Lane struct {
	Title  string `json:"title"`
	Label  string `json:"label"`
	Titles []Pick `json:"titles"`
}
type Seed struct {
	Source string  `json:"source,omitempty"`
	Stars  float64 `json:"stars,omitempty"`
	Key    string  `json:"key"`
	Title  string  `json:"title"`
	Kind   string  `json:"kind"`
}
type ImportSummary struct {
	ImportedAt time.Time              `json:"imported_at"`
	Ratings    int                    `json:"ratings"`
	Watched    int                    `json:"watched"`
	Watchlist  int                    `json:"watchlist"`
	Unmatched  int                    `json:"unmatched"`
	Sync       *letterboxd.SyncStatus `json:"sync,omitempty"`
}
type Report struct {
	Letterboxd    *ImportSummary `json:"letterboxd,omitempty"`
	Recent        []Pick         `json:"recent"`
	Hidden        []string       `json:"hidden"`
	Watchlist     []Pick         `json:"watchlist"`
	LibraryStatus string         `json:"library_status"`
	Status        string         `json:"status"`
	CheckedAt     *time.Time     `json:"checked_at"`
	Seeds         []Seed         `json:"seeds"`
	Lanes         []Lane         `json:"lanes,omitempty"`
	Titles        []Pick         `json:"titles"`
	Partial       bool           `json:"partial"`
}

func empty(status string) Report {
	return Report{Status: status, Seeds: []Seed{}, Titles: []Pick{}, Watchlist: []Pick{}, Recent: []Pick{}, Hidden: []string{}}
}
func key(kind string, id int) string { return fmt.Sprintf("%s:%d", kind, id) }
func normalized(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	// Plex's legacy agents append a show's year to its name.
	if n := len(s); n > 7 && s[n-7:n-5] == " (" && s[n-1:] == ")" {
		if _, err := time.Parse("2006", s[n-5:n-1]); err == nil {
			s = s[:n-7]
		}
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return r
		}
		return -1
	}, s)
}
func nameKey(kind, title, year string) string {
	if kind == "tv" {
		year = ""
	}
	return kind + ":" + normalized(title) + ":" + year
}

func contains(rows []string, v string) bool {
	for _, r := range rows {
		if r == v {
			return true
		}
	}
	return false
}

// Cache coalesces cold reads and refreshes after 30 minutes. Failed refreshes
// retain the original timestamp and picks; failures are retried after a minute.
// Client disconnects never abort a shared build or spawn duplicate work.
type Cache struct {
	Lifetime    context.Context
	workers     sync.WaitGroup
	History     History
	Catalog     Catalog
	Owner       int
	ProfilePath string
	// Taste, when it has a model, replaces per-request seed lookups.
	Taste       *Taste
	mu          sync.Mutex
	value       Report
	at          time.Time
	inflight    chan struct{}
	profileHash string
}

func (c *Cache) Get(ctx context.Context) Report {
	profile, loadErr := letterboxd.Load(c.ProfilePath)
	if profile != nil && profile.Owner != c.Owner {
		loadErr = fmt.Errorf("profile owner mismatch")
	}
	profileHash := letterboxd.PreferenceHash(profile) + c.tasteStamp()
	c.mu.Lock()
	if loadErr != nil {
		v := c.value
		if v.Titles == nil {
			v = empty("unavailable")
		} else {
			v.Status = "unavailable"
		}
		c.mu.Unlock()
		return v
	}
	if profileHash != c.profileHash {
		c.at = time.Time{}
	}
	ttl := 30 * time.Minute
	if c.value.Status != "ok" || c.value.Partial {
		ttl = time.Minute
	}
	if !c.at.IsZero() && time.Since(c.at) < ttl {
		v := withSync(c.value, profile)
		c.mu.Unlock()
		return v
	}
	done := c.inflight
	if done == nil {
		done = make(chan struct{})
		c.inflight = done
		c.workers.Add(1)
		go func() {
			defer c.workers.Done()
			parent := c.Lifetime
			if parent == nil {
				parent = ctx
			}
			work, cancel := context.WithTimeout(parent, 18*time.Second)
			defer cancel()
			var v Report
			var err error
			var builtHash string
			for {
				current, loadErr := letterboxd.Load(c.ProfilePath)
				if loadErr != nil {
					err = loadErr
					break
				}
				builtHash = letterboxd.PreferenceHash(current) + c.tasteStamp()
				if m := c.Taste.Model(); m != nil {
					v, err = BuildFromTaste(work, c.History, c.Catalog, c.Owner, time.Now(), m, current)
				} else {
					v, err = Build(work, c.History, c.Catalog, c.Owner, time.Now(), current)
				}
				latest, loadErr := letterboxd.Load(c.ProfilePath)
				if loadErr != nil {
					err = loadErr
					break
				}
				if work.Err() != nil || builtHash == letterboxd.PreferenceHash(latest)+c.tasteStamp() {
					break
				}
			}
			c.mu.Lock()
			if err != nil {
				v = c.value
				v.Status = "unavailable"
				if v.Titles == nil {
					v = empty("unavailable")
				}
			}
			c.value = v
			c.profileHash = builtHash
			c.at = time.Now()
			c.inflight = nil
			c.mu.Unlock()
			close(done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-done:
		c.mu.Lock()
		v := c.value
		c.mu.Unlock()
		latest, _ := letterboxd.Load(c.ProfilePath)
		return withSync(v, latest)
	case <-ctx.Done():
		return empty("unavailable")
	}
}

// Refresh rebuilds the taste model and then the feed from current Plex,
// Letterboxd and Overseerr reads, for the TV's Refresh button. The model gets
// 12 seconds so the feed keeps time to build; when it fails, the feed is
// rebuilt from the previous model. A feed under a minute old is served as is.
func (c *Cache) Refresh(ctx context.Context) Report {
	rebuild, cancel := context.WithTimeout(ctx, 12*time.Second)
	err := c.Taste.Rebuild(rebuild)
	cancel()
	if err != nil && c.Taste.Log != nil {
		c.Taste.Log("taste model refresh: %v", err)
	}
	c.mu.Lock()
	if time.Since(c.at) >= time.Minute {
		c.at = time.Time{}
	}
	c.mu.Unlock()
	return c.Get(ctx)
}

// tasteStamp changes when a new taste model lands, so the feed picks it up.
func (c *Cache) tasteStamp() string {
	if m := c.Taste.Model(); m != nil {
		return "/" + m.BuiltAt.Format(time.RFC3339Nano)
	}
	return ""
}

func withSync(v Report, p *letterboxd.Profile) Report {
	if p != nil && v.Letterboxd != nil {
		summary := *v.Letterboxd
		summary.Sync = p.Sync
		v.Letterboxd = &summary
	}
	return v
}

// Similar is an explicit browsing action, bounded by the caller's deadline.
func (c *Cache) Similar(ctx context.Context, title tastedata.BrowseTitle) Report {
	profile, err := letterboxd.Load(c.ProfilePath)
	if err != nil {
		return empty("unavailable")
	}
	report, err := BuildSimilar(ctx, c.History, c.Catalog, c.Owner, time.Now(), title, profile)
	if err != nil {
		return empty("unavailable")
	}
	return report
}

// Wait drains shared builds after the owning plugin cancels its lifetime.
func (c *Cache) Wait() { c.workers.Wait() }
