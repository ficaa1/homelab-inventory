// Package cve matches stored distro source packages through OSV. Background
// workers own network I/O; the authenticated page and alert APIs only read cache.
package cve

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

const defaultOSV = "https://api.osv.dev"
const defaultKEV = "https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json"
const defaultEPSS = "https://api.first.org/data/v1/epss"

type config struct {
	OSV, KEV, EPSS                  string
	Interval                        time.Duration
	Images, AlertKEV, AlertCritical bool
}
type Integration struct {
	host         plugin.Host
	mu           sync.RWMutex
	cfg          config
	source       func() ([]plugin.PackageHost, error)
	result       Result
	status       Status
	enrich       enrichment
	suppressions map[string]Suppression
	cancel       context.CancelFunc
	done         chan struct{}
	trigger      chan struct{}
	running      bool
	// started is the config the worker last ran with; a settings change that
	// alters what a scan matches triggers a rescan instead of waiting.
	started *config
}

func New(h plugin.Host) plugin.Plugin {
	p := &Integration{host: h, suppressions: map[string]Suppression{}, result: Result{Vulns: []Vulnerability{}, Hosts: []HostRow{}, Coverage: Coverage{Unsupported: []Unsupported{}, Skipped: []Skipped{}}}, status: Status{State: "never", Coverage: Coverage{Unsupported: []Unsupported{}, Skipped: []Skipped{}}}}
	p.enrich = enrichment{KEV: map[string]KEV{}, EPSS: map[string]EPSS{}, EPSSTimes: map[string]time.Time{}}
	p.load("result.json.gz", &struct {
		Result *Result `json:"result"`
		Status *Status `json:"status"`
	}{&p.result, &p.status})
	p.load("enrichment.json", &p.enrich)
	p.load("suppressions.json", &p.suppressions)
	p.status.Sources.KEV = p.enrich.Sources.KEV
	p.status.Sources.EPSS = p.enrich.Sources.EPSS
	if p.suppressions == nil {
		p.suppressions = map[string]Suppression{}
	}
	if p.enrich.KEV == nil {
		p.enrich.KEV = map[string]KEV{}
	}
	if p.enrich.EPSS == nil {
		p.enrich.EPSS = map[string]EPSS{}
	}
	if p.enrich.EPSSTimes == nil {
		p.enrich.EPSSTimes = map[string]time.Time{}
	}
	if p.status.State == "scanning" {
		p.status.State = "error"
		if p.status.LastScan != nil {
			p.status.LastScan.Error = ptr("scan interrupted")
		}
	}
	p.status.Phase = nil
	p.status.Progress = nil
	p.status.NextScanAt = nil
	_ = p.Configure(nil)
	return p
}
func (p *Integration) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "cve", Name: "Vulnerabilities", Description: "Match installed host and container packages against OSV distro advisories, with CISA KEV and EPSS prioritisation.", Version: "1", Tab: &plugin.Tab{Title: "Vulnerabilities", Icon: "bug", Views: []plugin.ViewRef{{Name: "vulns", Title: "Vulnerabilities"}, {Name: "packages", Title: "Packages"}, {Name: "hosts", Title: "Hosts"}}}, Settings: []plugin.Field{
		{Key: "osv_url", Label: "OSV API URL", Type: plugin.URL, Default: defaultOSV, Required: true, Help: "Package names and versions are sent to OSV.dev to match distro source-package advisories."},
		{Key: "kev_url", Label: "CISA KEV feed URL", Type: plugin.URL, Default: defaultKEV, Help: "Fetched at most daily. Leave empty to disable KEV."},
		{Key: "epss_url", Label: "FIRST EPSS API URL", Type: plugin.URL, Default: defaultEPSS, Help: "CVE IDs are sent to FIRST for EPSS scores, at most daily. Leave empty to disable EPSS."},
		{Key: "interval_hours", Label: "Scan interval (hours)", Type: plugin.Number, Default: 12, Help: "Between 1 and 168 hours."},
		{Key: "include_images", Label: "Include sampled container images", Type: plugin.Bool, Default: true},
		{Key: "alert_kev", Label: "Alert on known exploited vulnerabilities", Type: plugin.Bool, Default: true},
		{Key: "alert_critical", Label: "Alert on fixable critical vulnerabilities", Type: plugin.Bool, Default: false},
	}}
}
func (p *Integration) Configure(c plugin.Config) error {
	cfg := config{OSV: defaultOSV, KEV: defaultKEV, EPSS: defaultEPSS, Interval: 12 * time.Hour, Images: true, AlertKEV: true}
	for k, dst := range map[string]*string{"osv_url": &cfg.OSV, "kev_url": &cfg.KEV, "epss_url": &cfg.EPSS} {
		if raw, ok := c[k]; ok {
			s, ok := raw.(string)
			if !ok {
				return &plugin.FieldError{Key: k, Message: "expected a URL"}
			}
			*dst = strings.TrimRight(strings.TrimSpace(s), "/")
		}
		if *dst == "" && k != "osv_url" {
			continue
		}
		u, err := url.Parse(*dst)
		if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return &plugin.FieldError{Key: k, Message: "expected an HTTP URL without credentials, query or fragment"}
		}
	}
	if raw, ok := c["interval_hours"]; ok {
		n, err := strconv.ParseFloat(fmt.Sprint(raw), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 168 {
			return &plugin.FieldError{Key: "interval_hours", Message: "interval must be between 1 and 168 hours"}
		}
		cfg.Interval = time.Duration(n * float64(time.Hour))
	}
	for k, dst := range map[string]*bool{"include_images": &cfg.Images, "alert_kev": &cfg.AlertKEV, "alert_critical": &cfg.AlertCritical} {
		if raw, ok := c[k]; ok {
			b, ok := raw.(bool)
			if !ok {
				return &plugin.FieldError{Key: k, Message: "expected a boolean"}
			}
			*dst = b
		}
	}
	p.mu.Lock()
	p.cfg = cfg
	p.status.Sources.KEV.Enabled = cfg.KEV != ""
	p.status.Sources.EPSS.Enabled = cfg.EPSS != ""
	p.mu.Unlock()
	return nil
}
func (p *Integration) SetPackageSource(source func() ([]plugin.PackageHost, error)) {
	p.mu.Lock()
	p.source = source
	p.mu.Unlock()
}
func (p *Integration) Test(ctx context.Context) plugin.TestResult {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	var out batchResponse
	err := p.request(ctx, "POST", cfg.OSV+"/v1/querybatch", map[string]any{"queries": []any{}}, &out)
	if err != nil {
		return plugin.TestResult{Message: "OSV API unavailable", Checks: []plugin.Check{{Name: "OSV", Message: "OSV API unavailable"}}}
	}
	return plugin.TestResult{OK: true, Message: "OSV API reachable", Checks: []plugin.Check{{Name: "OSV", OK: true, Message: "OSV API reachable"}}}
}
func (p *Integration) Start() error {
	_ = p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	p.mu.Lock()
	p.cancel = cancel
	p.done = make(chan struct{})
	p.trigger = make(chan struct{}, 1)
	done := p.done
	trigger := p.trigger
	wait := p.resumeDelayLocked()
	if p.started != nil && !p.started.sameScan(p.cfg) {
		wait = 0
	}
	cfg := p.cfg
	p.started = &cfg
	if wait > 0 {
		next := p.host.Now().UTC().Add(wait)
		p.status.NextScanAt = &next
	} else {
		p.beginLocked()
	}
	p.mu.Unlock()
	go p.worker(ctx, done, trigger, wait)
	return nil
}

func (c config) sameScan(o config) bool {
	return c.OSV == o.OSV && c.KEV == o.KEV && c.EPSS == o.EPSS && c.Images == o.Images
}

// resumeDelayLocked keeps the schedule across restarts: after a successful
// scan the next one is due an interval later, not whenever the API restarts.
// A first scan, a failed one or one cut short by a restart runs straight away.
func (p *Integration) resumeDelayLocked() time.Duration {
	last := p.status.LastScan
	if p.status.State != "idle" || last == nil || last.FinishedAt == nil || last.Error != nil || p.result.GeneratedAt == nil {
		return 0
	}
	return min(max(0, last.FinishedAt.Add(p.cfg.Interval).Sub(p.host.Now())), p.cfg.Interval)
}
func (p *Integration) Close() error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.cancel = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}
func (p *Integration) beginLocked() {
	p.running = true
	p.status.State = "scanning"
	p.status.Phase = ptr("inventory")
	p.status.Progress = nil
	p.status.NextScanAt = nil
	p.status.LastScan = &LastScan{StartedAt: p.host.Now().UTC()}
}
func (p *Integration) startScan() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running || p.cancel == nil {
		return false
	}
	p.beginLocked()
	p.trigger <- struct{}{}
	return true
}
func (p *Integration) worker(ctx context.Context, done chan struct{}, trigger <-chan struct{}, wait time.Duration) {
	defer close(done)
	defer func() {
		p.mu.Lock()
		p.running = false
		if p.status.State == "scanning" {
			p.status.State = "error"
			p.status.Phase = nil
			p.status.Progress = nil
			if p.status.LastScan != nil {
				p.status.LastScan.Error = ptr("scan canceled")
			}
		}
		p.status.NextScanAt = nil
		p.mu.Unlock()
	}()
	for {
		if wait > 0 && !p.sleep(ctx, trigger, wait) {
			return
		}
		err := p.scan(ctx)
		now := p.host.Now().UTC()
		p.mu.Lock()
		p.running = false
		p.status.Phase = nil
		p.status.Progress = nil
		p.status.LastScan.FinishedAt = &now
		p.status.LastScan.DurationMS = max(0, now.Sub(p.status.LastScan.StartedAt).Milliseconds())
		p.status.LastScan.Error = nil
		if err != nil {
			p.status.State = "error"
			p.status.LastScan.Error = ptr(err.Error())
		} else {
			p.status.State = "idle"
		}
		interval := p.cfg.Interval
		next := now.Add(interval)
		p.status.NextScanAt = &next
		persistErr := p.save("result.json.gz", struct {
			Result Result `json:"result"`
			Status Status `json:"status"`
		}{p.result, p.status})
		p.mu.Unlock()
		if persistErr != nil {
			p.host.Logf("could not persist scan result")
		}
		// A scan briefly holds every advisory it read; give that memory back
		// rather than keeping the API's footprint at its peak until the next one.
		debug.FreeOSMemory()
		if ctx.Err() != nil {
			return
		}
		wait = interval
	}
}

// sleep waits for the next scheduled scan or a manual trigger, and reports
// false when the worker is shutting down instead.
func (p *Integration) sleep(ctx context.Context, trigger <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-trigger:
		// startScan already marked the scan as running.
	case <-timer.C:
		p.mu.Lock()
		if !p.running {
			p.beginLocked()
		} else {
			// A manual trigger raced the timer; consume that request in this scan.
			select {
			case <-trigger:
			default:
			}
		}
		p.mu.Unlock()
	}
	return true
}
func (p *Integration) phase(phase string, done, total int) {
	p.mu.Lock()
	p.status.Phase = &phase
	p.status.Progress = &Progress{done, total}
	p.mu.Unlock()
}

// load and save gzip files whose name ends in .gz; the scan result is tens of
// megabytes of JSON for a lab with a few dozen package sets.
func (p *Integration) load(name string, v any) {
	f, err := os.Open(filepath.Join(p.host.CacheDir(), name))
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		p.host.Logf("could not load %s", name)
		return
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(name, ".gz") {
		zr, err := gzip.NewReader(f)
		if err != nil {
			p.host.Logf("could not load %s", name)
			return
		}
		r = zr
	}
	if json.NewDecoder(r).Decode(v) != nil {
		p.host.Logf("could not load %s", name)
	}
}
func (p *Integration) save(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if strings.HasSuffix(name, ".gz") {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err = zw.Write(b); err == nil {
			err = zw.Close()
		}
		if err != nil {
			return err
		}
		b = buf.Bytes()
	}
	if err = os.MkdirAll(p.host.CacheDir(), 0700); err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(p.host.CacheDir(), name), b, 0600)
}
func ptr[T any](v T) *T { return &v }
func activeSuppression(s Suppression, now time.Time) bool {
	return s.Until == nil || *s.Until >= now.UTC().Format("2006-01-02")
}
func (p *Integration) snapshotLocked() Result {
	r := p.result
	r.Vulns = append([]Vulnerability{}, r.Vulns...)
	for i := range r.Vulns {
		v := &r.Vulns[i]
		v.Suppressed = nil
		if s, ok := p.suppressions[v.ID]; ok && activeSuppression(s, p.host.Now()) {
			v.Suppressed = &s
		}
		v.KEV = nil
		v.EPSS = nil
		if p.cfg.KEV != "" {
			if k, ok := p.enrich.KEV[v.ID]; ok {
				v.KEV = &k
			}
		}
		if p.cfg.EPSS != "" {
			if e, ok := p.enrich.EPSS[v.ID]; ok {
				v.EPSS = &e
			}
		}
		v.Priority = severityRank(v.Severity) * 100
		if v.KEV != nil {
			v.Priority += 1000
		}
		if v.EPSS != nil {
			v.Priority += int(math.Round(v.EPSS.Percentile * 99))
		}
		if v.Fix != "none" {
			v.Priority += 50
		}
	}
	return r
}
func (p *Integration) View(_ context.Context, name string, _ url.Values) (plugin.View, error) {
	title := map[string]string{"vulns": "Vulnerabilities", "packages": "Packages", "hosts": "Hosts"}[name]
	if title == "" {
		return plugin.View{}, plugin.ErrNotFound
	}
	p.mu.RLock()
	r := p.snapshotLocked()
	state := p.status.State
	p.mu.RUnlock()
	o := overview(r)
	return plugin.View{Title: title, UpdatedAt: func() time.Time {
		if r.GeneratedAt != nil {
			return *r.GeneratedAt
		}
		return time.Time{}
	}(), RefreshSeconds: 30, Blocks: []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "Vulnerabilities", Value: strconv.Itoa(o.Totals.Vulns)}, {Label: "Known exploited", Value: strconv.Itoa(o.Totals.KEV)}, {Label: "Fixable", Value: strconv.Itoa(o.Totals.Fixable)}}}, {Type: "notice", Tone: "info", Text: "Distro source-package advisories from OSV. Scan state: " + state + ". Open the Vulnerabilities page for triage and package details."}}}, nil
}
func (p *Integration) Alerts(ctx context.Context) ([]plugin.Alert, error) {
	p.mu.RLock()
	r := p.snapshotLocked()
	cfg := p.cfg
	p.mu.RUnlock()
	sortVulns(r.Vulns, "priority", "desc")
	out := []plugin.Alert{}
	for _, v := range r.Vulns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if v.Suppressed != nil {
			continue
		}
		fix := v.Fix != "none"
		suffix := ""
		if fix {
			suffix = " · fix available"
		}
		if cfg.AlertKEV && v.KEV != nil {
			sev := "warning"
			if fix {
				sev = "critical"
			}
			out = append(out, plugin.Alert{Rule: "kev:" + v.ID, Subject: v.ID, Severity: sev, Summary: fmt.Sprintf("%s (KEV) on %s%s", v.ID, hostCount(v.Hosts), suffix)})
		}
		if len(out) >= 50 {
			break
		}
		if cfg.AlertCritical && v.Severity == "critical" && fix {
			out = append(out, plugin.Alert{Rule: "critical:" + v.ID, Subject: v.ID, Severity: "warning", Summary: fmt.Sprintf("%s (critical) on %s%s", v.ID, hostCount(v.Hosts), suffix)})
		}
		if len(out) >= 50 {
			break
		}
	}
	return out, nil
}

func hostCount(n int) string {
	if n == 1 {
		return "1 host"
	}
	return strconv.Itoa(n) + " hosts"
}

var errInventory = errors.New("package inventory unavailable")
