package collect

import (
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Correlation is the step that runs after every collector, joining sections
// that reference each other.
//
// Why this is a separate step rather than work inside NetworkCollector:
// attributing a socket to a container needs the Docker section, and letting
// one collector read another's output would make the order in All() load-
// bearing and invisible. A collector that quietly breaks when you reorder the
// list is a trap. Keeping the join explicit means collectors stay independent
// and testable in isolation, and everything that needs two sections lives in
// one file you can read top to bottom.

// dockerCgroup matches a container ID inside a cgroup path. Docker writes
// several shapes depending on cgroup version and driver:
//
//	/docker/<id>                                  (cgroup v1, docker driver)
//	/system.slice/docker-<id>.scope               (cgroup v2, systemd driver)
//	/kubepods/.../docker-<id>.scope               (kubernetes)
//	/system.slice/libpod-<id>.scope               (podman)
var dockerCgroup = regexp.MustCompile(`(?:^|/)(?:docker[-/]|libpod-|crio-|containerd-)([0-9a-f]{12,64})`)

// unitCgroup matches a systemd unit in a cgroup path: the last path segment
// that names a .service or .scope.
var unitCgroup = regexp.MustCompile(`/([a-zA-Z0-9_.@\\:-]+\.(?:service|scope))`)

// correlate fills in derived, cross-section fields. It never fails the run:
// a missing correlation degrades the inventory, it does not invalidate it.
func correlate(env *Env, inv *inventory.Inventory) {
	correlateVPN(env, inv)
	if inv.Network == nil {
		return
	}
	inv.Network.EphemeralRange = readEphemeralRange(env)
	markEphemeral(inv.Network)
	attributeListeners(env, inv)
}

// correlateVPN joins Docker's namespace configuration with read-only proc
// observations. Capability-only candidates remain visible when stopped or
// when proc is inaccessible; no interface, peer or exit facts are invented.
func correlateVPN(env *Env, inv *inventory.Inventory) {
	if inv.Docker == nil || !inv.Docker.Available {
		return
	}
	type candidate struct {
		container  *inventory.Container
		tunnel     *vpnTunnel
		probeErr   error
		dependents []inventory.VPNDependent
	}
	var candidates []candidate
	for i := range inv.Docker.Containers {
		ct := &inv.Docker.Containers[i]
		if strings.HasPrefix(ct.NetworkMode, "container:") {
			continue // A namespace consumer is not its gateway.
		}
		tunnel, err := readVPNTunnel(env, ct)
		capable := false
		for _, cap := range ct.CapAdd {
			switch strings.TrimPrefix(strings.ToUpper(cap), "CAP_") {
			case "NET_ADMIN", "ALL":
				capable = true
			}
		}
		if tunnel == nil && !capable {
			continue
		}
		c := candidate{container: ct, tunnel: tunnel, probeErr: err, dependents: []inventory.VPNDependent{}}
		providerNS := vpnNetNS(env, ct)
		for j := range inv.Docker.Containers {
			dep := &inv.Docker.Containers[j]
			ref, shared := strings.CutPrefix(dep.NetworkMode, "container:")
			if !shared || !vpnContainerReference(ref, ct) {
				continue
			}
			d := inventory.VPNDependent{Name: dep.Name, State: dep.State, StartedAt: dep.StartedAt}
			if depNS := vpnNetNS(env, dep); providerNS != "" && depNS != "" {
				attached := providerNS == depNS
				d.Attached = &attached
			}
			c.dependents = append(c.dependents, d)
		}
		sort.Slice(c.dependents, func(i, j int) bool { return c.dependents[i].Name < c.dependents[j].Name })
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (len(a.dependents) > 0) != (len(b.dependents) > 0) {
			return len(a.dependents) > 0
		}
		if a.container.Name != b.container.Name {
			return a.container.Name < b.container.Name
		}
		return a.container.ID < b.container.ID
	})
	c := candidates[0]
	inv.VPN = &inventory.VPN{
		Container: c.container.Name, State: c.container.State,
		Health: c.container.Health, StartedAt: c.container.StartedAt,
		Dependents: c.dependents,
	}
	if c.tunnel != nil {
		inv.VPN.Interface = c.tunnel.name
		inv.VPN.InterfaceUp = true
		inv.VPN.RXBytes = &c.tunnel.rx
		inv.VPN.TXBytes = &c.tunnel.tx
	}
	if c.probeErr != nil {
		env.Infof("VPN gateway %q tunnel counters unavailable from proc", c.container.Name)
	}
	for _, other := range candidates[1:] {
		env.Infof("additional VPN gateway candidate %q was not selected", other.container.Name)
	}
}

func vpnContainerReference(ref string, ct *inventory.Container) bool {
	if ref == ct.Name || ref == strings.TrimPrefix(ct.Name, "/") || ref == ct.ID {
		return true
	}
	// Inventory IDs are shortened, but Docker network mode usually holds the
	// full ID. Never resolve an arbitrarily short prefix to a container.
	return len(ct.ID) >= 12 && len(ref) >= 12 &&
		(strings.HasPrefix(ref, ct.ID) || strings.HasPrefix(ct.ID, ref))
}

// readEphemeralRange reads the kernel's dynamic port range. The default is
// 32768-60999; a homelab that has tuned it downward would otherwise get
// misleading advice about which ports are safe to claim.
func readEphemeralRange(env *Env) [2]int {
	fallback := [2]int{32768, 60999}
	s, err := env.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		env.Infof("could not read the ephemeral port range, assuming %d-%d", fallback[0], fallback[1])
		return fallback
	}
	f := strings.Fields(s)
	if len(f) != 2 {
		return fallback
	}
	lo, err1 := strconv.Atoi(f[0])
	hi, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || lo <= 0 || hi < lo {
		return fallback
	}
	return [2]int{lo, hi}
}

func markEphemeral(n *inventory.Network) {
	lo, hi := n.EphemeralRange[0], n.EphemeralRange[1]
	for i := range n.Listeners {
		p := n.Listeners[i].Port
		n.Listeners[i].Ephemeral = p >= lo && p <= hi
	}
}

// attributeListeners resolves each socket to whatever is behind it.
//
// Three mechanisms, applied in order of how much they tell you:
//
//  1. The process's cgroup names a container. This is the strongest signal and
//     it is the only one that works for host-network containers such as Plex,
//     which publish no ports and look exactly like host processes to ss.
//  2. The socket belongs to docker-proxy, so the container is found through
//     docker's published-port table instead. The process is on the host; the
//     service is not.
//  3. The cgroup names a systemd unit, which is the answer for everything that
//     is not containerised.
//
// Known and intentional: when containers share a network namespace
// (network_mode: service:gerbil, or a vpnservice VPN sidecar) the owner reported
// is the namespace owner, not the service answering on the port. So :8080
// attributes to vpnservice even though qbittorrent's WebUI is behind it, and
// :443 to gerbil rather than traefik. That is the accurate answer to "who
// holds this port", which is the question port planning asks. Resolving the
// service behind the namespace would mean reading HostConfig.NetworkMode and
// following the chain — worth doing only if the ambiguity ever bites.
func attributeListeners(env *Env, inv *inventory.Inventory) {
	byID, published := dockerIndexes(inv.Docker)

	// One process owns many sockets; cache the cgroup read per PID.
	cgroups := map[int]string{}

	var attributed int
	for i := range inv.Network.Listeners {
		l := &inv.Network.Listeners[i]
		if l.PID == 0 {
			continue
		}

		cg, ok := cgroups[l.PID]
		if !ok {
			cg = readCgroup(env, l.PID)
			cgroups[l.PID] = cg
		}

		c := containerFromCgroup(cg, byID)
		switch {
		case c != nil:
			l.Owner = &inventory.ListenerOwner{
				Kind:           inventory.OwnerContainer,
				Name:           c.Name,
				ContainerID:    c.ID,
				ComposeProject: c.ComposeProject,
				ComposeService: c.ComposeService,
			}

		case isDockerProxy(l.Process):
			if pub, ok := published[portKey{l.Protocol, l.Port}]; ok {
				l.Owner = &inventory.ListenerOwner{
					Kind:           inventory.OwnerPublished,
					Name:           pub.container.Name,
					ContainerID:    pub.container.ID,
					ComposeProject: pub.container.ComposeProject,
					ComposeService: pub.container.ComposeService,
					ContainerPort:  pub.containerPort,
				}
			}

		default:
			if unit := unitFromCgroup(cg); unit != "" {
				l.Owner = &inventory.ListenerOwner{Kind: inventory.OwnerUnit, Name: unit}
			} else if l.Process != "" {
				l.Owner = &inventory.ListenerOwner{Kind: inventory.OwnerProcess, Name: l.Process}
			}
		}

		if l.Owner != nil {
			attributed++
		}
	}

	total := len(inv.Network.Listeners)
	if total > 0 {
		env.Infof("attributed %d of %d listening sockets to a container or unit", attributed, total)
	}
}

// portKey identifies a published host port.
type portKey struct {
	protocol string
	port     int
}

type publishedPort struct {
	container     *inventory.Container
	containerPort int
}

// dockerIndexes builds lookup tables from the Docker section: containers by
// full-and-short ID prefix, and published host ports to their container.
func dockerIndexes(d *inventory.Docker) (map[string]*inventory.Container, map[portKey]publishedPort) {
	byID := map[string]*inventory.Container{}
	published := map[portKey]publishedPort{}
	if d == nil {
		return byID, published
	}

	for i := range d.Containers {
		c := &d.Containers[i]
		byID[c.ID] = c
		for _, p := range c.Ports {
			for _, m := range parsePortMapping(p) {
				// v4 and v6 rows map to the same container; first wins.
				k := portKey{m.protocol, m.hostPort}
				if _, exists := published[k]; !exists {
					published[k] = publishedPort{container: c, containerPort: m.containerPort}
				}
			}
		}
	}
	return byID, published
}

// portMapping is one published host port and the container port behind it.
type portMapping struct {
	hostPort      int
	containerPort int
	protocol      string
}

// maxPortRange caps how many ports one entry may expand into. Docker will
// happily publish 1-65535; expanding that into individual map entries would be
// pointless and a memory hazard on a small VM.
const maxPortRange = 1024

// parsePortMapping reads one entry of docker's Ports column and returns every
// host port it reserves.
//
// The shapes in the wild:
//
//	0.0.0.0:8096->8096/tcp        published, single port
//	:::8096->8096/tcp             the IPv6 row for the same mapping
//	0.0.0.0:3010->8000/tcp        published on a different host port
//	0.0.0.0:8265-8266->8265-8266/tcp   a range (tdarr does this)
//	8096/tcp                      exposed but not published: reserves nothing
//
// The range form is why this returns a slice. The first version returned a
// single mapping and silently dropped ranges, which left four real
// docker-proxy sockets unattributed on the main VM.
func parsePortMapping(s string) []portMapping {
	hostSide, containerSide, found := strings.Cut(strings.TrimSpace(s), "->")
	if !found {
		return nil // exposed but not published
	}

	portSpec, proto, found := strings.Cut(containerSide, "/")
	if !found {
		return nil
	}
	containerLo, containerHi, ok := parsePortRange(portSpec)
	if !ok {
		return nil
	}

	// The host side is address:port, where the address may be IPv6 and full of
	// colons; the port spec is always after the last one.
	i := strings.LastIndex(hostSide, ":")
	if i < 0 {
		return nil
	}
	hostLo, hostHi, ok := parsePortRange(hostSide[i+1:])
	if !ok {
		return nil
	}

	// Docker always publishes ranges of equal width, but a mismatch would
	// silently misattribute ports, so refuse rather than guess.
	if hostHi-hostLo != containerHi-containerLo || hostHi-hostLo >= maxPortRange {
		return nil
	}

	out := make([]portMapping, 0, hostHi-hostLo+1)
	for off := 0; hostLo+off <= hostHi; off++ {
		out = append(out, portMapping{
			hostPort:      hostLo + off,
			containerPort: containerLo + off,
			protocol:      proto,
		})
	}
	return out
}

// parsePortRange accepts "8096" or "8265-8266".
func parsePortRange(s string) (lo, hi int, ok bool) {
	loStr, hiStr, isRange := strings.Cut(s, "-")
	lo, err := strconv.Atoi(loStr)
	if err != nil || lo < 1 || lo > 65535 {
		return 0, 0, false
	}
	if !isRange {
		return lo, lo, true
	}
	hi, err = strconv.Atoi(hiStr)
	if err != nil || hi < lo || hi > 65535 {
		return 0, 0, false
	}
	return lo, hi, true
}

func isDockerProxy(process string) bool {
	return process == "docker-proxy" || process == "rootlesskit-por"
}

// readCgroup returns the raw contents of /proc/<pid>/cgroup.
func readCgroup(env *Env, pid int) string {
	s, err := env.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return ""
	}
	return s
}

// containerFromCgroup resolves a cgroup path to a container.
//
// Docker writes the full 64-character ID into the cgroup path while the
// inventory stores the 12-character short form, so the lookup is by prefix.
func containerFromCgroup(cgroup string, byID map[string]*inventory.Container) *inventory.Container {
	m := dockerCgroup.FindStringSubmatch(cgroup)
	if m == nil {
		return nil
	}
	return byID[shortID(m[1])]
}

// unitFromCgroup extracts a systemd unit name, preferring a .service over the
// .scope that may nest inside it.
func unitFromCgroup(cgroup string) string {
	var scope string
	for _, m := range unitCgroup.FindAllStringSubmatch(cgroup, -1) {
		name := filepath.Base(m[1])
		if strings.HasSuffix(name, ".service") {
			return name
		}
		if scope == "" {
			scope = name
		}
	}
	return scope
}
