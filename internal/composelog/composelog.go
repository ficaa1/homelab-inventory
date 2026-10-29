// Package composelog turns the stored run history into version control for
// compose files.
//
// Every inventory already carries the full text of every compose file it
// found, along with a sha256. Stored inventories provide a history of changes
// without new collection, privileges, or
// storage. Only a question nobody had asked of data already on disk.
//
// # Versions, not runs
//
// A file that has not changed in three weeks has one version and two thousand
// runs. Grouping by content hash rather than by run is what makes the history
// readable: five versions with dates beats two thousand identical snapshots,
// and it is also what makes "show me the diff" a question with an obvious
// answer.
//
// # Why the diff is written here
//
// Go's standard library has no diff. This is a small line-based
// longest-common-subsequence implementation producing unified output — enough
// for compose files, which are short and edited by humans, and deliberately not
// a general-purpose diff library.
package composelog

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Version is one distinct content of one file.
type Version struct {
	// SHA256 identifies the content. It comes from the collector rather than
	// being recomputed, so it matches what the inventory recorded.
	SHA256 string `json:"sha256"`
	// FirstSeen and LastSeen bound the runs this content appeared in. A
	// version still present in the newest run has LastSeen at that run.
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	// Runs is how many stored runs carried this exact content.
	Runs int `json:"runs"`
	// RunID is a run that contained it, so the content can be fetched back.
	RunID   string `json:"run_id"`
	Bytes   int    `json:"bytes"`
	Content string `json:"content,omitempty"`
}

// File is one compose file and every version of it.
type File struct {
	Project string `json:"project"`
	Path    string `json:"path"`
	// ResolvedPath is where the file actually lives on the host. Portainer
	// records a stack's compose file at /data/compose/<id>, which exists only
	// inside the Portainer container — displaying that path alone tells a
	// reader to look somewhere that does not exist on the machine they are
	// standing on. ResolvedVia says how the translation was made.
	ResolvedPath string    `json:"resolved_path,omitempty"`
	ResolvedVia  string    `json:"resolved_via,omitempty"`
	Versions     []Version `json:"versions"`
	Changes      int       `json:"changes"`
}

// Report is one host's compose history.
type Report struct {
	Host  string `json:"host"`
	Runs  int    `json:"runs"`
	Files []File `json:"files"`
}

// Build groups the compose files in a set of runs into versions.
//
// runIDs must be parallel to invs. Runs may arrive in any order; a file absent
// from some of them simply has fewer sightings, which is normal — a project
// can be added, removed, or briefly unreadable.
func Build(invs []*inventory.Inventory, runIDs []string, includeContent bool) *Report {
	rep := &Report{Files: []File{}}

	type fileKey struct{ project, path string }
	type acc struct {
		resolved string
		via      string
		versions map[string]*Version
	}
	files := map[fileKey]*acc{}

	for i, inv := range invs {
		if inv == nil || inv.Docker == nil || !inv.Docker.Available {
			continue
		}
		if rep.Host == "" {
			rep.Host = inv.Host.Hostname
		}
		rep.Runs++
		runID := ""
		if i < len(runIDs) {
			runID = runIDs[i]
		}
		at := inv.CollectedAt

		for _, p := range inv.Docker.ComposeProjects {
			for _, f := range p.ConfigFiles {
				// A file that could not be read has no content and no hash;
				// recording it as an empty version would invent a change that
				// never happened.
				if f.SHA256 == "" || f.Content == "" {
					continue
				}
				k := fileKey{p.Name, f.Path}
				a := files[k]
				if a == nil {
					a = &acc{versions: map[string]*Version{}}
					files[k] = a
				}
				if f.ResolvedPath != "" {
					a.resolved = f.ResolvedPath
				}
				if f.ResolvedVia != "" {
					a.via = f.ResolvedVia
				}
				v := a.versions[f.SHA256]
				if v == nil {
					v = &Version{
						SHA256: f.SHA256, FirstSeen: at, LastSeen: at,
						RunID: runID, Bytes: len(f.Content),
					}
					if includeContent {
						v.Content = f.Content
					}
					a.versions[f.SHA256] = v
				}
				v.Runs++
				if at.Before(v.FirstSeen) {
					v.FirstSeen = at
					v.RunID = runID
				}
				if at.After(v.LastSeen) {
					v.LastSeen = at
				}
			}
		}
	}

	for k, a := range files {
		f := File{Project: k.project, Path: k.path, ResolvedPath: a.resolved, ResolvedVia: a.via}
		for _, v := range a.versions {
			f.Versions = append(f.Versions, *v)
		}
		// Newest first: the current content is what a reader wants first, and
		// the one before it is the usual comUTCon.
		sort.Slice(f.Versions, func(i, j int) bool {
			return f.Versions[i].LastSeen.After(f.Versions[j].LastSeen)
		})
		f.Changes = len(f.Versions) - 1
		rep.Files = append(rep.Files, f)
	}

	sort.Slice(rep.Files, func(i, j int) bool {
		a, b := rep.Files[i], rep.Files[j]
		// The files that have actually changed are the interesting ones.
		if a.Changes != b.Changes {
			return a.Changes > b.Changes
		}
		if a.Project != b.Project {
			return a.Project < b.Project
		}
		return a.Path < b.Path
	})
	return rep
}

// Find returns one file's history.
func (r *Report) Find(project, path string) (File, bool) {
	for _, f := range r.Files {
		if f.Project == project && (path == "" || f.Path == path) {
			return f, true
		}
	}
	return File{}, false
}

// Diff is a unified diff between two versions of a file.
type Diff struct {
	Project string  `json:"project"`
	Path    string  `json:"path"`
	From    Version `json:"from"`
	To      Version `json:"to"`
	Unified string  `json:"unified"`
	Added   int     `json:"added"`
	Removed int     `json:"removed"`
}

// Unified renders a unified diff between two texts.
//
// Context is fixed at three lines, which is the convention and is plenty for a
// compose file. The output is deliberately plain text: it is meant to be read,
// and pasting it into a terminal or a commit message should work.
func Unified(from, to string, fromLabel, toLabel string) (string, int, int) {
	a := splitLines(from)
	b := splitLines(to)
	ops := lcsDiff(a, b)

	var added, removed int
	for _, op := range ops {
		switch op.kind {
		case opAdd:
			added++
		case opDel:
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return "", 0, 0
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", fromLabel, toLabel)

	const context = 3
	// Walk the op list, emitting hunks around runs of changes.
	i := 0
	for i < len(ops) {
		if ops[i].kind == opSame {
			i++
			continue
		}
		start := i
		for start > 0 && ops[start-1].kind == opSame && start > i-context {
			start--
		}
		// Extend to the end of this run of changes, allowing small gaps of
		// context to be absorbed rather than starting a new hunk.
		end := i
		gap := 0
		for end < len(ops) {
			if ops[end].kind == opSame {
				gap++
				if gap > context*2 {
					break
				}
			} else {
				gap = 0
			}
			end++
		}
		if end > len(ops) {
			end = len(ops)
		}
		// Trim trailing context to at most `context` lines.
		trail := 0
		for end > start && ops[end-1].kind == opSame && trail >= context {
			end--
		}

		var aStart, bStart, aCount, bCount int
		for j := 0; j < start; j++ {
			switch ops[j].kind {
			case opSame:
				aStart++
				bStart++
			case opDel:
				aStart++
			case opAdd:
				bStart++
			}
		}
		for j := start; j < end; j++ {
			switch ops[j].kind {
			case opSame:
				aCount++
				bCount++
			case opDel:
				aCount++
			case opAdd:
				bCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart+1, aCount, bStart+1, bCount)
		for j := start; j < end; j++ {
			switch ops[j].kind {
			case opSame:
				out.WriteString(" " + ops[j].text + "\n")
			case opDel:
				out.WriteString("-" + ops[j].text + "\n")
			case opAdd:
				out.WriteString("+" + ops[j].text + "\n")
			}
		}
		i = end
	}
	return out.String(), added, removed
}

type opKind int

const (
	opSame opKind = iota
	opAdd
	opDel
)

type op struct {
	kind opKind
	text string
}

// lcsDiff is a straightforward longest-common-subsequence diff.
//
// It is O(n*m) in time and memory, which is fine for compose files — they are
// tens of lines — and would not be for anything large. The input is bounded by
// the collector's own file size cap, so the worst case is bounded too.
func lcsDiff(a, b []string) []op {
	n, m := len(a), len(b)
	// table[i][j] is the LCS length of a[i:] and b[j:].
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}

	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{opSame, a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, op{opDel, a[i]})
			i++
		default:
			ops = append(ops, op{opAdd, b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{opDel, a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{opAdd, b[j]})
	}
	return ops
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
