package letterboxd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

type SyncPart struct {
	State      string     `json:"state"`
	CheckedAt  *time.Time `json:"checked_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
	CompleteAt *time.Time `json:"complete_at,omitempty"`
	NextAt     *time.Time `json:"next_at,omitempty"`
}
type SyncStatus struct {
	Activity  SyncPart `json:"activity"`
	Watchlist SyncPart `json:"watchlist"`
	Ratings   SyncPart `json:"ratings"`
	Likes     SyncPart `json:"likes"`
}

// PreferenceHash excludes sync bookkeeping. Successful unchanged polls update
// the last-check display without making the TV rebuild recommendations.
func PreferenceHash(p *Profile) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(struct {
		Owner      int
		ImportedAt time.Time
		Films      []Film
	}{p.Owner, p.ImportedAt, p.Films})
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func recount(p *Profile) {
	p.Ratings, p.Watched, p.Watchlist, p.Likes, p.Unmatched = 0, 0, 0, 0, 0
	for _, f := range p.Films {
		if f.Rating != nil {
			p.Ratings++
		}
		if f.Watched {
			p.Watched++
		}
		if f.Watchlist {
			p.Watchlist++
		}
		if f.Liked {
			p.Likes++
		}
		if f.Match == nil {
			p.Unmatched++
		}
	}
}

type SyncCatalog interface {
	Searcher
	Detailer
}
type Syncer struct {
	Path, Username string
	Owner          int
	Source         PublicSource
	Catalog        SyncCatalog
	Log            func(string, ...any)
	mu             sync.Mutex
}

func (s *Syncer) Run(ctx context.Context) {
	if s.Username == "" {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 10*time.Minute)
		err := s.Step(work, time.Now())
		cancel()
		if err != nil && s.Log != nil {
			s.Log("Letterboxd sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func due(p SyncPart, now time.Time) bool { return p.NextAt == nil || !now.Before(*p.NextAt) }
func partStatus(previous SyncPart, now time.Time, err error, complete bool, interval time.Duration) SyncPart {
	previous.CheckedAt = &now
	next := now.Add(interval)
	previous.NextAt = &next
	if err == nil && complete {
		previous.State = "ok"
		previous.UpdatedAt = &now
		previous.CompleteAt = &now
	} else if complete {
		previous.State = remoteState(err)
	} else {
		previous.State = "partial"
		if previous.UpdatedAt == nil {
			previous.UpdatedAt = &now
		}
	}
	return previous
}
func findFilm(p *Profile, o Observation) int {
	if o.TMDB > 0 {
		for i, f := range p.Films {
			if f.Match != nil && f.Match.ID == o.TMDB && f.Match.Kind == o.Kind {
				return i
			}
		}
	}
	for i, f := range p.Films {
		if o.Film.Slug != "" && f.Slug == o.Film.Slug {
			return i
		}
		if Normalize(f.Title) == Normalize(o.Film.Title) && f.Year == o.Film.Year {
			return i
		}
	}
	return -1
}
func (s *Syncer) resolve(ctx context.Context, p *Profile, rows []Observation) ([]Observation, error) {
	out := append([]Observation{}, rows...)
	for i := range out {
		o := &out[i]
		if n := findFilm(p, *o); n >= 0 {
			o.Film.Match = p.Films[n].Match
			o.Film.IdentitySource = p.Films[n].IdentitySource
			if o.Film.Match != nil {
				continue
			}
		}
		if s.Catalog == nil {
			return nil, fmt.Errorf("catalog unavailable")
		}
		call, cancel := context.WithTimeout(ctx, 20*time.Second)
		if o.TMDB > 0 {
			d, e := s.Catalog.Details(call, o.Kind, o.TMDB)
			if e != nil {
				cancel()
				return nil, fmt.Errorf("catalog lookup unavailable")
			}
			o.Film.Match = &d.BrowseTitle
			o.Film.IdentitySource = "letterboxd_tmdb"
		} else {
			matches, e := s.Catalog.Search(call, o.Film.Title)
			if e != nil {
				cancel()
				return nil, fmt.Errorf("catalog lookup unavailable")
			}
			o.Film.Match = Match(o.Film, matches)
			o.Film.IdentitySource = "title_year"
			if o.Film.Match == nil {
				o.Film.Match = canonicalMatch(call, o.Film, s.Catalog)
				o.Film.IdentitySource = "letterboxd_tmdb"
			}
			if o.Film.Match == nil {
				cancel()
				return nil, fmt.Errorf("film identity unresolved")
			}
		}
		cancel()
	}
	return out, nil
}
func ensureFilm(p *Profile, o Observation, now time.Time) int {
	if n := findFilm(p, o); n >= 0 {
		return n
	}
	f := o.Film
	f.Rating = nil
	f.Liked = false
	f.Watchlist = false
	f.Watched = false
	if f.Date == "" {
		f.Date = now.Format("2006-01-02")
	}
	p.Films = append(p.Films, f)
	return len(p.Films) - 1
}
func applyRows(p *Profile, rows []Observation, kind string, now time.Time, complete bool) {
	if complete && kind == "watchlist" {
		for i := range p.Films {
			p.Films[i].Watchlist = false
		}
	}
	if complete && kind == "likes" {
		for i := range p.Films {
			p.Films[i].Liked = false
		}
	}
	if complete && kind == "films" {
		for i := range p.Films {
			p.Films[i].Watched = false
			p.Films[i].Rating = nil
		}
	}
	for _, o := range rows {
		n := ensureFilm(p, o, now)
		f := &p.Films[n]
		if o.Film.Slug != "" {
			f.Slug = o.Film.Slug
		}
		if f.Match == nil {
			f.Match = o.Film.Match
			f.IdentitySource = o.Film.IdentitySource
		}
		if kind == "watchlist" {
			f.Watchlist = true
			continue
		}
		if o.Film.Watched {
			f.Watched = true
		}
		if o.RatingKnown {
			f.Rating = o.Film.Rating
		}
		if o.LikeKnown {
			f.Liked = o.Film.Liked
		}
	}
}
func (s *Syncer) Step(ctx context.Context, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !usernamePattern.MatchString(s.Username) || s.Owner <= 0 || s.Source == nil {
		return fmt.Errorf("sync not configured")
	}
	before, e := os.ReadFile(s.Path)
	if e != nil {
		return fmt.Errorf("private baseline unavailable")
	}
	p, e := decodeProfile(before)
	if e != nil || p == nil {
		return fmt.Errorf("private baseline unavailable")
	}
	if p.Owner != s.Owner || (p.SyncUser != "" && p.SyncUser != s.Username) {
		return fmt.Errorf("sync owner mismatch")
	}
	beforeHash := sha256.Sum256(before)
	p.SyncUser = s.Username
	if p.Sync == nil {
		p.Sync = &SyncStatus{}
	}
	if p.RSSSeen == nil {
		p.RSSSeen = map[string]string{}
	}
	if !due(p.Sync.Activity, now) && !due(p.Sync.Watchlist, now) && !due(p.Sync.Ratings, now) && !due(p.Sync.Likes, now) {
		return nil
	}
	if due(p.Sync.Activity, now) {
		rows, e := s.Source.Feed(ctx)
		if e == nil {
			pending := []Observation{}
			seenFilm := map[string]bool{}
			seen := map[string]string{}
			sort.SliceStable(rows, func(i, j int) bool { return rows[i].Published.After(rows[j].Published) })
			for _, o := range rows {
				seen[o.GUID] = o.Fingerprint
				k := o.Kind + ":" + fmt.Sprint(o.TMDB)
				if o.TMDB == 0 {
					k = Normalize(o.Film.Title) + ":" + o.Film.Year
				}
				if seenFilm[k] {
					continue
				}
				seenFilm[k] = true
				old, known := p.RSSSeen[o.GUID]
				if known && old == o.Fingerprint {
					continue
				}
				if !known && !o.Published.After(p.ImportedAt) {
					continue
				}
				pending = append(pending, o)
			}
			pending, e = s.resolve(ctx, p, pending)
			if e == nil {
				applyRows(p, pending, "rss", now, false)
				p.RSSSeen = seen
			}
		}
		p.Sync.Activity = partStatus(p.Sync.Activity, now, e, true, 15*time.Minute)
	}
	for _, kind := range []string{"watchlist", "films", "likes"} {
		dst := &p.Sync.Watchlist
		interval := time.Hour
		if kind == "films" {
			dst = &p.Sync.Ratings
			interval = 24 * time.Hour
		}
		if kind == "likes" {
			dst = &p.Sync.Likes
			interval = 24 * time.Hour
		}
		if !due(*dst, now) {
			continue
		}
		scan := s.Source.Collection(ctx, kind)
		err := scan.Err
		// Watchlist removals require a fully validated, complete scan. Ratings can
		// apply explicit visible-row edits even if a later page is blocked.
		usable := scan.Complete || (kind == "films" && len(scan.Rows) > 0)
		if usable {
			rows, e := s.resolve(ctx, p, scan.Rows)
			if e == nil {
				applyRows(p, rows, kind, now, scan.Complete)
			} else {
				err = e
				usable = false
			}
		}
		complete := scan.Complete && err == nil
		if !complete && err == nil {
			err = remoteError{"incomplete"}
		}
		*dst = partStatus(*dst, now, err, complete || !usable, interval)
		if usable && !complete {
			dst.State = "partial"
			dst.UpdatedAt = &now
		}
	}
	if ctx.Err() != nil {
		return fmt.Errorf("sync deadline exceeded; baseline kept")
	}
	recount(p)
	if len(p.Films) > 10000 {
		return fmt.Errorf("profile exceeds limit")
	}
	// A manual import arriving during remote reads wins over this older snapshot.
	current, e := os.ReadFile(s.Path)
	if e != nil || sha256.Sum256(current) != beforeHash {
		return fmt.Errorf("baseline changed during sync; retrying later")
	}
	if e = saveIfCurrent(s.Path, *p, beforeHash); e != nil {
		return fmt.Errorf("cannot save private sync")
	}
	return nil
}
