package execallow

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func vpnRunner(t *testing.T, script string) *Runner {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n"+script), 0755); err != nil {
		t.Fatal(err)
	}
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, VPN: &inventory.VPN{Container: "gateway"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "gateway"}, {Name: "worker"}}}}
	return &Runner{Policy: Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return inv, nil }}
}

const exactInspect = `if [ "$1" = inspect ] && [ "$4" = '{{.Name}}' ]; then
[ "$2" = --type=container ] && [ "$3" = --format ] && [ "$5" = -- ] && [ "$6" = gateway ] || exit 2
printf '/gateway\n'
exit 0
fi
`

func TestVPNFixedArgvAndKeyDiscard(t *testing.T) {
	r := vpnRunner(t, exactInspect+`[ "$#" = 6 ] && [ "$1" = exec ] && [ "$2" = gateway ] && [ "$3" = wg ] && [ "$4" = show ] && [ "$5" = all ] && [ "$6" = dump ] || exit 2
printf 'wg0\tprivate-fixture\tpublic-fixture\t51820\toff\nwg0\tpeer-fixture\tpsk-fixture\t192.0.2.10:51820\t0.0.0.0/0\t1700000000\t10\t20\t0\n'
printf 'private-fixture' >&2
`)
	res := r.Run(context.Background(), "vpn-wg-dump", map[string]string{"container": "gateway"})
	if res.ExitCode != 0 || res.Error != "" || !strings.Contains(res.Stdout, "192.0.2.10:51820") {
		t.Fatal("WireGuard probe failed")
	}
	for _, key := range []string{"private-fixture", "public-fixture", "peer-fixture", "psk-fixture"} {
		if strings.Contains(res.Stdout+res.Stderr+res.Error+res.Command, key) {
			t.Fatal("key column escaped collection boundary")
		}
	}
	for _, name := range []string{"worker", "gateway-id", "--help"} {
		if r.Validate("vpn-wg-dump", map[string]string{"container": name}) == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	inv, _ := r.Inventory()
	inv.Docker.Containers = append(inv.Docker.Containers, inventory.Container{Name: "tunnel"})
	inv.VPNs = []inventory.VPN{*inv.VPN, {Container: "tunnel"}}
	for _, command := range []string{"vpn-wg-dump", "vpn-exit-check"} {
		for _, name := range []string{"gateway", "tunnel"} {
			if err := r.Validate(command, map[string]string{"container": name}); err != nil {
				t.Fatalf("gateway %s refused: %v", name, err)
			}
		}
	}
	inv.VPNs = []inventory.VPN{}
	if r.Validate("vpn-wg-dump", map[string]string{"container": "gateway"}) == nil {
		t.Fatal("empty vpns must override legacy vpn")
	}

	if r.Validate("vpn-exit-check", map[string]string{"container": "gateway", "url": "https://example.com"}) == nil {
		t.Fatal("accepted arbitrary argument")
	}
}
func TestVPNExitFallbackOnlyWhenCurlMissing(t *testing.T) {
	t.Setenv("VPN_CHECK_URL", "https://example.com/ip")
	for _, tc := range []struct {
		code, stderr string
		fallback     bool
	}{{"127", "", true}, {"126", "", true}, {"1", `exec: "curl": executable file not found in $PATH`, true}, {"22", "HTTP failure", false}, {"22", "curl: (22) requested URL returned Not Found", false}, {"28", "timeout", false}, {"0", "", false}} {
		t.Run(tc.code+tc.stderr, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "wget-called")
			script := exactInspect + `[ "$1" = exec ] && [ "$2" = gateway ] || exit 2
if [ "$3" = curl ]; then
[ "$#" = 7 ] && [ "$4" = -fsS ] && [ "$5" = --max-time ] && [ "$6" = 5 ] && [ "$7" = https://example.com/ip ] || exit 2
printf '%s' '` + tc.stderr + `' >&2
printf '192.0.2.20'
exit ` + tc.code + `
fi
[ "$#" = 7 ] && [ "$3" = wget ] && [ "$4" = -qO- ] && [ "$5" = -T ] && [ "$6" = 5 ] && [ "$7" = https://example.com/ip ] || exit 2
printf called > '` + marker + `'
printf '192.0.2.21'
`
			r := vpnRunner(t, script)
			res := r.Run(context.Background(), "vpn-exit-check", map[string]string{"container": "gateway"})
			_, err := os.Stat(marker)
			if (err == nil) != tc.fallback {
				t.Fatalf("fallback=%v want %v", err == nil, tc.fallback)
			}
			if tc.fallback && (res.ExitCode != 0 || res.Stdout != "192.0.2.21") {
				t.Fatal("fallback failed")
			}
		})
	}
}
func TestVPNLiveExactNameAndNarrowInspect(t *testing.T) {
	r := vpnRunner(t, `printf '/different\n'`)
	if r.Run(context.Background(), "vpn-wg-dump", map[string]string{"container": "gateway"}).ExitCode == 0 {
		t.Fatal("accepted live name mismatch")
	}
	argv := buildArgv("container-state", map[string]string{"container": "gateway"})
	if !reflect.DeepEqual(argv, []string{"docker", "inspect", "--type=container", "--format", containerStateFormat, "--", "gateway"}) || strings.Contains(strings.ToLower(argv[4]), "env") {
		t.Fatal("inspect scope widened")
	}
}
func TestWGOutputBoundsAndMalformedFailure(t *testing.T) {
	for _, raw := range []string{strings.Repeat("x", probeMaxOutput+1), "wg0\tkey-fixture\tkey-fixture\t1\toff\nwg0\tkey-fixture\tkey-fixture\tkey-fixture\tall\t1\t1\t1\t0"} {
		res := sanitizeProbeResult("vpn-wg-dump", Result{Stdout: raw, Stderr: raw})
		if res.Stdout != "" || res.Stderr != "" {
			t.Fatal("invalid output escaped")
		}
	}
	w := &boundedOutput{max: 16}
	_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
	if len(w.data) != 16 || !w.overflow {
		t.Fatal("unbounded collection")
	}
}
