package speed

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Reuse four HTTP/1.1 connections for cycling download ranges and bounded
// Cloudflare uploads; keep volume and request-rate limits explicit.
const (
	speedDownBytes  = 128 << 20
	speedUpBytes    = 25_000_000
	speedStreams    = 4
	speedSamples    = 10
	speedWindow     = 8 * time.Second
	speedIgnore     = time.Second
	speedDownCap    = 12 << 30
	speedUpCap      = 3_000_000_000
	speedUpWindow   = 2 * time.Second
	speedBackoff    = 15 * time.Minute
	speedRunLimit   = 40 * time.Second
	speedCooldown   = 2 * time.Minute
	speedHistoryMax = 50
)

var (
	ErrSpeedtestRunning = errors.New("speedtest already running")
	errNoData           = errors.New("no data")
	errFewSamples       = errors.New("too few samples")
	errSpeedCap         = errors.New("byte cap reached; incomplete window")
	errShortWindow      = errors.New("incomplete window")
	speedColo           = regexp.MustCompile(`^[A-Z]{3,4}$`)
)

type SpeedtestCooldown struct{ Wait time.Duration }

func (e SpeedtestCooldown) Error() string { return "speedtest started recently" }

type SpeedPhase struct {
	Bytes         int64   `json:"bytes"` // Payload bytes, including warm-up and unacknowledged uploads.
	MeasuredBytes int64   `json:"measured_bytes"`
	Seconds       float64 `json:"seconds"`
	Complete      bool    `json:"complete"`
}

type SpeedResult struct {
	Download         SpeedPhase           `json:"download"`
	Upload           SpeedPhase           `json:"upload"`
	Time             time.Time            `json:"time"`
	DownMbps         *float64             `json:"down_mbps"`
	UpMbps           *float64             `json:"up_mbps"`
	LatencyMs        *float64             `json:"latency_ms"`
	JitterMs         *float64             `json:"jitter_ms"`
	DownServer       string               `json:"downServer"`
	UpServer         string               `json:"upServer"`
	Colo             string               `json:"colo"`
	Error            string               `json:"error"`
	UploadRefused    bool                 `json:"uploadRefused"`
	UploadRetryAfter *time.Time           `json:"uploadRetryAfter"`
	ServerRetryAfter map[string]time.Time `json:"serverRetryAfter,omitempty"`
}

type SpeedLive struct {
	Phase string  `json:"phase"`
	Mbps  float64 `json:"mbps"`
}

type SpeedSnapshot struct {
	Running bool          `json:"running"`
	Current *SpeedLive    `json:"current"`
	Last    *SpeedResult  `json:"last"`
	History []SpeedResult `json:"history"`
}

type Speedtester struct {
	base         string
	freeURL      string
	scalewayURL  string
	freeSize     int64
	scalewaySize int64
	backoff      map[string]time.Time
	client       *http.Client
	probeClient  *http.Client
	history      *speedHistory

	payload []byte

	downBytes int
	window    time.Duration
	upWindow  time.Duration
	ignore    time.Duration
	downCap   int64
	upCap     int64

	mu      sync.Mutex
	running bool
	started time.Time
	phase   string
	mbps    float64
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

func NewSpeedtester(base, freeURL, scalewayURL string, freeSize, scalewaySize int64, historyPath string, client *http.Client) (*Speedtester, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	hist, err := openSpeedHistory(historyPath)
	if err != nil {
		return nil, err
	}
	payload := make([]byte, speedUpBytes)
	if _, err := rand.Read(payload); err != nil {
		return nil, err
	}
	st := &Speedtester{
		base:         strings.TrimRight(u.String(), "/"),
		freeURL:      freeURL,
		scalewayURL:  scalewayURL,
		freeSize:     freeSize,
		scalewaySize: scalewaySize,
		backoff:      make(map[string]time.Time),
		client:       client,
		probeClient:  client,
		history:      hist,

		payload:   payload,
		downBytes: speedDownBytes,
		window:    speedWindow,
		upWindow:  speedUpWindow,
		ignore:    speedIgnore,
		downCap:   speedDownCap,
		upCap:     speedUpCap,
	}
	st.started = hist.LatestStart()
	for _, r := range hist.List() {
		for _, server := range []string{"free", "scaleway", "cloudflare"} {
			if until := r.ServerRetryAfter[server]; until.After(st.backoff[server]) {
				st.backoff[server] = until
			}
		}
		if r.UploadRetryAfter != nil && r.UploadRetryAfter.After(st.backoff["cloudflare"]) {
			st.backoff["cloudflare"] = *r.UploadRetryAfter
		}
	}
	return st, nil
}

// Start begins one run in the background. The id is the start time, which is
// enough to tell runs apart in logs; it is not stored in history.
func (s *Speedtester) Start(now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("speedtest shut down")
	}
	if s.running {
		return "", ErrSpeedtestRunning
	}
	if !s.started.IsZero() {
		if wait := speedCooldown - now.Sub(s.started); wait > 0 {
			return "", SpeedtestCooldown{Wait: wait}
		}
	}
	// Persist the start before accepting, so a crash cannot bypass cooldown.
	if err := s.history.Append(SpeedResult{Time: now.UTC(), Error: "interrupted"}); err != nil {
		return "", err
	}
	s.running, s.started, s.phase, s.mbps = true, now, "latency", 0
	ctx, cancel := context.WithTimeout(context.Background(), speedRunLimit)
	s.cancel = cancel
	s.done = make(chan struct{})
	go s.run(ctx, now)
	return strconv.FormatInt(now.UnixNano(), 36), nil
}

func (s *Speedtester) Snapshot() SpeedSnapshot {
	s.mu.Lock()
	snap := SpeedSnapshot{Running: s.running}
	if s.running {
		snap.Current = &SpeedLive{Phase: s.phase, Mbps: s.mbps}
	}
	snap.History = s.history.List()
	if s.running && len(snap.History) > 0 && snap.History[0].Time.Equal(s.started) {
		snap.History = snap.History[1:]
	}
	s.mu.Unlock()
	if len(snap.History) > 0 {
		last := snap.History[0]
		snap.Last = &last
	}
	return snap
}

// Close refuses new runs, cancels active requests, and waits for persistence.
func (s *Speedtester) Close() {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	done := s.done
	s.mu.Unlock()
	if done != nil {
		<-done
	}
	s.client.CloseIdleConnections()
	s.probeClient.CloseIdleConnections()
}

func (s *Speedtester) run(ctx context.Context, started time.Time) {
	res := s.measure(ctx, started)
	if err := s.history.Finish(res); err != nil {
		_ = err // failure remains visible through retained history; no raw upstream logs
	}
	s.mu.Lock()
	s.cancel()
	s.running = false
	s.phase, s.mbps = "", 0
	close(s.done)
	s.mu.Unlock()
}

// measure runs the three phases in order. A failed phase is recorded as a
// short error and leaves its field nil; the other phases still run.
func (s *Speedtester) measure(ctx context.Context, started time.Time) SpeedResult {
	res := SpeedResult{Time: started.UTC()}
	var errs []string

	stats, err := s.latency(ctx)
	res.Colo = stats.colo
	if err != nil {
		var retry *speedRetryError
		if !errors.As(err, &retry) {
			errs = append(errs, "latency: "+shortErr(err))
		}
	} else {
		res.LatencyMs, res.JitterMs = &stats.median, &stats.jitter
	}

	phase, down, server, err := s.download(ctx)
	res.DownServer = server
	res.Download = phase
	if err != nil {
		errs = append(errs, "download: "+shortErr(err))
	} else {
		res.DownMbps = &down
	}

	res.UpServer = "cloudflare"
	phase, up, err := s.upload(ctx)
	res.Upload = phase
	if err != nil {
		var refused *speedRetryError
		if errors.As(err, &refused) {
			res.UploadRefused = true
			res.UploadRetryAfter = &refused.until
		} else {
			errs = append(errs, "upload: "+shortErr(err))
		}
	} else {
		res.UpMbps = &up
	}

	res.Error = strings.Join(errs, "; ")
	s.mu.Lock()
	res.ServerRetryAfter = make(map[string]time.Time)
	for server, until := range s.backoff {
		if time.Now().Before(until) {
			res.ServerRetryAfter[server] = until
		}
	}
	s.mu.Unlock()
	return res
}

// shortErr keeps server-controlled text out of the stored result.
func shortErr(err error) string {
	var fallback *speedFallbackError
	if errors.As(err, &fallback) {
		return fallback.Error()
	}
	var status speedHTTPError
	if errors.As(err, &status) {
		return fmt.Sprintf("HTTP %d", status)
	}
	switch {
	case errors.Is(err, errNoData), errors.Is(err, errFewSamples), errors.Is(err, errSpeedCap), errors.Is(err, errShortWindow):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "request failed"
	}
}

type latencyStats struct {
	median, jitter float64
	colo           string
}

func (s *Speedtester) latency(ctx context.Context) (latencyStats, error) {
	s.setLive("latency", 0)
	if until := s.retryUntil("cloudflare"); time.Now().Before(until) {
		return latencyStats{}, &speedRetryError{status: 429, until: until}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var st latencyStats
	var ms []float64
	// Do not reuse a connection with queued transfer bytes from the prior run.
	s.probeClient.CloseIdleConnections()
	defer s.probeClient.CloseIdleConnections()
	for i := 0; i < speedSamples && ctx.Err() == nil; i++ {
		d, colo, err := s.probe(ctx)
		if colo != "" {
			st.colo = colo
		}
		if err == nil {
			ms = append(ms, d)
		} else {
			var retry *speedRetryError
			if errors.As(err, &retry) {
				if retry.status == 429 {
					s.deferServer("cloudflare", err)
					return st, err
				}
				break
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return st, err
	}
	switch len(ms) {
	case 0:
		return st, errNoData
	case 1:
		return st, errFewSamples
	}
	if len(ms) != speedSamples {
		return st, errFewSamples
	}
	st.median, st.jitter = summarizeLatency(ms)
	return st, nil
}

// summarizeLatency takes samples in request order; jitter is the mean absolute
// difference between successive samples.
func summarizeLatency(ms []float64) (median, jitter float64) {
	sorted := append([]float64(nil), ms...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	var sum float64
	for i := 1; i < n; i++ {
		sum += math.Abs(ms[i] - ms[i-1])
	}
	jitter = sum / float64(n-1)
	return round2(median), round2(jitter)
}

// probe times time-to-first-byte after the connection is ready, so TLS setup
// on the first sample does not count as latency.
func (s *Speedtester) probe(ctx context.Context) (float64, string, error) {
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var connected, firstByte atomic.Int64
	trace := &httptrace.ClientTrace{
		GotConn:              func(httptrace.GotConnInfo) { connected.Store(time.Now().UnixNano()) },
		GotFirstResponseByte: func() { firstByte.Store(time.Now().UnixNano()) },
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(pctx, trace), http.MethodGet, s.base+"/__down?bytes=0", nil)
	if err != nil {
		return 0, "", err
	}
	start := time.Now()
	resp, err := s.probeClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	from := start
	if c := connected.Load(); c != 0 {
		from = time.Unix(0, c)
	}
	to := time.Now()
	if n := firstByte.Load(); n != 0 {
		to = time.Unix(0, n)
	}
	ttfb := float64(to.Sub(from)) / float64(time.Millisecond)
	ttfb = math.Max(0, ttfb-cloudflareServerMs(resp.Header.Get("Server-Timing")))
	colo := coloFrom(resp.Header)
	if resp.StatusCode != http.StatusOK {
		return 0, colo, speedResponseError(resp)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return ttfb, colo, err
}

func coloFrom(h http.Header) string {
	if c := h.Get("cf-meta-colo"); speedColo.MatchString(c) {
		return c
	}
	ray := h.Get("cf-ray")
	if i := strings.LastIndexByte(ray, '-'); i >= 0 && speedColo.MatchString(ray[i+1:]) {
		return ray[i+1:]
	}
	return ""
}

type speedHTTPError int

func (e speedHTTPError) Error() string { return fmt.Sprintf("HTTP %d", e) }

// Cloudflare's own engine subtracts cfReqDur, or the cfSpeed* duration sum.
func cloudflareServerMs(header string) float64 {
	var sum float64
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(item), ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		if name != "cfreqdur" && name != "cfrequestduration" && !strings.HasPrefix(name, "cfspeed") {
			continue
		}
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || key != "dur" {
				continue
			}
			n, err := strconv.ParseFloat(strings.Trim(value, "\""), 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
				continue
			}
			if name == "cfreqdur" || name == "cfrequestduration" {
				return n
			}
			sum += n
		}
	}
	return sum
}

// Each phase must fill its configured warm-up and measurement window.
// Any stream failure, parent cancellation or safety cap invalidates its number.
func (s *Speedtester) flow(parent context.Context, name string, capBytes int64, one func(context.Context, *meter) error) (SpeedPhase, float64, error) {
	window := s.window
	if name == "upload" {
		window = s.upWindow
	}
	begun := time.Now()
	deadline := begun.Add(s.ignore + window)
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	m := &meter{start: begun.Add(s.ignore), end: deadline, limit: capBytes, cancel: cancel}
	s.setLive(name, 0)
	stop := make(chan struct{})
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				s.setLive(name, m.mbps(now))
			}
		}
	}()
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	for i := 0; i < speedStreams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := one(ctx, m); err != nil {
					var status speedHTTPError
					if ctx.Err() == nil || errors.As(err, &status) || (!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)) {
						errMu.Lock()
						var retry, previous *speedRetryError
						if firstErr == nil || (errors.As(err, &retry) && (!errors.As(firstErr, &previous) || retry.until.After(previous.until))) {
							firstErr = err
						}
						errMu.Unlock()
						cancel()
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	ended := time.Now()
	close(stop)
	<-reported
	m.mu.Lock()
	phase := SpeedPhase{Bytes: m.total, MeasuredBytes: m.counted, Seconds: math.Max(0, math.Min(window.Seconds(), ended.Sub(m.start).Seconds()))}
	capped := m.total >= m.limit
	m.mu.Unlock()
	switch {
	case firstErr != nil:
		return phase, 0, firstErr
	case capped:
		return phase, 0, errSpeedCap
	case parent.Err() != nil:
		return phase, 0, parent.Err()
	case ended.Before(deadline):
		return phase, 0, errShortWindow
	case phase.MeasuredBytes == 0:
		return phase, 0, errNoData
	}
	phase.Complete = true
	return phase, round2(float64(phase.MeasuredBytes) * 8 / window.Seconds() / 1e6), nil
}

type speedFallbackError struct{ free, scaleway string }

func (e *speedFallbackError) Error() string { return "free: " + e.free + "; scaleway: " + e.scaleway }

// Retry-After accepts delta-seconds and HTTP dates; missing/invalid values use
// a conservative cooldown. Server-controlled text never reaches the result.
type speedRetryError struct {
	status speedHTTPError
	until  time.Time
}

func (e *speedRetryError) Error() string { return "server backoff" }
func (e *speedRetryError) Unwrap() error { return e.status }

func speedResponseError(resp *http.Response) error {
	status := speedHTTPError(resp.StatusCode)
	if resp.StatusCode != 429 && resp.StatusCode != 403 {
		return status
	}
	now := time.Now()
	until := now.Add(speedBackoff)
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= int64((time.Duration(1<<63-1)/time.Second)) {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(value); err == nil && date.After(now) {
		until = date
	}
	return &speedRetryError{status: status, until: until.UTC()}
}

func (s *Speedtester) retryUntil(server string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.backoff[server]
}
func (s *Speedtester) deferServer(server string, err error) {
	until := time.Now().Add(speedBackoff)
	var retry *speedRetryError
	if errors.As(err, &retry) {
		until = retry.until
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if until.After(s.backoff[server]) {
		s.backoff[server] = until
	}
}

func (s *Speedtester) upload(ctx context.Context) (SpeedPhase, float64, error) {
	if until := s.retryUntil("cloudflare"); time.Now().Before(until) {
		return SpeedPhase{}, 0, &speedRetryError{status: 429, until: until}
	}
	phase, mbps, err := s.flow(ctx, "upload", s.upCap, s.upOne)
	var retry *speedRetryError
	if errors.As(err, &retry) {
		s.deferServer("cloudflare", err)
	}
	return phase, mbps, err
}

// Each server gets at most one four-stream attempt. Failures cancel all streams
// and suppress that server on later runs; fallback shares the payload budget.
func (s *Speedtester) download(ctx context.Context) (SpeedPhase, float64, string, error) {
	var used int64
	var primary string
	for _, src := range []struct {
		name, url string
		size      int64
	}{
		{"free", s.freeURL, s.freeSize}, {"scaleway", s.scalewayURL, s.scalewaySize},
	} {
		var phase SpeedPhase
		var mbps float64
		var err error
		if until := s.retryUntil(src.name); time.Now().Before(until) {
			err = &speedRetryError{status: 429, until: until}
		} else {
			var offset atomic.Int64
			phase, mbps, err = s.flow(ctx, "download", s.downCap-used, func(ctx context.Context, m *meter) error {
				var start, end int64
				for {
					start = offset.Load()
					end = min(start+int64(s.downBytes), src.size) - 1
					if offset.CompareAndSwap(start, (end+1)%src.size) {
						break
					}
				}
				return s.rangeOne(ctx, m, src.url, start, end, src.size)
			})
			if err != nil && ctx.Err() == nil && !errors.Is(err, errSpeedCap) {
				s.deferServer(src.name, err)
			}
		}
		phase.Bytes += used
		if err == nil || ctx.Err() != nil || errors.Is(err, errSpeedCap) {
			return phase, mbps, src.name, err
		}
		if src.name == "scaleway" {
			return phase, 0, src.name, &speedFallbackError{free: primary, scaleway: shortErr(err)}
		}
		primary, used = shortErr(err), phase.Bytes
	}
	panic("missing download servers")
}

func (s *Speedtester) rangeOne(ctx context.Context, m *meter, url string, start, end, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return speedResponseError(resp)
	}
	var gotStart, gotEnd, total int64
	if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &gotStart, &gotEnd, &total); err != nil || gotStart != start || gotEnd != end || total < size || resp.ContentLength != end-start+1 {
		return errors.New("unexpected download range")
	}
	return readSpeedDownload(resp.Body, m)
}

func readSpeedDownload(body io.Reader, m *meter) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if m.add(n, time.Now()) {
				return errSpeedCap
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *Speedtester) upOne(ctx context.Context, m *meter) error {
	body := &meteredReader{r: bytes.NewReader(s.payload), m: m}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+"/__up?bytes="+strconv.Itoa(len(s.payload)), body)
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(s.payload))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, drainErr := io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return speedResponseError(resp)
	}
	if drainErr != nil {
		return drainErr
	}
	if body.total.Load() != int64(len(s.payload)) {
		return errors.New("upload not fully accepted")
	}
	// Only acknowledge payload accepted by the server. Transport buffering and
	// the final cancelled requests must not inflate upload throughput.
	m.mu.Lock()
	m.counted += body.counted.Load()
	m.mu.Unlock()
	return nil
}

func (s *Speedtester) setLive(phase string, mbps float64) {
	s.mu.Lock()
	s.phase, s.mbps = phase, mbps
	s.mu.Unlock()
}

type meter struct {
	mu      sync.Mutex
	start   time.Time
	end     time.Time
	limit   int64
	cancel  context.CancelFunc
	total   int64
	counted int64
}

func (m *meter) add(n int, at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.total += int64(n)
	if !at.Before(m.start) && (m.end.IsZero() || !at.After(m.end)) {
		m.counted += int64(n)
	}
	if m.total >= m.limit {
		m.cancel()
	}
	return m.total >= m.limit
}

func (m *meter) mbps(now time.Time) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	elapsed := now.Sub(m.start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(m.counted) * 8 / elapsed / 1e6
}

type meteredReader struct {
	r       io.Reader
	m       *meter
	counted atomic.Int64
	total   atomic.Int64
}

func (r *meteredReader) Read(p []byte) (int, error) {
	r.m.mu.Lock()
	capped := r.m.total >= r.m.limit
	r.m.mu.Unlock()
	if capped {
		return 0, errSpeedCap
	}
	n, err := r.r.Read(p)
	if n > 0 {
		r.total.Add(int64(n))
		at := time.Now()
		r.m.mu.Lock()
		r.m.total += int64(n)
		if !at.Before(r.m.start) && !at.After(r.m.end) {
			r.counted.Add(int64(n))
		}
		capped := r.m.total >= r.m.limit
		r.m.mu.Unlock()
		if capped {
			r.m.cancel()
		}
	}
	return n, err
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
