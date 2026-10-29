// Package packages queries stored software inventories without contacting hosts.
package packages

import (
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type Query struct {
	Q, Scope      string
	Limit, Offset int
}

// Response keeps software null for agents that did not collect packages.
// Pagination spans all selected sets; metadata for empty/failed sets is retained.
type Response struct {
	Host          string    `json:"host"`
	CollectedAt   time.Time `json:"collected_at"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	AgentRevision int       `json:"agent_revision"`
	Software      *Software `json:"software"`
	Limit         int       `json:"limit"`
	Offset        int       `json:"offset"`
	Total         int       `json:"total"`
}
type Software struct {
	CollectedAt  time.Time                  `json:"collected_at"`
	DurationMS   int64                      `json:"duration_ms"`
	Status       string                     `json:"status"`
	Error        string                     `json:"error,omitempty"`
	ImagesStatus string                     `json:"images_status"`
	ImagesError  string                     `json:"images_error,omitempty"`
	Sets         []Set                      `json:"sets"`
	Runtimes     []inventory.RuntimeVersion `json:"runtimes"`
}
type Set struct {
	Scope           string   `json:"scope"`
	ImageRef        string   `json:"image_ref,omitempty"`
	Containers      []string `json:"containers,omitempty"`
	SampleContainer string   `json:"sample_container,omitempty"`
	RepoDigests     []string `json:"repo_digests,omitempty"`
	Architecture    string   `json:"architecture,omitempty"`
	OS              string   `json:"os,omitempty"`
	inventory.PackageSet
}

// Sets exposes the host plus one sample per immutable image ID.
func Sets(s *inventory.Software) []Set {
	out := []Set{}
	if s == nil {
		return out
	}
	out = append(out, Set{Scope: "host", PackageSet: s.Host})
	for _, im := range s.Images {
		out = append(out, Set{Scope: "image:" + im.ImageID, ImageRef: im.ImageRef, Containers: im.Containers, SampleContainer: im.SampleContainer, RepoDigests: im.RepoDigests, Architecture: im.Architecture, OS: im.OS, PackageSet: im.PackageSet})
	}
	return out
}
func Host(inv *inventory.Inventory, q Query) Response {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	if q.Limit > 1000 {
		q.Limit = 1000
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	r := Response{Host: inv.Host.Hostname, CollectedAt: inv.CollectedAt, AgentRevision: inv.Tool.Revision, Limit: q.Limit, Offset: q.Offset}
	if inv.Software == nil {
		return r
	}
	s := inv.Software
	r.Software = &Software{CollectedAt: s.CollectedAt, DurationMS: s.DurationMS, Status: s.Status, Error: s.Error, ImagesStatus: s.ImagesStatus, ImagesError: s.ImagesError, Sets: []Set{}, Runtimes: s.Runtimes}
	for _, set := range Sets(s) {
		if !scopeMatches(set, q.Scope) {
			continue
		}
		all := set.Packages
		set.Packages = []inventory.Package{}
		for _, p := range all {
			if !strings.Contains(strings.ToLower(p.Name), strings.ToLower(q.Q)) {
				continue
			}
			n := r.Total
			r.Total++
			if n >= q.Offset && n-q.Offset < q.Limit {
				set.Packages = append(set.Packages, p)
			}
		}
		r.Software.Sets = append(r.Software.Sets, set)
	}
	return r
}
func scopeMatches(set Set, scope string) bool {
	if scope == "" || scope == set.Scope || scope == strings.TrimPrefix(set.Scope, "image:") {
		return true
	}
	for _, c := range set.Containers {
		if scope == c || scope == "container:"+c {
			return true
		}
	}
	return false
}

type Match struct {
	Host       string   `json:"host"`
	Scope      string   `json:"scope"`
	Containers []string `json:"containers,omitempty"`
	inventory.Package
	Distro        inventory.Distro `json:"distro"`
	Manager       string           `json:"manager"`
	Status        string           `json:"status"`
	Error         string           `json:"error,omitempty"`
	CollectedAt   time.Time        `json:"collected_at"`
	ConfirmedAt   time.Time        `json:"confirmed_at"`
	AgentRevision int              `json:"agent_revision"`
}

// Coverage accompanies search results so a zero-match failed host is visible.
type Coverage struct {
	Host          string    `json:"host"`
	CollectedAt   time.Time `json:"collected_at"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	AgentRevision int       `json:"agent_revision"`
	Software      *Software `json:"software"`
}
type SearchResult struct {
	Matches  []Match    `json:"matches"`
	Coverage []Coverage `json:"coverage"`
}

func Search(invs []*inventory.Inventory, name string) SearchResult {
	r := SearchResult{Matches: []Match{}, Coverage: []Coverage{}}
	prefix := strings.HasSuffix(name, "*")
	needle := strings.TrimSuffix(name, "*")
	for _, inv := range invs {
		summary := Host(inv, Query{Limit: 1})
		if summary.Software != nil {
			summary.Software.Runtimes = nil
			for i := range summary.Software.Sets {
				summary.Software.Sets[i].Packages = []inventory.Package{}
			}
		}
		r.Coverage = append(r.Coverage, Coverage{Host: inv.Host.Hostname, CollectedAt: inv.CollectedAt, AgentRevision: inv.Tool.Revision, Software: summary.Software})
		for _, set := range Sets(inv.Software) {
			for _, p := range set.Packages {
				if p.Name != needle && !(prefix && strings.HasPrefix(p.Name, needle)) {
					continue
				}
				if p.Source == "" {
					p.Source = p.Name
				}
				if p.SourceVersion == "" {
					p.SourceVersion = p.Version
				}
				r.Matches = append(r.Matches, Match{Host: inv.Host.Hostname, Scope: set.Scope, Containers: set.Containers, Package: p, Distro: set.Distro, Manager: set.Manager, Status: set.Status, Error: set.Error, CollectedAt: set.CollectedAt, AgentRevision: inv.Tool.Revision})
			}
		}
	}
	return r
}
