package collect

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestVPNProviderWithoutInterface(t *testing.T) {
	t.Parallel()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	inv := &inventory.Inventory{Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{
		{ID: "aaaaaaaaaaaa", Name: "gateway", State: "exited", CapAdd: []string{"NET_ADMIN"}, StartedAt: &stamp},
		{Name: "worker", State: "running", NetworkMode: "container:aaaaaaaaaaaabbbbbbbbbbbb", StartedAt: &stamp},
		{Name: "batch", State: "exited", NetworkMode: "container:gateway"},
	}}}
	// Network is intentionally absent: this join must not depend on it.
	correlate(&Env{RootFS: t.TempDir()}, inv)
	v := inv.VPN
	if v == nil || v.Container != "gateway" || v.State != "exited" || v.StartedAt != &stamp || v.InterfaceUp || v.Interface != "" {
		t.Fatalf("stopped provider was not preserved: %+v", v)
	}
	if len(v.Dependents) != 2 || v.Dependents[0].Name != "batch" || v.Dependents[1].Name != "worker" || v.Dependents[1].StartedAt != &stamp {
		t.Fatalf("dependent facts missing or unordered: %+v", v.Dependents)
	}
	for _, d := range v.Dependents {
		if d.Attached != nil {
			t.Fatal("configuration alone invented attachment")
		}
	}
	if v.RXBytes != nil || v.TXBytes != nil || v.LatestHandshake != nil || v.Endpoint != nil || v.ExitIP != nil {
		t.Fatal("unobserved tunnel facts were invented")
	}
	b, err := json.Marshal(v)
	if err != nil || !strings.Contains(string(b), `"attached":null`) || !strings.Contains(string(b), `"rxBytes":null`) {
		t.Fatalf("lost null semantics: %s (%v)", b, err)
	}
}

func TestVPNProcCountersAndAttachment(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux proc namespace fixture")
	}
	root := t.TempDir()
	for pid, ns := range map[string]string{"101": "net:[1001]", "102": "net:[1001]", "103": "net:[1002]"} {
		dir := filepath.Join(root, "proc", pid, "ns")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(ns, filepath.Join(dir, "net")); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(root, "proc/101/net")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev"), []byte(vpnNetDevFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "alpha", State: "exited", CapAdd: []string{"NET_ADMIN"}},
		{Name: "gateway", State: "running", PID: 101, Health: "healthy"}, // Interface alone detects it.
		{Name: "attached", State: "running", PID: 102, NetworkMode: "container:gateway"},
		{Name: "detached", State: "running", PID: 103, NetworkMode: "container:gateway"},
		{Name: "unknown", State: "running", PID: 104, NetworkMode: "container:gateway"},
	}}}
	env := &Env{RootFS: root, diags: &inv.Diagnostics, name: "correlate"}
	correlate(env, inv)
	v := inv.VPN
	if v == nil || v.Container != "gateway" || v.Health != "healthy" || !v.InterfaceUp || v.Interface != "wg0" || v.RXBytes == nil || *v.RXBytes != 123 || v.TXBytes == nil || *v.TXBytes != 456 {
		t.Fatalf("unexpected gateway facts: %+v", v)
	}
	if len(v.Dependents) != 3 || v.Dependents[0].Attached == nil || !*v.Dependents[0].Attached || v.Dependents[1].Attached == nil || *v.Dependents[1].Attached || v.Dependents[2].Attached != nil {
		t.Fatalf("namespace identity not preserved: %+v", v.Dependents)
	}
	if len(inv.Diagnostics) != 1 || inv.Diagnostics[0].Severity != inventory.SeverityInfo || !strings.Contains(inv.Diagnostics[0].Message, "alpha") {
		t.Fatalf("other gateway candidate not diagnosed: %+v", inv.Diagnostics)
	}
	// Equal dependency preference is resolved by name, regardless of order.
	inv.Docker.Containers[0].Name = "zeta"
	inv.Docker.Containers = []inventory.Container{
		inv.Docker.Containers[0],
		{Name: "beta", CapAdd: []string{"CAP_NET_ADMIN"}},
	}
	correlate(env, inv)
	if inv.VPN.Container != "beta" || inv.VPN.Dependents == nil || len(inv.VPN.Dependents) != 0 {
		t.Fatal("candidate name ordering or empty dependent semantics changed")
	}
}

const vpnNetDevFixture = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
   wg0: 123 1 0 0 0 0 0 0 456 2 0 0 0 0 0 0
`

func TestParseVPNNetDev(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"", "wg0: 1 2", strings.Replace(vpnNetDevFixture, "123", "-1", 1), strings.Replace(vpnNetDevFixture, "456", "invalid", 1)} {
		if _, err := parseVPNNetDev(raw); err == nil {
			t.Fatal("accepted invalid counters")
		}
	}
	if tunnel, err := parseVPNNetDev(strings.Replace(vpnNetDevFixture, "wg0", "eth0", 1)); err != nil || tunnel != nil {
		t.Fatal("ordinary interfaces were reported as a tunnel")
	}
}

func TestVPNCapableProviderWithNoTunnel(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dir := filepath.Join(root, "proc/100/net")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dev"), []byte(strings.Replace(vpnNetDevFixture, "wg0", "eth0", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "gateway", State: "running", PID: 100, CapAdd: []string{"NET_ADMIN"}},
		{Name: "worker", State: "running", NetworkMode: "container:gateway"},
	}}}
	for _, fixtureRoot := range []string{root, t.TempDir()} {
		correlate(&Env{RootFS: fixtureRoot}, inv)
		if inv.VPN == nil || inv.VPN.InterfaceUp || inv.VPN.RXBytes != nil || len(inv.VPN.Dependents) != 1 || inv.VPN.Dependents[0].Attached != nil {
			t.Fatalf("missing interface lost provider or invented facts: %+v", inv.VPN)
		}
	}
}

func TestDockerRuntimeFormat(t *testing.T) {
	t.Parallel()
	tmpl, err := template.New("runtime").Funcs(template.FuncMap{"json": func(value any) (string, error) {
		b, err := json.Marshal(value)
		return string(b), err
	}}).Parse(dockerRuntimeFormat)
	if err != nil {
		t.Fatal(err)
	}
	for _, health := range []any{nil, map[string]any{"Status": "healthy"}} {
		data := map[string]any{
			"Id": "aaaaaaaaaaaa", "RestartCount": 2,
			"HostConfig": map[string]any{"NetworkMode": "bridge", "CapAdd": []string{"NET_ADMIN"}},
			"State":      map[string]any{"Pid": 100, "StartedAt": "2026-01-02T03:04:05Z", "Health": health},
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			t.Fatal(err)
		}
		_, payload, _ := strings.Cut(buf.String(), " ")
		var facts runtimeFacts
		if err := json.Unmarshal([]byte(payload), &facts); err != nil || facts.PID != 100 || facts.RestartCount != 2 || facts.StartedAt == nil || len(facts.CapAdd) != 1 {
			t.Fatalf("runtime format failed: %+v (%v)", facts, err)
		}
		if (health == nil && facts.Health != "") || (health != nil && facts.Health != "healthy") {
			t.Fatal("health status was not preserved")
		}
	}
}
