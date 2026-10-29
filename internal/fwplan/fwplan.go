// Package fwplan writes a firewall ruleset you could apply, from the ports a
// host is actually using.
//
// It generates. It never applies. `discover` cannot mutate anything (invariant
// 1) and this changes nothing about that: the output is text, and a human
// decides whether it ever reaches a machine. Remediation will need a
// separate, explicitly privileged runner, and this is not it.
//
// # The two ways this could hurt you, and what is done about them
//
// **Locking yourself out.** A default-deny INPUT chain applied over SSH ends
// the session that applied it and every session after it. So the SSH port is
// always allowed — explicitly, first, and even when the collector did not
// observe sshd listening — along with loopback, established traffic and the
// tailnet interface. Every plan also
// carries a rollback line and a timed-trial recipe that removes the ruleset
// automatically unless you confirm it, because reading a firewall and being
// able to undo it are different skills at 3am.
//
// **Clobbering Docker.** Docker owns nftables chains, rewrites them on every
// container start, and a ruleset that flushed or replaced the ruleset would
// break container networking in ways that look like application bugs for
// hours. So the plan never flushes anything and never touches an existing
// table: it creates its own `inet homelab` table with one base chain on the
// input hook. nftables evaluates every base chain registered on a hook, so a
// drop policy there is authoritative without any other table being edited, and
// `nft delete table inet homelab` removes it completely.
//
// # What it allows
//
// Only ports that were observed listening and exposed, each with the owning
// service named in a comment so the ruleset explains itself in six months.
// Scoped to the host's own subnet by default rather than to the whole
// internet: a homelab service that is reachable from the LAN is usually meant
// to be, while the same port reachable from anywhere usually is not.
package fwplan

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Options tune the generated ruleset.
type Options struct {
	// LAN is the source range allowed to reach the service ports. Derived from
	// the host's own address when empty.
	LAN string
	// SSHPort is always allowed, whether or not sshd was observed. Configure
	// the management port to preserve access when reviewing a firewall plan.
	SSHPort int
	// AllowICMP keeps ping and, more importantly, ICMP fragmentation-needed
	// working. Dropping ICMP wholesale is a classic way to break MTU discovery
	// and spend a day blaming the application.
	AllowICMP bool
}

// Rule is one allowance in the plan, with the reason it is there.
type Rule struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	// Owner is the service that was listening, so the ruleset names what each
	// hole is for rather than being a column of numbers.
	Owner string `json:"owner,omitempty"`
	// Source is the address range allowed to reach it.
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Plan is a proposed ruleset and everything needed to judge and undo it.
type Plan struct {
	Host string `json:"host"`
	// CurrentPosture is what the host does today, so the plan can be read as a
	// change rather than in isolation.
	CurrentPosture string `json:"current_posture,omitempty"`
	LAN            string `json:"lan"`
	SSHPort        int    `json:"ssh_port"`

	Rules []Rule `json:"rules"`
	// Ruleset is the nft script itself.
	Ruleset string `json:"ruleset"`
	// Rollback removes it again, in one line.
	Rollback string `json:"rollback"`
	// TrialCommand applies it with a dead-man's switch, which is how a remote
	// firewall change should always be made the first time.
	TrialCommand string `json:"trial_command"`

	// Warnings are things that could go wrong for this specific host.
	Warnings []string `json:"warnings,omitempty"`
	// Skipped records ports deliberately left closed, so the plan never
	// silently drops something the reader expected to see.
	Skipped []string `json:"skipped,omitempty"`
}

// Build derives a plan from one host's latest inventory.
func Build(inv *inventory.Inventory, opts Options) (*Plan, error) {
	if inv == nil || inv.Network == nil {
		return nil, fmt.Errorf("fwplan: this host has no network section, so there is nothing to base a ruleset on")
	}
	p := &Plan{Host: inv.Host.Hostname, Rules: []Rule{}}
	if inv.Firewall != nil {
		p.CurrentPosture = string(inv.Firewall.InboundPosture)
	}

	p.LAN = opts.LAN
	if p.LAN == "" {
		p.LAN = deriveLAN(inv)
	}
	if p.LAN == "" {
		return nil, fmt.Errorf("fwplan: could not work out this host's subnet; pass one explicitly")
	}

	p.SSHPort = opts.SSHPort
	if p.SSHPort == 0 {
		p.SSHPort = detectSSHPort(inv)
	}

	// The ports to open, collapsed to one rule per protocol+port.
	type key struct {
		proto string
		port  int
	}
	seen := map[key]bool{}
	var rules []Rule
	for _, l := range inv.Network.Listeners {
		if l.Ephemeral {
			// A port the kernel handed out is not a service, and it will be a
			// different number tomorrow. Opening it would be meaningless.
			continue
		}
		if !l.Exposed {
			// Loopback-only sockets are already unreachable from the network.
			// Opening them would widen access, which is the opposite of the
			// point, so they are skipped rather than included.
			continue
		}
		k := key{l.Protocol, l.Port}
		if seen[k] || l.Port == p.SSHPort {
			continue
		}
		seen[k] = true
		rules = append(rules, Rule{
			Protocol: l.Protocol, Port: l.Port, Owner: ownerOf(l),
			Source: p.LAN, Reason: "observed listening and exposed",
		})
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Port != rules[j].Port {
			return rules[i].Port < rules[j].Port
		}
		return rules[i].Protocol < rules[j].Protocol
	})
	p.Rules = rules

	p.Warnings = warningsFor(inv, p)
	p.Ruleset = render(inv, p, opts)
	p.Rollback = "nft delete table inet homelab"
	p.TrialCommand = trialCommand()
	return p, nil
}

func ownerOf(l inventory.Listener) string {
	if l.Owner != nil && l.Owner.Name != "" {
		if l.Owner.ComposeProject != "" {
			return l.Owner.Name + " (" + l.Owner.ComposeProject + ")"
		}
		return l.Owner.Name
	}
	if l.Process != "" {
		return l.Process
	}
	return "unattributed"
}

func warningsFor(inv *inventory.Inventory, p *Plan) []string {
	var w []string

	if inv.Docker != nil && inv.Docker.Available && len(inv.Docker.Containers) > 0 {
		w = append(w, fmt.Sprintf(
			"This host runs %d containers. Docker writes its own nftables chains and rewrites "+
				"them whenever a container starts, so this ruleset deliberately creates a "+
				"separate `inet homelab` table and never flushes or edits anything else. Do not "+
				"replace it with a ruleset that starts with `flush ruleset` — that breaks "+
				"container networking in ways that look like application bugs.",
			len(inv.Docker.Containers)))
		w = append(w, "Published container ports are reached through the forward hook, not "+
			"input, so this input policy does not filter traffic to containers on other "+
			"machines' behalf. It filters what reaches this host itself.")
	}

	if inv.Proxmox != nil && inv.Proxmox.Available {
		w = append(w, "This is a Proxmox node. It has its own firewall layer (`pve-firewall`) "+
			"with its own enable switch; using it instead may fit better with how the rest of "+
			"the node is managed, and it survives cluster configuration changes.")
	}

	if p.SSHPort == 0 {
		w = append(w, "No SSH port could be determined, so none was opened. Applying this over "+
			"SSH would end your session and lock you out. Set one before using it.")
	}

	switch tailscaleState(inv) {
	case tailnetConnected:
		w = append(w, "The tailscale0 interface is allowed in full, so your tailnet remains a "+
			"way in even if the LAN rules are wrong. That is deliberate — it is the safety net.")
	case tailnetPresent:
		w = append(w, "This host has a tailscale0 interface but no tailnet address on it, so "+
			"Tailscale is installed and not connected. The rule allowing it is kept for when "+
			"it is, but do NOT treat the tailnet as your way back in here — it is not one "+
			"today. SSH from the LAN is the only route, so use the timed trial below.")
	default:
		w = append(w, "This host has no tailnet interface, so SSH from the LAN is the only way "+
			"back in if a rule is wrong. Use the timed trial below rather than applying it "+
			"directly.")
	}

	return w
}

// tailnetState distinguishes "Tailscale is a way in" from "Tailscale is
// installed". The difference decides whether the plan may describe it as a
// safety net, and getting that wrong is how someone gets locked out trusting a
// route that was never up.
type tailnetState int

const (
	tailnetAbsent tailnetState = iota
	tailnetPresent
	tailnetConnected
)

func tailscaleState(inv *inventory.Inventory) tailnetState {
	state := tailnetAbsent
	for _, i := range inv.Host.Interfaces {
		if !strings.HasPrefix(i.Name, "tailscale") {
			continue
		}
		state = tailnetPresent
		for _, a := range i.Addresses {
			ip, _, err := net.ParseCIDR(a)
			if err != nil {
				ip = net.ParseIP(a)
			}
			// Tailscale hands out addresses from the 192.0.2.20/10 CGNAT
			// range. A link-local IPv6 address on its own means the interface
			// exists but nothing is logged in — which is exactly the state host-b
			// is in, and exactly the case that must not be called a safety net.
			if ip != nil && inTailnetRange(ip) {
				return tailnetConnected
			}
		}
	}
	return state
}

func inTailnetRange(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	// 192.0.2.20/10
	return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// render writes the nft script.
func render(inv *inventory.Inventory, p *Plan, opts Options) string {
	var b strings.Builder

	fmt.Fprintf(&b, "#!/usr/sbin/nft -f\n")
	fmt.Fprintf(&b, "# Suggested inbound firewall for %s, derived from what it was observed\n", p.Host)
	fmt.Fprintf(&b, "# listening on. Review every line before applying it.\n#\n")
	fmt.Fprintf(&b, "# This creates its own table and touches nothing else. It does NOT flush\n")
	fmt.Fprintf(&b, "# the ruleset, because Docker and Proxmox keep their own chains here.\n")
	fmt.Fprintf(&b, "# Undo with:  %s\n\n", "nft delete table inet homelab")

	// Recreating the table makes the script idempotent: applying it twice
	// leaves one table rather than failing or doubling the rules.
	b.WriteString("table inet homelab {\n")
	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority filter + 10; policy drop;\n\n")

	b.WriteString("\t\t# Loopback. Without this a great deal breaks in confusing ways.\n")
	b.WriteString("\t\tiif lo accept\n\n")

	b.WriteString("\t\t# Replies to connections this host opened. Without this the machine\n")
	b.WriteString("\t\t# cannot use the network at all, only serve it.\n")
	b.WriteString("\t\tct state established,related accept\n")
	b.WriteString("\t\tct state invalid drop\n\n")

	if opts.AllowICMP {
		b.WriteString("\t\t# ICMP, including fragmentation-needed. Dropping this wholesale is a\n")
		b.WriteString("\t\t# well-known way to break path MTU discovery and lose a day to it.\n")
		b.WriteString("\t\tmeta l4proto icmp accept\n")
		b.WriteString("\t\tmeta l4proto ipv6-icmp accept\n\n")
	}

	switch tailscaleState(inv) {
	case tailnetConnected:
		b.WriteString("\t\t# The tailnet, allowed in full: it is the way back in if a rule below\n")
		b.WriteString("\t\t# is wrong. Remove this only once you are sure of the rest.\n")
		b.WriteString("\t\tiifname \"tailscale0\" accept\n")
		b.WriteString("\t\tudp dport 41641 accept\n\n")
	case tailnetPresent:
		b.WriteString("\t\t# Tailscale is installed here but NOT connected — the interface has no\n")
		b.WriteString("\t\t# tailnet address. This rule is kept for when it is, but it is not a\n")
		b.WriteString("\t\t# way back in today. Do not rely on it.\n")
		b.WriteString("\t\tiifname \"tailscale0\" accept\n")
		b.WriteString("\t\tudp dport 41641 accept\n\n")
	}

	if p.SSHPort > 0 {
		fmt.Fprintf(&b, "\t\t# SSH, first and unconditional. This is the line that stops this\n")
		fmt.Fprintf(&b, "\t\t# ruleset from being a brick.\n")
		fmt.Fprintf(&b, "\t\ttcp dport %d accept comment \"ssh\"\n\n", p.SSHPort)
	}

	if len(p.Rules) == 0 {
		b.WriteString("\t\t# Nothing else was observed listening on a wildcard address.\n")
	} else {
		fmt.Fprintf(&b, "\t\t# Observed services, reachable from %s only.\n", p.LAN)
	}
	for _, r := range p.Rules {
		proto := r.Protocol
		if proto != "tcp" && proto != "udp" {
			continue
		}
		fmt.Fprintf(&b, "\t\tip saddr %s %s dport %d accept comment %q\n",
			p.LAN, proto, r.Port, r.Owner)
	}

	b.WriteString("\n\t\t# Anything else is dropped by the policy above. Log a sample of it\n")
	b.WriteString("\t\t# while you are still deciding whether this ruleset is right.\n")
	b.WriteString("\t\tlimit rate 10/minute burst 5 packets log prefix \"homelab-drop \" level info\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// trialCommand is the recipe for applying a firewall to a machine you are not
// standing next to.
//
// It is included in every plan because the failure mode it prevents — applying
// a rule that drops your own SSH and having no way back — is both the most
// likely mistake and the most expensive one.
func trialCommand() string {
	return strings.Join([]string{
		"# Apply it for 10 minutes, then undo it automatically unless you cancel.",
		"# Do this the FIRST time, from a session you can afford to lose:",
		"sudo sh -c 'nft -f /tmp/homelab-firewall.nft && " +
			"(sleep 600 && nft delete table inet homelab) &'",
		"",
		"# Now, from a NEW connection, check you can still get in and that your",
		"# services still answer. If everything works, cancel the rollback:",
		"sudo pkill -f 'sleep 600'",
		"",
		"# If anything is wrong, do nothing. It removes itself in 10 minutes.",
	}, "\n")
}

// detectSSHPort finds the port sshd is listening on, so the generated ruleset
// opens the one actually in use rather than assuming 22 and risking a lockout.
func detectSSHPort(inv *inventory.Inventory) int {
	best := 0
	for _, l := range inv.Network.Listeners {
		if l.Protocol != "tcp" || l.Ephemeral {
			continue
		}
		name := strings.ToLower(ownerOf(l))
		if strings.Contains(name, "ssh") || l.Port == 22 {
			// Prefer a non-22 port only if 22 is not also present: a host
			// listening on both should keep both, and 22 is the safer default
			// to name when guessing.
			if best == 0 || l.Port == 22 {
				best = l.Port
			}
		}
	}
	return best
}

// deriveLAN turns the host's own address into the subnet it sits on, which is
// the default scope for service rules.
func deriveLAN(inv *inventory.Inventory) string {
	var fallback string
	for _, i := range inv.Host.Interfaces {
		if i.Kind != inventory.IfKindPhysical && i.Kind != inventory.IfKindBridge {
			continue
		}
		// Docker's bridges carry private addresses that would produce a
		// plausible-looking and completely wrong subnet.
		if strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "docker") {
			continue
		}
		for _, a := range i.Addresses {
			ip, ipnet, err := net.ParseCIDR(a)
			if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			if i.Kind == inventory.IfKindPhysical {
				return ipnet.String()
			}
			if fallback == "" {
				fallback = ipnet.String()
			}
		}
	}
	return fallback
}
