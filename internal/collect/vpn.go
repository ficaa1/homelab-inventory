package collect

import (
	"errors"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type vpnTunnel struct {
	name   string
	rx, tx uint64
}

// proc's per-process net/dev is scoped to that process's network namespace.
// Interface presence and counters are the only available tunnel facts: they
// cannot prove handshake freshness, link operstate, peer endpoint or exit IP.
func readVPNTunnel(env *Env, ct *inventory.Container) (*vpnTunnel, error) {
	if !vpnProcessRunning(ct) {
		return nil, nil
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("proc is unavailable on this platform")
	}
	raw, err := env.ReadFile("/proc/" + strconv.Itoa(ct.PID) + "/net/dev")
	if err != nil {
		return nil, err
	}
	return parseVPNNetDev(raw)
}

func parseVPNNetDev(raw string) (*vpnTunnel, error) {
	var tunnels []vpnTunnel
	for _, line := range strings.Split(raw, "\n") {
		name, counts, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || !vpnInterfaceName(name) {
			continue
		}
		fields := strings.Fields(counts)
		if len(fields) != 16 {
			return nil, errors.New("invalid tunnel counters")
		}
		rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
		tx, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			return nil, errors.New("invalid tunnel counters")
		}
		tunnels = append(tunnels, vpnTunnel{name: name, rx: rx, tx: tx})
	}
	if len(tunnels) == 0 {
		if !strings.Contains(raw, "Inter-") || !strings.Contains(raw, "Receive") {
			return nil, errors.New("invalid network device table")
		}
		return nil, nil
	}
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i].name < tunnels[j].name })
	return &tunnels[0], nil
}

func vpnInterfaceName(name string) bool {
	for _, prefix := range []string{"wg", "tun", "tap"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}

func vpnProcessRunning(ct *inventory.Container) bool {
	return ct.PID > 0 && (ct.State == "running" || ct.State == "paused" || ct.State == "restarting")
}

func vpnNetNS(env *Env, ct *inventory.Container) string {
	if runtime.GOOS != "linux" || !vpnProcessRunning(ct) {
		return ""
	}
	ns, err := os.Readlink(env.Path("/proc/" + strconv.Itoa(ct.PID) + "/ns/net"))
	if err != nil || !strings.HasPrefix(ns, "net:[") || !strings.HasSuffix(ns, "]") {
		return ""
	}
	if id, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(ns, "net:["), "]"), 10, 64); err != nil || id == 0 {
		return ""
	}
	return ns
}
