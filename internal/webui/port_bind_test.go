package webui

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/portmap"
)

func TestPortBindLabels(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable; bind labels need JavaScript")
	}
	listeners := []inventory.Listener{
		{Protocol: "tcp", Port: 8001, Address: "192.0.2.8"},
		{Protocol: "tcp", Port: 8002, Address: "127.0.0.53"},
		{Protocol: "tcp", Port: 8002, Address: "0:0:0:0:0:0:0:1"},
		{Protocol: "tcp", Port: 8002, Address: "::ffff:127.0.0.1"},
		{Protocol: "tcp", Port: 8003, Address: "127.0.0.1"},
		{Protocol: "tcp", Port: 8003, Address: "192.0.2.8"},
		{Protocol: "tcp", Port: 8004, Address: "127.0.0.1"},
		{Protocol: "tcp", Port: 8004, Address: "::", Exposed: true},
	}
	pm := portmap.Build([]*inventory.Inventory{{
		Host:    inventory.Host{Hostname: "fixture"},
		Network: &inventory.Network{Listeners: listeners},
	}})
	ports := append(pm.Hosts[0].Ports, portmap.Port{Port: 8005})
	fixture, err := json.Marshal(ports)
	if err != nil {
		t.Fatal(err)
	}
	_, tail, ok := strings.Cut(body(t), "  function portBindLabel(")
	fn, _, foundEnd := strings.Cut(tail, "\n  }")
	if !ok || !foundEnd {
		t.Fatal("port bind label function missing")
	}
	cmd := exec.Command(node, "-e", "function portBindLabel("+fn+"\n}"+`
const assert = require("node:assert/strict");
const ports = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
assert.deepEqual(ports.map(portBindLabel),
  ["Specific address", "Loopback only", "Specific address", "Wildcard bind", "Unknown"]);
`)
	cmd.Stdin = bytes.NewReader(fixture)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("port bind labels: %v\n%s", err, out)
	}
}
