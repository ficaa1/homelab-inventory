package fwplan

import (
	"net"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func listener(proto string, port int, process, owner string, exposed bool) inventory.Listener {
	l := inventory.Listener{Protocol: proto, Port: port, Process: process, Exposed: exposed}
	if exposed {
		l.Address = "0.0.0.0"
	} else {
		l.Address = "127.0.0.1"
	}
	if owner != "" {
		l.Owner = &inventory.ListenerOwner{Name: owner}
	}
	return l
}

func host(name string, ifaces []inventory.Interface, ls ...inventory.Listener) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host:          inventory.Host{Hostname: name, Interfaces: ifaces},
		Network:       &inventory.Network{Listeners: ls},
	}
}

func eth(addr string) []inventory.Interface {
	return []inventory.Interface{
		{Name: "lo", Kind: inventory.IfKindLoopback, Addresses: []string{"127.0.0.1/8"}},
		{Name: "ens18", Kind: inventory.IfKindPhysical, Addresses: []string{addr}},
	}
}

func build(t *testing.T, inv *inventory.Inventory, opts Options) *Plan {
	t.Helper()
	p, err := Build(inv, opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return p
}

// A ruleset that assumes the default SSH port can lock out an operator.
func TestSSHPortIsDetectedAndAlwaysAllowed(t *testing.T) {
	t.Parallel()
	inv := host("host-a", eth("192.0.2.10/24"),
		listener("tcp", 2222, "sshd", "ssh.service", true),
		listener("tcp", 8989, "docker-proxy", "sonarr", true),
	)
	p := build(t, inv, Options{})

	if p.SSHPort != 2222 {
		t.Fatalf("SSHPort = %d, want 2222 — the port actually in use", p.SSHPort)
	}
	if !strings.Contains(p.Ruleset, "tcp dport 2222 accept") {
		t.Errorf("the ruleset does not open SSH:\n%s", p.Ruleset)
	}
	// It must be allowed before the service rules, so a mistake further down
	// cannot strand you.
	sshAt := strings.Index(p.Ruleset, "tcp dport 2222 accept")
	svcAt := strings.Index(p.Ruleset, "dport 8989")
	if sshAt < 0 || svcAt < 0 || sshAt > svcAt {
		t.Error("SSH is not allowed before the service rules")
	}
	// And it must not be duplicated as an ordinary service rule.
	if strings.Count(p.Ruleset, "dport 2222") != 1 {
		t.Errorf("SSH appears more than once:\n%s", p.Ruleset)
	}
}

// A generated firewall with no SSH rule is a brick. If the port cannot be
// worked out, that has to be shouted about rather than quietly omitted.
func TestUnknownSSHPortIsWarnedAboutLoudly(t *testing.T) {
	t.Parallel()
	inv := host("box", eth("192.0.2.23/24"),
		listener("tcp", 8080, "someapp", "", true))
	p := build(t, inv, Options{})

	if p.SSHPort != 0 {
		t.Fatalf("SSHPort = %d, want 0 when nothing looks like sshd", p.SSHPort)
	}
	var warned bool
	for _, w := range p.Warnings {
		if strings.Contains(w, "lock you out") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no lockout warning when the SSH port is unknown: %v", p.Warnings)
	}
}

// Docker rewrites its own nftables chains on every container start. A ruleset
// that flushed would break container networking in ways that look like
// application bugs.
func TestNeverFlushesAndUsesItsOwnTable(t *testing.T) {
	t.Parallel()
	inv := host("host-a", eth("192.0.2.10/24"),
		listener("tcp", 2222, "sshd", "ssh.service", true))
	inv.Docker = &inventory.Docker{Available: true, Containers: []inventory.Container{
		{Name: "plex", Image: "i", State: "running"},
	}}
	p := build(t, inv, Options{})

	for _, forbidden := range []string{"flush ruleset", "flush table", "delete table ip filter",
		"chain INPUT", "chain FORWARD", "table ip filter", "table ip nat"} {
		if strings.Contains(p.Ruleset, forbidden) {
			t.Errorf("the ruleset touches something it must not (%q):\n%s", forbidden, p.Ruleset)
		}
	}
	if !strings.Contains(p.Ruleset, "table inet homelab") {
		t.Error("the ruleset does not use its own table")
	}
	if p.Rollback != "nft delete table inet homelab" {
		t.Errorf("rollback = %q, want a one-line removal of just this table", p.Rollback)
	}
	var warned bool
	for _, w := range p.Warnings {
		if strings.Contains(w, "Docker") && strings.Contains(w, "flush ruleset") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no Docker warning on a host running containers: %v", p.Warnings)
	}
}

// The safety-net claim must track reality. host-b has a tailscale0 interface with
// only a link-local address — Tailscale installed, not connected — and calling
// that a way back in is how somebody gets locked out trusting a route that was
// never up.
func TestTailnetIsOnlyCalledASafetyNetWhenItIsOne(t *testing.T) {
	t.Parallel()
	base := []inventory.Interface{
		{Name: "lo", Kind: inventory.IfKindLoopback, Addresses: []string{"127.0.0.1/8"}},
		{Name: "vmbr0", Kind: inventory.IfKindBridge, Addresses: []string{"192.0.2.11/24"}},
	}
	ssh := listener("tcp", 2222, "sshd", "ssh.service", true)

	// Synthetic first host in the standard CGNAT range, not a real tailnet address.
	connected := host("host-a", append(base, inventory.Interface{
		Name: "tailscale0", Kind: inventory.IfKindTunnel,
		Addresses: []string{net.IPv4(100, 64, 0, 1).String() + "/32"}}), ssh)
	p := build(t, connected, Options{})
	if !hasWarning(p, "safety net") {
		t.Errorf("a connected tailnet should be called a safety net: %v", p.Warnings)
	}

	// Installed but not logged in: only a link-local IPv6 address.
	notUp := host("host-b", append(base, inventory.Interface{
		Name: "tailscale0", Kind: inventory.IfKindTunnel,
		Addresses: []string{"fe80::4d98:596a:e10f:39e5/64"}}), ssh)
	p = build(t, notUp, Options{})
	if hasWarning(p, "safety net") {
		t.Errorf("an unconnected tailnet was called a safety net: %v", p.Warnings)
	}
	if !hasWarning(p, "not connected") {
		t.Errorf("an unconnected tailnet was not flagged: %v", p.Warnings)
	}
	if !strings.Contains(p.Ruleset, "NOT connected") {
		t.Errorf("the ruleset itself does not say the tailnet is down:\n%s", p.Ruleset)
	}

	// No interface at all.
	none := host("node1", base, ssh)
	p = build(t, none, Options{})
	if hasWarning(p, "safety net") {
		t.Errorf("a host with no tailnet claimed one: %v", p.Warnings)
	}
	if strings.Contains(p.Ruleset, "tailscale0") {
		t.Errorf("a rule was emitted for an interface that does not exist:\n%s", p.Ruleset)
	}
}

func hasWarning(p *Plan, substr string) bool {
	for _, w := range p.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// Opening a loopback-only port would widen access, which is the opposite of
// the point. Ephemeral ports are a different number tomorrow.
func TestOnlyExposedNonEphemeralPortsAreOpened(t *testing.T) {
	t.Parallel()
	inv := host("host-c", eth("192.0.2.12/24"),
		listener("tcp", 22, "sshd", "ssh.service", true),
		listener("tcp", 5432, "postgres", "", false),       // loopback only
		listener("tcp", 45123, "rpc.statd", "", true),      // ephemeral
		listener("tcp", 445, "smbd", "smbd.service", true), // real service
	)
	inv.Network.Listeners[2].Ephemeral = true

	p := build(t, inv, Options{})
	if strings.Contains(p.Ruleset, "5432") {
		t.Error("a loopback-only port was opened to the network")
	}
	if strings.Contains(p.Ruleset, "45123") {
		t.Error("an ephemeral port was opened")
	}
	if !strings.Contains(p.Ruleset, "dport 445") {
		t.Error("a real exposed service was not opened")
	}
}

// The essentials, without which a default-deny chain breaks the host in ways
// that are hard to attribute.
func TestRulesetKeepsTheHostFunctional(t *testing.T) {
	t.Parallel()
	inv := host("box", eth("192.0.2.23/24"), listener("tcp", 22, "sshd", "ssh.service", true))
	p := build(t, inv, Options{AllowICMP: true})

	for _, essential := range []string{
		"iif lo accept",
		"ct state established,related accept",
		"ct state invalid drop",
		"meta l4proto icmp accept",
		"policy drop",
	} {
		if !strings.Contains(p.Ruleset, essential) {
			t.Errorf("the ruleset is missing %q:\n%s", essential, p.Ruleset)
		}
	}
}

// host-a has seventeen Docker bridges holding private addresses; deriving the LAN
// from one of those would scope every rule to a network nothing is on.
func TestLANIsDerivedFromTheRealInterface(t *testing.T) {
	t.Parallel()
	inv := host("host-a", []inventory.Interface{
		{Name: "docker0", Kind: inventory.IfKindBridge, Addresses: []string{"172.17.0.1/16"}},
		{Name: "br-0ae78", Kind: inventory.IfKindBridge, Addresses: []string{"192.0.2.28/20"}},
		{Name: "ens18", Kind: inventory.IfKindPhysical, Addresses: []string{"192.0.2.10/24"}},
	}, listener("tcp", 2222, "sshd", "ssh.service", true))

	p := build(t, inv, Options{})
	if p.LAN != "192.0.2.0/24" {
		t.Errorf("LAN = %q, want the physical interface's subnet", p.LAN)
	}

	// host-b's address lives on vmbr0, a bridge that is not Docker's.
	pve := host("host-b", []inventory.Interface{
		{Name: "vmbr0", Kind: inventory.IfKindBridge, Addresses: []string{"192.0.2.11/24"}},
	}, listener("tcp", 2222, "sshd", "ssh.service", true))
	if p := build(t, pve, Options{}); p.LAN != "192.0.2.0/24" {
		t.Errorf("LAN = %q for a host addressed on vmbr0", p.LAN)
	}

	// An explicit option always wins.
	if p := build(t, inv, Options{LAN: "10.0.0.0/8"}); p.LAN != "10.0.0.0/8" {
		t.Errorf("LAN = %q, want the explicit option", p.LAN)
	}
}

// Every plan must carry the means to undo itself, because applying a firewall
// to a machine you are not sitting next to is the case that goes wrong.
func TestEveryPlanCarriesRollbackAndATimedTrial(t *testing.T) {
	t.Parallel()
	inv := host("box", eth("192.0.2.23/24"), listener("tcp", 22, "sshd", "ssh.service", true))
	p := build(t, inv, Options{})

	if p.Rollback == "" {
		t.Error("no rollback")
	}
	if !strings.Contains(p.TrialCommand, "sleep") || !strings.Contains(p.TrialCommand, "delete table") {
		t.Errorf("the trial command has no dead-man's switch:\n%s", p.TrialCommand)
	}
	if !strings.Contains(p.Ruleset, p.Rollback) {
		t.Error("the ruleset does not tell the reader how to undo it")
	}
}

func TestRefusesWithoutTheDataToWorkFrom(t *testing.T) {
	t.Parallel()
	if _, err := Build(nil, Options{}); err == nil {
		t.Error("accepted a nil inventory")
	}
	noNet := &inventory.Inventory{Host: inventory.Host{Hostname: "x"}}
	if _, err := Build(noNet, Options{}); err == nil {
		t.Error("accepted a host with no network section")
	}
	noAddr := host("x", []inventory.Interface{
		{Name: "lo", Kind: inventory.IfKindLoopback, Addresses: []string{"127.0.0.1/8"}},
	})
	if _, err := Build(noAddr, Options{}); err == nil {
		t.Error("invented a subnet for a host with no routable address")
	}
}

func TestRulesAreNamedAndOrdered(t *testing.T) {
	t.Parallel()
	inv := host("host-a", eth("192.0.2.10/24"),
		listener("tcp", 2222, "sshd", "ssh.service", true),
		listener("tcp", 8989, "docker-proxy", "sonarr", true),
		listener("tcp", 2283, "docker-proxy", "immich_server", true),
	)
	inv.Network.Listeners[1].Owner.ComposeProject = "arrstack"
	p := build(t, inv, Options{})

	if len(p.Rules) != 2 {
		t.Fatalf("rules = %d, want 2 (SSH is separate)", len(p.Rules))
	}
	if p.Rules[0].Port != 2283 {
		t.Errorf("rules are not ordered by port: %+v", p.Rules)
	}
	// The comment is what makes the ruleset readable in six months.
	if !strings.Contains(p.Ruleset, `comment "sonarr (arrstack)"`) {
		t.Errorf("a rule is not labelled with its service:\n%s", p.Ruleset)
	}
}
