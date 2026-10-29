package softstore

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/packages"
)

type Note struct {
	Scope      string `json:"scope"`
	Kind       string `json:"kind"`
	FromStatus string `json:"from_status,omitempty"`
	ToStatus   string `json:"to_status,omitempty"`
	Error      string `json:"error,omitempty"`
}
type Entry struct {
	FromCollectedAt time.Time         `json:"from_collected_at"`
	ToCollectedAt   time.Time         `json:"to_collected_at"`
	Changes         []packages.Change `json:"changes"`
	Notes           []Note            `json:"notes"`
}

// Changes compares consecutive change-only history snapshots, newest first.
// Equal references never load blobs. The initial snapshot is a baseline.
func (s *Store) Changes(host string, limit int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 {
		limit = 30
	}
	if limit > 365 {
		limit = 365
	}
	dir, err := s.hostDir(host)
	if err != nil {
		return nil, err
	}
	rows, err := s.history(dir, limit+1)
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for i := len(rows) - 1; i > 0; i-- {
		from, to := rows[i-1], rows[i]
		e := Entry{FromCollectedAt: from.CollectedAt, ToCollectedAt: to.CollectedAt, Changes: []packages.Change{}, Notes: []Note{}}
		type sample struct {
			set       Set
			container string
		}
		sets := func(v Snapshot) map[string]sample {
			m := map[string]sample{"host": {set: v.Software.Host}}
			for _, im := range v.Software.Images {
				m["image:"+im.ImageID] = sample{im.PackageSet, im.SampleContainer}
			}
			return m
		}
		a, b := sets(from), sets(to)
		scopes := []string{}
		for scope := range a {
			scopes = append(scopes, scope)
		}
		for scope := range b {
			if _, ok := a[scope]; !ok {
				scopes = append(scopes, scope)
			}
		}
		sort.Strings(scopes)
		for _, scope := range scopes {
			prev, was := a[scope]
			next, now := b[scope]
			note := Note{Scope: scope, FromStatus: prev.set.Status, ToStatus: next.set.Status, Error: next.set.Error}
			switch {
			case !was:
				note.Kind = "appeared"
			case !now:
				note.Kind = "disappeared"
			case prev.set.Status == "unsupported" && next.set.Status == "unsupported":
				// A distroless image stays without a package database; not news.
				continue
			case prev.set.Status != "ok" || next.set.Status != "ok":
				note.Kind = "incomplete"
			case prev.container != next.container:
				note.Kind = "sample_changed"
			default:
				if prev.set.PackagesRef != next.set.PackagesRef {
					old, err := s.loadSet(prev.set)
					if err != nil {
						return nil, err
					}
					fresh, err := s.loadSet(next.set)
					if err != nil {
						return nil, err
					}
					e.Changes = append(e.Changes, packages.Diff(scope, old.Packages, fresh.Packages)...)
				}
				continue
			}
			e.Notes = append(e.Notes, note)
		}
		aRuntime, _ := json.Marshal(from.Software.Runtimes)
		bRuntime, _ := json.Marshal(to.Software.Runtimes)
		if string(aRuntime) != string(bRuntime) {
			e.Notes = append(e.Notes, Note{Scope: "runtimes", Kind: "changed"})
		}
		out = append(out, e)
	}
	return out, nil
}
