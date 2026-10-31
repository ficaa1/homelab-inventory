package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// TdarrStatus is a cached read snapshot, not the shrink poller's batch history.
// Unknown sections/fields stay null. Worker counts are configured limits.
type TdarrStatus struct {
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
	ServerVersion *string           `json:"server_version"`
	Nodes         []TdarrNodeStatus `json:"nodes"`
	Queue         *TdarrQueueStatus `json:"queue"`
	LastBatch     *TdarrBatchStatus `json:"last_batch"`
}
type TdarrBatchStatus struct {
	State      string     `json:"state"`
	FinishedAt *time.Time `json:"finished_at"`
	Outcome    *string    `json:"outcome"`
}
type TdarrNodeStatus struct {
	Name    *string             `json:"name"`
	Online  bool                `json:"online"`
	Paused  *bool               `json:"paused"`
	Workers TdarrWorkerLimits   `json:"workers"`
	Active  []TdarrActiveStatus `json:"active"`
}
type TdarrWorkerLimits struct {
	TranscodeGPU   *int `json:"transcode_gpu"`
	TranscodeCPU   *int `json:"transcode_cpu"`
	HealthcheckGPU *int `json:"healthcheck_gpu"`
	HealthcheckCPU *int `json:"healthcheck_cpu"`
}
type TdarrActiveStatus struct {
	File *string  `json:"file"`
	Pct  *float64 `json:"pct"`
	FPS  *float64 `json:"fps"`
}
type TdarrQueueStatus struct {
	Queued     *int `json:"queued"`
	Processing *int `json:"processing"`
	Error      *int `json:"error"`
}

type tdarrStatusNode struct {
	Name    *string `json:"nodeName"`
	Paused  *bool   `json:"nodePaused"`
	Workers map[string]struct {
		Idle *bool `json:"idle"`
	} `json:"workers"`
	Limits struct {
		TranscodeGPU   *int `json:"transcodegpu"`
		TranscodeCPU   *int `json:"transcodecpu"`
		HealthcheckGPU *int `json:"healthcheckgpu"`
		HealthcheckCPU *int `json:"healthcheckcpu"`
	} `json:"workerLimits"`
}
type tdarrStatusFile struct {
	Transcode   *string `json:"TranscodeDecisionMaker"`
	Healthcheck *string `json:"HealthCheck"`
	Medium      *string `json:"fileMedium"`
}

// Status uses only the two GET snapshots and a fixed FileJSONDB/getAll
// database SELECT. There is deliberately no caller-supplied path/body/mode.
// The complete operation is bounded, and failures never become empty queues.
func (t *Tdarr) Status(ctx context.Context) TdarrStatus {
	if !t.Configured() {
		return TdarrStatus{Status: "not_configured"}
	}
	for {
		if ctx.Err() != nil {
			return TdarrStatus{Status: "unavailable", Error: "tdarr: upstream unavailable"}
		}
		t.statusCache.mu.Lock()
		if time.Now().Before(t.statusCache.expires) {
			// Decode a private copy: exported slices and pointers belong to the caller.
			var out TdarrStatus
			_ = json.Unmarshal(t.statusCache.result, &out)
			t.statusCache.mu.Unlock()
			return out
		}
		if done := t.statusCache.refresh; done != nil {
			t.statusCache.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return TdarrStatus{Status: "unavailable", Error: "tdarr: upstream unavailable"}
			}
		}
		done := make(chan struct{})
		t.statusCache.refresh = done
		t.statusCache.mu.Unlock()

		out := t.refreshStatus(ctx)
		t.statusCache.mu.Lock()
		// A cancelled caller must not poison the shared cache. Other callers retry.
		if ctx.Err() == nil {
			t.statusCache.result, _ = json.Marshal(out)
			t.statusCache.expires = time.Now().Add(30 * time.Second)
		}
		t.statusCache.refresh = nil
		close(done)
		t.statusCache.mu.Unlock()
		return out
	}
}

// FreshStatus bypasses the display cache for the final batch queue guard.
func (t *Tdarr) FreshStatus(ctx context.Context) TdarrStatus {
	if !t.Configured() {
		return TdarrStatus{Status: "not_configured"}
	}
	return t.refreshStatus(ctx)
}

func (t *Tdarr) refreshStatus(ctx context.Context) TdarrStatus {
	out := TdarrStatus{Status: "unavailable"}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var server struct {
		Status  string  `json:"status"`
		Version *string `json:"version"`
	}
	var nodes map[string]*tdarrStatusNode
	counts := tdarrStatusCounts{queuedKnown: true, failedKnown: true}
	calls := []struct {
		path, body string
		dst        any
	}{
		{"/api/v2/status", "", &server},
		{"/api/v2/get-nodes", "", &nodes},
		{"/api/v2/cruddb", `{"data":{"collection":"FileJSONDB","mode":"getAll"}}`, &counts},
	}
	for _, c := range calls {
		if err := t.statusRead(ctx, c.path, c.body, c.dst); err != nil {
			out.Error = err.Error()
			return out
		}
	}
	if server.Status != "good" || nodes == nil {
		out.Error = "tdarr: incomplete status response"
		return out
	}
	out.ServerVersion = server.Version
	out.Nodes = make([]TdarrNodeStatus, 0, len(nodes))
	processing, processingKnown := 0, true
	for _, n := range nodes {
		if n == nil {
			out.Error = "tdarr: incomplete node response"
			out.ServerVersion = nil
			out.Nodes = nil
			return out
		}
		row := TdarrNodeStatus{Name: n.Name, Online: true, Paused: n.Paused,
			Workers: TdarrWorkerLimits{n.Limits.TranscodeGPU, n.Limits.TranscodeCPU, n.Limits.HealthcheckGPU, n.Limits.HealthcheckCPU}}
		if n.Workers != nil {
			row.Active = make([]TdarrActiveStatus, 0)
			for _, w := range n.Workers {
				if w.Idle == nil {
					processingKnown = false
					row.Active = nil
					break
				}
				if !*w.Idle {
					processing++
					row.Active = append(row.Active, TdarrActiveStatus{})
				}
			}
		} else {
			processingKnown = false
		}
		out.Nodes = append(out.Nodes, row)
	}
	sort.Slice(out.Nodes, func(i, j int) bool {
		a, b := "", ""
		if out.Nodes[i].Name != nil {
			a = *out.Nodes[i].Name
		}
		if out.Nodes[j].Name != nil {
			b = *out.Nodes[j].Name
		}
		return a < b
	})
	q := &TdarrQueueStatus{}
	// Busy workers lack a verified file identity in our live fixture, so we
	// cannot subtract their still-Queued DB records reliably.
	if counts.queuedKnown && processingKnown && processing == 0 {
		q.Queued = &counts.queued
	}
	if counts.failedKnown {
		q.Error = &counts.failed
	}
	if processingKnown {
		q.Processing = &processing
	}
	out.Queue = q
	out.Status = "ok"
	return out
}

func (t *Tdarr) statusRead(ctx context.Context, path, body string, out any) error {
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, t.base+path, strings.NewReader(body))
	if err != nil {
		return errors.New("tdarr: request failed")
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return errors.New("tdarr: upstream unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("tdarr: upstream refused status read")
	}
	// FileJSONDB includes probe metadata: allow 64 MiB, but never an unbounded read.
	limited := &io.LimitedReader{R: resp.Body, N: (64 << 20) + 1}
	dec := json.NewDecoder(limited)
	if counts, ok := out.(*tdarrStatusCounts); ok {
		err = counts.decode(dec)
	} else {
		err = dec.Decode(out)
	}
	if limited.N == 0 {
		return errors.New("tdarr: status response too large")
	}
	if err != nil {
		return errors.New("tdarr: invalid status response")
	}
	_, err = dec.Token()
	if limited.N == 0 {
		return errors.New("tdarr: status response too large")
	}
	if err != io.EOF {
		return errors.New("tdarr: invalid status response")
	}
	return nil
}

func knownTdarrDecision(s string, health bool) bool {
	if s == "Queued" || s == "Not required" || s == "Hold" {
		return true
	}
	if health {
		return s == "Success" || s == "Error" || s == "Cancelled"
	}
	return s == "Transcode success" || s == "Transcode error" || s == "Transcode cancelled"
}

// Only queue flags survive each file; probe metadata is consumed token-wise.
type tdarrStatusCounts struct {
	queued, failed           int
	queuedKnown, failedKnown bool
}

func (c *tdarrStatusCounts) add(f tdarrStatusFile) {
	// Count files once across the transcode and standalone healthcheck queues.
	// These are DB flags, including parked/held libraries; not runnable jobs.
	if f.Transcode == nil || f.Healthcheck == nil {
		c.queuedKnown = false
		c.failedKnown = false
		return
	}
	trans, health := *f.Transcode, *f.Healthcheck
	if !knownTdarrDecision(trans, false) || !knownTdarrDecision(health, true) {
		c.queuedKnown, c.failedKnown = false, false
		return
	}
	if trans == "Queued" || (health == "Queued" && f.Medium != nil && *f.Medium != "audio") {
		c.queued++
	}
	if health == "Queued" && f.Medium == nil && trans != "Queued" {
		c.queuedKnown = false
	}
	if trans == "Transcode error" || trans == "Transcode cancelled" || health == "Error" || health == "Cancelled" {
		c.failed++
	}
}

func (c *tdarrStatusCounts) decode(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil || token != json.Delim('[') {
		return errors.New("expected file array")
	}
	for dec.More() {
		token, err = dec.Token()
		if err != nil || token != json.Delim('{') {
			return errors.New("expected file object")
		}
		var f tdarrStatusFile
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				return err
			}
			var dst **string
			switch key {
			case "TranscodeDecisionMaker":
				dst = &f.Transcode
			case "HealthCheck":
				dst = &f.Healthcheck
			case "fileMedium":
				dst = &f.Medium
			}
			if dst != nil {
				if err := dec.Decode(dst); err != nil {
					return err
				}
			} else if err := skipTdarrValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		c.add(f)
	}
	_, err = dec.Token()
	return err
}

// Skip nested values without retaining maps, arrays or raw file records.
func skipTdarrValue(dec *json.Decoder) error {
	depth := 0
	for {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
