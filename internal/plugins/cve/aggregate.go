package cve

import (
	"cmp"
	"sort"
)

func overview(r Result) Overview {
	o := Overview{GeneratedAt: r.GeneratedAt, TopFixes: []PackageRow{}, EOL: []EOLRow{}}
	hosts, sets, pkgs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range r.Vulns {
		if v.Suppressed != nil {
			o.Totals.Suppressed++
			continue
		}
		o.Totals.Vulns++
		o.Totals.Severity.add(v.Severity)
		if v.KEV != nil {
			o.Totals.KEV++
		}
		if v.Fix != "none" {
			o.Totals.Fixable++
		}
		for _, a := range v.Affected {
			hosts[a.Host] = true
			sets[a.Host+"\x00"+a.Scope] = true
			for _, b := range a.Binaries {
				pkgs[a.Host+"\x00"+a.Scope+"\x00"+b.Name] = true
			}
		}
	}
	o.Totals.HostsAffected = len(hosts)
	o.Totals.SetsAffected = len(sets)
	o.Totals.PackagesAffected = len(pkgs)
	rows := packageRows(r.Vulns, "")
	scores := map[string]int{}
	for _, v := range r.Vulns {
		if v.Suppressed != nil {
			continue
		}
		seen := map[string]bool{}
		for _, a := range v.Affected {
			key := a.Ecosystem + "\x00" + a.Package
			if a.Fixable && !seen[key] {
				scores[key] += v.Priority
				seen[key] = true
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if c := cmp.Compare(scores[a.Ecosystem+"\x00"+a.Name], scores[b.Ecosystem+"\x00"+b.Name]); c != 0 {
			return c > 0
		}
		return a.Ecosystem+"\x00"+a.Name < b.Ecosystem+"\x00"+b.Name
	})
	for _, row := range rows {
		if row.Fixable > 0 {
			o.TopFixes = append(o.TopFixes, row)
			if len(o.TopFixes) == 5 {
				break
			}
		}
	}
	for _, h := range r.Hosts {
		if h.EOL != nil {
			o.EOL = append(o.EOL, EOLRow{SetRef: SetRef{h.Host, h.Scope, h.ImageRef}, Distro: distroLabel(h.Distro), EOLDate: h.EOL.Date})
		}
	}
	return o
}
func packageRows(vulns []Vulnerability, host string) []PackageRow {
	type accumulator struct {
		row       PackageRow
		hosts     map[string]bool
		sets      map[SetRef]bool
		installed map[string]bool
		vulns     map[string]Vulnerability
		fixed     map[string]bool
		allFixed  map[string]bool
	}
	acc := map[string]*accumulator{}
	for _, v := range vulns {
		if v.Suppressed != nil {
			continue
		}
		for _, a := range v.Affected {
			if host != "" && host != a.Host {
				continue
			}
			key := a.Ecosystem + "\x00" + a.Package
			x := acc[key]
			if x == nil {
				x = &accumulator{row: PackageRow{Ecosystem: a.Ecosystem, Name: a.Package, Installed: []string{}, SetRefs: []SetRef{}, Top: []string{}}, hosts: map[string]bool{}, sets: map[SetRef]bool{}, installed: map[string]bool{}, vulns: map[string]Vulnerability{}, fixed: map[string]bool{}, allFixed: map[string]bool{}}
				acc[key] = x
			}
			x.hosts[a.Host] = true
			x.sets[SetRef{a.Host, a.Scope, a.ImageRef}] = true
			x.installed[a.Installed] = true
			if _, ok := x.vulns[v.ID]; !ok {
				x.allFixed[v.ID] = true
			}
			x.vulns[v.ID] = v
			if !a.Fixable {
				x.allFixed[v.ID] = false
			}
			if a.Fixable && a.Fixed != nil {
				x.fixed[v.ID] = true
				if x.row.FixVersion == nil || versionCompare(a.Ecosystem, *a.Fixed, *x.row.FixVersion) > 0 {
					x.row.FixVersion = ptr(*a.Fixed)
				}
			}
		}
	}
	out := make([]PackageRow, 0, len(acc))
	for _, x := range acc {
		x.row.Hosts = len(x.hosts)
		x.row.Sets = len(x.sets)
		for s := range x.installed {
			x.row.Installed = append(x.row.Installed, s)
		}
		sort.Slice(x.row.Installed, func(i, j int) bool {
			return versionCompare(x.row.Ecosystem, x.row.Installed[i], x.row.Installed[j]) < 0
		})
		for s := range x.sets {
			x.row.SetRefs = append(x.row.SetRefs, s)
		}
		sort.Slice(x.row.SetRefs, func(i, j int) bool {
			a, b := x.row.SetRefs[i], x.row.SetRefs[j]
			return a.Host+"\x00"+a.Scope < b.Host+"\x00"+b.Scope
		})
		vv := make([]Vulnerability, 0, len(x.vulns))
		for _, v := range x.vulns {
			vv = append(vv, v)
			x.row.Vulns.add(v.Severity)
			if v.KEV != nil {
				x.row.KEV++
			}
			if x.fixed[v.ID] {
				x.row.Fixable++
			}
			if x.allFixed[v.ID] && x.fixed[v.ID] {
				x.row.Fixes++
			}
			x.row.Priority += v.Priority
		}
		sortVulns(vv, "priority", "desc")
		for _, v := range vv[:min(3, len(vv))] {
			x.row.Top = append(x.row.Top, v.ID)
		}
		out = append(out, x.row)
	}
	return out
}
func hostRows(r Result, nowEOL func(string) *EOL) []HostRow {
	out := append([]HostRow{}, r.Hosts...)
	for i := range out {
		h := &out[i]
		h.Vulns = Counts{}
		h.KEV = 0
		h.Fixable = 0
		if h.Ecosystem != nil {
			h.EOL = nowEOL(*h.Ecosystem)
		}
		for _, v := range r.Vulns {
			if v.Suppressed != nil {
				continue
			}
			found, fix := false, false
			for _, a := range v.Affected {
				if a.Host == h.Host && a.Scope == h.Scope {
					found = true
					fix = fix || a.Fixable
				}
			}
			if found {
				h.Vulns.add(v.Severity)
				if v.KEV != nil {
					h.KEV++
				}
				if fix {
					h.Fixable++
				}
			}
		}
	}
	return out
}
func sortVulns(rows []Vulnerability, key, order string) {
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		c := 0
		switch key {
		case "severity":
			c = cmp.Compare(severityRank(a.Severity), severityRank(b.Severity))
		case "epss":
			av, bv := -1.0, -1.0
			if a.EPSS != nil {
				av = a.EPSS.Score
			}
			if b.EPSS != nil {
				bv = b.EPSS.Score
			}
			c = cmp.Compare(av, bv)
		case "published":
			if a.Published == nil {
				if b.Published != nil {
					c = -1
				}
			} else if b.Published == nil {
				c = 1
			} else {
				c = a.Published.Compare(*b.Published)
			}
		case "hosts":
			c = cmp.Compare(a.Hosts, b.Hosts)
		case "id":
			c = cmp.Compare(a.ID, b.ID)
		default:
			c = cmp.Compare(a.Priority, b.Priority)
		}
		if c == 0 {
			return a.ID < b.ID
		}
		if order == "asc" {
			return c < 0
		}
		return c > 0
	})
}
