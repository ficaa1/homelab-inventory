package cve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type osvPackage struct {
	Name      string `json:"name"`
	Ecosystem string `json:"ecosystem"`
}
type query struct {
	Package   osvPackage `json:"package"`
	Version   string     `json:"version"`
	PageToken string     `json:"page_token,omitempty"`
}
type match struct {
	ID       string `json:"id"`
	Modified string `json:"modified"`
}
type batchResult struct {
	Vulns         []match `json:"vulns"`
	NextPageToken string  `json:"next_page_token"`
}
type batchResponse struct {
	Results []batchResult `json:"results"`
}
type severity struct {
	Type  string `json:"type"`
	Score string `json:"score"`
}
type osvEvent struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}
type osvRange struct {
	Type   string     `json:"type"`
	Events []osvEvent `json:"events"`
}
type vendorInfo struct {
	Urgency  string `json:"urgency,omitempty"`
	Priority string `json:"priority,omitempty"`
	Status   string `json:"status,omitempty"`
}
type osvAffected struct {
	Package           osvPackage `json:"package"`
	Ranges            []osvRange `json:"ranges"`
	EcosystemSpecific vendorInfo `json:"ecosystem_specific"`
	Severity          []severity `json:"severity,omitempty"`
}
type advisory struct {
	// TrimmedFor names the packages whose affected entries the cached copy kept.
	TrimmedFor       []osvPackage  `json:"trimmed_for,omitempty"`
	ID               string        `json:"id"`
	Upstream         []string      `json:"upstream"`
	Aliases          []string      `json:"aliases"`
	Summary          string        `json:"summary"`
	Details          string        `json:"details"`
	Published        *time.Time    `json:"published"`
	Modified         string        `json:"modified"`
	Severity         []severity    `json:"severity"`
	CWEIDs           []string      `json:"cwe_ids"`
	References       []Reference   `json:"references"`
	Affected         []osvAffected `json:"affected"`
	DatabaseSpecific struct {
		CWEIDs   []string `json:"cwe_ids"`
		Severity string   `json:"severity,omitempty"`
	} `json:"database_specific"`
}
type queryWork struct {
	Query       query
	Occurrences []Affected
	Matches     []match
}

func (p *Integration) request(ctx context.Context, method, target string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return errors.New("could not encode upstream request")
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errors.New("invalid upstream request")
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.host.HTTP().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("upstream request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil || len(b) > 8<<20 {
		return errors.New("upstream response unreadable or exceeds 8 MiB")
	}
	if json.Unmarshal(b, out) != nil {
		return errors.New("invalid upstream JSON")
	}
	return nil
}
func (p *Integration) scan(ctx context.Context) (err error) {
	p.mu.RLock()
	cfg, source := p.cfg, p.source
	p.mu.RUnlock()
	var r Result
	defer func() {
		if err != nil {
			p.mu.Lock()
			p.status.Sources.OSV.OK = false
			p.status.Sources.OSV.Error = ptr(err.Error())
			if p.result.GeneratedAt == nil && r.Hosts != nil {
				for i := range r.Hosts {
					if r.Hosts[i].Status == "scanned" {
						r.Hosts[i].Status = "failed"
						r.Hosts[i].Reason = ptr("OSV matching incomplete")
					}
				}
				p.result = r
			}
			p.mu.Unlock()
		}
	}()
	if source == nil {
		return errInventory
	}
	hosts, err := source()
	if err != nil {
		return errInventory
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	works, r := inventoryQueries(hosts, cfg.Images, p.host.Now())
	p.mu.Lock()
	p.status.Coverage = r.Coverage
	p.mu.Unlock()
	p.phase("matching", 0, len(works))
	skipped, err := p.match(ctx, cfg.OSV, works)
	if err != nil {
		return err
	}
	r.Coverage.Skipped = skipped
	p.mu.Lock()
	p.status.Coverage.Skipped = skipped
	p.mu.Unlock()
	// Per-CVE distro records (DEBIAN-CVE-*, UBUNTU-CVE-*) cover every CVE; the
	// aggregate notices list many CVEs at once and would group under one of them.
	needed := map[string]string{}
	packages := map[string]map[osvPackage]bool{}
	for _, w := range works {
		kept := w.Matches[:0]
		for _, v := range w.Matches {
			if aggregateNotice(v.ID) {
				continue
			}
			kept = append(kept, v)
			needed[v.ID] = v.Modified
			if packages[v.ID] == nil {
				packages[v.ID] = map[osvPackage]bool{}
			}
			packages[v.ID][w.Query.Package] = true
		}
		w.Matches = kept
	}
	p.phase("details", 0, len(needed))
	records, err := p.details(ctx, cfg.OSV, needed, packages)
	if err != nil {
		return err
	}
	r.Vulns = group(works, records)
	now := p.host.Now().UTC()
	r.GeneratedAt = &now
	p.mu.Lock()
	p.status.Sources.OSV = SourceStatus{OK: true, UpdatedAt: &now}
	p.mu.Unlock()
	p.phase("enrichment", 0, len(r.Vulns))
	if err = p.enrichment(ctx, cfg, r.Vulns); err != nil {
		return err
	}
	p.mu.Lock()
	p.result = r
	p.mu.Unlock()
	return nil
}
func inventoryQueries(hosts []plugin.PackageHost, images bool, now time.Time) ([]*queryWork, Result) {
	r := Result{Vulns: []Vulnerability{}, Hosts: []HostRow{}, Coverage: Coverage{Hosts: len(hosts), Unsupported: []Unsupported{}, Skipped: []Skipped{}}}
	byQuery := map[query]*queryWork{}
	for _, h := range hosts {
		sets := h.Sets
		if sets == nil {
			sets = []plugin.PackageSet{{Scope: "host", Status: "uncollected", Containers: []string{}}}
		}
		for _, s := range sets {
			eco, reason := ecosystem(s.Distro)
			row := HostRow{Host: h.Host, Scope: s.Scope, ImageRef: s.ImageRef, Containers: append([]string{}, s.Containers...), Distro: s.Distro, CollectedAt: h.CollectedAt, Packages: len(s.Packages), Status: "scanned"}
			if eco != "" {
				row.Ecosystem = &eco
				row.EOL = distroEOL(eco, now)
			}
			if s.Status != "ok" {
				reason = "package collection status: " + s.Status
			}
			if s.Scope != "host" && !images {
				reason = "container image scanning disabled"
			}
			r.Coverage.Sets++
			r.Coverage.Packages += len(s.Packages)
			if reason != "" {
				row.Status = "unsupported"
				row.Reason = &reason
				r.Coverage.Unsupported = append(r.Coverage.Unsupported, Unsupported{h.Host, s.Scope, s.ImageRef, reason})
			} else {
				r.Coverage.SetsScanned++
				// One occurrence per source version per set, containing all installed binaries.
				occurrences := map[query]*Affected{}
				for _, pkg := range s.Packages {
					name, version := pkg.Source, pkg.SourceVersion
					if name == "" {
						name = pkg.Name
					}
					if version == "" {
						version = pkg.Version
					}
					q := query{Package: osvPackage{name, eco}, Version: version}
					a := occurrences[q]
					if a == nil {
						a = &Affected{Host: h.Host, Scope: s.Scope, ImageRef: s.ImageRef, Containers: append([]string{}, s.Containers...), Ecosystem: eco, Package: name, Installed: version, Binaries: []Binary{}}
						occurrences[q] = a
					}
					a.Binaries = append(a.Binaries, Binary{pkg.Name, pkg.Version})
				}
				for q, a := range occurrences {
					sort.Slice(a.Binaries, func(i, j int) bool {
						if a.Binaries[i].Name == a.Binaries[j].Name {
							return a.Binaries[i].Version < a.Binaries[j].Version
						}
						return a.Binaries[i].Name < a.Binaries[j].Name
					})
					a.Binaries = uniqueBinaries(a.Binaries)
					w := byQuery[q]
					if w == nil {
						w = &queryWork{Query: q}
						byQuery[q] = w
					}
					w.Occurrences = append(w.Occurrences, *a)
				}
			}
			r.Hosts = append(r.Hosts, row)
		}
	}
	sort.Slice(r.Hosts, func(i, j int) bool {
		a, b := r.Hosts[i], r.Hosts[j]
		if a.Host != b.Host {
			return a.Host < b.Host
		}
		if a.Scope == "host" {
			return b.Scope != "host"
		}
		if b.Scope == "host" {
			return false
		}
		return a.Scope < b.Scope
	})
	works := make([]*queryWork, 0, len(byQuery))
	for _, w := range byQuery {
		works = append(works, w)
	}
	sort.Slice(works, func(i, j int) bool {
		a, b := works[i].Query, works[j].Query
		return a.Package.Ecosystem+"\x00"+a.Package.Name+"\x00"+a.Version < b.Package.Ecosystem+"\x00"+b.Package.Name+"\x00"+b.Version
	})
	return works, r
}
func uniqueBinaries(in []Binary) []Binary {
	out := []Binary{}
	for _, b := range in {
		if len(out) == 0 || out[len(out)-1] != b {
			out = append(out, b)
		}
	}
	return out
}

// match runs querybatch chunks four at a time. A chunk that fails (usually a
// dense batch outrunning the ten-second client timeout) is split in half and
// retried, so one slow request cannot abort a scan; a single query that still
// fails after a pause is a real upstream outage and ends the scan.
func (p *Integration) match(ctx context.Context, base string, works []*queryWork) ([]Skipped, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks := make(chan []*queryWork)
	var mu sync.Mutex
	skipped := []Skipped{}
	skip := func(w *queryWork, err error) {
		mu.Lock()
		sets := map[string]bool{}
		for _, o := range w.Occurrences {
			sets[o.Host+"\x00"+o.Scope] = true
		}
		skipped = append(skipped, Skipped{Ecosystem: w.Query.Package.Ecosystem, Package: w.Query.Package.Name, Version: w.Query.Version, Sets: len(sets), Reason: "OSV did not answer within the timeout: " + err.Error()})
		mu.Unlock()
	}
	var first error
	done := 0
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range chunks {
				if err := p.matchSplit(ctx, base, chunk, skip); err != nil {
					mu.Lock()
					if first == nil {
						first = err
						cancel()
					}
					mu.Unlock()
					return
				}
				mu.Lock()
				done += len(chunk)
				p.phase("matching", done, len(works))
				mu.Unlock()
			}
		}()
	}
send:
	for start := 0; start < len(works); start += 100 {
		select {
		case chunks <- works[start:min(start+100, len(works))]:
		case <-ctx.Done():
			break send
		}
	}
	close(chunks)
	wg.Wait()
	if first != nil {
		return nil, first
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A handful of oversized packages is expected; many failures mean OSV is down.
	if len(skipped) > max(10, len(works)/50) {
		return nil, fmt.Errorf("OSV did not answer for %d package queries", len(skipped))
	}
	sort.Slice(skipped, func(i, j int) bool {
		a, b := skipped[i], skipped[j]
		return a.Ecosystem+"\x00"+a.Package+"\x00"+a.Version < b.Ecosystem+"\x00"+b.Package+"\x00"+b.Version
	})
	return skipped, nil
}
func (p *Integration) matchSplit(ctx context.Context, base string, works []*queryWork, skip func(*queryWork, error)) error {
	err := p.matchChunk(ctx, base, works)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if len(works) == 1 {
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
		if err = p.matchChunk(ctx, base, works); err != nil && ctx.Err() == nil {
			skip(works[0], err)
			return nil
		}
		return err
	}
	half := len(works) / 2
	if err := p.matchSplit(ctx, base, works[:half], skip); err != nil {
		return err
	}
	return p.matchSplit(ctx, base, works[half:], skip)
}

// matchChunk commits matches only when every page of the chunk arrived, so a
// retry never duplicates them.
func (p *Integration) matchChunk(ctx context.Context, base string, works []*queryWork) error {
	found := make([][]match, len(works))
	pending := make([]int, len(works))
	for i := range works {
		pending[i] = i
	}
	tokens := make([]string, len(works))
	seen := map[int]map[string]bool{}
	for len(pending) > 0 {
		qs := make([]query, len(pending))
		for j, i := range pending {
			qs[j] = works[i].Query
			qs[j].PageToken = tokens[j]
		}
		var response batchResponse
		if err := p.request(ctx, "POST", base+"/v1/querybatch", map[string]any{"queries": qs}, &response); err != nil {
			return err
		}
		if len(response.Results) != len(pending) {
			return errors.New("OSV batch result count mismatch")
		}
		next, nextTokens := []int{}, []string{}
		for j, result := range response.Results {
			i := pending[j]
			found[i] = append(found[i], result.Vulns...)
			if result.NextPageToken != "" {
				if seen[i] == nil {
					seen[i] = map[string]bool{}
				}
				if seen[i][result.NextPageToken] {
					return errors.New("OSV repeated pagination token")
				}
				seen[i][result.NextPageToken] = true
				next = append(next, i)
				nextTokens = append(nextTokens, result.NextPageToken)
			}
		}
		pending, tokens = next, nextTokens
	}
	for i, w := range works {
		w.Matches = append(w.Matches[:0], found[i]...)
	}
	return nil
}

// fetchAdvisory retries transient failures (timeouts, 429, 5xx) with backoff.
func (p *Integration) fetchAdvisory(ctx context.Context, base, id string, a *advisory) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		*a = advisory{}
		if err = p.request(ctx, "GET", base+"/v1/vulns/"+url.PathEscape(id), nil, a); err == nil || ctx.Err() != nil {
			return err
		}
		if strings.Contains(err.Error(), "HTTP 4") && !strings.Contains(err.Error(), "HTTP 429") {
			return err
		}
	}
	return err
}

// trimRecord keeps only the affected entries for packages this scan queried:
// one Ubuntu kernel CVE lists every kernel flavour of every release.
func trimRecord(a advisory, queried map[osvPackage]bool) advisory {
	a.Details = truncate(a.Details, 4000)
	if len(a.References) > 20 {
		a.References = a.References[:20]
	}
	affected := []osvAffected{}
	for _, entry := range a.Affected {
		if !queried[entry.Package] {
			continue
		}
		ranges := []osvRange{}
		for _, rng := range entry.Ranges {
			if rng.Type == "ECOSYSTEM" {
				ranges = append(ranges, rng)
			}
		}
		entry.Ranges = ranges
		affected = append(affected, entry)
	}
	a.Affected = affected
	a.CWEIDs = union(a.CWEIDs, a.DatabaseSpecific.CWEIDs)
	a.TrimmedFor = make([]osvPackage, 0, len(queried))
	for pkg := range queried {
		a.TrimmedFor = append(a.TrimmedFor, pkg)
	}
	sort.Slice(a.TrimmedFor, func(i, j int) bool {
		return a.TrimmedFor[i].Ecosystem+"\x00"+a.TrimmedFor[i].Name < a.TrimmedFor[j].Ecosystem+"\x00"+a.TrimmedFor[j].Name
	})
	return a
}

// covers reports whether a cached record was trimmed for every package that
// matched it now; a newly installed package refetches it.
func covers(a advisory, queried map[osvPackage]bool) bool {
	have := map[osvPackage]bool{}
	for _, pkg := range a.TrimmedFor {
		have[pkg] = true
	}
	for pkg := range queried {
		if !have[pkg] {
			return false
		}
	}
	return true
}

func aggregateNotice(id string) bool {
	for _, prefix := range []string{"USN-", "DSA-", "DLA-", "DTSA-"} {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
func (p *Integration) details(ctx context.Context, base string, needed map[string]string, packages map[string]map[osvPackage]bool) (map[string]advisory, error) {
	store, err := openAdvisoryStore(filepath.Join(p.host.CacheDir(), "advisories"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	records := map[string]advisory{}
	var mu sync.Mutex
	var first error
	done, fetched := 0, 0
	fail := func(err error) {
		mu.Lock()
		if first == nil {
			first = err
			cancel()
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range jobs {
				if ctx.Err() != nil {
					return
				}
				a, ok := store.get(id)
				if !ok || !sameModified(a.Modified, needed[id]) || !covers(a, packages[id]) {
					if err := p.fetchAdvisory(ctx, base, id, &a); err != nil {
						fail(err)
						return
					}
					if a.ID != id {
						fail(errors.New("OSV advisory ID mismatch"))
						return
					}
					a = trimRecord(a, packages[id])
					store.put(a)
					mu.Lock()
					fetched++
					// Save progress so an interrupted first scan resumes.
					if fetched%1000 == 0 {
						if err := store.flush(); err != nil {
							mu.Unlock()
							fail(err)
							return
						}
					}
					mu.Unlock()
				}
				mu.Lock()
				records[id] = a
				done++
				p.phase("details", done, len(needed))
				mu.Unlock()
			}
		}()
	}
send:
	for id := range needed {
		select {
		case jobs <- id:
		case <-ctx.Done():
			break send
		}
	}
	close(jobs)
	wg.Wait()
	// Keep what was fetched even when the scan stops; the next one resumes.
	flushErr := store.flush()
	if first != nil {
		return nil, first
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if flushErr != nil {
		return nil, flushErr
	}
	store.prune(needed)
	if err := store.flush(); err != nil {
		return nil, err
	}
	return records, nil
}
func union(a, b []string) []string {
	m := map[string]bool{}
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		m[s] = true
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// sameModified compares OSV timestamps at microsecond precision: querybatch
// reports microseconds and /v1/vulns nanoseconds for the same record, so a
// string comparison would refetch every advisory on every scan.
func sameModified(cached, listed string) bool {
	if cached == listed {
		return true
	}
	a, errA := time.Parse(time.RFC3339Nano, cached)
	b, errB := time.Parse(time.RFC3339Nano, listed)
	return errA == nil && errB == nil && a.Truncate(time.Microsecond).Equal(b.Truncate(time.Microsecond))
}
