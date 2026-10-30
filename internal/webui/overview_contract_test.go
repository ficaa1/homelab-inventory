package webui

import (
	"os/exec"
	"testing"
)

func TestOverviewRenderingContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; browser overview contract needs JavaScript")
	}
	if out, err := exec.Command(node, "testdata/overview-contract.cjs", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("overview rendering: %v\n%s", err, out)
	}
}
