package cve

import (
	"sort"
	"strings"
)

func canonical(a advisory) string {
	for _, id := range append(append([]string{}, a.Upstream...), a.Aliases...) {
		if strings.HasPrefix(id, "CVE-") {
			return id
		}
	}
	return a.ID
}
func fixedVersion(a osvAffected, installed string) *string {
	var best *string
	for _, r := range a.Ranges {
		if r.Type != "ECOSYSTEM" {
			continue
		}
		introduced := "0"
		open := true
		for _, e := range r.Events {
			if e.Introduced != "" {
				introduced = e.Introduced
				open = true
			}
			if open && e.Fixed != "" && (introduced == "0" || versionCompare(a.Package.Ecosystem, installed, introduced) >= 0) && versionCompare(a.Package.Ecosystem, e.Fixed, installed) > 0 {
				if best == nil || versionCompare(a.Package.Ecosystem, e.Fixed, *best) < 0 {
					best = ptr(e.Fixed)
				}
			}
			if e.Fixed != "" || e.LastAffected != "" || e.Limit != "" {
				open = false
			}
		}
	}
	return best
}

// applySeverity records the best CVSS v3 score and the highest distro rating.
// finishSeverity then prefers the distro's rating, as Trivy and Grype do for OS
// packages: Ubuntu's "negligible" or Debian's "unimportant" describe the
// distro's build, where NVD's score describes upstream in general.
func applySeverity(v *Vulnerability, entries []severity, eco string, vendor vendorInfo) {
	for _, s := range entries {
		if c := cvssV3(s.Score); c != nil {
			if v.CVSS == nil || v.CVSS.Version == "4.0" || c.Score > v.CVSS.Score {
				v.CVSS = c
			}
		} else if strings.HasPrefix(s.Score, "CVSS:4.") && v.CVSS == nil {
			v.CVSS = &CVSS{Vector: s.Score, Version: "4.0"}
		}
	}
	rating, source := "unknown", ""
	if strings.HasPrefix(eco, "Ubuntu:") {
		rating, source = vendorSeverity(vendor.Priority), "ubuntu"
		for _, s := range entries {
			if strings.EqualFold(s.Type, "Ubuntu") && severityRank(vendorSeverity(s.Score)) > severityRank(rating) {
				rating = vendorSeverity(s.Score)
			}
		}
	} else if strings.HasPrefix(eco, "Debian:") {
		rating, source = vendorSeverity(vendor.Urgency), "debian"
	}
	if rating != "unknown" && (v.vendor == "" || severityRank(rating) > severityRank(v.vendor)) {
		v.vendor, v.vendorSource = rating, source
	}
}
func finishSeverity(v *Vulnerability) {
	switch {
	case v.vendor != "":
		v.Severity, v.SeveritySource = v.vendor, v.vendorSource
	case v.CVSS != nil && v.CVSS.Version != "4.0":
		v.Severity, v.SeveritySource = scoreSeverity(v.CVSS.Score), "cvss_v3"
	default:
		v.Severity, v.SeveritySource = "unknown", "none"
	}
}
func group(works []*queryWork, records map[string]advisory) []Vulnerability {
	byID := map[string]*Vulnerability{}
	for _, w := range works {
		sort.Slice(w.Matches, func(i, j int) bool { return w.Matches[i].ID < w.Matches[j].ID })
		seen := map[string]bool{}
		for _, m := range w.Matches {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			a, ok := records[m.ID]
			if !ok {
				continue
			}
			id := canonical(a)
			v := byID[id]
			if v == nil {
				v = &Vulnerability{VulnRow: VulnRow{ID: id, Severity: "unknown", SeveritySource: "none", OSVIDs: []string{}, Packages: []string{}, Fix: "none"}, Aliases: []string{}, CWEIDs: []string{}, Affected: []Affected{}}
				byID[id] = v
			}
			v.OSVIDs = union(v.OSVIDs, []string{a.ID})
			v.Aliases = union(v.Aliases, union(a.Aliases, a.Upstream))
			v.CWEIDs = union(v.CWEIDs, a.CWEIDs)
			if v.Published == nil || a.Published != nil && a.Published.Before(*v.Published) {
				v.Published = a.Published
			}
			if v.Summary == "" {
				v.Summary = a.Summary
				if v.Summary == "" {
					v.Summary = truncate(strings.Split(a.Details, "\n")[0], 240)
				}
			}
			// Details and references stay in the advisory cache; the detail
			// endpoint reads them on demand (see withText).
			var fixed *string
			var status *string
			for _, entry := range a.Affected {
				if entry.Package != w.Query.Package {
					continue
				}
				f := fixedVersion(entry, w.Query.Version)
				if f != nil && (fixed == nil || versionCompare(w.Query.Package.Ecosystem, *f, *fixed) > 0) {
					fixed = f
				}
				vendor := entry.EcosystemSpecific
				if vendor.Status != "" {
					status = ptr(vendor.Status)
				} else if vendor.Priority != "" {
					status = ptr(vendor.Priority)
				} else if vendor.Urgency != "" {
					status = ptr(vendor.Urgency)
				}
				applySeverity(v, append(append([]severity{}, a.Severity...), entry.Severity...), w.Query.Package.Ecosystem, vendor)
			}
			// OSV's query result determines membership even if an advisory has no
			// affected range (e.g. an enumerated version without a published fix).
			applySeverity(v, a.Severity, w.Query.Package.Ecosystem, vendorInfo{})
			for _, occ := range w.Occurrences {
				occ.Fixed = fixed
				occ.Fixable = fixed != nil
				occ.VendorStatus = status
				found := -1
				for i, old := range v.Affected {
					if old.Host == occ.Host && old.Scope == occ.Scope && old.Ecosystem == occ.Ecosystem && old.Package == occ.Package && old.Installed == occ.Installed {
						found = i
						break
					}
				}
				if found < 0 {
					v.Affected = append(v.Affected, occ)
				} else {
					old := &v.Affected[found]
					if fixed != nil && (old.Fixed == nil || versionCompare(occ.Ecosystem, *fixed, *old.Fixed) > 0) {
						old.Fixed = fixed
						old.Fixable = true
					}
					if old.VendorStatus == nil {
						old.VendorStatus = status
					}
				}
			}
		}
	}
	out := make([]Vulnerability, 0, len(byID))
	for _, v := range byID {
		finishSeverity(v)
		hosts, sets := map[string]bool{}, map[string]bool{}
		fixable := 0
		for _, a := range v.Affected {
			hosts[a.Host] = true
			sets[a.Host+"\x00"+a.Scope] = true
			v.Packages = union(v.Packages, []string{a.Package})
			if a.Fixable {
				fixable++
			}
		}
		v.Hosts = len(hosts)
		v.Sets = len(sets)
		if fixable == len(v.Affected) && fixable > 0 {
			v.Fix = "all"
		} else if fixable > 0 {
			v.Fix = "some"
		}
		v.Priority = severityRank(v.Severity) * 100
		if fixable > 0 {
			v.Priority += 50
		}
		sort.Slice(v.Affected, func(i, j int) bool {
			a, b := v.Affected[i], v.Affected[j]
			return a.Host+"\x00"+a.Scope+"\x00"+a.Package+"\x00"+a.Installed < b.Host+"\x00"+b.Scope+"\x00"+b.Package+"\x00"+b.Installed
		})
		out = append(out, *v)
	}
	sortVulns(out, "priority", "desc")
	return out
}
