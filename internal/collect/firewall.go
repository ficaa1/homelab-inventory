package collect

import (
	"context"
	"fmt"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// FirewallCollector reads the host's packet filter.
//
// This is the other half of `network.listeners[].exposed`. That flag says a
// socket is bound to a wildcard address — what the *process* asked for. It
// cannot say whether anything can reach it, because that is decided by the
// kernel's packet filter. A wildcard bind behind a default-deny INPUT chain is
// unreachable; the same bind with an empty ruleset is reachable by anything
// that can route to the machine. Collecting only the first half means reading a
// statement of intent as a statement of risk.
//
// # Why three sources
//
// A host can keep rules in nftables or legacy iptables, and Proxmox layers its
// own enable switch above both. Reading one and reporting "no rules" would be
// wrong on a host using either of the others, so all three are tried and
// Sources records which answered.
//
// # Why the rules are not parsed
//
// Chains, hooks and policies are parsed, because they are stable, small, and
// are exactly what the derived verdict needs. Rule bodies are kept verbatim.
// nftables' expression grammar is large and still growing, and a half-parsed
// rule is worse than an unparsed one — it invites a consumer to reason about a
// match it did not actually understand. A human reading `iifname != "tailscale0"
// ip saddr 192.0.2.20/10 drop` learns more than any struct this could invent.
type FirewallCollector struct{}

func (FirewallCollector) Name() string { return "firewall" }

func (c FirewallCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	fw := &inventory.Firewall{Sources: []string{}, Tables: []inventory.FirewallTable{}}

	if env.Run.Available("nft") {
		// `nft list ruleset` needs root. Unprivileged it prints nothing and
		// exits non-zero, which would otherwise look exactly like "no rules" —
		// the most dangerous possible misreading of this section.
		res, err := env.Run.Run(ctx, "nft", "list", "ruleset")
		switch {
		case err != nil:
			env.WarnErr("could not run nft", err)
		case res.ExitCode != 0:
			env.Warnf("nft list ruleset exited %d (needs root); "+
				"nftables rules were NOT read, and an empty result here does not mean none exist",
				res.ExitCode)
		default:
			tables := parseNFT(res.Stdout)
			fw.Tables = append(fw.Tables, tables...)
			fw.Available = true
			fw.Sources = append(fw.Sources, "nft")
		}
	} else {
		env.Infof("nft is not installed")
	}

	if env.Run.Available("iptables-save") {
		res, err := env.Run.Run(ctx, "iptables-save")
		switch {
		case err != nil:
			env.WarnErr("could not run iptables-save", err)
		case res.ExitCode != 0:
			env.Warnf("iptables-save exited %d (needs root); legacy iptables rules were NOT read",
				res.ExitCode)
		default:
			tables := parseIPTablesSave(res.Stdout, "ip")
			fw.Tables = append(fw.Tables, tables...)
			fw.Available = true
			fw.Sources = append(fw.Sources, "iptables-save")
		}
	}

	if env.Run.Available("pve-firewall") {
		// pve-firewall prints perl locale warnings to stderr on this node;
		// only stdout is parsed, so they cannot corrupt the verdict.
		res, err := env.Run.Run(ctx, "pve-firewall", "status")
		if err != nil {
			env.WarnErr("could not run pve-firewall status", err)
		} else {
			pve := parsePVEFirewallStatus(res.Stdout)
			fw.Proxmox = &pve
			fw.Available = true
			fw.Sources = append(fw.Sources, "pve-firewall")
		}
	}

	if !fw.Available {
		env.Warnf("no packet filter could be read; this host's firewall state is unknown")
	}

	fw.InboundPosture, fw.FiltersInbound, fw.InboundSummary = assessInbound(fw)
	inv.Firewall = fw
	return nil
}

// parseNFT reads `nft list ruleset`.
//
// The format is brace-nested and indented with tabs, which is enough structure
// to walk without a real parser: table lines open a family+name, chain lines
// open a chain, a line with `type ... hook ... policy ...` describes a base
// chain, and everything else inside a chain is a rule.
func parseNFT(out string) []inventory.FirewallTable {
	var tables []inventory.FirewallTable
	var table *inventory.FirewallTable
	var chain *inventory.FirewallChain

	flushChain := func() {
		if chain != nil && table != nil {
			table.Chains = append(table.Chains, *chain)
		}
		chain = nil
	}
	flushTable := func() {
		flushChain()
		if table != nil {
			tables = append(tables, *table)
		}
		table = nil
	}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "table "):
			flushTable()
			// "table ip filter {"
			f := strings.Fields(strings.TrimSuffix(line, "{"))
			t := inventory.FirewallTable{Chains: []inventory.FirewallChain{}}
			if len(f) >= 3 {
				t.Family, t.Name = f[1], f[2]
			} else if len(f) == 2 {
				// A family-less table defaults to ip.
				t.Family, t.Name = "ip", f[1]
			}
			table = &t

		case strings.HasPrefix(line, "chain "):
			flushChain()
			f := strings.Fields(strings.TrimSuffix(line, "{"))
			if len(f) >= 2 {
				chain = &inventory.FirewallChain{Name: f[1], Rules: []string{}}
			}

		case line == "}":
			// Closing a chain if one is open, otherwise closing the table.
			if chain != nil {
				flushChain()
			} else {
				flushTable()
			}

		case chain != nil && strings.HasPrefix(line, "type "):
			// "type filter hook input priority filter; policy accept;"
			chain.Hook = fieldAfter(line, "hook")
			chain.Priority = strings.TrimSuffix(fieldAfter(line, "priority"), ";")
			chain.Policy = strings.TrimSuffix(fieldAfter(line, "policy"), ";")

		case chain != nil:
			// A rule can carry a comment, and a comment can carry anything.
			chain.Rules = append(chain.Rules, redact.Text(line))
		}
	}
	flushTable()
	return tables
}

// fieldAfter returns the token following key on a whitespace-separated line.
func fieldAfter(line, key string) string {
	f := strings.Fields(line)
	for i, v := range f {
		if v == key && i+1 < len(f) {
			return f[i+1]
		}
	}
	return ""
}

// parseIPTablesSave reads iptables-save output into the same shape.
//
// The formats do not really correspond — iptables has tables and chains but no
// hooks — so the built-in chain name is mapped to the hook it is equivalent to.
// That is what makes one verdict function able to read both.
func parseIPTablesSave(out, family string) []inventory.FirewallTable {
	var tables []inventory.FirewallTable
	var table *inventory.FirewallTable
	chains := map[string]*inventory.FirewallChain{}
	var order []string

	flush := func() {
		if table == nil {
			return
		}
		for _, name := range order {
			table.Chains = append(table.Chains, *chains[name])
		}
		tables = append(tables, *table)
		table = nil
		chains = map[string]*inventory.FirewallChain{}
		order = nil
	}

	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue

		case strings.HasPrefix(line, "*"):
			flush()
			t := inventory.FirewallTable{
				Family: family, Name: strings.TrimPrefix(line, "*"),
				Chains: []inventory.FirewallChain{},
			}
			table = &t

		case strings.HasPrefix(line, ":"):
			// ":INPUT ACCEPT [7548245:1753500870]"
			f := strings.Fields(strings.TrimPrefix(line, ":"))
			if len(f) < 2 || table == nil {
				continue
			}
			ch := &inventory.FirewallChain{Name: f[0], Rules: []string{}}
			if p := strings.ToLower(f[1]); p != "-" {
				// A user chain has "-" as its policy; only a built-in has one.
				ch.Policy = p
				ch.Hook = iptablesHook(f[0])
			}
			chains[f[0]] = ch
			order = append(order, f[0])

		case strings.HasPrefix(line, "-A ") && table != nil:
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			if ch, ok := chains[f[1]]; ok {
				ch.Rules = append(ch.Rules, redact.Text(line))
			}

		case line == "COMMIT":
			flush()
		}
	}
	flush()
	return tables
}

// iptablesHook maps a built-in chain name to the nftables hook it corresponds
// to, so one verdict function can read both formats.
func iptablesHook(chain string) string {
	switch chain {
	case "INPUT":
		return "input"
	case "FORWARD":
		return "forward"
	case "OUTPUT":
		return "output"
	case "PREROUTING":
		return "prerouting"
	case "POSTROUTING":
		return "postrouting"
	}
	return ""
}

// parsePVEFirewallStatus reads `pve-firewall status`.
//
// The output is "Status: disabled/running", which is a genuinely misleading
// string to skim: the second word is the *service*, and the service runs
// happily while the firewall itself is off. Only the first word decides
// whether anything is filtered.
func parsePVEFirewallStatus(out string) inventory.ProxmoxFirewall {
	pve := inventory.ProxmoxFirewall{}
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "Status:")
		if !ok {
			continue
		}
		pve.Status = strings.TrimSpace(rest)
		state, _, _ := strings.Cut(pve.Status, "/")
		pve.Enabled = strings.EqualFold(strings.TrimSpace(state), "enabled")
		break
	}
	return pve
}

// assessInbound answers the question the section exists for: is a port that is
// bound to a wildcard address actually reachable?
//
// The only signal that settles it without interpreting rule semantics is the
// *default verdict* of the input base chains. "Are there rules?" does not
// settle it, and an earlier version of this function got that wrong: host-a has
// exactly one INPUT rule — a jump into Tailscale's chain, which accepts tailnet
// traffic and drops spoofed tailnet source addresses — and counting it made a
// host with forty LAN-reachable ports report as filtered. Under an accept
// policy a port is reachable unless some rule specifically closes it, and
// deciding that would mean parsing every expression, which this declines to do.
//
// So: default-deny is a real answer, default-allow is a real answer, and the
// rule count is reported as context rather than promoted to a verdict.
func assessInbound(fw *inventory.Firewall) (inventory.Posture, bool, string) {
	if !fw.Available {
		return inventory.PostureUnknown, false,
			"no packet filter could be read, so this host's inbound filtering is unknown — " +
				"this is not the same as having no rules"
	}

	var (
		inputChains int
		inputRules  int
		defaultDrop bool
	)
	for _, t := range fw.Tables {
		for _, ch := range t.Chains {
			if ch.Hook != "input" {
				continue
			}
			inputChains++
			inputRules += len(ch.Rules)
			if strings.EqualFold(ch.Policy, "drop") || strings.EqualFold(ch.Policy, "reject") {
				defaultDrop = true
			}
		}
	}

	// Proxmox's own firewall sits above nftables and has its own switch, so an
	// enabled PVE firewall is a default-deny regardless of what the raw tables
	// look like.
	if fw.Proxmox != nil && fw.Proxmox.Enabled {
		return inventory.PostureDefaultDeny, true, "the Proxmox firewall is enabled"
	}

	switch {
	case defaultDrop:
		return inventory.PostureDefaultDeny, true,
			"an input chain defaults to drop, so a port is unreachable unless a rule opens it"
	case inputChains == 0:
		return inventory.PostureDefaultAllow, false,
			"nothing is hooked to input: every listening port is reachable from anything " +
				"that can route to this host"
	case inputRules > 0:
		return inventory.PostureDefaultAllow, false, fmt.Sprintf(
			"every input chain accepts by default; the %d rule(s) present may close specific "+
				"things, but anything they do not name is reachable", inputRules)
	default:
		return inventory.PostureDefaultAllow, false,
			"every input chain accepts by default and carries no rules: every listening port " +
				"is reachable from anything that can route to this host"
	}
}
