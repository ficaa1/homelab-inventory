package webui

import (
	"os/exec"
	"testing"
)

func TestVPNPanelContract(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; VPN browser contract needs JavaScript")
	}
	if out, err := exec.Command(node, "testdata/vpn-contract.cjs", "pages/network.js").CombinedOutput(); err != nil {
		t.Fatalf("VPN panel: %v\n%s", err, out)
	}
}
