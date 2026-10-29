// Package portmap aggregates listening sockets across hosts into the view you
// need to answer "what is on this port, and which ports are free?".
//
// This lives in internal/ rather than inside cmd/portmap because the CLI is
// not the only consumer. The API will serve exactly this structure over
// HTTP, and the web UI will render it. Putting the aggregation in a command's
// main package would mean writing it twice and letting the two drift.
//
// The division of labour: this package produces data with structured fields,
// and formatting decisions — how to render an owner, how to truncate a long
// list of bind addresses — belong to whatever is displaying it. An API that
// returned pre-formatted strings would be useless to a web UI that wants to
// link a compose project to its page.
package portmap

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// PortMap is the aggregated view over one or more inventories.
type PortMap struct {
	GeneratedAt time.Time `json:"generated_at"`
	Hosts       []Host    `json:"hosts"`
	// Shared lists ports reserved on more than one host. Not a conflict —
	// separate machines may reuse a port freely — but the thing to know before
	// pointing a reverse proxy or a firewall rule at one of them.
	Shared []SharedPort `json:"shared"`
	// Warnings records inventories that could not be used as-is, so a caller
	// never silently reports on stale or partial input.
	Warnings []string `json:"warnings,omitempty"`
}

// Host is one machine's reserved ports.
type Host struct {
	Name           string    `json:"name"`
	OS             string    `json:"os,omitempty"`
	CollectedAt    time.Time `json:"collected_at"`
	SchemaVersion  string    `json:"schema_version"`
	EphemeralRange [2]int    `json:"ephemeral_range"`
	Ports          []Port    `json:"ports"`

	// InboundPosture carries the firewall verdict alongside the ports,
	// because "exposed" is only half an answer without it. A port bound to a
	// wildcard address is what the process asked for; whether anyone can take
	// it up is decided by the packet filter. Empty when the host's inventory
	// predates the firewall collector.
	InboundPosture string `json:"inbound_posture,omitempty"`
	FiltersInbound bool   `json:"filters_inbound,omitempty"`
	InboundSummary string `json:"inbound_summary,omitempty"`
}

// Port is one protocol+port on one host, collapsed from every socket bound to
// it. A published container port appears twice in the raw data (IPv4 and
// IPv6) and a service like Plex binds dozens; Sockets records how many
// collapsed into this entry.
type Port struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`

	// Owner fields mirror inventory.ListenerOwner, flattened. Kind is empty
	// when the socket could not be attributed.
	Kind           inventory.OwnerKind `json:"kind,omitempty"`
	Owner          string              `json:"owner,omitempty"`
	ContainerID    string              `json:"container_id,omitempty"`
	ComposeProject string              `json:"compose_project,omitempty"`
	ComposeService string              `json:"compose_service,omitempty"`
	// ContainerPort is set only when it differs from Port.
	ContainerPort int `json:"container_port,omitempty"`
	// Process is the listening process, kept even when Owner is set: it is
	// what you would look for in `ps`.
	Process string `json:"process,omitempty"`

	Addresses []string `json:"addresses"`
	Zones     []string `json:"zones,omitempty"`
	Exposed   bool     `json:"exposed"`
	Ephemeral bool     `json:"ephemeral"`
	Sockets   int      `json:"sockets"`
}

// SharedPort is a port reserved on several hosts.
type SharedPort struct {
	Port  int      `json:"port"`
	Hosts []string `json:"hosts"`
}

// Match is one result of looking a port up across hosts.
type Match struct {
	Host string `json:"host"`
	Port Port   `json:"port"`
}

// LoadDir reads every *.json inventory in dir.
//
// The API will replace this with a database read, which is why Build takes
// inventories rather than a directory: the aggregation does not care where
// they came from.
func LoadDir(dir string) ([]*inventory.Inventory, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)

	var out []*inventory.Inventory
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		var inv inventory.Inventory
		if err := json.Unmarshal(data, &inv); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", p, err)
		}
		if inv.Host.Hostname == "" {
			inv.Host.Hostname = strings.TrimSuffix(filepath.Base(p), ".json")
		}
		out = append(out, &inv)
	}
	return out, nil
}

// Build aggregates inventories into a PortMap.
func Build(invs []*inventory.Inventory) *PortMap {
	pm := &PortMap{
		GeneratedAt: time.Now().UTC(),
		Hosts:       []Host{},
		Shared:      []SharedPort{},
	}

	for _, inv := range invs {
		if inv == nil {
			continue
		}
		name := inv.Host.Hostname
		if inv.SchemaVersion != inventory.SchemaVersion {
			pm.Warnings = append(pm.Warnings, fmt.Sprintf(
				"%s is schema v%s but this build expects v%s; re-collect it",
				name, inv.SchemaVersion, inventory.SchemaVersion))
		}
		if inv.Network == nil {
			pm.Warnings = append(pm.Warnings, name+" has no network section")
			continue
		}
		h := Host{
			Name:           name,
			OS:             inv.Host.OS.Pretty,
			CollectedAt:    inv.CollectedAt,
			SchemaVersion:  inv.SchemaVersion,
			EphemeralRange: inv.Network.EphemeralRange,
			Ports:          collapse(inv.Network.Listeners),
		}
		if inv.Firewall != nil {
			h.InboundPosture = string(inv.Firewall.InboundPosture)
			h.FiltersInbound = inv.Firewall.FiltersInbound
			h.InboundSummary = inv.Firewall.InboundSummary
		}
		pm.Hosts = append(pm.Hosts, h)
	}
	sort.Slice(pm.Hosts, func(a, b int) bool { return pm.Hosts[a].Name < pm.Hosts[b].Name })

	pm.Shared = sharedPorts(pm.Hosts)
	return pm
}

// collapse merges listener rows into one entry per protocol+port.
func collapse(listeners []inventory.Listener) []Port {
	type key struct {
		protocol string
		port     int
	}
	merged := map[key]*Port{}

	for _, l := range listeners {
		k := key{l.Protocol, l.Port}
		p, ok := merged[k]
		if !ok {
			p = &Port{
				Port:      l.Port,
				Protocol:  l.Protocol,
				Ephemeral: l.Ephemeral,
				Addresses: []string{},
			}
			merged[k] = p
		}
		p.Sockets++
		p.Exposed = p.Exposed || l.Exposed
		p.Addresses = appendUnique(p.Addresses, l.Address)
		if l.Zone != "" {
			p.Zones = appendUnique(p.Zones, l.Zone)
		}
		if p.Process == "" {
			p.Process = l.Process
		}
		// First attributed socket wins. Sockets on the same port belong to the
		// same service in every case seen so far; if that ever stops being
		// true the Sockets count is the hint that something is worth a look.
		if p.Kind == "" && l.Owner != nil {
			p.Kind = l.Owner.Kind
			p.Owner = l.Owner.Name
			p.ContainerID = l.Owner.ContainerID
			p.ComposeProject = l.Owner.ComposeProject
			p.ComposeService = l.Owner.ComposeService
			if l.Owner.ContainerPort != 0 && l.Owner.ContainerPort != l.Port {
				p.ContainerPort = l.Owner.ContainerPort
			}
		}
	}

	out := make([]Port, 0, len(merged))
	for _, p := range merged {
		sort.Strings(p.Addresses)
		sort.Strings(p.Zones)
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Port != out[b].Port {
			return out[a].Port < out[b].Port
		}
		return out[a].Protocol < out[b].Protocol
	})
	return out
}

func sharedPorts(hosts []Host) []SharedPort {
	byPort := map[int][]string{}
	for _, h := range hosts {
		for _, p := range h.Ports {
			if p.Ephemeral {
				continue // a port the kernel handed out is not a reservation
			}
			byPort[p.Port] = appendUnique(byPort[p.Port], h.Name)
		}
	}
	var out []SharedPort
	for port, names := range byPort {
		if len(names) > 1 {
			sort.Strings(names)
			out = append(out, SharedPort{Port: port, Hosts: names})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Port < out[b].Port })
	if out == nil {
		out = []SharedPort{}
	}
	return out
}

// Lookup returns every host that has something on port.
func (pm *PortMap) Lookup(port int) []Match {
	var out []Match
	for _, h := range pm.Hosts {
		for _, p := range h.Ports {
			if p.Port == port {
				out = append(out, Match{Host: h.Name, Port: p})
			}
		}
	}
	return out
}

// Free returns up to n ports in [lo, hi] that are unused on every host.
//
// Requiring a port to be free everywhere is the conservative choice: a compose
// file copied from one box to another will not collide, and a service can be
// moved without renumbering. Ports inside any host's kernel ephemeral range
// are excluded — a port the kernel may hand to an outbound connection is a bad
// home for a service that binds late in boot.
func (pm *PortMap) Free(lo, hi, n int) []int {
	if lo < 1 || hi > 65535 || hi < lo || n <= 0 {
		return nil
	}
	used := map[int]bool{}
	for _, h := range pm.Hosts {
		for _, p := range h.Ports {
			used[p.Port] = true
		}
	}

	var free []int
	for port := lo; port <= hi && len(free) < n; port++ {
		if used[port] || pm.inAnyEphemeralRange(port) {
			continue
		}
		free = append(free, port)
	}
	return free
}

// inAnyEphemeralRange reports whether port falls in the dynamic range of any
// host. Hosts can be tuned differently, and a port that is safe on one box and
// not on another is not a port worth suggesting.
func (pm *PortMap) inAnyEphemeralRange(port int) bool {
	for _, h := range pm.Hosts {
		lo, hi := h.EphemeralRange[0], h.EphemeralRange[1]
		if lo > 0 && port >= lo && port <= hi {
			return true
		}
	}
	return false
}

// FilterHost returns a copy of pm containing only the named host. Shared is
// cleared because "shared with another host" is meaningless once every other
// host has been dropped.
func (pm *PortMap) FilterHost(name string) *PortMap {
	out := *pm
	out.Hosts = nil
	for _, h := range pm.Hosts {
		if h.Name == name {
			out.Hosts = append(out.Hosts, h)
		}
	}
	out.Shared = nil
	return &out
}

// HostNames returns the host names in the map, for messages.
func (pm *PortMap) HostNames() []string {
	out := make([]string, 0, len(pm.Hosts))
	for _, h := range pm.Hosts {
		out = append(out, h.Name)
	}
	return out
}

// Reserved returns the non-ephemeral ports of a host, and how many were
// hidden.
func (h Host) Reserved() (ports []Port, hidden int) {
	for _, p := range h.Ports {
		if p.Ephemeral {
			hidden++
			continue
		}
		ports = append(ports, p)
	}
	return ports, hidden
}

func appendUnique(ss []string, s string) []string {
	if s == "" {
		return ss
	}
	for _, existing := range ss {
		if existing == s {
			return ss
		}
	}
	return append(ss, s)
}
