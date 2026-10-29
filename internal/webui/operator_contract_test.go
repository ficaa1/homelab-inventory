package webui

import (
	"os/exec"
	"testing"
)

func TestOperatorTokenContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; browser helper contract needs JavaScript")
	}
	if out, err := exec.Command(node, "testdata/operator-contract.cjs", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("operator helpers: %v\n%s", err, out)
	}
}

func TestRestartDisabledContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; browser restart contract needs JavaScript")
	}
	if out, err := exec.Command(node, "testdata/restart-contract.cjs", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("restart helpers: %v\n%s", err, out)
	}
}
