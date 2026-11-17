package cve

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func reply(status int, v any) plugin.APIResponse {
	b, _ := json.Marshal(v)
	return plugin.APIResponse{Status: status, ContentType: "application/json", Body: b}
}
func apiError(status int, message string) plugin.APIResponse {
	return reply(status, map[string]string{"error": message})
}
func (p *Integration) API(ctx context.Context, req plugin.APIRequest) (plugin.APIResponse, error) {
	if err := ctx.Err(); err != nil {
		return apiError(408, "request canceled"), nil
	}
	if req.Method == "POST" && req.Path == "scan" {
		if !p.startScan() {
			return apiError(409, "scan already running"), nil
		}
		return reply(202, map[string]string{"state": "scanning"}), nil
	}
	if (req.Method == "PUT" || req.Method == "DELETE") && strings.HasPrefix(req.Path, "suppressions/") {
		return p.suppress(req), nil
	}
	if req.Method != "GET" {
		return apiError(404, "unknown endpoint"), nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if req.Path == "status" {
		return reply(200, p.status), nil
	}
	r := p.snapshotLocked()
	// Support and suppression dates are evaluated on every read, including after
	// a restart without waiting for the next successful OSV scan.
	if req.Path == "hosts" || req.Path == "overview" {
		r.Hosts = hostRows(r, func(eco string) *EOL { return distroEOL(eco, p.host.Now()) })
	}
	switch req.Path {
	case "overview":
		return reply(200, overview(r)), nil
	case "hosts":
		return reply(200, map[string]any{"items": r.Hosts}), nil
	case "vulns":
		return listVulns(r, req.Query), nil
	case "packages":
		return listPackages(r, req.Query), nil
	}
	if strings.HasPrefix(req.Path, "vulns/") {
		id := strings.TrimPrefix(req.Path, "vulns/")
		for _, v := range r.Vulns {
			if v.ID == id {
				return reply(200, p.withText(v)), nil
			}
		}
		return apiError(404, "unknown vulnerability"), nil
	}
	return apiError(404, "unknown endpoint"), nil
}

// Rows without a CVE alias keep their OSV id (e.g. an Alpine or Debian advisory).
var cveID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{2,80}$`)

func (p *Integration) suppress(req plugin.APIRequest) plugin.APIResponse {
	id := strings.TrimPrefix(req.Path, "suppressions/")
	if !cveID.MatchString(id) {
		return apiError(400, "invalid vulnerability id")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	known := false
	for _, v := range p.result.Vulns {
		if v.ID == id {
			known = true
			break
		}
	}
	if !known {
		return apiError(404, "unknown vulnerability")
	}
	next := make(map[string]Suppression, len(p.suppressions))
	for k, v := range p.suppressions {
		next[k] = v
	}
	if req.Method == "DELETE" {
		delete(next, id)
	} else {
		var in struct {
			State  string  `json:"state"`
			Reason string  `json:"reason"`
			Until  *string `json:"until"`
		}
		d := json.NewDecoder(bytes.NewReader(req.Body))
		d.DisallowUnknownFields()
		if len(req.Body) > 64<<10 || d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			return apiError(400, "invalid suppression")
		}
		if in.State != "accepted" && in.State != "not_affected" && in.State != "false_positive" {
			return apiError(400, "invalid suppression state")
		}
		in.Reason = strings.TrimSpace(in.Reason)
		if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 500 {
			return apiError(400, "reason is required and must be at most 500 characters")
		}
		if in.Until != nil {
			t, err := time.Parse("2006-01-02", *in.Until)
			if err != nil || t.Format("2006-01-02") != *in.Until {
				return apiError(400, "until must be YYYY-MM-DD")
			}
		}
		next[id] = Suppression{State: in.State, Reason: in.Reason, By: req.Actor, At: p.host.Now().UTC(), Until: in.Until}
	}
	if p.save("suppressions.json", next) != nil {
		return apiError(409, "could not persist suppression")
	}
	p.suppressions = next
	for _, v := range p.snapshotLocked().Vulns {
		if v.ID == id {
			return reply(200, v.VulnRow)
		}
	}
	return apiError(404, "unknown vulnerability")
}

type page struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
}

func pagination(q url.Values) (page, bool) {
	p := page{Limit: 50}
	for k, dst := range map[string]*int{"offset": &p.Offset, "limit": &p.Limit} {
		if raw := q.Get(k); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || k == "limit" && n == 0 {
				return p, false
			}
			*dst = n
		}
	}
	p.Limit = min(p.Limit, 200)
	return p, true
}
func has(list, value string) bool {
	for _, v := range strings.Split(list, ",") {
		if v == value {
			return true
		}
	}
	return false
}
func validSeverity(s string) bool {
	if s == "" {
		return true
	}
	for _, v := range strings.Split(s, ",") {
		if v != "critical" && v != "high" && v != "medium" && v != "low" && v != "unknown" {
			return false
		}
	}
	return true
}
func validCommon(q url.Values) bool {
	return validSeverity(q.Get("severity")) && (q.Get("order") == "" || q.Get("order") == "desc" || q.Get("order") == "asc") && (q.Get("kev") == "" || q.Get("kev") == "0" || q.Get("kev") == "1") && (q.Get("fix") == "" || q.Get("fix") == "0" || q.Get("fix") == "1")
}
func matches(v Vulnerability, q url.Values, withSeverity bool) bool {
	suppressed := q.Get("suppressed")
	if suppressed != "all" {
		if suppressed == "1" && v.Suppressed == nil {
			return false
		}
		if suppressed != "1" && v.Suppressed != nil {
			return false
		}
	}
	if withSeverity && q.Get("severity") != "" && !has(q.Get("severity"), v.Severity) {
		return false
	}
	if q.Get("kev") == "1" && v.KEV == nil || q.Get("fix") == "1" && v.Fix == "none" {
		return false
	}
	needle := strings.ToLower(q.Get("q"))
	textMatch := needle == "" || strings.Contains(strings.ToLower(v.ID+" "+v.Summary+" "+strings.Join(v.Packages, " ")), needle)
	occurrence := false
	for _, a := range v.Affected {
		if !textMatch {
			for _, b := range a.Binaries {
				if strings.Contains(strings.ToLower(b.Name), needle) {
					textMatch = true
					break
				}
			}
		}
		if q.Get("host") != "" && q.Get("host") != a.Host || q.Get("set") != "" && q.Get("set") != a.Scope || q.Get("ecosystem") != "" && q.Get("ecosystem") != a.Ecosystem {
			continue
		}
		if q.Get("package") != "" && q.Get("package") != a.Package {
			found := false
			for _, b := range a.Binaries {
				if b.Name == q.Get("package") {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		occurrence = true
	}
	return occurrence && textMatch
}

type facets struct {
	Severity   Counts `json:"severity"`
	KEV        int    `json:"kev"`
	Fixable    int    `json:"fixable"`
	Suppressed int    `json:"suppressed"`
}

func listVulns(r Result, q url.Values) plugin.APIResponse {
	pg, ok := pagination(q)
	key := q.Get("sort")
	if key == "" {
		key = "priority"
	}
	if !ok || !validCommon(q) || !has("priority,severity,epss,published,hosts,id", key) || !has(",0,1,all", q.Get("suppressed")) {
		return apiError(400, "invalid filter, sort or pagination")
	}
	rows := []Vulnerability{}
	f := facets{}
	for _, v := range r.Vulns {
		if matches(v, q, false) {
			f.Severity.add(v.Severity)
			if v.KEV != nil {
				f.KEV++
			}
			if v.Fix != "none" {
				f.Fixable++
			}
			if v.Suppressed != nil {
				f.Suppressed++
			}
			if q.Get("severity") == "" || has(q.Get("severity"), v.Severity) {
				rows = append(rows, v)
			}
		}
	}
	sortVulns(rows, key, q.Get("order"))
	total := len(rows)
	items := []VulnRow{}
	start := min(pg.Offset, total)
	for _, v := range rows[start : start+min(pg.Limit, total-start)] {
		items = append(items, v.VulnRow)
	}
	return reply(200, struct {
		Total int `json:"total"`
		page
		Facets facets    `json:"facets"`
		Items  []VulnRow `json:"items"`
	}{total, pg, f, items})
}
func listPackages(r Result, q url.Values) plugin.APIResponse {
	pg, ok := pagination(q)
	key := q.Get("sort")
	if key == "" {
		key = "priority"
	}
	if !ok || !validCommon(q) || !has("priority,vulns,hosts,name", key) {
		return apiError(400, "invalid filter, sort or pagination")
	}
	// Package severity, KEV and fix filters select packages; their counts still
	// describe all unsuppressed CVEs on that source package.
	rows := packageRows(r.Vulns, q.Get("host"))
	filtered := []PackageRow{}
	for _, row := range rows {
		if !strings.Contains(strings.ToLower(row.Name), strings.ToLower(q.Get("q"))) || q.Get("kev") == "1" && row.KEV == 0 || q.Get("fix") == "1" && row.Fixable == 0 {
			continue
		}
		if s := q.Get("severity"); s != "" {
			counts := map[string]int{"critical": row.Vulns.Critical, "high": row.Vulns.High, "medium": row.Vulns.Medium, "low": row.Vulns.Low, "unknown": row.Vulns.Unknown}
			found := false
			for sev, n := range counts {
				if n > 0 && has(s, sev) {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		filtered = append(filtered, row)
	}
	sum := func(c Counts) int { return c.Critical + c.High + c.Medium + c.Low + c.Unknown }
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		c := 0
		switch key {
		case "vulns":
			c = cmp.Compare(sum(a.Vulns), sum(b.Vulns))
		case "hosts":
			c = cmp.Compare(a.Hosts, b.Hosts)
		case "name":
			c = cmp.Compare(a.Name, b.Name)
		default:
			c = cmp.Compare(a.Priority, b.Priority)
		}
		if c == 0 {
			return a.Ecosystem+"\x00"+a.Name < b.Ecosystem+"\x00"+b.Name
		}
		if q.Get("order") == "asc" {
			return c < 0
		}
		return c > 0
	})
	total := len(filtered)
	start := min(pg.Offset, total)
	items := filtered[start : start+min(pg.Limit, total-start)]
	return reply(200, struct {
		Total int `json:"total"`
		page
		Items []PackageRow `json:"items"`
	}{total, pg, items})
}

// withText adds the longest description and up to 20 distinct references from
// the cached advisories behind one CVE. It reads local shards, never the network.
func (p *Integration) withText(v Vulnerability) Vulnerability {
	v.Details, v.References = "", []Reference{}
	store, err := openAdvisoryStore(filepath.Join(p.host.CacheDir(), "advisories"))
	if err != nil {
		return v
	}
	seen := map[Reference]bool{}
	for _, id := range v.OSVIDs {
		a, ok := store.get(id)
		if !ok {
			continue
		}
		if len(a.Details) > len(v.Details) {
			v.Details = a.Details
		}
		for _, ref := range a.References {
			if !seen[ref] && len(v.References) < 20 {
				seen[ref] = true
				v.References = append(v.References, ref)
			}
		}
	}
	return v
}
