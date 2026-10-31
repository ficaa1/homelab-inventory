package engine

import (
	"context"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

type tasteSeed struct {
	watch         *tastedata.WatchTitle
	title         tastedata.BrowseTitle
	source        string
	stars, weight float64
	at            time.Time
	negative      bool
}

// Build combines substantial Plex viewing with explicit Letterboxd taste.
// Ratings/likes supply stronger positive signals; low-rated neighbors only
// subtract score from existing picks and can never introduce recommendations.
func Build(ctx context.Context, h History, c Catalog, owner int, now time.Time, profiles ...*letterboxd.Profile) (Report, error) {
	return build(ctx, h, c, owner, now, nil, nil, profiles...)
}

// BuildFromTaste serves the background taste model, applying the current Plex
// history, watched titles and availability; it makes no recommendation reads.
func BuildFromTaste(ctx context.Context, h History, c Catalog, owner int, now time.Time, model *TasteModel, profile *letterboxd.Profile) (Report, error) {
	return build(ctx, h, c, owner, now, nil, model, profile)
}

// BuildSimilar keeps owner exclusions and quality gates while following one explicit title.
func BuildSimilar(ctx context.Context, h History, c Catalog, owner int, now time.Time, title tastedata.BrowseTitle, profile *letterboxd.Profile) (Report, error) {
	return build(ctx, h, c, owner, now, &title, nil, profile)
}

func build(ctx context.Context, h History, c Catalog, owner int, now time.Time, focus *tastedata.BrowseTitle, model *TasteModel, profiles ...*letterboxd.Profile) (Report, error) {
	var profile *letterboxd.Profile
	if len(profiles) > 0 {
		profile = profiles[0]
	}
	if profile != nil && profile.Owner != owner {
		return empty("unavailable"), fmt.Errorf("Letterboxd owner mismatch")
	}
	if c == nil || owner <= 0 || (h == nil && profile == nil) {
		return empty("unconfigured"), nil
	}
	var history []tastedata.WatchTitle
	var historyErr error
	if h != nil {
		history, historyErr = h.WatchHistory(ctx, owner)
	}
	if historyErr != nil && profile == nil {
		return empty("unavailable"), fmt.Errorf("watch history unavailable")
	}
	sort.SliceStable(history, func(i, j int) bool { return history[i].At.After(history[j].At) })
	watched := map[string]bool{}
	watchedIDs := map[string]bool{}
	chosen := []tasteSeed{}
	if profile != nil {
		for _, f := range profile.Films {
			if !f.Watched {
				continue
			}
			kind := "movie"
			if f.Match != nil {
				kind = f.Match.Kind
				watchedIDs[key(kind, f.Match.ID)] = true
				watched[nameKey(kind, f.Match.Title, f.Year)] = true
			}
			watched[nameKey(kind, f.Title, f.Year)] = true
		}
		favorites := []letterboxd.Film{}
		negatives := []letterboxd.Film{}
		for _, f := range profile.Films {
			if f.Match == nil {
				continue
			}
			if f.Rating != nil && *f.Rating <= 2 {
				negatives = append(negatives, f)
			} else if (f.Rating != nil && *f.Rating >= 4) || (f.Rating == nil && f.Liked) {
				favorites = append(favorites, f)
			}
		}
		rank := func(f letterboxd.Film) float64 {
			if f.Rating != nil {
				return *f.Rating
			}
			return 4
		}
		sort.SliceStable(favorites, func(i, j int) bool {
			if rank(favorites[i]) == rank(favorites[j]) {
				return favorites[i].Date > favorites[j].Date
			}
			return rank(favorites[i]) > rank(favorites[j])
		})
		// Daily windows let the full positive library participate over time while
		// keeping each feed bounded. Ratings still control each seed's strength.
		if len(favorites) > 0 {
			offset := int(now.Unix()/86400) % len(favorites)
			for n := 0; n < min(4, len(favorites)); n++ {
				f := favorites[(offset+n)%len(favorites)]
				stars := 0.0
				if f.Rating != nil {
					stars = *f.Rating
				}
				chosen = append(chosen, tasteSeed{title: *f.Match, source: "letterboxd", stars: stars, weight: 1.4 + 0.3*(rank(f)-4), at: now})
			}
		}
		sort.SliceStable(negatives, func(i, j int) bool { return *negatives[i].Rating < *negatives[j].Rating })
		for _, f := range negatives[:min(3, len(negatives))] {
			chosen = append(chosen, tasteSeed{title: *f.Match, source: "letterboxd", stars: *f.Rating, weight: 0.6 * (3 - *f.Rating), negative: true, at: now})
		}
	}
	hasFavorites := false
	for _, s := range chosen {
		if !s.negative {
			hasFavorites = true
		}
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, v := range history {
		if v.Seconds < 300 || v.At.After(now.Add(24*time.Hour)) {
			continue
		}
		year := ""
		if v.Year > 0 {
			year = fmt.Sprint(v.Year)
		}
		watched[nameKey(v.Kind, v.Title, year)] = true
		if v.Percent < 50 || (v.Percent < 85 && v.Seconds < 1200) || v.At.Before(now.AddDate(0, 0, -365)) {
			continue
		}
		// An explicit low rating takes precedence over a recent Plex play.
		rejected := false
		if profile != nil {
			for _, f := range profile.Films {
				if f.Rating != nil && *f.Rating <= 2 && (nameKey(v.Kind, v.Title, year) == nameKey("movie", f.Title, f.Year) || (f.Match != nil && nameKey(v.Kind, v.Title, year) == nameKey(f.Match.Kind, f.Match.Title, f.Match.Year))) {
					rejected = true
					break
				}
			}
		}
		if rejected {
			continue
		}
		limit := 4
		if v.Kind == "movie" && hasFavorites {
			limit = 2
		}
		k := v.Kind + ":" + v.Key
		if !seen[k] && counts[v.Kind] < limit {
			v := v
			chosen = append(chosen, tasteSeed{watch: &v, source: "plex", weight: 1, at: v.At})
			seen[k] = true
			counts[v.Kind]++
		}
	}
	if focus != nil {
		chosen = []tasteSeed{{title: *focus, source: "explicit", weight: 1, at: now}}
	} else if model != nil {
		chosen = nil
	}
	report := empty("ok")
	stamp := now.UTC()
	report.CheckedAt = &stamp
	if profile != nil {
		report.Letterboxd = &ImportSummary{ImportedAt: profile.ImportedAt, Ratings: profile.Ratings, Watched: profile.Watched, Watchlist: profile.Watchlist, Unmatched: profile.Unmatched, Sync: profile.Sync}
	}
	if len(chosen) == 0 && model == nil && (profile == nil || profile.Watchlist == 0) {
		report.Status = "empty"
		return report, nil
	}
	type result struct {
		seed tasteSeed
		rows []tastedata.SuggestedTitle
		err  error
	}
	results := make([]result, len(chosen))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	var library map[string]string
	var libraryErr error
	wg.Add(1)
	go func() { defer wg.Done(); library, libraryErr = c.RecommendationAvailability(ctx) }()
	for i, s := range chosen {
		wg.Add(1)
		go func(i int, s tasteSeed) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i].err = ctx.Err()
				return
			}
			defer func() { <-sem }()
			if s.watch != nil {
				title, e := h.WatchIdentity(ctx, *s.watch)
				if e != nil {
					results[i].err = e
					return
				}
				s.title = title
			}
			rows, e := c.Recommendations(ctx, s.title.Kind, s.title.ID)
			results[i] = result{s, rows, e}
		}(i, s)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return empty("unavailable"), fmt.Errorf("recommendations timed out")
	}
	availability := func(kind string, id int) string {
		if libraryErr != nil {
			return "unknown"
		}
		if s, ok := library[key(kind, id)]; ok {
			return s
		}
		return "not_in_plex"
	}
	if profile != nil {
		for _, f := range profile.Films {
			if f.Watchlist && f.Match != nil {
				report.Watchlist = append(report.Watchlist, Pick{BrowseTitle: *f.Match, Reason: "From your Letterboxd watchlist", Availability: availability(f.Match.Kind, f.Match.ID), Because: []string{}, Seeds: []string{}})
				if len(report.Watchlist) == 100 {
					break
				}
			}
		}
	}
	seedIDs := map[string]bool{}
	for _, r := range results {
		if r.seed.title.ID > 0 {
			seedIDs[key(r.seed.title.Kind, r.seed.title.ID)] = true
			watched[nameKey(r.seed.title.Kind, r.seed.title.Title, r.seed.title.Year)] = true
		}
	}
	type ranked struct {
		pick       Pick
		score      float64
		first      int
		letterboxd bool
	}
	picks := map[string]*ranked{}
	negative := map[string]float64{}
	used := map[string]bool{}
	failures := 0
	order := 0
	for _, r := range results {
		if r.err != nil {
			failures++
			continue
		}
		s := r.seed
		sk := key(s.title.Kind, s.title.ID)
		if s.negative {
			for rank, v := range r.rows {
				negative[key(v.Kind, v.ID)] += s.weight / (10 + float64(rank))
			}
			continue
		}
		if used[sk] {
			continue
		}
		used[sk] = true
		report.Seeds = append(report.Seeds, Seed{Key: sk, Title: s.title.Title, Kind: s.title.Kind, Source: s.source, Stars: s.stars})
		for rank, v := range r.rows {
			k := key(v.Kind, v.ID)
			date, e := time.Parse("2006-01-02", v.Date)
			if v.Adult || v.Rating < 6 || v.Rating > 10 || v.Votes < 50 || v.Poster == "" || e != nil || date.After(now) || seedIDs[k] || watchedIDs[k] || watched[nameKey(v.Kind, v.Title, v.Year)] || watched[nameKey(v.Kind, v.Title, "")] {
				continue
			}
			p := picks[k]
			if p == nil {
				p = &ranked{pick: Pick{BrowseTitle: v.BrowseTitle, Availability: availability(v.Kind, v.ID), Rating: v.Rating, ReleasedAt: v.Date}, first: order}
				order++
				picks[k] = p
			}
			if contains(p.pick.Seeds, sk) {
				continue
			}
			p.pick.Seeds = append(p.pick.Seeds, sk)
			if len(p.pick.Because) < 3 {
				p.pick.Because = append(p.pick.Because, s.title.Title)
			}
			if s.source == "explicit" {
				p.pick.Reason = "More like " + s.title.Title
			}
			if s.source == "letterboxd" && !p.letterboxd {
				p.letterboxd = true
				if s.stars > 0 {
					p.pick.Reason = fmt.Sprintf("Because you rated %s %.1f/5 on Letterboxd", s.title.Title, s.stars)
				} else {
					p.pick.Reason = "Because you liked " + s.title.Title + " on Letterboxd"
				}
			}
			age := math.Max(0, now.Sub(s.at).Hours()/24)
			p.score += s.weight * (0.65 + 0.35*math.Exp(-age/120)) / (10 + float64(rank))
		}
	}
	if len(results) > 0 && failures == len(results) && len(report.Watchlist) == 0 {
		return empty("unavailable"), fmt.Errorf("recommendations unavailable")
	}
	report.Partial = failures > 0 || libraryErr != nil || historyErr != nil
	report.LibraryStatus = "ok"
	if libraryErr != nil {
		report.LibraryStatus = "unavailable"
	}
	if model != nil {
		report.Seeds = append(report.Seeds, model.Seeds...)
		byKey := map[string]Pick{}
		for i, t := range model.Picks {
			if watchedIDs[key(t.Kind, t.ID)] || watched[nameKey(t.Kind, t.Title, t.Year)] || watched[nameKey(t.Kind, t.Title, "")] {
				continue
			}
			p := Pick{BrowseTitle: t.BrowseTitle, Reason: t.Reason, Because: t.Because, Seeds: t.Seeds, Availability: availability(t.Kind, t.ID), Rating: t.Rating, ReleasedAt: t.Date}
			byKey[key(t.Kind, t.ID)] = p
			if len(report.Titles) < 100 {
				report.Titles = append(report.Titles, p)
			}
			// Recent releases come from the stronger half; a new title is not
			// interesting merely for being new.
			if date, err := time.Parse("2006-01-02", p.ReleasedAt); err == nil && !date.Before(now.AddDate(-1, 0, 0)) && i < len(model.Picks)/2 {
				report.Recent = append(report.Recent, p)
			}
		}
		// The first hundred are mostly films; the Shows filter needs more series.
		shows, listed := 0, map[string]bool{}
		for _, p := range report.Titles {
			listed[key(p.Kind, p.ID)] = true
			if p.Kind == "tv" {
				shows++
			}
		}
		for _, t := range model.Picks {
			if p, ok := byKey[key(t.Kind, t.ID)]; ok && shows < 50 && t.Kind == "tv" && !listed[key(t.Kind, t.ID)] {
				report.Titles = append(report.Titles, p)
				shows++
			}
		}
		sort.SliceStable(report.Recent, func(i, j int) bool { return report.Recent[i].ReleasedAt > report.Recent[j].ReleasedAt })
		for _, l := range model.Lanes {
			lane := Lane{Title: l.Title, Label: strings.Join(l.Names[:min(2, len(l.Names))], " & "), Titles: []Pick{}}
			for _, k := range l.Picks {
				if p, ok := byKey[k]; ok {
					lane.Titles = append(lane.Titles, p)
				}
			}
			if len(lane.Titles) > 0 {
				report.Lanes = append(report.Lanes, lane)
			}
		}
		return report, nil
	}
	rankedPicks := []*ranked{}
	for k, p := range picks {
		p.score -= negative[k]
		if p.score > 0 {
			rankedPicks = append(rankedPicks, p)
		}
	}
	sort.Slice(rankedPicks, func(i, j int) bool {
		if rankedPicks[i].score == rankedPicks[j].score {
			return rankedPicks[i].first < rankedPicks[j].first
		}
		return rankedPicks[i].score > rankedPicks[j].score
	})
	scores := map[string]float64{}
	for _, p := range rankedPicks {
		scores[key(p.pick.Kind, p.pick.ID)] = p.score
		report.Titles = append(report.Titles, p.pick)
		if date, err := time.Parse("2006-01-02", p.pick.ReleasedAt); err == nil && !date.Before(now.AddDate(-1, 0, 0)) {
			report.Recent = append(report.Recent, p.pick)
		}
	}
	// Greedy novelty penalty spreads adjacent picks across their source titles
	// and media kinds while retaining relevance as the primary signal.
	report.Titles = diversify(report.Titles, scores)
	if len(report.Titles) > 100 {
		report.Titles = report.Titles[:100]
	}
	sort.SliceStable(report.Recent, func(i, j int) bool { return report.Recent[i].ReleasedAt > report.Recent[j].ReleasedAt })
	if len(report.Recent) > 100 {
		report.Recent = report.Recent[:100]
	}
	return report, nil
}

func diversify(pool []Pick, scores map[string]float64) []Pick {
	out := make([]Pick, 0, len(pool))
	used := map[string]int{}
	kinds := map[string]int{}
	remaining := append([]Pick{}, pool...)
	for len(remaining) > 0 {
		best := -1
		bestScore := -1.0
		for i, p := range remaining {
			overlap := 0
			for _, s := range p.Seeds {
				overlap += used[s]
			}
			score := scores[key(p.Kind, p.ID)] / (1 + 0.32*float64(overlap) + 0.12*float64(kinds[p.Kind]))
			if score > bestScore {
				best, bestScore = i, score
			}
		}
		p := remaining[best]
		out = append(out, p)
		for _, s := range p.Seeds {
			used[s]++
		}
		kinds[p.Kind]++
		remaining = append(remaining[:best], remaining[best+1:]...)
	}
	return out
}
