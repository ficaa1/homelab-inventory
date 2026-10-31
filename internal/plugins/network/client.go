package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type cached struct {
	data []byte
	err  error
	at   time.Time
}
type sample struct {
	Metric  map[string]string
	Current plugin.Point
	Points  []plugin.Point
}
type wireSample struct {
	Metric map[string]string   `json:"metric"`
	Value  []json.RawMessage   `json:"value"`
	Values [][]json.RawMessage `json:"values"`
}

func (n *Network) get(ctx context.Context, path string, q url.Values, out any, useCache bool) error {
	if path != "/api/v1/query" && path != "/api/v1/query_range" && path != "/api/v1/status/buildinfo" {
		return errors.New("Prometheus endpoint is not allowed")
	}
	cacheQuery := url.Values{}
	for k, values := range q {
		if k != "start" && k != "end" {
			cacheQuery[k] = values
		}
	}
	key := path + "?" + cacheQuery.Encode()
	n.cacheMu.Lock()
	c, found := n.cache[key]
	n.cacheMu.Unlock()
	if !useCache || !found || n.host.Now().Sub(c.at) >= 30*time.Second {
		c = cached{at: n.host.Now()}
		target, err := url.Parse(n.base + path)
		if err != nil {
			return errors.New("Prometheus request failed")
		}
		target.RawQuery = q.Encode()
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target.String(), nil)
		if err != nil {
			cancel()
			return errors.New("Prometheus request failed")
		}
		if n.token != "" {
			req.Header.Set("Authorization", "Bearer "+n.token)
		}
		resp, err := plugin.WithoutRedirects(n.host.HTTP()).Do(req)
		if err != nil {
			c.err = errors.New("Prometheus request failed")
		} else {
			if resp.StatusCode != 200 {
				c.err = fmt.Errorf("Prometheus returned HTTP %d", resp.StatusCode)
			} else {
				c.data, err = io.ReadAll(io.LimitReader(resp.Body, plugin.MaxHTTPBody+1))
				if err != nil || len(c.data) > plugin.MaxHTTPBody {
					c.data = nil
					c.err = errors.New("Prometheus response could not be read")
				}
			}
			resp.Body.Close()
		}
		cancel()
		if useCache && ctx.Err() == nil {
			n.cacheMu.Lock()
			if n.cache == nil || len(n.cache) >= 128 || n.cacheBytes+len(c.data) > 16<<20 {
				n.cache = map[string]cached{}
				n.cacheBytes = 0
			}
			n.cacheBytes -= len(n.cache[key].data)
			n.cache[key] = c
			n.cacheBytes += len(c.data)
			n.cacheMu.Unlock()
		}
	}
	if c.err != nil {
		return c.err
	}
	if json.Unmarshal(c.data, out) != nil {
		return errors.New("Prometheus returned invalid JSON")
	}
	return nil
}
func (n *Network) query(ctx context.Context, expression string, ranged, useCache bool) ([]sample, error) {
	// Cache keys intentionally omit wall time; a hit reuses the exact sample
	// window for 30 seconds instead of creating a new entry on every request.
	path := "/api/v1/query"
	q := url.Values{"query": {expression}}
	if ranged {
		path = "/api/v1/query_range"
		now := n.host.Now().UTC()
		duration := n.windowDuration()
		// 299 intervals yield at most 300 points, including both endpoints.
		step := int64(math.Ceil(duration.Seconds() / 299))
		q.Set("step", strconv.FormatInt(step, 10))
		// Align windows to the cache lifetime so overview and traffic share them.
		aligned := now.Truncate(30 * time.Second)
		q.Set("start", strconv.FormatInt(aligned.Add(-duration).Unix(), 10))
		q.Set("end", strconv.FormatInt(aligned.Unix(), 10))
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string       `json:"resultType"`
			Result     []wireSample `json:"result"`
		} `json:"data"`
	}
	if err := n.get(ctx, path, q, &response, useCache); err != nil {
		return nil, err
	}
	expected := "vector"
	if ranged {
		expected = "matrix"
	}
	if response.Status != "success" || response.Data.ResultType != expected {
		return nil, errors.New("Prometheus returned an invalid query result")
	}
	if len(response.Data.Result) > 500 {
		return nil, errors.New("Prometheus result exceeds 500 series")
	}
	out := make([]sample, 0, len(response.Data.Result))
	for _, wire := range response.Data.Result {
		s := sample{Metric: wire.Metric}
		if ranged {
			if len(wire.Values) > 300 {
				return nil, errors.New("Prometheus range exceeds 300 points")
			}
			previous := math.Inf(-1)
			for _, pair := range wire.Values {
				point, err := decodePoint(pair)
				if err != nil || *point[0] <= previous {
					return nil, errors.New("Prometheus returned invalid range points")
				}
				previous = *point[0]
				s.Points = append(s.Points, point)
			}
		} else {
			point, err := decodePoint(wire.Value)
			if err != nil {
				return nil, err
			}
			s.Current = point
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Metric["instance"]+"\x00"+out[i].Metric["job"]+"\x00"+out[i].Metric["name"] < out[j].Metric["instance"]+"\x00"+out[j].Metric["job"]+"\x00"+out[j].Metric["name"]
	})
	return out, nil
}
func decodePoint(raw []json.RawMessage) (plugin.Point, error) {
	if len(raw) != 2 {
		return plugin.Point{}, errors.New("Prometheus returned an invalid sample")
	}
	var timestamp float64
	var value string
	if json.Unmarshal(raw[0], &timestamp) != nil || math.IsNaN(timestamp) || math.IsInf(timestamp, 0) || json.Unmarshal(raw[1], &value) != nil {
		return plugin.Point{}, errors.New("Prometheus returned an invalid sample")
	}
	point := plugin.Point{&timestamp, nil}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return plugin.Point{}, errors.New("Prometheus returned an invalid sample value")
	}
	if !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
		point[1] = &parsed
	}
	return point, nil
}
