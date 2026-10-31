// Package twins scores optional, owner-supplied sparse recommendation artifacts.
package twins

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// Film is one MovieLens film the model knows.
type Film struct {
	TMDB  int
	Title string
	Count int     // ratings in MovieLens
	Mean  float32 // average MovieLens rating
}

// Weight is one learnt link from a film to another.
type Weight struct {
	To int32
	W  float32
}

// Model is the sparse EASE result: for each film, its strongest links.
type Model struct {
	Source string // dataset and parameters, for the log
	Films  []Film
	Rows   [][]Weight
	byTMDB map[int]int32
}

// Film returns the film with a TMDB id.
func (m *Model) Film(tmdb int) (int, bool) {
	if m.byTMDB == nil {
		m.byTMDB = make(map[int]int32, len(m.Films))
		for i, f := range m.Films {
			m.byTMDB[f.TMDB] = int32(i)
		}
	}
	i, ok := m.byTMDB[tmdb]
	return int(i), ok
}

// Score sums each film's links from the owner's films, weighted by what the
// owner thought of them (positive for loved, negative for disliked).
func (m *Model) Score(owner map[int]float64) []float64 {
	score := make([]float64, len(m.Films))
	for tm, w := range owner {
		i, ok := m.Film(tm)
		if !ok || w == 0 {
			continue
		}
		for _, e := range m.Rows[i] {
			score[e.To] += w * float64(e.W)
		}
	}
	return score
}

// Because lists the owner's films that contribute most to film j's score.
func (m *Model) Because(owner map[int]float64, j int, n int) []int {
	type c struct {
		tmdb int
		v    float64
	}
	all := []c{}
	for tm, w := range owner {
		i, ok := m.Film(tm)
		if !ok || w <= 0 {
			continue
		}
		for _, e := range m.Rows[i] {
			if int(e.To) == j && e.W > 0 {
				all = append(all, c{tm, w * float64(e.W)})
			}
		}
	}
	sort.Slice(all, func(a, b int) bool { return all[a].v > all[b].v || (all[a].v == all[b].v && all[a].tmdb < all[b].tmdb) })
	out := []int{}
	for _, x := range all[:min(n, len(all))] {
		out = append(out, x.tmdb)
	}
	return out
}

// Similarity is the cosine between two films' outgoing links: films that
// point at the same things belong to the same taste.
func (m *Model) Similarity(i, j int) float64 {
	a := map[int32]float64{}
	na, nb, dot := 0.0, 0.0, 0.0
	for _, e := range m.Rows[i] {
		a[e.To] = float64(e.W)
		na += float64(e.W) * float64(e.W)
	}
	for _, e := range m.Rows[j] {
		nb += float64(e.W) * float64(e.W)
		dot += a[e.To] * float64(e.W)
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

// LoadJSON deliberately never decodes legacy gob artifacts during restore.
func LoadJSON(path string) (*Model, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (2<<30)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > 2<<30 {
		return nil, fmt.Errorf("artifact too large")
	}
	var m Model
	if json.Unmarshal(b, &m) != nil || len(m.Films) > 20000 || len(m.Films) != len(m.Rows) {
		return nil, fmt.Errorf("invalid artifact")
	}
	seen := map[int]bool{}
	for i, f := range m.Films {
		if f.TMDB <= 0 || seen[f.TMDB] || len(m.Rows[i]) > 2000 {
			return nil, fmt.Errorf("invalid artifact")
		}
		seen[f.TMDB] = true
		for _, w := range m.Rows[i] {
			if w.To < 0 || int(w.To) >= len(m.Films) || math.IsNaN(float64(w.W)) || math.IsInf(float64(w.W), 0) {
				return nil, fmt.Errorf("invalid artifact")
			}
		}
	}
	m.Film(0)
	return &m, nil
}
