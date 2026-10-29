package deps

import (
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func inv(cs ...inventory.Container) *inventory.Inventory {
	return &inventory.Inventory{
		Host:   inventory.Host{Hostname: "host-a"},
		Docker: &inventory.Docker{Available: true, Containers: cs},
	}
}

func node(t *testing.T, r *Report, name string) Node {
	t.Helper()
	for _, n := range r.Nodes {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("no node %q", name)
	return Node{}
}

func dep(n Node, name string) (Dependent, bool) {
	for _, d := range n.Dependents {
		if d.Name == name {
			return d, true
		}
	}
	return Dependent{}, false
}

// The case this package exists for. Stopping vpnservice takes five services off
// the network without stopping them: they keep running, keep looking healthy,
// and cannot talk to anything.
func TestSharedNetworkNamespaceIsTheSevereEdge(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "abc123", Name: "vpnservice", State: "running",
			ComposeProject: "alternatevpnservicestack", NetworkMode: "alternatevpnservicestack_default"},
		inventory.Container{ID: "def456", Name: "qbittorrent", State: "running",
			ComposeProject: "alternatevpnservicestack", NetworkMode: "container:abc123"},
		inventory.Container{ID: "ghi789", Name: "prowlarr", State: "running",
			ComposeProject: "alternatevpnservicestack", NetworkMode: "container:abc123"},
	))

	g := node(t, r, "vpnservice")
	d, ok := dep(g, "qbittorrent")
	if !ok {
		t.Fatalf("vpnservice has no qbittorrent dependent: %+v", g.Dependents)
	}
	if d.Kind != KindNetwork {
		t.Errorf("kind = %q, want network", d.Kind)
	}
	if d.Effect == "" {
		t.Error("the severe edge has no explanation of what actually happens")
	}
	// The container others rely on sorts first: that is the list worth reading
	// before touching anything.
	if r.Nodes[0].Name != "vpnservice" {
		t.Errorf("nodes[0] = %q, want vpnservice", r.Nodes[0].Name)
	}

	// The inverse edge is what a reader of the *dependent* wants.
	if q := node(t, r, "qbittorrent"); q.SharesNetworkWith != "vpnservice" {
		t.Errorf("SharesNetworkWith = %q, want vpnservice", q.SharesNetworkWith)
	}
}

// Docker reports the full id in NetworkMode while the schema stores a short
// one, so the match has to work in both directions.
func TestResolvesShortAndLongContainerIDs(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "abc123def456", Name: "vpnservice", State: "running"},
		// Short id in the schema, full id in NetworkMode.
		inventory.Container{ID: "zzz", Name: "qbt", State: "running",
			NetworkMode: "container:abc123def4567890abcdef"},
	))
	if q := node(t, r, "qbt"); q.SharesNetworkWith != "vpnservice" {
		t.Errorf("SharesNetworkWith = %q, want vpnservice", q.SharesNetworkWith)
	}
}

// A namespace target that is no longer in the inventory must not vanish
// silently — naming the raw id beats saying nothing.
func TestUnknownNamespaceTargetIsStillReported(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "zzz", Name: "orphan", State: "running",
			NetworkMode: "container:0123456789abcdef"},
	))
	n := node(t, r, "orphan")
	if n.SharesNetworkWith == "" {
		t.Error("a dependent on a missing container reported no dependency at all")
	}
}

// The three edge kinds need different reactions, so they must not be merged.
func TestDeclaredAndProjectEdgesAreDistinct(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "1", Name: "database", State: "running", ComposeProject: "immich"},
		inventory.Container{ID: "2", Name: "immich_server", State: "running",
			ComposeProject: "immich", DependsOn: []string{"database"}},
		inventory.Container{ID: "3", Name: "redis", State: "running", ComposeProject: "immich"},
	))

	db := node(t, r, "database")
	d, ok := dep(db, "immich_server")
	if !ok || d.Kind != KindDeclared {
		t.Errorf("immich_server -> database should be a declared edge, got %+v", d)
	}
	// A stronger edge must not be displaced by mere project membership.
	if d.Kind == KindProject {
		t.Error("project membership overwrote a declared dependency")
	}
	// Same-stack members still show up, because `compose down` takes them.
	if d2, ok := dep(db, "redis"); !ok || d2.Kind != KindProject {
		t.Errorf("redis should be a project edge, got %+v", d2)
	}
}

func TestCriticalListsOnlyNetworkProviders(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "1", Name: "vpnservice", State: "running"},
		inventory.Container{ID: "2", Name: "qbt", State: "running", NetworkMode: "container:1"},
		inventory.Container{ID: "3", Name: "grafana", State: "running", ComposeProject: "monitoring"},
		inventory.Container{ID: "4", Name: "cadvisor", State: "running", ComposeProject: "monitoring"},
	))
	c := r.Critical()
	if len(c) != 1 || c[0].Name != "vpnservice" {
		t.Errorf("Critical = %+v, want just vpnservice", c)
	}
}

// A section that did not run is not a host with no containers.
func TestNoDockerIsEmptyNotAnError(t *testing.T) {
	t.Parallel()
	if r := Build(&inventory.Inventory{Host: inventory.Host{Hostname: "host-b"}}); len(r.Nodes) != 0 {
		t.Errorf("nodes = %+v, want none", r.Nodes)
	}
	if r := Build(nil); r == nil || len(r.Nodes) != 0 {
		t.Error("a nil inventory should give an empty report, not a panic")
	}
}

// A container in a one-container stack has no project edges to itself.
func TestNoSelfEdges(t *testing.T) {
	t.Parallel()
	r := Build(inv(
		inventory.Container{ID: "1", Name: "plex", State: "running", ComposeProject: "plex"},
	))
	if n := node(t, r, "plex"); len(n.Dependents) != 0 {
		t.Errorf("a lone container has dependents: %+v", n.Dependents)
	}
}
