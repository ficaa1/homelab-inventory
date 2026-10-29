package composelog

import (
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func run(host string, at time.Time, files ...inventory.ComposeFile) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   at,
		Host:          inventory.Host{Hostname: host},
		Docker: &inventory.Docker{Available: true, ComposeProjects: []inventory.ComposeProject{
			{Name: "immich", ConfigFiles: files},
		}},
	}
}

func file(content, sha string) inventory.ComposeFile {
	return inventory.ComposeFile{
		Path: "/opt/immich/docker-compose.yml", Exists: true,
		SHA256: sha, Content: content,
	}
}

// The point of grouping by content hash: a file untouched for three weeks is
// one version and two thousand runs, not two thousand snapshots.
func TestVersionsCollapseIdenticalRuns(t *testing.T) {
	t.Parallel()
	t0 := time.Now().Add(-72 * time.Hour)
	var invs []*inventory.Inventory
	var ids []string
	for i := 0; i < 100; i++ {
		invs = append(invs, run("host-a", t0.Add(time.Duration(i)*time.Minute), file("image: v1\n", "aaa")))
		ids = append(ids, "run"+string(rune('a'+i%26)))
	}
	// One edit, then it settles again.
	for i := 100; i < 150; i++ {
		invs = append(invs, run("host-a", t0.Add(time.Duration(i)*time.Minute), file("image: v2\n", "bbb")))
		ids = append(ids, "later")
	}

	r := Build(invs, ids, true)
	if len(r.Files) != 1 {
		t.Fatalf("files = %d, want 1", len(r.Files))
	}
	f := r.Files[0]
	if len(f.Versions) != 2 {
		t.Fatalf("versions = %d, want 2 from 150 runs", len(f.Versions))
	}
	if f.Changes != 1 {
		t.Errorf("Changes = %d, want 1", f.Changes)
	}
	// Newest first: the current content is what a reader wants first.
	if f.Versions[0].SHA256 != "bbb" {
		t.Errorf("versions are not newest-first: %s", f.Versions[0].SHA256)
	}
	if f.Versions[0].Runs != 50 || f.Versions[1].Runs != 100 {
		t.Errorf("run counts = %d, %d; want 50 and 100", f.Versions[0].Runs, f.Versions[1].Runs)
	}
	if r.Runs != 150 {
		t.Errorf("Runs = %d, want 150", r.Runs)
	}
}

// A file the collector could not read has no content and no hash. Recording it
// as an empty version would invent a change that never happened.
func TestUnreadableFilesAreNotVersions(t *testing.T) {
	t.Parallel()
	now := time.Now()
	invs := []*inventory.Inventory{
		run("host-a", now.Add(-time.Hour), file("image: v1\n", "aaa")),
		run("host-a", now.Add(-30*time.Minute), inventory.ComposeFile{
			Path: "/opt/immich/docker-compose.yml", Exists: false,
			ReadError: "permission denied",
		}),
		run("host-a", now, file("image: v1\n", "aaa")),
	}
	r := Build(invs, []string{"a", "b", "c"}, true)
	if len(r.Files) != 1 {
		t.Fatalf("files = %d", len(r.Files))
	}
	if n := len(r.Files[0].Versions); n != 1 {
		t.Errorf("versions = %d, want 1 — an unreadable run is a gap, not a change", n)
	}
}

// A host whose docker collector did not run is not a host with no compose
// files.
func TestSkipsRunsWithoutDocker(t *testing.T) {
	t.Parallel()
	now := time.Now()
	bare := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: now}
	r := Build([]*inventory.Inventory{bare, run("host-a", now, file("x\n", "aaa"))},
		[]string{"a", "b"}, true)
	if r.Runs != 1 {
		t.Errorf("Runs = %d, want 1", r.Runs)
	}
}

func TestUnifiedDiff(t *testing.T) {
	t.Parallel()
	from := `services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:v1.118
    restart: always
    ports:
      - 2283:2283
`
	to := `services:
  immich-server:
    image: ghcr.io/immich-app/immich-server:v1.119
    restart: unless-stopped
    ports:
      - 2283:2283
`
	d, added, removed := Unified(from, to, "before", "after")
	if added != 2 || removed != 2 {
		t.Errorf("added/removed = %d/%d, want 2/2", added, removed)
	}
	for _, want := range []string{
		"--- before", "+++ after", "@@",
		"-    image: ghcr.io/immich-app/immich-server:v1.118",
		"+    image: ghcr.io/immich-app/immich-server:v1.119",
		"-    restart: always",
		"+    restart: unless-stopped",
		" services:",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
}

func TestUnifiedDiffOfIdenticalTextIsEmpty(t *testing.T) {
	t.Parallel()
	same := "a\nb\nc\n"
	d, added, removed := Unified(same, same, "x", "y")
	if d != "" || added != 0 || removed != 0 {
		t.Errorf("identical texts produced a diff: %q (%d/%d)", d, added, removed)
	}
}

func TestUnifiedHandlesAdditionsAndDeletionsAtTheEdges(t *testing.T) {
	t.Parallel()
	d, added, removed := Unified("", "one\ntwo\n", "empty", "filled")
	if added != 2 || removed != 0 {
		t.Errorf("adding to an empty file: %d/%d, want 2/0", added, removed)
	}
	if !strings.Contains(d, "+one") || !strings.Contains(d, "+two") {
		t.Errorf("diff = %q", d)
	}

	d, added, removed = Unified("one\ntwo\n", "", "filled", "empty")
	if added != 0 || removed != 2 {
		t.Errorf("emptying a file: %d/%d, want 0/2", added, removed)
	}
	if !strings.Contains(d, "-one") {
		t.Errorf("diff = %q", d)
	}
}

// Line endings should not read as a whole-file rewrite.
func TestUnifiedNormalisesLineEndings(t *testing.T) {
	t.Parallel()
	if d, a, rm := Unified("a\r\nb\r\n", "a\nb\n", "x", "y"); d != "" || a != 0 || rm != 0 {
		t.Errorf("CRLF vs LF produced a diff: %q (%d/%d)", d, a, rm)
	}
}

func TestFind(t *testing.T) {
	t.Parallel()
	now := time.Now()
	r := Build([]*inventory.Inventory{run("host-a", now, file("x\n", "aaa"))}, []string{"a"}, true)
	if _, ok := r.Find("immich", "/opt/immich/docker-compose.yml"); !ok {
		t.Error("Find missed an existing file")
	}
	if _, ok := r.Find("immich", ""); !ok {
		t.Error("Find with an empty path should match the project's file")
	}
	if _, ok := r.Find("nope", ""); ok {
		t.Error("Find invented a project")
	}
}

// Sorting puts the files that have actually changed first, because those are
// the ones anyone came to look at.
func TestChangedFilesSortFirst(t *testing.T) {
	t.Parallel()
	now := time.Now()
	mk := func(at time.Time, sha string) *inventory.Inventory {
		return &inventory.Inventory{
			CollectedAt: at, Host: inventory.Host{Hostname: "host-a"},
			Docker: &inventory.Docker{Available: true, ComposeProjects: []inventory.ComposeProject{
				{Name: "stable", ConfigFiles: []inventory.ComposeFile{
					{Path: "/a.yml", SHA256: "same", Content: "a\n"}}},
				{Name: "churny", ConfigFiles: []inventory.ComposeFile{
					{Path: "/b.yml", SHA256: sha, Content: sha + "\n"}}},
			}},
		}
	}
	r := Build([]*inventory.Inventory{mk(now.Add(-2*time.Hour), "v1"), mk(now.Add(-time.Hour), "v2"), mk(now, "v3")},
		[]string{"a", "b", "c"}, false)
	if len(r.Files) != 2 {
		t.Fatalf("files = %d", len(r.Files))
	}
	if r.Files[0].Project != "churny" {
		t.Errorf("the changed file did not sort first: %+v", r.Files[0].Project)
	}
	// Content is omitted when not asked for, so a listing stays small.
	if r.Files[0].Versions[0].Content != "" {
		t.Error("content was included when it was not requested")
	}
}
