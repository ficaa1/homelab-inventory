package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/twins"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

// TasteSource is the read-only public metadata the taste model weighs.
type TasteSource interface {
	TitleFacts(context.Context, string, int) (tastedata.TitleFacts, error)
	RecommendationsPage(context.Context, string, int, int) ([]tastedata.SuggestedTitle, error)
	PersonWork(context.Context, int) ([]tastedata.SuggestedTitle, error)
	Search(context.Context, string) ([]tastedata.BrowseTitle, error)
}

// TasteModel ranks titles against the owner's whole Letterboxd history. It is
// built in the background because a cold build reads thousands of titles.
type TasteModel struct {
	Owner       int         `json:"owner"`
	ProfileHash string      `json:"profile_hash"`
	BuiltAt     time.Time   `json:"built_at"`
	Floor       float64     `json:"floor"`
	Seeds       []Seed      `json:"seeds"`
	Picks       []TastePick `json:"picks"`
	// Lanes and Familiar come from the taste twins only.
	Lanes    []TasteLane `json:"lanes,omitempty"`
	Familiar []string    `json:"familiar,omitempty"`
	// Known is everything the model believes the owner has watched.
	Known []KnownTitle `json:"known"`
}

// KnownTitle is one watched title and how the model reads it.
type KnownTitle struct {
	tastedata.BrowseTitle
	Source string `json:"source"`
	Label  string `json:"label"`
	Effect string `json:"effect"`
}

func effect(f tasteFilm) string {
	switch {
	case f.unknown:
		return "seen"
	case f.signal >= 0.75:
		return "strong like"
	case f.signal >= 0.25:
		return "like"
	case f.signal > -0.25:
		return "neutral"
	case f.signal > -0.75:
		return "dislike"
	}
	return "strong dislike"
}

func knownTitles(films []tasteFilm) []KnownTitle {
	out := []KnownTitle{}
	for _, f := range films {
		out = append(out, KnownTitle{BrowseTitle: tastedata.BrowseTitle{ID: f.title.ID, Kind: f.title.Kind, Title: f.title.Title, Year: f.title.Year}, Source: f.source, Label: f.label, Effect: effect(f)})
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title) })
	return out
}

// applyVerdicts lets the owner's explicit word override Letterboxd and Plex.
func applyVerdicts(films []tasteFilm, verdicts map[string]Verdict) []tasteFilm {
	at := map[string]int{}
	for i, f := range films {
		at[key(f.title.Kind, f.title.ID)] = i
	}
	keys := []string{}
	for k := range verdicts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	drop := map[int]bool{}
	for _, k := range keys {
		v := verdicts[k]
		var kind string
		var id int
		if n, _ := fmt.Sscanf(strings.Replace(k, ":", " ", 1), "%s %d", &kind, &id); n != 2 {
			continue
		}
		f := tasteFilm{title: tastedata.BrowseTitle{ID: id, Kind: kind, Title: v.Title, Year: v.Year}, source: "you"}
		if i, ok := at[k]; ok {
			f.title = films[i].title
		}
		f.signal = verdictSignal[v.Verdict]
		f.unknown = v.Verdict == "seen"
		f.label = map[string]string{"loved": "You loved it", "liked": "You liked it", "seen": "You've seen it", "meh": "You thought it was meh", "disliked": "You disliked it", "pass": "You passed (not seen)", "ignore": "Not yours: ignored"}[v.Verdict]
		if i, ok := at[k]; ok {
			if v.Verdict == "ignore" {
				drop[i] = true
				continue
			}
			films[i] = f
		} else if v.Verdict != "ignore" {
			films = append(films, f)
		}
	}
	out := films[:0]
	for i, f := range films {
		if !drop[i] {
			out = append(out, f)
		}
	}
	return out
}

type TastePick struct {
	tastedata.SuggestedTitle
	Score   float64  `json:"score"`
	Reason  string   `json:"reason"`
	Because []string `json:"because"`
	Seeds   []string `json:"seeds"`
	// lead is the creator behind a film the owner loved, when this pick
	// is their work.
	lead string
}

// Feature families: who made a title matters more than what genre it is.
var familyWeight = map[string]float64{"dir": 1.6, "wri": 1.2, "dop": 0.6, "mus": 0.4, "cast": 0.5, "kw": 0.35, "gen": 0.5, "cty": 0.4, "lang": 0.4, "dec": 0.3}

// featurePrior shrinks rare features toward neutral; one film by a director
// says more than one film carrying a common keyword or genre.
var featurePrior = map[string]float64{"dir": 1.5, "wri": 1.5, "dop": 1.5, "mus": 1.5, "cast": 1.5, "kw": 3, "gen": 6, "cty": 6, "lang": 6, "dec": 6}

func features(f tastedata.TitleFacts) []string {
	out := []string{}
	for _, p := range f.People {
		out = append(out, p.Role+":"+strconv.Itoa(p.ID))
	}
	for _, k := range f.Keywords {
		out = append(out, "kw:"+k)
	}
	for _, g := range f.Genres {
		out = append(out, "gen:"+g)
	}
	for _, c := range f.Countries {
		out = append(out, "cty:"+c)
	}
	if f.Language != "" {
		out = append(out, "lang:"+f.Language)
	}
	if len(f.Date) >= 4 {
		out = append(out, "dec:"+f.Date[:3]+"0s")
	}
	seen := map[string]bool{}
	uniq := out[:0]
	for _, v := range out {
		if !seen[v] {
			seen[v] = true
			uniq = append(uniq, v)
		}
	}
	return uniq
}

// bayes pulls thinly voted ratings toward an ordinary 6.5. New releases have
// had little time to collect votes, so they get a lighter prior and floor.
func bayes(rating float64, votes int, date string, now time.Time) float64 {
	prior := 400.0
	if fresh(date, now) {
		prior = 100
	}
	return (rating*float64(votes) + 6.5*prior) / (float64(votes) + prior)
}

func fresh(date string, now time.Time) bool {
	d, err := time.Parse("2006-01-02", date)
	return err == nil && d.After(now.AddDate(0, -18, 0))
}

type tasteFilm struct {
	title  tastedata.BrowseTitle
	signal float64
	stars  float64
	source string
	label  string
	// unknown marks a title known to be watched with no word on how it
	// went: excluded from picks, but not evidence about taste.
	unknown bool
}

// starSignal reads the owner's own scale: 3 is ok, 3.5 good, 4 very good,
// 4.5 amazing, 5 perfect. Only below 3 is a dislike.
var starSignal = map[float64]float64{5: 1, 4.5: 0.8, 4: 0.55, 3.5: 0.3, 3: 0}

func ratingSignal(r float64) float64 {
	if v, ok := starSignal[r]; ok {
		return v
	}
	if r > 3 {
		return 0.3
	}
	return math.Max(-1, (r-3)*0.6)
}

// signals scores each logged film on the owner's scale; a like adds a little.
func signals(p *letterboxd.Profile) []tasteFilm {
	out := []tasteFilm{}
	for _, f := range p.Films {
		if f.Match == nil || (f.Rating == nil && !f.Liked) {
			continue
		}
		v := tasteFilm{title: *f.Match, source: "letterboxd", signal: 0.6, label: "Liked on Letterboxd"}
		if f.Rating != nil {
			v.stars = *f.Rating
			v.label = "Rated " + strconv.FormatFloat(*f.Rating, 'f', -1, 64) + "/5 on Letterboxd"
			if f.Liked {
				v.label += ", liked"
			}
			v.signal = ratingSignal(*f.Rating)
			if f.Liked {
				v.signal = math.Min(1, v.signal+0.2)
			}
		}
		out = append(out, v)
	}
	return out
}

// tvShows reads series taste from how deep the owner went into each show.
// Plex history only ever adds evidence: shows leave Plex, get watched
// elsewhere or are revisited one episode at a time, so a low completion is
// never read as a dislike.
func tvShows(ctx context.Context, b *tasteBuilder, rows []tastedata.WatchTitle, now time.Time, rated map[string]bool) []tasteFilm {
	type show struct {
		first   tastedata.WatchTitle
		title   tastedata.BrowseTitle
		eps     map[[2]int]bool
		seasons map[int]bool
		recent  int
	}
	plays := map[string]*show{}
	for i, v := range rows {
		if v.Kind != "tv" || v.Percent < 85 {
			continue
		}
		s := plays[v.Key]
		if s == nil {
			s = &show{first: v, eps: map[[2]int]bool{}, seasons: map[int]bool{}}
			plays[v.Key] = s
		}
		ep := [2]int{v.Season, v.Episode}
		if v.Season == 0 && v.Episode == 0 {
			ep = [2]int{-1, i}
		}
		s.eps[ep] = true
		s.seasons[v.Season] = true
		if v.At.After(now.AddDate(0, 0, -60)) {
			s.recent++
		}
	}
	keys := []string{}
	for k, s := range plays {
		if len(s.eps) >= 3 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	each(ctx, len(keys), func(i int) { plays[keys[i]].title = b.identity(ctx, plays[keys[i]].first) })
	// Plex can hold one series under two entries; merge them by TMDB id.
	merged := map[string]*show{}
	for _, k := range keys {
		s := plays[k]
		if s.title.ID == 0 {
			continue
		}
		id := key("tv", s.title.ID)
		m := merged[id]
		if m == nil {
			m = &show{title: s.title, eps: map[[2]int]bool{}, seasons: map[int]bool{}}
			merged[id] = m
		}
		for ep := range s.eps {
			m.eps[ep] = true
		}
		for se := range s.seasons {
			m.seasons[se] = true
		}
		m.recent += s.recent
	}
	ids := []string{}
	for k := range merged {
		// A show rated on Letterboxd already speaks for itself.
		if !rated[k] {
			ids = append(ids, k)
		}
	}
	sort.Strings(ids)
	facts := make([]tastedata.TitleFacts, len(ids))
	each(ctx, len(ids), func(i int) { facts[i], _ = b.facts(ctx, "tv", merged[ids[i]].title.ID) })
	out := []tasteFilm{}
	for i, k := range ids {
		s := merged[k]
		if facts[i].Title != "" {
			s.title.Title = facts[i].Title
		}
		watched, aired := float64(len(s.eps)), float64(facts[i].Episodes)
		finished := aired > 0 && watched >= 0.8*aired
		signal, label := 0.0, ""
		switch {
		case finished && aired >= 6:
			signal, label = 0.8, "Finished"
		case (len(s.seasons) >= 2 && watched >= 10) || watched >= 15:
			signal, label = 0.6, fmt.Sprintf("%d seasons", len(s.seasons))
		case finished:
			signal, label = 0.5, "Finished"
		case s.recent >= 4:
			signal, label = 0.4, "Watching now"
		case watched >= 3:
			signal, label = 0.2, "Sampled"
		}
		if aired > 0 {
			label += fmt.Sprintf(" · %d of %d episodes on Plex", len(s.eps), facts[i].Episodes)
		} else {
			label += fmt.Sprintf(" · %d episodes on Plex", len(s.eps))
		}
		if signal > 0 && s.title.Title != "" {
			out = append(out, tasteFilm{title: s.title, signal: signal, source: "plex", label: label})
		}
	}
	return out
}

// plexFilms lists films played to the end on Plex: watched, so never
// recommended, but with no word on whether they were any good.
func plexFilms(ctx context.Context, b *tasteBuilder, rows []tastedata.WatchTitle) []tasteFilm {
	first := map[string]tastedata.WatchTitle{}
	keys := []string{}
	for _, v := range rows {
		if _, ok := first[v.Key]; v.Kind == "movie" && v.Percent >= 85 && !ok {
			first[v.Key] = v
			keys = append(keys, v.Key)
		}
	}
	sort.Strings(keys)
	titles := make([]tastedata.BrowseTitle, len(keys))
	each(ctx, len(keys), func(i int) { titles[i] = b.identity(ctx, first[keys[i]]) })
	out := []tasteFilm{}
	seen := map[string]bool{}
	for _, t := range titles {
		if k := key(t.Kind, t.ID); t.ID > 0 && !seen[k] {
			seen[k] = true
			out = append(out, tasteFilm{title: t, source: "plex", label: "Watched on Plex", unknown: true})
		}
	}
	return out
}

// identity resolves a Plex play to TMDB through Plex's own GUID. Old plays
// point at Plex items since removed; those fall back to an exact title (and
// year, when known) match, never a fuzzy one.
func (b *tasteBuilder) identity(ctx context.Context, v tastedata.WatchTitle) tastedata.BrowseTitle {
	rows := b.list(ctx, "plex:"+v.Kind+":"+v.Key, func() ([]tastedata.SuggestedTitle, error) {
		if b.history == nil {
			return nil, fmt.Errorf("no history")
		}
		t, err := b.history.WatchIdentity(ctx, v)
		if err != nil {
			return []tastedata.SuggestedTitle{}, nil
		}
		return []tastedata.SuggestedTitle{{BrowseTitle: t}}, nil
	})
	if len(rows) == 1 && rows[0].ID > 0 {
		return rows[0].BrowseTitle
	}
	title, year := v.Title, ""
	if v.Year > 0 {
		year = strconv.Itoa(v.Year)
	}
	if n := len(title); n > 7 && title[n-7:n-5] == " (" {
		title, year = title[:n-7], title[n-5:n-1]
	}
	found := b.list(ctx, "search:"+v.Kind+":"+normalized(title)+":"+year, func() ([]tastedata.SuggestedTitle, error) {
		rows, err := b.src.Search(ctx, title)
		out := []tastedata.SuggestedTitle{}
		for _, f := range rows {
			out = append(out, tastedata.SuggestedTitle{BrowseTitle: f})
		}
		return out, err
	})
	for _, r := range found {
		if r.Kind == v.Kind && normalized(r.Title) == normalized(title) && (year == "" || r.Year == year) {
			return r.BrowseTitle
		}
	}
	return tastedata.BrowseTitle{}
}

// tasteCache keeps public TMDB metadata between builds, so only new titles
// are fetched after the first (slow) build.
type tasteCache struct {
	Facts map[string]cachedFacts `json:"facts"`
	Lists map[string]cachedList  `json:"lists"`
}
type cachedFacts struct {
	At    time.Time            `json:"at"`
	Facts tastedata.TitleFacts `json:"facts"`
}
type cachedList struct {
	At   time.Time                  `json:"at"`
	Rows []tastedata.SuggestedTitle `json:"rows"`
}

type tasteBuilder struct {
	src     TasteSource
	history History
	cache   *tasteCache
	now     time.Time
	mu      sync.Mutex
	fails   int
}

func (b *tasteBuilder) facts(ctx context.Context, kind string, id int) (tastedata.TitleFacts, bool) {
	k := key(kind, id)
	b.mu.Lock()
	c, ok := b.cache.Facts[k]
	b.mu.Unlock()
	if ok && b.now.Sub(c.At) < 30*24*time.Hour {
		return c.Facts, true
	}
	f, err := b.src.TitleFacts(ctx, kind, id)
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.fails++
		return c.Facts, ok
	}
	b.cache.Facts[k] = cachedFacts{At: b.now, Facts: f}
	return f, true
}

func (b *tasteBuilder) list(ctx context.Context, k string, read func() ([]tastedata.SuggestedTitle, error)) []tastedata.SuggestedTitle {
	b.mu.Lock()
	c, ok := b.cache.Lists[k]
	b.mu.Unlock()
	if ok && b.now.Sub(c.At) < 14*24*time.Hour {
		return c.Rows
	}
	rows, err := read()
	b.mu.Lock()
	defer b.mu.Unlock()
	if err != nil {
		b.fails++
		return c.Rows
	}
	// Overviews are fetched with the facts of the few rows that survive.
	slim := make([]tastedata.SuggestedTitle, len(rows))
	for i, r := range rows {
		slim[i] = tastedata.SuggestedTitle{BrowseTitle: tastedata.BrowseTitle{ID: r.ID, Kind: r.Kind, Title: r.Title, Year: r.Year}, Rating: r.Rating, Votes: r.Votes, Adult: r.Adult, Date: r.Date}
	}
	b.cache.Lists[k] = cachedList{At: b.now, Rows: slim}
	return slim
}

// each runs fn over n items with four requests in flight.
func each(ctx context.Context, n int, fn func(int)) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i := 0; i < n && ctx.Err() == nil; i++ {
		sem <- struct{}{}
		wg.Add(1)
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			fn(i)
		}(i)
	}
	wg.Wait()
}

// BuildTaste ranks candidates three ways at once: how many loved titles point
// to a candidate, how closely its makers and themes match what the owner rates
// highly, and how its reception compares with the owner's own favourites.
// twins, when loaded, then ranks the films MovieLens knows instead.
func BuildTaste(ctx context.Context, src TasteSource, h History, owner int, now time.Time, profile *letterboxd.Profile, verdicts map[string]Verdict, cache *tasteCache, twins *twins.Model) (TasteModel, error) {
	if profile == nil || profile.Owner != owner {
		return TasteModel{}, fmt.Errorf("no profile for owner")
	}
	b := &tasteBuilder{src: src, history: h, cache: cache, now: now}
	films := signals(profile)
	have := map[string]bool{}
	for _, f := range films {
		have[key(f.title.Kind, f.title.ID)] = true
	}
	for _, f := range profile.Films {
		if k := ""; f.Match != nil && f.Watched {
			if k = key(f.Match.Kind, f.Match.ID); !have[k] {
				have[k] = true
				films = append(films, tasteFilm{title: *f.Match, source: "letterboxd", label: "Logged on Letterboxd", unknown: true})
			}
		}
	}
	var history []tastedata.WatchTitle
	if h != nil {
		history, _ = h.WatchHistory(ctx, owner)
	}
	for _, f := range append(tvShows(ctx, b, history, now, have), plexFilms(ctx, b, history)...) {
		if k := key(f.title.Kind, f.title.ID); !have[k] {
			have[k] = true
			films = append(films, f)
		}
	}
	films = applyVerdicts(films, verdicts)
	exclude := map[string]bool{}
	for _, f := range films {
		exclude[key(f.title.Kind, f.title.ID)] = true
	}
	for k, v := range verdicts {
		if v.Verdict == "ignore" {
			delete(exclude, k)
		}
	}
	known := knownTitles(films)
	// Titles watched with no opinion stay excluded but teach nothing.
	opinions := films[:0:0]
	for _, f := range films {
		if !f.unknown {
			opinions = append(opinions, f)
		}
	}
	films = opinions

	// 1. What the owner's ratings say about people, themes and reception.
	facts := make([]tastedata.TitleFacts, len(films))
	haveFacts := make([]bool, len(films))
	each(ctx, len(films), func(i int) { facts[i], haveFacts[i] = b.facts(ctx, films[i].title.Kind, films[i].title.ID) })
	sum, count := map[string]float64{}, map[string]float64{}
	names := map[string]string{}
	best := map[string]tasteFilm{}
	loved := []float64{}
	for i, f := range films {
		if !haveFacts[i] {
			continue
		}
		for _, p := range facts[i].People {
			names[p.Role+":"+strconv.Itoa(p.ID)] = p.Name
		}
		for _, ft := range features(facts[i]) {
			sum[ft] += f.signal
			count[ft]++
			if f.signal > best[ft].signal {
				best[ft] = f
			}
		}
		if f.signal > 0.4 && facts[i].Votes >= 100 {
			loved = append(loved, facts[i].Rating)
		}
	}
	affinity := map[string]float64{}
	for ft := range sum {
		affinity[ft] = sum[ft] / (count[ft] + featurePrior[family(ft)])
	}
	sort.Float64s(loved)
	floor := 7.0
	if len(loved) >= 5 {
		floor = math.Min(7.8, math.Max(6.5, loved[len(loved)/5]))
	}

	// 2. Candidates: recommendations of every film rated above average (and of
	// disliked ones, as a penalty), plus the work of the most-liked creators.
	type cand struct {
		row      tastedata.SuggestedTitle
		pos, neg float64
		from     []tasteFilm
	}
	cands := map[string]*cand{}
	eligible := func(r tastedata.SuggestedTitle) bool {
		date, err := time.Parse("2006-01-02", r.Date)
		votes := 300
		if fresh(r.Date, now) {
			votes = 80
		}
		return !r.Adult && r.Votes >= votes && r.Rating <= 10 && err == nil && !date.After(now) && bayes(r.Rating, r.Votes, r.Date, now) >= floorFor(floor, r.Kind)-0.6 && !exclude[key(r.Kind, r.ID)]
	}
	add := func(r tastedata.SuggestedTitle, w float64, from tasteFilm) {
		if !eligible(r) {
			return
		}
		k := key(r.Kind, r.ID)
		c := cands[k]
		if c == nil {
			c = &cand{row: r}
			cands[k] = c
		}
		if w < 0 {
			c.neg -= w
			return
		}
		c.pos += w
		c.from = append(c.from, from)
	}
	seeds := []tasteFilm{}
	for _, f := range films {
		if f.signal > 0 || f.signal < -0.4 {
			seeds = append(seeds, f)
		}
	}
	recs := make([][]tastedata.SuggestedTitle, len(seeds))
	each(ctx, len(seeds), func(i int) {
		t := seeds[i].title
		for page := 1; page <= 2; page++ {
			recs[i] = append(recs[i], b.list(ctx, fmt.Sprintf("rec:%s:%d:%d", t.Kind, t.ID, page), func() ([]tastedata.SuggestedTitle, error) {
				return src.RecommendationsPage(ctx, t.Kind, t.ID, page)
			})...)
		}
	})
	for i, rows := range recs {
		for rank, r := range rows {
			add(r, seeds[i].signal/(1+float64(rank)/10), seeds[i])
		}
	}
	people := []string{}
	for ft, a := range affinity {
		if fam := family(ft); (fam == "dir" || fam == "wri" || fam == "dop") && a > 0.15 {
			people = append(people, ft)
		}
	}
	sort.Slice(people, func(i, j int) bool {
		wi, wj := affinity[people[i]]*familyWeight[family(people[i])], affinity[people[j]]*familyWeight[family(people[j])]
		return wi > wj || (wi == wj && people[i] < people[j])
	})
	people = people[:min(40, len(people))]
	work := make([][]tastedata.SuggestedTitle, len(people))
	each(ctx, len(people), func(i int) {
		id, _ := strconv.Atoi(strings.SplitN(people[i], ":", 2)[1])
		work[i] = b.list(ctx, "person:"+strconv.Itoa(id), func() ([]tastedata.SuggestedTitle, error) { return src.PersonWork(ctx, id) })
	})
	for i, rows := range work {
		ft := people[i]
		for _, r := range rows {
			add(r, 0.5*affinity[ft]*familyWeight[family(ft)], best[ft])
		}
	}
	if ctx.Err() != nil {
		return TasteModel{}, ctx.Err()
	}

	// 3. Score the strongest candidates on their full metadata.
	pool := []*cand{}
	for _, c := range cands {
		if c.pos > 0 {
			pool = append(pool, c)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		if pool[i].pos == pool[j].pos {
			return key(pool[i].row.Kind, pool[i].row.ID) < key(pool[j].row.Kind, pool[j].row.ID)
		}
		return pool[i].pos > pool[j].pos
	})
	pool = pool[:min(1500, len(pool))]
	type scored struct {
		pick TastePick
		lead string
	}
	out := make([]*scored, len(pool))
	each(ctx, len(pool), func(i int) {
		c := pool[i]
		f, ok := b.facts(ctx, c.row.Kind, c.row.ID)
		if !ok || f.Poster == "" {
			return
		}
		content, lead, leadW := 0.0, "", 0.15
		for _, ft := range features(f) {
			w := familyWeight[family(ft)] * affinity[ft]
			content += w
			if fam := family(ft); (fam == "dir" || fam == "wri") && w > leadW {
				lead, leadW = ft, w
			}
		}
		score := math.Log1p(c.pos) - 1.2*math.Log1p(c.neg) + 0.9*content + 0.8*(bayes(f.Rating, f.Votes, f.Date, now)-floorFor(floor, f.Kind))
		if score <= 0 {
			return
		}
		pick := explain(TastePick{SuggestedTitle: f.SuggestedTitle, Score: score}, c.from, lead, names[lead], best[lead])
		if lead != "" && best[lead].signal >= 0.55 {
			pick.lead = lead
		}
		out[i] = &scored{pick: pick, lead: lead}
	})
	if ctx.Err() != nil {
		return TasteModel{}, ctx.Err()
	}
	if len(pool) > 0 && b.fails > len(pool)/2 {
		return TasteModel{}, fmt.Errorf("catalog unavailable: %d failed reads", b.fails)
	}
	ranked := []*scored{}
	for _, s := range out {
		if s != nil {
			ranked = append(ranked, s)
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].pick.Score == ranked[j].pick.Score {
			return key(ranked[i].pick.Kind, ranked[i].pick.ID) < key(ranked[j].pick.Kind, ranked[j].pick.ID)
		}
		return ranked[i].pick.Score > ranked[j].pick.Score
	})
	// One director's whole filmography should not fill the top of the feed:
	// each further title by the same lead creator counts for less.
	model := TasteModel{Owner: owner, ProfileHash: tasteHash(profile, verdicts, twins), BuiltAt: now.UTC(), Floor: floor, Seeds: []Seed{}, Picks: []TastePick{}, Known: known}
	// Films and series are chosen separately: series clear a higher floor and
	// would otherwise lose almost every slot to films.
	type chosen struct {
		pick TastePick
		v    float64
	}
	choose := func(kind string, limit int) []chosen {
		rows := []*scored{}
		for _, s := range ranked {
			if s.pick.Kind == kind {
				rows = append(rows, s)
			}
		}
		used := map[string]int{}
		out := []chosen{}
		for len(rows) > 0 && len(out) < limit {
			bestI, bestS := 0, math.Inf(-1)
			for i, s := range rows[:min(len(rows), 200)] {
				v := s.pick.Score / (1 + 0.6*float64(used[s.lead]))
				if v > bestS {
					bestI, bestS = i, v
				}
			}
			s := rows[bestI]
			if s.lead != "" {
				used[s.lead]++
			}
			out = append(out, chosen{s.pick, bestS})
			rows = append(rows[:bestI], rows[bestI+1:]...)
		}
		return out
	}
	movies, series := choose("movie", 400), choose("tv", 150)
	for len(movies) > 0 || len(series) > 0 {
		if len(series) == 0 || (len(movies) > 0 && movies[0].v >= series[0].v) {
			model.Picks, movies = append(model.Picks, movies[0].pick), movies[1:]
		} else {
			model.Picks, series = append(model.Picks, series[0].pick), series[1:]
		}
	}
	if twins != nil {
		model.Picks, model.Lanes = twinPicks(ctx, b, twins, films, exclude, model.Picks, now)
		model.Familiar = familiar(twins, model.Picks)
	}
	// Seed buttons: the loved titles behind the most picks.
	refs := map[string]int{}
	for _, p := range model.Picks {
		for _, k := range p.Seeds {
			refs[k]++
		}
	}
	sort.SliceStable(films, func(i, j int) bool {
		return refs[key(films[i].title.Kind, films[i].title.ID)] > refs[key(films[j].title.Kind, films[j].title.ID)]
	})
	for _, f := range films {
		if k := key(f.title.Kind, f.title.ID); refs[k] > 0 && len(model.Seeds) < 8 {
			model.Seeds = append(model.Seeds, Seed{Key: k, Title: f.title.Title, Kind: f.title.Kind, Source: f.source, Stars: f.stars})
		}
	}
	return model, nil
}

func family(ft string) string { return strings.SplitN(ft, ":", 2)[0] }

// floorFor raises the bar for series: TMDB rates them about half a point
// more generously than films, and the floor is learnt from films.
func floorFor(floor float64, kind string) float64 {
	if kind == "tv" {
		return floor + 0.5
	}
	return floor
}

func verb(f tasteFilm) string {
	switch {
	case f.source == "plex":
		return "watched"
	case f.source == "you" && f.signal < 0.75:
		return "liked"
	}
	return "loved"
}

// tasteHash changes whenever either source of explicit taste changes, or a
// different twins model is loaded.
func tasteHash(p *letterboxd.Profile, verdicts map[string]Verdict, twins *twins.Model) string {
	b, _ := json.Marshal(verdicts)
	h := letterboxd.PreferenceHash(p) + ":" + fmt.Sprintf("%x", sha256.Sum256(b))[:16]
	if twins != nil {
		h += ":" + fmt.Sprintf("%x", sha256.Sum256([]byte(twins.Source)))[:8]
	}
	return h
}

func explain(p TastePick, from []tasteFilm, lead, leadName string, leadFilm tasteFilm) TastePick {
	sort.SliceStable(from, func(i, j int) bool { return from[i].signal > from[j].signal })
	p.Because, p.Seeds = []string{}, []string{}
	for _, f := range from {
		k := key(f.title.Kind, f.title.ID)
		if f.title.ID == 0 || contains(p.Seeds, k) {
			continue
		}
		p.Seeds = append(p.Seeds, k)
		if len(p.Because) < 3 {
			p.Because = append(p.Because, f.title.Title)
		}
	}
	rated := func(f tasteFilm) string {

		if f.stars > 0 {
			return fmt.Sprintf("%s (%s/5)", f.title.Title, strconv.FormatFloat(f.stars, 'f', -1, 64))
		}
		return f.title.Title
	}
	switch {
	case lead != "" && leadName != "" && leadFilm.title.ID != 0:
		verb := map[string]string{"dir": "made", "wri": "wrote"}[family(lead)]
		p.Reason = fmt.Sprintf("From %s, who %s %s", leadName, verb, rated(leadFilm))
		if k := key(leadFilm.title.Kind, leadFilm.title.ID); !contains(p.Seeds, k) {
			p.Seeds = append(p.Seeds, k)
		}
	case len(from) >= 2:
		p.Reason = fmt.Sprintf("Because you %s %s and %s", verb(from[0]), rated(from[0]), rated(from[1]))
	case len(from) == 1:
		p.Reason = fmt.Sprintf("Because you %s %s", verb(from[0]), rated(from[0]))
	}
	return p
}

// Taste keeps the model current: it rebuilds daily and whenever the owner's
// Letterboxd preferences change, and persists both the model and the
// metadata cache so a restart serves the last model at once.
type Taste struct {
	Source      TasteSource
	History     History
	Owner       int
	ProfilePath string
	Dir         string
	Feedback    *Feedback
	// Twins, when loaded, ranks the films MovieLens knows (tastetwins).
	Twins  *twins.Model
	Log    func(string, ...any)
	mu     sync.Mutex
	model  *TasteModel
	loaded bool
	kick   chan struct{}
	// busy lets one build run at a time, whether daily, kicked or forced.
	busy chan struct{}
	once sync.Once
}

func (t *Taste) kicks() chan struct{} {
	t.once.Do(func() { t.kick, t.busy = make(chan struct{}, 1), make(chan struct{}, 1) })
	return t.kick
}

// Refresh asks the background loop to rebuild now, e.g. after a verdict.
func (t *Taste) Refresh() {
	if t == nil {
		return
	}
	select {
	case t.kicks() <- struct{}{}:
	default:
	}
}

// Stale reports whether explicit taste has changed since the model was built.
func (t *Taste) Stale() bool {
	m := t.Model()
	if m == nil {
		return true
	}
	profile, err := letterboxd.Load(t.ProfilePath)
	verdicts, ferr := t.verdicts()
	return err != nil || ferr != nil || m.ProfileHash != tasteHash(profile, verdicts, t.Twins)
}

func (t *Taste) verdicts() (map[string]Verdict, error) {
	if t.Feedback == nil {
		return map[string]Verdict{}, nil
	}
	return t.Feedback.Load()
}

func (t *Taste) modelPath() string { return t.Dir + "/taste-model.json" }
func (t *Taste) cachePath() string { return t.Dir + "/taste-cache.json" }

// Model returns the last good model, or nil before the first build.
func (t *Taste) Model() *TasteModel {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.loaded {
		t.loaded = true
		var m TasteModel
		if b, err := os.ReadFile(t.modelPath()); err == nil && json.Unmarshal(b, &m) == nil && m.Owner == t.Owner {
			t.model = &m
		}
	}
	return t.model
}

func (t *Taste) Run(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		if err := t.Step(ctx, time.Now()); err != nil && t.Log != nil {
			t.Log("taste model: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-t.kicks():
		}
	}
}

// Step rebuilds when the model is missing, a day old, or built from older
// preferences. A failed build keeps serving the previous model.
func (t *Taste) Step(ctx context.Context, now time.Time) error {
	return t.step(ctx, now, false)
}

// Rebuild builds now even when the model is current, so new Plex plays and
// catalog changes count without waiting a day. A model under a minute old is
// kept, so pressing Refresh twice costs one build.
func (t *Taste) Rebuild(ctx context.Context) error {
	if t == nil {
		return nil
	}
	return t.step(ctx, time.Now(), true)
}

func (t *Taste) step(ctx context.Context, now time.Time, force bool) error {
	t.kicks()
	select {
	case t.busy <- struct{}{}:
		defer func() { <-t.busy }()
	case <-ctx.Done():
		return ctx.Err()
	}
	profile, err := letterboxd.Load(t.ProfilePath)
	if err != nil || profile == nil || profile.Owner != t.Owner || t.Owner <= 0 {
		return err
	}
	verdicts, err := t.verdicts()
	if err != nil {
		return err
	}
	if m := t.Model(); m != nil && m.ProfileHash == tasteHash(profile, verdicts, t.Twins) {
		if age := now.Sub(m.BuiltAt); age < time.Minute || !force && age < 24*time.Hour {
			return nil
		}
	}
	cache := &tasteCache{Facts: map[string]cachedFacts{}, Lists: map[string]cachedList{}}
	if b, err := os.ReadFile(t.cachePath()); err == nil {
		_ = json.Unmarshal(b, cache)
		if cache.Facts == nil || cache.Lists == nil {
			cache = &tasteCache{Facts: map[string]cachedFacts{}, Lists: map[string]cachedList{}}
		}
	}
	work, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	started := time.Now()
	m, buildErr := BuildTaste(work, t.Source, t.History, t.Owner, now, profile, verdicts, cache, t.Twins)
	// Keep what was fetched even when the build failed; drop entries no
	// longer refreshed, so the cache tracks the current taste.
	for k, v := range cache.Facts {
		if now.Sub(v.At) > 90*24*time.Hour {
			delete(cache.Facts, k)
		}
	}
	for k, v := range cache.Lists {
		if now.Sub(v.At) > 60*24*time.Hour {
			delete(cache.Lists, k)
		}
	}
	if b, err := json.Marshal(cache); err == nil {
		if err := fsx.WriteAtomic(t.cachePath(), b, 0600); err != nil && t.Log != nil {
			t.Log("taste cache: %v", err)
		}
	}
	if buildErr != nil {
		return buildErr
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomic(t.modelPath(), b, 0600); err != nil {
		return err
	}
	t.mu.Lock()
	t.model = &m
	t.mu.Unlock()
	if t.Log != nil {
		t.Log("taste model: %d picks from %d seeds, quality floor %.2f, built in %s", len(m.Picks), len(m.Seeds), m.Floor, time.Since(started).Round(time.Second))
	}
	return nil
}
