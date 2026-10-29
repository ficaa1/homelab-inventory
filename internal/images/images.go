// Package images reports what each container is actually running, and how
// firmly it is pinned there.
//
// Moving tags can change without an operator requesting it, and the
// only record that it happened is two stored inventories and internal/drift
// noticing afterwards. That is a reasonable way to run a homelab, and it is
// worth being a deliberate choice rather than an accident.
//
// # What "pinned" means here
//
// Three levels, because they are genuinely different promises:
//
//   - a digest (`@sha256:…`) is immutable — that exact image, forever
//   - a specific tag (`:v1.119.0`) is stable by convention, and can still be
//     moved by whoever publishes it
//   - `:latest`, or no tag at all, is a moving target by design
//
// Nothing here is an error. A fleet that pins everything to digests never gets
// a security fix without human action, which is its own risk. The point is to
// be able to see which containers are in which category rather than assuming.
package images

import (
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Pinning is how firmly a container is held to one image.
type Pinning string

const (
	// PinDigest references an immutable content hash.
	PinDigest Pinning = "digest"
	// PinTag references a specific tag, which is stable by convention only.
	PinTag Pinning = "tag"
	// PinFloating is :latest or an omitted tag — a moving target.
	PinFloating Pinning = "floating"
)

// Ref is a parsed container image reference.
type Ref struct {
	// Raw is exactly what Docker reported, kept because every derived field is
	// a guess about a format that has grown several times.
	Raw string `json:"raw"`
	// Registry is empty for Docker Hub, which omits it by convention.
	Registry   string  `json:"registry,omitempty"`
	Repository string  `json:"repository"`
	Tag        string  `json:"tag,omitempty"`
	Digest     string  `json:"digest,omitempty"`
	Pinning    Pinning `json:"pinning"`
}

// Container is one running thing and its image.
type Container struct {
	Host    string `json:"host"`
	Name    string `json:"name"`
	Project string `json:"compose_project,omitempty"`
	State   string `json:"state"`
	Image   Ref    `json:"image"`
}

// Counts summarises a report.
type Counts struct {
	Total    int `json:"total"`
	Digest   int `json:"digest"`
	Tag      int `json:"tag"`
	Floating int `json:"floating"`
}

// Report is the fleet's image inventory.
type Report struct {
	Counts     Counts      `json:"counts"`
	Containers []Container `json:"containers"`
	// Shared lists images used by more than one container, which is worth
	// knowing before pinning one of them: they move together.
	Shared []SharedImage `json:"shared,omitempty"`
}

// SharedImage is one image reference used in several places.
type SharedImage struct {
	Repository string   `json:"repository"`
	Tag        string   `json:"tag,omitempty"`
	Users      []string `json:"users"`
}

// Build assembles the report from every host's latest inventory.
func Build(invs []*inventory.Inventory) *Report {
	rep := &Report{Containers: []Container{}}
	byImage := map[string][]string{}

	for _, inv := range invs {
		if inv == nil || inv.Docker == nil || !inv.Docker.Available {
			continue
		}
		host := inv.Host.Hostname
		for _, c := range inv.Docker.Containers {
			ref := Parse(c.Image)
			rep.Containers = append(rep.Containers, Container{
				Host: host, Name: c.Name, Project: c.ComposeProject,
				State: c.State, Image: ref,
			})
			rep.Counts.Total++
			switch ref.Pinning {
			case PinDigest:
				rep.Counts.Digest++
			case PinTag:
				rep.Counts.Tag++
			default:
				rep.Counts.Floating++
			}
			key := ref.Repository
			if ref.Tag != "" {
				key += ":" + ref.Tag
			}
			byImage[key] = append(byImage[key], host+"/"+c.Name)
		}
	}

	sort.Slice(rep.Containers, func(i, j int) bool {
		a, b := rep.Containers[i], rep.Containers[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		return a.Name < b.Name
	})

	for key, users := range byImage {
		if len(users) < 2 {
			continue
		}
		repo, tag, _ := strings.Cut(key, ":")
		sort.Strings(users)
		rep.Shared = append(rep.Shared, SharedImage{Repository: repo, Tag: tag, Users: users})
	}
	sort.Slice(rep.Shared, func(i, j int) bool { return rep.Shared[i].Repository < rep.Shared[j].Repository })
	return rep
}

// Parse splits a container image reference into its parts.
//
// The format is more awkward than it looks. A colon can introduce a tag or a
// registry port; a slash can separate a registry, a namespace, or neither.
// `plexinc/pms-docker:1.40`, `ghcr.io/immich-app/immich-server:release`,
// `registry.local:5000/thing` and a bare `redis` all have to come out right,
// and the rule that makes it work is that the first path segment is only a
// registry when it looks like a host — it contains a dot or a colon, or it is
// literally localhost.
func Parse(raw string) Ref {
	r := Ref{Raw: raw, Pinning: PinFloating}
	s := strings.TrimSpace(raw)
	if s == "" {
		return r
	}

	// A digest always comes last and is unambiguous.
	if i := strings.Index(s, "@"); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		r.Pinning = PinDigest
	}

	// Split off a registry before looking for a tag, so a registry port is not
	// mistaken for one.
	rest := s
	if i := strings.Index(s, "/"); i >= 0 {
		head := s[:i]
		if strings.ContainsAny(head, ".:") || head == "localhost" {
			r.Registry = head
			rest = s[i+1:]
		}
	}

	if i := strings.LastIndex(rest, ":"); i >= 0 {
		r.Repository = rest[:i]
		r.Tag = rest[i+1:]
	} else {
		r.Repository = rest
	}

	if r.Pinning != PinDigest {
		switch r.Tag {
		case "", "latest":
			r.Pinning = PinFloating
		default:
			r.Pinning = PinTag
		}
	}
	return r
}

// Floating returns the containers on a moving target, which is the list worth
// looking at when an auto-updater is running.
func (r *Report) Floating() []Container {
	out := make([]Container, 0, r.Counts.Floating)
	for _, c := range r.Containers {
		if c.Image.Pinning == PinFloating {
			out = append(out, c)
		}
	}
	return out
}

// ForHost narrows the report to one machine.
func (r *Report) ForHost(host string) *Report {
	out := &Report{Containers: []Container{}}
	for _, c := range r.Containers {
		if c.Host != host {
			continue
		}
		out.Containers = append(out.Containers, c)
		out.Counts.Total++
		switch c.Image.Pinning {
		case PinDigest:
			out.Counts.Digest++
		case PinTag:
			out.Counts.Tag++
		default:
			out.Counts.Floating++
		}
	}
	return out
}
