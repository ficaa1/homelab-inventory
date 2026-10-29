package packages

import (
	"sort"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Change identifies a binary package by name and architecture. Full revisions
// retain source-package changes even when the binary version is unchanged.
type Change struct {
	Scope string             `json:"scope"`
	Name  string             `json:"name"`
	Arch  string             `json:"arch"`
	Kind  string             `json:"kind"`
	From  *inventory.Package `json:"from"`
	To    *inventory.Package `json:"to"`
}

// Diff compares two complete package lists. Callers establish set availability
// and sample identity first; a failed scan cannot establish package absence.
func Diff(scope string, from, to []inventory.Package) []Change {
	out := []Change{}
	key := func(p inventory.Package) string { return p.Name + "\x00" + p.Arch }
	old := map[string]inventory.Package{}
	for _, p := range from {
		old[key(p)] = p
	}
	for _, p := range to {
		prev, ok := old[key(p)]
		delete(old, key(p))
		c := Change{Scope: scope, Name: p.Name, Arch: p.Arch, To: &p}
		if !ok {
			c.Kind = "added"
		} else if prev != p {
			c.Kind = "changed"
			c.From = &prev
		} else {
			continue
		}
		out = append(out, c)
	}
	for _, p := range old {
		out = append(out, Change{Scope: scope, Name: p.Name, Arch: p.Arch, Kind: "removed", From: &p})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Arch < out[j].Arch
	})
	return out
}
