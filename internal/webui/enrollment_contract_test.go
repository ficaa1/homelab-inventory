package webui

import (
	"os/exec"
	"testing"
)

func TestEnrollmentCommandContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; enrollment browser contract needs JavaScript")
	}
	if out, err := exec.Command(node, "testdata/enrollment-contract.cjs", "index.html").CombinedOutput(); err != nil {
		t.Fatalf("enrollment command flow: %v\n%s", err, out)
	}
}
