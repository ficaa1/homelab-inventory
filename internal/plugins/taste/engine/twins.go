package engine

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/twins"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

// TasteLane is one strand of the owner's taste: the loved films that point
// at the same things, and the picks they explain best.
type TasteLane struct {
	Title   string   `json:"title"`
	Names   []string `json:"names"` // the three most loved anchors
	Anchors []string `json:"anchors"`
	Picks   []string `json:"picks"`
}

// twinWeights marks the films the owner loved (4 stars, a like, or a loved
// or liked verdict). The hold-out evaluation found plain marks beat graded
// stars: the twins model learnt from MovieLens 4+ ratings, the same mark.
func twinWeights(tw *twins.Model, films []tasteFilm) (map[int]float64, map[int]tasteFilm) {
	w, by := map[int]float64{}, map[int]tasteFilm{}
	for _, f := range films {
		if f.title.Kind != "movie" {
			continue
		}
		if _, ok := tw.Film(f.title.ID); !ok {
			continue
		}
		by[f.title.ID] = f
		if f.signal >= 0.5 {
			w[f.title.ID] = 1
		}
	}
	return w, by
}

// twinAcclaim lifts films MovieLens raters liked overall, as a share of the
// strongest twin score per star above 3.5. The hold-out evaluation found it
// neither helps nor hurts recall at 0.1-0.3.
var twinAcclaim = 0.0

var twinDebug func(string, ...any)

// twinPicks ranks the films MovieLens knows by what the people who loved the
// owner's favourites also loved, and interleaves them with the content
// model's picks for everything MovieLens cannot know: series, and films too
// new or too rare to have 500 ratings.
func twinPicks(ctx context.Context, b *tasteBuilder, tw *twins.Model, films []tasteFilm, exclude map[string]bool, content []TastePick, now time.Time) ([]TastePick, []TasteLane) {
	owner, by := twinWeights(tw, films)
	raw := tw.Score(owner)
	score := append([]float64(nil), raw...)
	if twinAcclaim > 0 {
		hi := 0.0
		for _, s := range raw {
			hi = math.Max(hi, s)
		}
		for j, f := range tw.Films {
			c := float64(f.Count)
			score[j] += twinAcclaim * hi * ((float64(f.Mean)*c+3.5*2000)/(c+2000) - 3.5)
		}
	}
	order := []int{}
	for j, f := range tw.Films {
		if _, mine := by[f.TMDB]; !mine && !exclude[key("movie", f.TMDB)] && score[j] > 0 {
			order = append(order, j)
		}
	}
	sort.Slice(order, func(a, c int) bool {
		return score[order[a]] > score[order[c]] || (score[order[a]] == score[order[c]] && order[a] < order[c])
	})
	order = order[:min(500, len(order))]
	facts := make([]tastedata.TitleFacts, len(order))
	ok := make([]bool, len(order))
	each(ctx, len(order), func(i int) {
		facts[i], ok[i] = b.facts(ctx, "movie", tw.Films[order[i]].TMDB)
	})
	twins := []TastePick{}
	at := map[int]int{} // model index -> position in twins
	for i, j := range order {
		f := facts[i]
		date, err := time.Parse("2006-01-02", f.Date)
		if !ok[i] || f.Poster == "" || f.Adult || err != nil || date.After(now) {
			continue
		}
		from := []tasteFilm{}
		for _, id := range tw.Because(owner, j, 3) {
			from = append(from, by[id])
		}
		at[j] = len(twins)
		twins = append(twins, explain(TastePick{SuggestedTitle: f.SuggestedTitle, Score: score[j]}, from, "", "", tasteFilm{}))
	}
	// The content model keeps what MovieLens has no opinion on: series, and
	// films too new or too rare for it, when they come from a creator behind
	// a film the owner loved. TMDB's own recommendations for new films lean
	// on franchise and genre (a Ratatouille fan gets the new Mario film).
	rest := []TastePick{}
	for _, p := range content {
		_, known := tw.Film(p.ID)
		if twinDebug != nil && p.Kind == "movie" && !known {
			twinDebug("unknown film %s (%s), lead %q", p.Title, p.Year, p.lead)
		}
		if p.Kind == "tv" || (!known && p.lead != "") {
			rest = append(rest, p)
		}
	}
	out := make([]TastePick, 0, 400)
	i, k := 0, 0
	for len(out) < 400 && (i < len(twins) || k < len(rest)) {
		if i < len(twins) && (len(out)%3 != 2 || k >= len(rest)) {
			out = append(out, twins[i])
			i++
		} else {
			out = append(out, rest[k])
			k++
		}
	}
	// Series past the mixed cap stay in the model for the Shows lists.
	for ; k < len(rest); k++ {
		if rest[k].Kind == "tv" {
			out = append(out, rest[k])
		}
	}
	shown := map[int]bool{}
	for _, p := range out {
		if p.Kind == "movie" {
			shown[p.ID] = true
		}
	}
	for j, i := range at {
		if !shown[twins[i].ID] {
			delete(at, j)
		}
	}
	return out, twinLanes(tw, owner, by, raw, order, at, twins)
}

// twinLanes groups the loved films by what they point at (average-linkage
// clustering on the similarity of their links), then gives each pick to the
// lane that explains the largest share of its score.
func twinLanes(tw *twins.Model, owner map[int]float64, by map[int]tasteFilm, total []float64, order []int, at map[int]int, picks []TastePick) []TasteLane {
	loved := []int{}
	for id := range owner {
		loved = append(loved, id)
	}
	sort.Ints(loved)
	n := len(loved)
	if n < 6 {
		return nil
	}
	idx := make([]int, n)
	for i, id := range loved {
		idx[i], _ = tw.Film(id)
	}
	sim := make([][]float64, n)
	for i := range sim {
		sim[i] = make([]float64, n)
		for j := 0; j < i; j++ {
			sim[i][j] = tw.Similarity(idx[i], idx[j])
			sim[j][i] = sim[i][j]
		}
	}
	clusters := make([][]int, n)
	for i := range clusters {
		clusters[i] = []int{i}
	}
	alive := make([]bool, n)
	for i := range alive {
		alive[i] = true
	}
	// Merge until seven strands remain; average linkage keeps one broad
	// strand from swallowing the rest.
	for groups := n; groups > 7; groups-- {
		bi, bj, best := -1, -1, math.Inf(-1)
		for i := 0; i < n; i++ {
			for j := i + 1; alive[i] && j < n; j++ {
				if alive[j] && sim[i][j] > best {
					bi, bj, best = i, j, sim[i][j]
				}
			}
		}
		si, sj := float64(len(clusters[bi])), float64(len(clusters[bj]))
		for k := 0; k < n; k++ {
			if alive[k] && k != bi && k != bj {
				sim[bi][k] = (si*sim[bi][k] + sj*sim[bj][k]) / (si + sj)
				sim[k][bi] = sim[bi][k]
			}
		}
		clusters[bi] = append(clusters[bi], clusters[bj]...)
		clusters[bj], alive[bj] = nil, false
	}
	type lane struct {
		members []int
		score   []float64
	}
	lanes := []lane{}
	for _, c := range clusters {
		if len(c) < 3 { // a lone film is an accident of history, not a strand
			continue
		}
		w := map[int]float64{}
		for _, m := range c {
			w[loved[m]] = owner[loved[m]]
		}
		lanes = append(lanes, lane{c, tw.Score(w)})
	}
	sort.SliceStable(lanes, func(i, j int) bool { return len(lanes[i].members) > len(lanes[j].members) })
	out := make([]TasteLane, len(lanes))
	for li, l := range lanes {
		anchors := append([]int(nil), l.members...)
		sort.Slice(anchors, func(a, c int) bool {
			fa, fc := by[loved[anchors[a]]], by[loved[anchors[c]]]
			ca, cc := tw.Films[idx[anchors[a]]].Count, tw.Films[idx[anchors[c]]].Count
			return fa.signal > fc.signal || (fa.signal == fc.signal && ca > cc)
		})
		out[li].Anchors = []string{}
		names := []string{}
		for _, m := range anchors {
			out[li].Anchors = append(out[li].Anchors, key("movie", loved[m]))
			if len(names) < 3 {
				names = append(names, by[loved[m]].title.Title)
			}
		}
		out[li].Title, out[li].Names = "Because you loved "+joinTitles(names), names
		out[li].Picks = []string{}
	}
	for _, j := range order {
		p, ok := at[j]
		if !ok {
			continue
		}
		best, share := -1, 0.0
		for li, l := range lanes {
			if s := l.score[j] / total[j]; s > share {
				best, share = li, s
			}
		}
		if best >= 0 && len(out[best].Picks) < 40 {
			out[best].Picks = append(out[best].Picks, key("movie", picks[p].ID))
		}
	}
	return out
}

func joinTitles(t []string) string {
	switch len(t) {
	case 0:
		return ""
	case 1:
		return t[0]
	}
	s := t[0]
	for _, x := range t[1 : len(t)-1] {
		s += ", " + x
	}
	return s + " and " + t[len(t)-1]
}

// familiar lists the twin picks the owner has most likely seen already: the
// most-rated films among the strongest matches. Asking about these first
// clears the obvious classics out of the feed fastest.
func familiar(tw *twins.Model, picks []TastePick) []string {
	type f struct {
		k     string
		count int
	}
	all := []f{}
	for _, p := range picks[:min(300, len(picks))] {
		if j, ok := tw.Film(p.ID); ok && p.Kind == "movie" {
			all = append(all, f{key("movie", p.ID), tw.Films[j].Count})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].count > all[j].count })
	out := []string{}
	for _, x := range all[:min(120, len(all))] {
		out = append(out, x.k)
	}
	// MovieLens has no series; the most-voted series picks stand in for
	// the shows the owner has most likely seen.
	shows := []TastePick{}
	for _, p := range picks {
		if p.Kind == "tv" {
			shows = append(shows, p)
		}
	}
	sort.SliceStable(shows, func(i, j int) bool { return shows[i].Votes > shows[j].Votes })
	for _, p := range shows[:min(60, len(shows))] {
		out = append(out, key("tv", p.ID))
	}
	return out
}
