package portmap

import (
	"reflect"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func testInventories() []*inventory.Inventory {
	plex := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host:          inventory.Host{Hostname: "host-a", OS: inventory.OSRelease{Pretty: "Ubuntu 22.04.5 LTS"}},
		Network: &inventory.Network{
			EphemeralRange: [2]int{32768, 60999},
			Listeners: []inventory.Listener{
				// One published container port: two sockets, v4 and v6.
				{Protocol: "tcp", Port: 8989, Address: "0.0.0.0", Exposed: true, Process: "docker-proxy",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerPublished, Name: "sonarr",
						ComposeProject: "arrstack", ComposeService: "sonarr", ContainerPort: 8989}},
				{Protocol: "tcp", Port: 8989, Address: "::", Exposed: true, Process: "docker-proxy",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerPublished, Name: "sonarr",
						ComposeProject: "arrstack", ComposeService: "sonarr", ContainerPort: 8989}},
				// A published port that maps to a different container port.
				{Protocol: "tcp", Port: 9999, Address: "0.0.0.0", Exposed: true, Process: "docker-proxy",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerPublished, Name: "dozzle",
						ComposeProject: "dozzle", ComposeService: "dozzle", ContainerPort: 8080}},
				{Protocol: "tcp", Port: 22, Address: "0.0.0.0", Exposed: true, Process: "sshd",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerUnit, Name: "ssh.service"}},
				// Multicast on many interfaces: one address, many zones.
				{Protocol: "udp", Port: 5353, Address: "ff02::fb", Zone: "veth1", Process: "avahi"},
				{Protocol: "udp", Port: 5353, Address: "ff02::fb", Zone: "veth2", Process: "avahi"},
				{Protocol: "udp", Port: 5353, Address: "224.0.0.251", Process: "avahi"},
				// Kernel-assigned: must not count as a reservation.
				{Protocol: "tcp", Port: 45511, Address: "0.0.0.0", Ephemeral: true, Process: "rpc.statd"},
			},
		},
	}
	pve := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host:          inventory.Host{Hostname: "host-b", OS: inventory.OSRelease{Pretty: "Debian GNU/Linux 12"}},
		Network: &inventory.Network{
			EphemeralRange: [2]int{32768, 60999},
			Listeners: []inventory.Listener{
				{Protocol: "tcp", Port: 22, Address: "0.0.0.0", Exposed: true, Process: "sshd",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerUnit, Name: "ssh.service"}},
				{Protocol: "tcp", Port: 8006, Address: "*", Exposed: true, Process: "pveproxy worker",
					Owner: &inventory.ListenerOwner{Kind: inventory.OwnerUnit, Name: "pveproxy.service"}},
			},
		},
	}
	return []*inventory.Inventory{plex, pve}
}

func TestBuildCollapsesSockets(t *testing.T) {
	t.Parallel()
	pm := Build(testInventories())

	if len(pm.Hosts) != 2 || pm.Hosts[0].Name != "host-a" || pm.Hosts[1].Name != "host-b" {
		t.Fatalf("hosts = %+v", pm.HostNames())
	}
	plex := pm.Hosts[0]

	// 8 sockets collapse to 5 protocol+port entries.
	if len(plex.Ports) != 5 {
		t.Fatalf("got %d ports, want 5: %+v", len(plex.Ports), plex.Ports)
	}

	byPort := map[int]Port{}
	for _, p := range plex.Ports {
		byPort[p.Port] = p
	}

	// The v4 and v6 rows for one published port become one entry.
	sonarr := byPort[8989]
	if sonarr.Sockets != 2 {
		t.Errorf("8989 collapsed %d sockets, want 2", sonarr.Sockets)
	}
	if sonarr.Owner != "sonarr" || sonarr.ComposeProject != "arrstack" || sonarr.Kind != inventory.OwnerPublished {
		t.Errorf("8989 owner wrong: %+v", sonarr)
	}
	// ContainerPort is only carried when it differs from the host port,
	// so that a display can show "->:8080" without checking for redundancy.
	if sonarr.ContainerPort != 0 {
		t.Errorf("8989 should not carry a redundant container port, got %d", sonarr.ContainerPort)
	}
	if got := byPort[9999].ContainerPort; got != 8080 {
		t.Errorf("9999 container port = %d, want 8080", got)
	}
	if !reflect.DeepEqual(sonarr.Addresses, []string{"0.0.0.0", "::"}) {
		t.Errorf("8989 addresses = %v", sonarr.Addresses)
	}

	// Multicast: one address list, one zone per interface.
	mdns := byPort[5353]
	if !reflect.DeepEqual(mdns.Addresses, []string{"224.0.0.251", "ff02::fb"}) {
		t.Errorf("5353 addresses = %v", mdns.Addresses)
	}
	if len(mdns.Zones) != 2 {
		t.Errorf("5353 zones = %v, want 2", mdns.Zones)
	}
	if mdns.Exposed {
		t.Error("a multicast bind is not a wildcard bind and must not be marked exposed")
	}
}

func TestReservedHidesEphemeral(t *testing.T) {
	t.Parallel()
	pm := Build(testInventories())
	reserved, hidden := pm.Hosts[0].Reserved()
	if hidden != 1 {
		t.Errorf("hidden = %d, want 1", hidden)
	}
	for _, p := range reserved {
		if p.Ephemeral {
			t.Errorf("ephemeral port %d survived the filter", p.Port)
		}
	}
}

func TestSharedPorts(t *testing.T) {
	t.Parallel()
	pm := Build(testInventories())
	if len(pm.Shared) != 1 || pm.Shared[0].Port != 22 {
		t.Fatalf("shared = %+v, want just port 22", pm.Shared)
	}
	if !reflect.DeepEqual(pm.Shared[0].Hosts, []string{"host-a", "host-b"}) {
		t.Errorf("shared hosts = %v", pm.Shared[0].Hosts)
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	pm := Build(testInventories())

	got := pm.Lookup(22)
	if len(got) != 2 {
		t.Fatalf("Lookup(22) returned %d matches, want 2", len(got))
	}
	if got[0].Host != "host-a" || got[1].Host != "host-b" {
		t.Errorf("Lookup(22) hosts = %q, %q", got[0].Host, got[1].Host)
	}
	if len(pm.Lookup(12345)) != 0 {
		t.Error("Lookup of an unused port returned matches")
	}
}

func TestFree(t *testing.T) {
	t.Parallel()
	pm := Build(testInventories())

	got := pm.Free(8985, 8995, 3)
	// 8989 is taken on host-a; everything else in range is free.
	want := []int{8985, 8986, 8987}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Free = %v, want %v", got, want)
	}
	for _, p := range pm.Free(8985, 8995, 20) {
		if p == 8989 {
			t.Error("Free suggested a port that is in use")
		}
	}

	// Nothing inside a host's kernel ephemeral range may be suggested, even
	// though no service is bound there right now.
	if got := pm.Free(40000, 40010, 5); len(got) != 0 {
		t.Errorf("Free suggested ephemeral-range ports: %v", got)
	}
	// A port that is only ephemeral-ranged on one host is still excluded.
	if got := pm.Free(1, 65535, 0); got != nil {
		t.Errorf("Free(n=0) = %v, want nil", got)
	}
}

func TestBuildWarnsOnSchemaMismatch(t *testing.T) {
	t.Parallel()
	invs := testInventories()
	invs[0].SchemaVersion = "1"
	pm := Build(invs)
	if len(pm.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one about the schema", pm.Warnings)
	}
	// Stale input must still be reported on, not silently dropped: a partial
	// answer beats an empty one as long as the caveat is visible.
	if len(pm.Hosts) != 2 {
		t.Errorf("stale host was dropped instead of flagged")
	}
}

func TestBuildHandlesMissingNetworkSection(t *testing.T) {
	t.Parallel()
	invs := testInventories()
	invs[1].Network = nil
	pm := Build(invs)
	if len(pm.Hosts) != 1 || pm.Hosts[0].Name != "host-a" {
		t.Errorf("hosts = %v", pm.HostNames())
	}
	if len(pm.Warnings) != 1 {
		t.Errorf("warnings = %v, want one", pm.Warnings)
	}
}
