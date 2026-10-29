package inventory

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAgentSourceRevision catches agent changes even when the wire schema is
// unchanged. Discover's local dependencies are derived rather than listed by
// hand so a new collector package cannot silently escape the guard.
func TestAgentSourceRevision(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "list", "-deps", "-f",
		`{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}`, "./cmd/discover")
	cmd.Dir = root
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("listing discover dependencies: %v\n%s", err, output)
	}

	var paths []string
	for _, pkg := range strings.Fields(string(output)) {
		// Resolve package directories from the main module rather than assuming
		// the old module's import prefix. The digest still uses relative paths.
		dir, err := filepath.Rel(root, pkg)
		if err != nil || dir == ".." || strings.HasPrefix(dir, ".."+string(filepath.Separator)) {
			t.Fatalf("dependency outside the main module: %s (%v)", pkg, err)
		}
		dir = filepath.ToSlash(dir)
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatal(err)
		}
		// Include all Go sources in each package, even files selected only on
		// another platform, because discover is installed on different OSes.
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := dir + "/" + name
			if path == "internal/inventory/agent_source_hash.go" {
				continue
			}
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		t.Fatal("discover has no local Go sources to hash")
	}
	sort.Strings(paths)
	hash := sha256.New()
	for _, path := range paths {
		source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		source, err = format.Source(bytes.ReplaceAll(source, []byte("\r\n"), []byte("\n")))
		if err != nil {
			t.Fatalf("formatting %s: %v", path, err)
		}
		// Relative paths and separators make renames visible and prevent
		// adjacent file contents from producing an ambiguous digest.
		fmt.Fprintf(hash, "%s\x00", path)
		hash.Write(source)
		hash.Write([]byte{0})
	}
	got := fmt.Sprintf("%x", hash.Sum(nil))
	if got != agentSourceHash {
		t.Fatalf("discover source changed: bump AgentRevision in internal/inventory/inventory.go, then paste the new hash %q into agentSourceHash in internal/inventory/agent_source_hash.go", got)
	}
}
