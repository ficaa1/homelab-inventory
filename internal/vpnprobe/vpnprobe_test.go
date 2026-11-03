package vpnprobe

import (
	"context"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestWireGuardNewestPeerAndCounters(t *testing.T) {
	raw := "wg0\t_\t_\t0\toff\nwg0\t_\t_\t192.0.2.10:51820\t_\t1700000000\t10\t20\t0\nwg1\t_\t_\t0\toff\nwg1\t_\t_\t[2001:db8::1]:51820\t_\t1700000010\t30\t40\t0"
	f, err := parseWGDump(raw)
	if err != nil || f.LatestHandshake == nil || f.LatestHandshake.Unix() != 1700000010 || f.Endpoint == nil || *f.Endpoint != "[2001:db8::1]:51820" || *f.RXBytes != 40 || *f.TXBytes != 60 {
		t.Fatal("incorrect multi-interface facts")
	}
	for _, bad := range []string{"", raw + "\ninvalid", "wg0\t_\t_\t0\toff\nwg0\t_\t_\t192.0.2.10:5\t_\t-1\t1\t2\t0"} {
		if _, err := parseWGDump(bad); err == nil {
			t.Fatal("accepted invalid dump")
		}
	}
	f, err = parseWGDump("wg0\t_\t_\t0\toff")
	if err != nil || f.LatestHandshake != nil || f.RXBytes != nil {
		t.Fatal("empty interface should have unavailable counters")
	}
}
func TestExitFormats(t *testing.T) {
	for _, raw := range []string{"192.0.2.20\n", `{"ip":"192.0.2.20","country":"ZZ","city":"Example City","org":"Example Network"}`} {
		f, err := parseExit(raw)
		if err != nil || f.ExitIP == nil || *f.ExitIP != "192.0.2.20" {
			t.Fatal("exit parse failed")
		}
	}
	for _, raw := range []string{"", `{"ip":"not-an-ip"}`, "<html>error</html>"} {
		if _, err := parseExit(raw); err == nil {
			t.Fatal("accepted invalid exit")
		}
	}
}
func TestInspectStoppedZeroStartedAt(t *testing.T) {
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "gateway"}}}}
	r := &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }, Execute: func(context.Context, string, map[string]string) execallow.Result {
		return execallow.Result{Stdout: `{"name":"/gateway","state":"exited","health":"","startedAt":"0001-01-01T00:00:00Z"}`}
	}}
	c, err := Inspect(context.Background(), r, "gateway")
	if err != nil || c.State != "exited" || !c.StartedAt.IsZero() {
		t.Fatal("stopped container rejected")
	}
}
