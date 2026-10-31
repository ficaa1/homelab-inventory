// Package plexload reads fixed Prometheus queries for a configured exporter target.
package plexload

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func selector(job, instance string) string {
	return `{job=` + strconv.Quote(job) + `,instance=` + strconv.Quote(instance) + `}`
}
func querySet(target string) []string {
	idle := strings.TrimSuffix(target, "}") + `,mode="idle"}`
	cpu := cpuQuery(target)
	gpu := `plex_gpu_util_pct` + target
	return []string{
		`up` + target, `min(timestamp(node_cpu_seconds_total` + idle + `))`, cpu,
		`node_load1` + target, `node_load5` + target, `node_load15` + target,
		`count(node_cpu_seconds_total` + idle + `)`,
		`node_memory_MemTotal_bytes` + target, `node_memory_MemAvailable_bytes` + target,
		`plex_gpu_sample_timestamp_seconds` + target, gpu,
		`plex_gpu_encoder_pct` + target, `plex_gpu_decoder_pct` + target,
		`plex_gpu_mem_used_bytes` + target, `plex_gpu_mem_total_bytes` + target,
		`timestamp(up` + target + `)`, `timestamp(node_load1` + target + `)`,
		`timestamp(node_load5` + target + `)`, `timestamp(node_load15` + target + `)`,
		`timestamp(node_memory_MemTotal_bytes` + target + `)`, `timestamp(node_memory_MemAvailable_bytes` + target + `)`,
	}
}
func cpuQuery(target string) string {
	idle := strings.TrimSuffix(target, "}") + `,mode="idle"}`
	return `clamp_max(clamp_min(100 * (1 - avg(rate(node_cpu_seconds_total` + idle + `[2m]))), 0), 100)`
}

type Report struct {
	Label             string     `json:"label"`
	Status            string     `json:"status"`
	Error             string     `json:"error,omitempty"`
	Host              string     `json:"host"`
	Source            string     `json:"source"`
	SampledAt         *time.Time `json:"sampled_at"`
	CPUPct            *float64   `json:"cpu_pct"`
	CPUCores          *int64     `json:"cpu_cores"`
	Load              Load       `json:"load"`
	Mem               Memory     `json:"mem"`
	GPU               GPU        `json:"gpu"`
	CPUSpark          []float64  `json:"cpu_spark"`
	TranscodeSessions *int       `json:"transcode_sessions"`
}
type Load struct {
	One     *float64 `json:"one"`
	Five    *float64 `json:"five"`
	Fifteen *float64 `json:"fifteen"`
}
type Memory struct {
	UsedBytes      *int64   `json:"used_bytes"`
	AvailableBytes *int64   `json:"available_bytes"`
	TotalBytes     *int64   `json:"total_bytes"`
	Pct            *float64 `json:"pct"`
}
type GPU struct {
	Status  string   `json:"status"`
	Reason  string   `json:"reason,omitempty"`
	Devices []Device `json:"devices"`
}
type Device struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	SampledAt *time.Time `json:"sampled_at"`
	Util      *float64   `json:"gpu_util_pct"`
	Encoder   *float64   `json:"gpu_encoder_pct"`
	Decoder   *float64   `json:"gpu_decoder_pct"`
	Mem       GPUMemory  `json:"gpu_mem"`
	Spark     []float64  `json:"spark"`
}
type GPUMemory struct {
	UsedBytes  *int64 `json:"used_bytes"`
	TotalBytes *int64 `json:"total_bytes"`
}
type Client struct {
	base    string
	http    *http.Client
	mu      sync.Mutex
	cached  Report
	expires time.Time
	target  string
	label   string
	token   string
	loading chan struct{}
}

func New(base, token, job, instance, label string, client *http.Client) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, target: selector(job, instance), label: label, http: client}
}
func (c *Client) empty(status, reason string) Report {
	return Report{Status: status, Error: reason, Label: c.label, Host: c.label, Source: "prometheus+nvidia-smi", GPU: GPU{Status: status, Reason: reason}}
}

// Fetch coalesces concurrent refreshes and caches for 15 seconds. Request
// cancellation never becomes a cached outage; waiting callers cancel independently.
func (c *Client) Fetch(ctx context.Context, now time.Time) Report {
	if c.base == "" {
		return c.empty("not_configured", "")
	}
	for {
		if ctx.Err() != nil {
			return c.empty("unavailable", "request canceled")
		}
		c.mu.Lock()
		if now.Before(c.expires) {
			r := c.cached
			c.mu.Unlock()
			return r
		}
		if pending := c.loading; pending != nil {
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return c.empty("unavailable", "request canceled")
			case <-pending:
				continue
			}
		}
		c.loading = make(chan struct{})
		c.mu.Unlock()
		child, cancel := context.WithTimeout(ctx, 3*time.Second)
		r := c.fetch(child, now)
		cancel()
		c.mu.Lock()
		if ctx.Err() == nil {
			c.cached = r
			c.expires = now.Add(15 * time.Second)
		}
		close(c.loading)
		c.loading = nil
		c.mu.Unlock()
		return r
	}
}

type series struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}
type response struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string   `json:"resultType"`
		Result     []series `json:"result"`
	} `json:"data"`
}

func (c *Client) query(ctx context.Context, expression string, now time.Time, ranged bool) ([]series, error) {
	u, e := url.Parse(c.base)
	if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid configuration")
	}
	path := "/api/v1/query"
	q := url.Values{"query": {expression}, "time": {strconv.FormatInt(now.Unix(), 10)}}
	if ranged {
		path = "/api/v1/query_range"
		q = url.Values{"query": {expression}, "start": {strconv.FormatInt(now.Add(-time.Hour).Unix(), 10)}, "end": {strconv.FormatInt(now.Unix(), 10)}, "step": {"125"}}
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return nil, e
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, e := c.http.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("upstream status")
	}
	body, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if e != nil || len(body) > 1<<20 {
		return nil, errors.New("invalid response")
	}
	var out response
	if json.Unmarshal(body, &out) != nil || out.Status != "success" || (!ranged && out.Data.ResultType != "vector") || (ranged && out.Data.ResultType != "matrix") {
		return nil, errors.New("invalid response")
	}
	return out.Data.Result, nil
}
func pair(p []json.RawMessage) (float64, float64, bool) {
	if len(p) != 2 {
		return 0, 0, false
	}
	var ts float64
	var raw string
	if json.Unmarshal(p[0], &ts) != nil || json.Unmarshal(p[1], &raw) != nil {
		return 0, 0, false
	}
	v, e := strconv.ParseFloat(raw, 64)
	return ts, v, e == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && !math.IsNaN(ts) && !math.IsInf(ts, 0)
}
func fresh(ts float64, now time.Time) bool {
	age := float64(now.Unix()) - ts
	return age >= -5 && age <= 90
}
func scalar(s []series, now time.Time) *float64 {
	if len(s) != 1 {
		return nil
	}
	ts, v, ok := pair(s[0].Value)
	if !ok || !fresh(ts, now) {
		return nil
	}
	return &v
}
func bytes(v *float64) *int64 {
	if v == nil || *v < 0 || *v >= float64(math.MaxInt64) || math.Trunc(*v) != *v {
		return nil
	}
	n := int64(*v)
	return &n
}
func pct(v *float64) *float64 {
	if v == nil || *v < 0 || *v > 100 {
		return nil
	}
	return v
}
func nonnegative(v *float64) *float64 {
	if v != nil && *v >= 0 {
		return v
	}
	return nil
}
func points(s series, now time.Time) []float64 {
	if len(s.Values) == 0 || len(s.Values) > 30 {
		return nil
	}
	out := make([]float64, 0, len(s.Values))
	last := math.Inf(-1)
	for _, p := range s.Values {
		ts, v, ok := pair(p)
		if !ok || ts <= last || ts < float64(now.Add(-time.Hour).Unix()) || ts > float64(now.Unix()) || v < 0 || v > 100 {
			return nil
		}
		last = ts
		out = append(out, v)
	}
	return out
}
func (c *Client) fetch(ctx context.Context, now time.Time) Report {
	queries := querySet(c.target)
	vals := make([][]series, len(queries))
	errs := make([]error, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) { defer wg.Done(); vals[i], errs[i] = c.query(ctx, q, now, false) }(i, q)
	}
	var cpuSpark, gpuSpark []series
	wg.Add(2)
	go func() {
		defer wg.Done()
		cpuSpark, _ = c.query(ctx, cpuQuery(c.target)+` and (min(timestamp(node_cpu_seconds_total`+strings.TrimSuffix(c.target, "}")+`,mode="idle"})) > time() - 90)`, now, true)
	}()
	// Reject historical stale collector values as well as stale current devices.
	go func() {
		defer wg.Done()
		gpuSpark, _ = c.query(ctx, `plex_gpu_util_pct`+c.target+` and on(uuid) (plex_gpu_sample_timestamp_seconds`+c.target+` > time() - 90)`, now, true)
	}()
	wg.Wait()
	r := c.empty("unavailable", "node exporter unavailable")
	up := scalar(vals[0], now)
	upTimestamp := scalar(vals[15], now)
	if up == nil || *up != 1 || upTimestamp == nil || !fresh(*upTimestamp, now) {
		return r
	}
	raw := scalar(vals[1], now)
	if raw != nil && fresh(*raw, now) {
		r.CPUPct = pct(scalar(vals[2], now))
		r.CPUCores = bytes(scalar(vals[6], now))
		if r.CPUCores != nil && *r.CPUCores == 0 {
			r.CPUCores = nil
		}
	}
	measurement := func(index, stampIndex int) *float64 {
		stamp := scalar(vals[stampIndex], now)
		if stamp == nil || !fresh(*stamp, now) {
			return nil
		}
		return scalar(vals[index], now)
	}
	r.Load = Load{nonnegative(measurement(3, 16)), nonnegative(measurement(4, 17)), nonnegative(measurement(5, 18))}
	r.Mem.TotalBytes = bytes(measurement(7, 19))
	r.Mem.AvailableBytes = bytes(measurement(8, 20))
	if r.Mem.TotalBytes != nil && *r.Mem.TotalBytes == 0 {
		r.Mem.TotalBytes = nil
	}
	if r.Mem.TotalBytes != nil && r.Mem.AvailableBytes != nil && *r.Mem.AvailableBytes > *r.Mem.TotalBytes {
		r.Mem.AvailableBytes = nil
	}
	if r.Mem.TotalBytes != nil && r.Mem.AvailableBytes != nil {
		used := *r.Mem.TotalBytes - *r.Mem.AvailableBytes
		percent := 100 * float64(used) / float64(*r.Mem.TotalBytes)
		r.Mem.UsedBytes = &used
		r.Mem.Pct = &percent
	}
	if r.CPUPct == nil && r.Load.One == nil && r.Load.Five == nil && r.Load.Fifteen == nil && r.Mem.TotalBytes == nil {
		return r
	}
	r.Status, r.Error = "ok", ""
	if r.CPUPct == nil || r.CPUCores == nil || r.Load.One == nil || r.Load.Five == nil || r.Load.Fifteen == nil || r.Mem.Pct == nil {
		r.Status, r.Error = "partial", "some host metrics unavailable"
	}
	// Use the oldest raw measurement time, not Prometheus evaluation time.
	var oldest float64 = math.Inf(1)
	if raw != nil && r.CPUPct != nil {
		oldest = *raw
	}
	for _, idx := range []int{16, 17, 18, 19, 20} {
		stamp := scalar(vals[idx], now)
		if stamp != nil && fresh(*stamp, now) && *stamp < oldest {
			oldest = *stamp
		}
	}

	if !math.IsInf(oldest, 1) {
		t := time.Unix(int64(oldest), 0).UTC()
		r.SampledAt = &t
	}
	if r.CPUPct != nil && len(cpuSpark) == 1 {
		r.CPUSpark = points(cpuSpark[0], now)
	}
	r.GPU = buildGPU(vals[9:15], gpuSpark, now)
	for _, err := range errs[9:15] {
		if err != nil {
			r.GPU.Status, r.GPU.Reason = "unavailable", "gpu metrics unavailable"
			if len(r.GPU.Devices) > 0 {
				r.GPU.Status = "partial"
			}
			break
		}
	}
	if r.GPU.Status != "ok" && r.Status == "ok" {
		r.Status = "partial"
	}
	return r
}
func buildGPU(vals [][]series, sparks []series, now time.Time) GPU {
	g := GPU{Status: "unavailable", Reason: "no gpu metrics"}
	any := false
	for _, ss := range vals {
		if len(ss) > 0 {
			any = true
		}
	}
	if !any {
		return g
	}
	g.Reason = "gpu metrics unavailable"
	// Timestamp series identify devices. Duplicate UUIDs are ambiguous, not summed.
	stamps := map[string][]series{}
	for _, s := range vals[0] {
		stamps[s.Metric["uuid"]] = append(stamps[s.Metric["uuid"]], s)
	}
	for id, ss := range stamps {
		stamp := scalar(ss, now)
		if id == "" || stamp == nil || !fresh(*stamp, now) {
			continue
		}
		d := Device{ID: id, Name: ss[0].Metric["name"], Status: "ok"}
		t := time.Unix(int64(*stamp), 0).UTC()
		d.SampledAt = &t
		fields := make([]*float64, 5)
		for i := 1; i < 6; i++ {
			matches := []series{}
			for _, s := range vals[i] {
				if s.Metric["uuid"] == id {
					matches = append(matches, s)
				}
			}
			fields[i-1] = scalar(matches, now)
		}
		d.Util, d.Encoder, d.Decoder = pct(fields[0]), pct(fields[1]), pct(fields[2])
		d.Mem = GPUMemory{bytes(fields[3]), bytes(fields[4])}
		if d.Mem.TotalBytes != nil && *d.Mem.TotalBytes == 0 {
			d.Mem.TotalBytes = nil
		}
		if d.Mem.TotalBytes != nil && d.Mem.UsedBytes != nil && *d.Mem.UsedBytes > *d.Mem.TotalBytes {
			d.Mem.UsedBytes = nil
		}
		if d.Util == nil || d.Encoder == nil || d.Decoder == nil || d.Mem.TotalBytes == nil || d.Mem.UsedBytes == nil {
			d.Status = "partial"
		}
		for _, s := range sparks {
			if s.Metric["uuid"] == id {
				d.Spark = points(s, now)
			}
		}
		g.Devices = append(g.Devices, d)
	}
	if len(g.Devices) == 0 {
		return g
	}
	sort.Slice(g.Devices, func(i, j int) bool { return g.Devices[i].ID < g.Devices[j].ID })
	g.Status, g.Reason = "ok", ""
	if len(g.Devices) != len(stamps) {
		g.Status, g.Reason = "partial", "some gpu metrics unavailable"
	}
	for _, d := range g.Devices {
		if d.Status != "ok" {
			g.Status, g.Reason = "partial", "some gpu metrics unavailable"
		}
	}
	return g
}
