// Package duskpersonal stores profile-local feedback and builds content recommendations.
package duskpersonal

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ficaa1/homelab-inventory/internal/mediadata"
)

var ErrInvalid = errors.New("invalid personal discovery input")

type feedback struct {
	Kind    string
	ID      int
	Verdict string
	Title   string
	Year    string
}
type imported struct {
	Name    string
	Year    string
	Verdict string
}
type personal struct {
	Hidden       map[string]bool
	Feedback     map[string]feedback
	Imports      []imported
	ImportCursor int
}
type Store struct {
	mu   sync.Mutex
	path string
	data map[string]personal
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "personal.json"), data: map[string]personal{}}
	b, e := os.ReadFile(s.path)
	if e == nil {
		e = json.Unmarshal(b, &s.data)
		if s.data == nil {
			s.data = map[string]personal{}
		}
	}
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	return s, nil
}
func validProfile(p string) bool     { return strings.TrimSpace(p) != "" && len(p) <= 128 }
func valid(kind string, id int) bool { return (kind == "movie" || kind == "tv") && id > 0 }
func key(kind string, id int) string { return fmt.Sprintf("%s:%d", kind, id) }
func (s *Store) snapshot(p string) personal {
	b, _ := json.Marshal(s.data[p])
	var v personal
	_ = json.Unmarshal(b, &v)
	if v.Hidden == nil {
		v.Hidden = map[string]bool{}
	}
	if v.Feedback == nil {
		v.Feedback = map[string]feedback{}
	}
	return v
}
func (s *Store) save(p string, v personal) error {
	all := make(map[string]personal, len(s.data)+1)
	for k, x := range s.data {
		all[k] = x
	}
	all[p] = v
	b, e := json.Marshal(all)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(s.path), ".personal-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(f.Name(), s.path); e != nil {
		return e
	}
	s.data = all
	return nil
}
func (s *Store) SetHidden(profile, kind string, id int, hidden bool) error {
	if !validProfile(profile) || !valid(kind, id) {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.snapshot(profile)
	k := key(kind, id)
	if hidden {
		if len(v.Hidden) >= 5000 && !v.Hidden[k] {
			return ErrInvalid
		}
		v.Hidden[k] = true
	} else {
		delete(v.Hidden, k)
	}
	return s.save(profile, v)
}
func (s *Store) Verdict(profile, kind string, id int, verdict, title, year string) error {
	if !validProfile(profile) || !valid(kind, id) || len(title) > 300 || len(year) > 4 {
		return ErrInvalid
	}
	switch verdict {
	case "", "loved", "liked", "meh", "disliked", "seen", "pass", "skip", "undo":
	default:
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.snapshot(profile)
	k := key(kind, id)
	if verdict == "undo" || verdict == "" {
		delete(v.Feedback, k)
	} else {
		if strings.TrimSpace(title) == "" || len(v.Feedback) >= 5000 && v.Feedback[k].ID == 0 {
			return ErrInvalid
		}
		v.Feedback[k] = feedback{kind, id, verdict, title, year}
	}
	return s.save(profile, v)
}

// Import accepts pasted Letterboxd CSV only; URLs are neither fetched nor retained.
func (s *Store) Import(profile, input string) (map[string]int, error) {
	if !validProfile(profile) || len(input) > 65536 {
		return nil, ErrInvalid
	}
	r := csv.NewReader(strings.NewReader(input))
	header, e := r.Read()
	if e != nil {
		return nil, ErrInvalid
	}
	columns := map[string]int{}
	for i, h := range header {
		columns[strings.ToLower(strings.TrimSpace(h))] = i
	}
	nameIdx, ok := columns["name"]
	if !ok {
		return nil, ErrInvalid
	}
	get := func(row []string, k string) string {
		if i, ok := columns[k]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	items := []imported{}
	seen := map[string]bool{}
	rows := 0
	for {
		row, e := r.Read()
		if e == io.EOF {
			break
		}
		rows++
		if e != nil || rows > 500 {
			return nil, ErrInvalid
		}
		name := strings.TrimSpace(row[nameIdx])
		year := get(row, "year")
		if name == "" || len(name) > 300 || len(year) > 4 {
			return nil, ErrInvalid
		}
		verdict := "seen"
		if x := strings.ToLower(get(row, "watched")); x == "false" || x == "no" || x == "0" {
			verdict = "watchlist"
		}
		if x := strings.ToLower(get(row, "watchlist")); x == "true" || x == "yes" || x == "1" {
			verdict = "watchlist"
		}
		if x := get(row, "rating"); x != "" {
			rating, e := strconv.ParseFloat(x, 64)
			if e != nil || math.IsNaN(rating) || math.IsInf(rating, 0) || rating < 0 || rating > 5 {
				return nil, ErrInvalid
			}
			switch {
			case rating >= 4.5:
				verdict = "loved"
			case rating >= 3.5:
				verdict = "liked"
			case rating >= 2.5:
				verdict = "meh"
			default:
				verdict = "disliked"
			}
		}
		k := strings.ToLower(name) + "|" + year
		if !seen[k] {
			items = append(items, imported{name, year, verdict})
			seen[k] = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.snapshot(profile)
	v.Imports = items
	v.ImportCursor = 0
	if e = s.save(profile, v); e != nil {
		return nil, e
	}
	return map[string]int{"imported": len(items)}, nil
}
func positive(v string) bool { return v == "loved" || v == "liked" }
func allTitles(r mediadata.Discovery) []mediadata.Title {
	out := append([]mediadata.Title{}, r.Titles...)
	out = append(out, r.Recent...)
	return append(out, r.Watchlist...)
}
func resolve(v personal, r mediadata.Discovery) personal {
	for _, t := range allTitles(r) {
		for _, i := range v.Imports {
			if t.Kind == "movie" && strings.EqualFold(t.Title, i.Name) && (i.Year == "" || i.Year == t.Year) {
				k := key(t.Kind, t.ID)
				if _, ok := v.Feedback[k]; !ok {
					v.Feedback[k] = feedback{t.Kind, t.ID, i.Verdict, t.Title, t.Year}
				}
			}
		}
	}
	return v
}
func features(t mediadata.Title) map[string]bool {
	m := map[string]bool{}
	for _, list := range [][]string{t.Directors, t.Creators, t.Cast} {
		for _, x := range list {
			m[strings.ToLower(x)] = true
		}
	}
	for _, x := range strings.Fields(strings.ToLower(t.Overview)) {
		x = strings.Trim(x, ".,!?;:()\"")
		if len(x) > 4 {
			m[x] = true
		}
	}
	return m
}

// Apply fetches at most three seed expansions and never holds storage locks over I/O.
func (s *Store) Apply(ctx context.Context, profile string, report mediadata.Discovery, similar func(context.Context, string, int) (mediadata.Discovery, error)) (mediadata.Discovery, error) {
	if !validProfile(profile) {
		return report, ErrInvalid
	}
	s.mu.Lock()
	v := resolve(s.snapshot(profile), report)
	e := s.save(profile, v)
	s.mu.Unlock()
	if e != nil {
		return report, e
	}
	seeds := []feedback{}
	for _, f := range v.Feedback {
		if positive(f.Verdict) && !v.Hidden[key(f.Kind, f.ID)] {
			seeds = append(seeds, f)
		}
	}
	sort.Slice(seeds, func(i, j int) bool {
		if seeds[i].Verdict != seeds[j].Verdict {
			return seeds[i].Verdict == "loved"
		}
		return key(seeds[i].Kind, seeds[i].ID) < key(seeds[j].Kind, seeds[j].ID)
	})

	history := map[string]bool{}
	sources := map[string]string{}
	for _, f := range seeds {
		sources[key(f.Kind, f.ID)] = "taste"
	}
	for _, t := range report.Recent {
		if !valid(t.Kind, t.ID) {
			continue
		}
		k := key(t.Kind, t.ID)
		history[k] = true
		if len(seeds) >= 3 || sources[k] != "" || v.Hidden[k] {
			continue
		}
		if f, ok := v.Feedback[k]; ok && !positive(f.Verdict) {
			continue
		}
		seeds = append(seeds, feedback{Kind: t.Kind, ID: t.ID, Title: t.Title, Year: t.Year})
		sources[k] = "plex"
	}
	candidates := append([]mediadata.Title{}, report.Titles...)
	affinity := map[string]int{}
	seedFeatures := map[string]bool{}
	for _, t := range allTitles(report) {
		if sources[key(t.Kind, t.ID)] != "" {
			for x := range features(t) {
				seedFeatures[x] = true
			}
		}
	}
	for i, f := range seeds {
		if i >= 3 {
			break
		}
		report.Seeds = append(report.Seeds, mediadata.Seed{Key: key(f.Kind, f.ID), Title: f.Title, Kind: f.Kind, Source: sources[key(f.Kind, f.ID)]})
		if similar == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		more, err := similar(ctx, f.Kind, f.ID)
		if err != nil {
			report.Partial = true
			continue
		}
		for j, t := range more.Titles {
			if j >= 100 {
				break
			}
			k := key(t.Kind, t.ID)
			affinity[k] += 10
			candidates = append(candidates, t)
		}
	}
	seen := map[string]bool{}
	out := []mediadata.Title{}
	score := map[string]int{}
	for _, t := range candidates {
		k := key(t.Kind, t.ID)
		if !valid(t.Kind, t.ID) || seen[k] || v.Hidden[k] || history[k] {
			continue
		}
		seen[k] = true
		if f, ok := v.Feedback[k]; ok && f.Verdict != "watchlist" {
			continue
		}
		for x := range features(t) {
			if seedFeatures[x] {
				score[k]++
			}
		}
		score[k] += affinity[k]
		if affinity[k] > 0 {
			t.Because = append(t.Because, "Similar to your viewing and taste")
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return score[key(out[i].Kind, out[i].ID)] > score[key(out[j].Kind, out[j].ID)] })
	if len(out) > 100 {
		out = out[:100]
	}
	report.Titles = out
	report.Hidden = []string{}
	for k := range v.Hidden {
		report.Hidden = append(report.Hidden, k)
	}
	sort.Strings(report.Hidden)
	filter := func(titles []mediadata.Title) []mediadata.Title {
		out := []mediadata.Title{}
		for _, t := range titles {
			if !v.Hidden[key(t.Kind, t.ID)] {
				out = append(out, t)
			}
		}
		return out
	}
	report.Recent = filter(report.Recent)

	report.Watchlist = filter(report.Watchlist)
	// Imported watchlists remain useful even when their titles leave trending.
	metadata := map[string]mediadata.Title{}
	for _, t := range allTitles(report) {
		metadata[key(t.Kind, t.ID)] = t
	}
	watchSeen := map[string]bool{}
	watchlist := []mediadata.Title{}
	for _, t := range report.Watchlist {
		k := key(t.Kind, t.ID)
		if !watchSeen[k] && len(watchlist) < 100 {
			watchSeen[k] = true
			watchlist = append(watchlist, t)
		}
	}
	keys := []string{}
	for k, f := range v.Feedback {
		if f.Verdict == "watchlist" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if watchSeen[k] || v.Hidden[k] || len(watchlist) >= 100 {
			continue
		}
		f := v.Feedback[k]
		t, ok := metadata[k]
		if !ok {
			t = mediadata.Title{ID: f.ID, Kind: f.Kind, Title: f.Title, Year: f.Year, Availability: "unknown"}
		}
		watchSeen[k] = true
		watchlist = append(watchlist, t)
	}
	report.Watchlist = watchlist
	if report.Status == "" && len(report.Titles) > 0 {
		report.Status = "ok"
	}
	return report, nil
}
func (s *Store) Taste(profile, kind string, report mediadata.Discovery, familiar bool) any {
	s.mu.Lock()
	v := resolve(s.snapshot(profile), report)
	s.mu.Unlock()
	titles := []mediadata.Title{}
	seen := map[string]bool{}
	source := report.Titles
	if familiar {
		source = append([]mediadata.Title{}, report.Recent...)
		for _, t := range report.Titles {
			if t.State == "available" || t.Availability == "available" {
				source = append(source, t)
			}
		}
	}
	for _, t := range source {
		k := key(t.Kind, t.ID)
		if t.Kind != kind || seen[k] || v.Hidden[k] {
			continue
		}
		seen[k] = true
		if _, ok := v.Feedback[k]; ok {
			continue
		}
		titles = append(titles, t)
		if len(titles) >= 50 {
			break
		}
	}
	if familiar {
		return map[string]any{"titles": titles, "judged": len(v.Feedback)}
	}
	return map[string]any{"picks": titles, "judged": len(v.Feedback)}
}

// Enrich resolves at most three imported names through the shared Media search.
// Its persistent cursor advances even on failed lookups, avoiding repeated remote
// work for unavailable titles. Reimporting a CSV explicitly starts a new pass.
func (s *Store) Enrich(ctx context.Context, profile string, search func(context.Context, string) ([]mediadata.Title, error)) error {
	if !validProfile(profile) || search == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	v := s.snapshot(profile)
	pending := []imported{}
	for v.ImportCursor < len(v.Imports) && len(pending) < 3 {
		item := v.Imports[v.ImportCursor]
		v.ImportCursor++
		resolved := false
		for _, f := range v.Feedback {
			if f.Kind == "movie" && strings.EqualFold(f.Title, item.Name) && (item.Year == "" || item.Year == f.Year) {
				resolved = true
				break
			}
		}
		if !resolved {
			pending = append(pending, item)
		}
	}
	err := s.save(profile, v)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	var first error
	for _, item := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		titles, err := search(ctx, item.Name)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if len(titles) > 100 {
			titles = titles[:100]
		}
		matches := []mediadata.Title{}
		for _, t := range titles {
			if valid(t.Kind, t.ID) && t.Kind == "movie" && strings.EqualFold(t.Title, item.Name) && (item.Year == "" || item.Year == t.Year) {
				matches = append(matches, t)
			}
		}
		s.mu.Lock()
		current := resolve(s.snapshot(profile), mediadata.Discovery{Titles: matches})
		err = s.save(profile, current)
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return first
}
