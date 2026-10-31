package engine

import (
	"encoding/json"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"os"
	"sort"
	"sync"
)

// Preferences contains only explicit TV dismissals, independent of the imported
// Letterboxd baseline. A failed write never reports a dismissal as saved.
type Preferences struct {
	Path  string
	Owner int
	mu    sync.Mutex
}
type savedPreferences struct {
	Owner  int             `json:"owner"`
	Hidden map[string]bool `json:"hidden"`
}

func (p *Preferences) load() (savedPreferences, error) {
	v := savedPreferences{Owner: p.Owner, Hidden: map[string]bool{}}
	b, err := os.ReadFile(p.Path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if len(b) > 512*1024 || json.Unmarshal(b, &v) != nil || v.Owner != p.Owner || v.Hidden == nil {
		return v, fmt.Errorf("invalid discovery preferences")
	}
	return v, nil
}
func (p *Preferences) Set(kind string, id int, hide bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Owner <= 0 || (kind != "movie" && kind != "tv") || id <= 0 || id >= 1000000000 {
		return fmt.Errorf("invalid preference")
	}
	v, err := p.load()
	if err != nil {
		return err
	}
	k := key(kind, id)
	if hide {
		if len(v.Hidden) >= 10000 && !v.Hidden[k] {
			return fmt.Errorf("preference limit")
		}
		v.Hidden[k] = true
	} else {
		delete(v.Hidden, k)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(p.Path, b, 0600)
}
func (p *Preferences) Apply(r Report) (Report, error) {
	if p == nil {
		return r, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v, err := p.load()
	if err != nil {
		return r, err
	}
	r.Hidden = []string{}
	for k, hide := range v.Hidden {
		if hide {
			r.Hidden = append(r.Hidden, k)
		}
	}
	sort.Strings(r.Hidden)
	filter := func(rows []Pick) []Pick {
		out := make([]Pick, 0, len(rows))
		for _, t := range rows {
			if !v.Hidden[key(t.Kind, t.ID)] {
				out = append(out, t)
			}
		}
		return out
	}
	r.Titles = filter(r.Titles)
	r.Recent = filter(r.Recent)
	lanes := r.Lanes
	r.Lanes = nil
	for _, l := range lanes {
		l.Titles = filter(l.Titles)
		r.Lanes = append(r.Lanes, l)
	}
	// A personal watchlist is explicit intent and remains independently accessible.
	return r, nil
}
