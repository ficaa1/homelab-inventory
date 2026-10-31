package webui

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Parse the final embedded page, including injected features. Checking only
// index.html misses malformed assets merged into its script at startup.
func TestEmbeddedScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for JavaScript syntax validation")
	}
	blocks := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindAllStringSubmatch(body(t), -1)
	if len(blocks) == 0 {
		t.Fatal("no embedded dashboard script")
	}
	for _, block := range blocks {
		cmd := exec.Command(node, "--check", "-")
		cmd.Stdin = strings.NewReader(block[1])
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("embedded script cannot parse: %v\n%s", err, out)
		}
	}
}
