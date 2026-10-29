// Package deps answers "what breaks if I stop this?".
//
// On a host with eighteen compose stacks the answer is rarely obvious, and it
// is least obvious in the case that matters most here: five services reach the
// network through vpnservice's namespace, so stopping vpnservice takes qBittorrent,
// Prowlarr, Jackett, Deluge and FlareSolverr off the network without stopping
// them. They keep running, keep looking healthy, and cannot talk to anything.
//
// # Three kinds of edge, and they are not equally severe
//
//   - **network**: the dependent shares this container's network namespace
//     (`NetworkMode: container:<id>`). Stopping the target severs the
//     dependent's networking entirely while leaving it running, which is the
//     most confusing failure of the three because nothing looks stopped.
//   - **declared**: compose's own `depends_on`. Compose will stop and start
//     them in order, so this is the mildest edge — it describes intent more
//     than consequence.
//   - **project**: same compose stack. Not really a dependency at all, but it
//     is what `docker compose down` acts on, so it belongs in the answer to
//     "what am I about to affect".
//
// The distinction is the point. Reporting one undifferentiated "depends on"
// list would put "these will be restarted in order" next to "these will
// silently lose all networking", and those need different reactions.
package deps

import (
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// EdgeKind is why one container depends on another.
type EdgeKind string

const (
	// KindNetwork is a shared network namespace: the severe one.
	KindNetwork EdgeKind = "network"
	// KindDeclared is compose's depends_on.
	KindDeclared EdgeKind = "declared"
	// KindProject is membership of the same compose stack.
	KindProject EdgeKind = "project"
)

// Dependent is one container affected by stopping another.
type Dependent struct {
	Name string   `json:"name"`
	Kind EdgeKind `json:"kind"`
	// Effect is the plain sentence: what actually happens to this one.
	Effect string `json:"effect"`
}

// Node is one container and what relies on it.
type Node struct {
	Name    string `json:"name"`
	Host    string `json:"host"`
	Project string `json:"compose_project,omitempty"`
	State   string `json:"state"`
	// NetworkMode is carried because "container:<id>" is the whole reason this
	// package exists, and seeing it makes the graph self-explanatory.
	NetworkMode string `json:"network_mode,omitempty"`
	// Dependents are the containers affected if this one stops, worst first.
	Dependents []Dependent `json:"dependents,omitempty"`
	// Provides counts them, so a UI can rank without walking the list.
	Provides int `json:"provides"`
	// SharesNetworkWith names the container this one routes through, if any.
	// It is the inverse edge, and it is what a reader of a *dependent* wants.
	SharesNetworkWith string `json:"shares_network_with,omitempty"`
}

// Report is one host's dependency graph.
type Report struct {
	Host  string `json:"host"`
	Nodes []Node `json:"nodes"`
}

// Build derives the graph for one host.
func Build(inv *inventory.Inventory) *Report {
	rep := &Report{Nodes: []Node{}}
	if inv == nil || inv.Docker == nil || !inv.Docker.Available {
		return rep
	}
	rep.Host = inv.Host.Hostname

	// Docker reports NetworkMode as "container:<full id>", while the schema
	// stores short ids, so match on prefix in both directions.
	byID := map[string]string{}
	for _, c := range inv.Docker.Containers {
		if c.ID != "" {
			byID[c.ID] = c.Name
		}
	}
	resolve := func(mode string) string {
		id, ok := strings.CutPrefix(mode, "container:")
		if !ok {
			return ""
		}
		if name, hit := byID[id]; hit {
			return name
		}
		for short, name := range byID {
			if strings.HasPrefix(id, short) || strings.HasPrefix(short, id) {
				return name
			}
		}
		// The target is not in this inventory — it may have been removed while
		// the dependent kept running. Naming the raw id beats saying nothing.
		return "container " + shorten(id)
	}

	// Compose depends_on labels name services, which may differ from container names.
	byService := map[string][]string{}
	for _, c := range inv.Docker.Containers {
		if c.ComposeProject != "" && c.ComposeService != "" {
			key := c.ComposeProject + "\x00" + c.ComposeService
			byService[key] = append(byService[key], c.Name)
		}
	}

	nodes := map[string]*Node{}
	get := func(name string) *Node {
		n := nodes[name]
		if n == nil {
			n = &Node{Name: name, Host: rep.Host}
			nodes[name] = n
		}
		return n
	}

	for _, c := range inv.Docker.Containers {
		n := get(c.Name)
		n.Project, n.State, n.NetworkMode = c.ComposeProject, c.State, c.NetworkMode

		if target := resolve(c.NetworkMode); target != "" && target != c.Name {
			n.SharesNetworkWith = target
			t := get(target)
			t.Dependents = append(t.Dependents, Dependent{
				Name: c.Name, Kind: KindNetwork,
				Effect: "loses all networking but keeps running",
			})
		}
		for _, dep := range c.DependsOn {
			dep, _, _ = strings.Cut(dep, ":")
			names := byService[c.ComposeProject+"\x00"+dep]
			if len(names) == 0 {
				names = []string{dep}
			}
			for _, dep := range names {
				if dep == "" || dep == c.Name {
					continue
				}
				t := get(dep)
				t.Dependents = append(t.Dependents, Dependent{
					Name: c.Name, Kind: KindDeclared,
					Effect: "compose will stop it first and start it after",
				})
			}
		}
	}

	// Project membership, added last so it never displaces a stronger edge.
	byProject := map[string][]string{}
	for _, c := range inv.Docker.Containers {
		if c.ComposeProject != "" {
			byProject[c.ComposeProject] = append(byProject[c.ComposeProject], c.Name)
		}
	}
	for _, members := range byProject {
		if len(members) < 2 {
			continue
		}
		for _, a := range members {
			n := get(a)
			for _, b := range members {
				if a == b || hasDependent(n.Dependents, b) {
					continue
				}
				n.Dependents = append(n.Dependents, Dependent{
					Name: b, Kind: KindProject,
					Effect: "same stack, so `compose down` takes it too",
				})
			}
		}
	}

	for _, n := range nodes {
		sort.Slice(n.Dependents, func(i, j int) bool {
			a, b := n.Dependents[i], n.Dependents[j]
			if a.Kind != b.Kind {
				return severity(a.Kind) > severity(b.Kind)
			}
			return a.Name < b.Name
		})
		n.Provides = len(n.Dependents)
		rep.Nodes = append(rep.Nodes, *n)
	}
	sort.Slice(rep.Nodes, func(i, j int) bool {
		// The containers other things rely on, first: that is the list worth
		// reading before touching anything.
		a, b := rep.Nodes[i], rep.Nodes[j]
		an, bn := countKind(a, KindNetwork), countKind(b, KindNetwork)
		if an != bn {
			return an > bn
		}
		if a.Provides != b.Provides {
			return a.Provides > b.Provides
		}
		return a.Name < b.Name
	})
	return rep
}

func hasDependent(list []Dependent, name string) bool {
	for _, d := range list {
		if d.Name == name {
			return true
		}
	}
	return false
}

func countKind(n Node, k EdgeKind) int {
	c := 0
	for _, d := range n.Dependents {
		if d.Kind == k {
			c++
		}
	}
	return c
}

func severity(k EdgeKind) int {
	switch k {
	case KindNetwork:
		return 3
	case KindDeclared:
		return 2
	default:
		return 1
	}
}

func shorten(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Critical returns the containers whose loss would silently break another's
// networking — the case worth knowing before a restart.
func (r *Report) Critical() []Node {
	var out []Node
	for _, n := range r.Nodes {
		if countKind(n, KindNetwork) > 0 {
			out = append(out, n)
		}
	}
	return out
}
