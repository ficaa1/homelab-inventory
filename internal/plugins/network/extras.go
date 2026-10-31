package network

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network/speed"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func speedAction() plugin.ActionSpec {
	return plugin.ActionSpec{ID: "speedtest-run", Label: "Run speed test (uses network bandwidth)", Operator: true}
}
func (n *Network) configureExtra(c plugin.Config) error {
	for _, key := range []string{"routes_url", "qbit_url", "speedtest_url", "speed_download_url", "speed_fallback_url"} {
		v, _ := c[key].(string)
		if v == "" {
			continue
		}
		u, e := url.Parse(v)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return &plugin.FieldError{Key: key, Message: "Use a base URL without credentials or query"}
		}
	}
	if v, ok := c["seed_hours"].(float64); ok && (v < 0 || v > 100000) {
		return &plugin.FieldError{Key: "seed_hours", Message: "Use hours between 0 and 100000"}
	}
	if n.speed != nil {
		n.speed.Close()
		n.speed = nil
	}
	n.extra = c
	return nil
}
func (n *Network) extraText(key string) string {
	s, _ := n.extra[key].(string)
	if strings.HasSuffix(key, "_url") {
		return strings.TrimRight(s, "/")
	}
	return s
}
func (n *Network) fixed(ctx context.Context, client *http.Client, base, path, method string, body io.Reader) (*http.Response, error) {
	r, e := http.NewRequestWithContext(ctx, method, base+path, body)
	if e != nil {
		return nil, errors.New("invalid service request")
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("Referer", base+"/")
	resp, e := plugin.WithoutRedirects(client).Do(r)
	if e != nil {
		return nil, errors.New("network service unavailable")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("network service refused request (HTTP %d)", resp.StatusCode)
	}
	return resp, nil
}
func (n *Network) extraJSON(ctx context.Context, client *http.Client, base, path string, out any) error {
	resp, e := n.fixed(ctx, client, base, path, "GET", nil)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if json.NewDecoder(resp.Body).Decode(out) != nil {
		return errors.New("invalid network response")
	}
	return nil
}
func (n *Network) extraView(ctx context.Context, name string) (plugin.View, error) {
	v := plugin.View{Title: "Network", UpdatedAt: n.host.Now(), RefreshSeconds: 30, Blocks: []plugin.Block{}}
	switch name {
	case "routes":
		base := n.extraText("routes_url")
		if base == "" {
			return v, errors.New("route provider is not configured")
		}
		var out struct {
			HTTP *struct {
				Routers  map[string]struct{ Rule, Service string }
				Services map[string]struct {
					LoadBalancer *struct{ Servers []struct{ URL string } }
				}
			} `json:"http"`
		}
		if e := n.extraJSON(ctx, n.host.HTTP(), base, "/api/v1/traefik-config", &out); e != nil {
			return v, e
		}
		if out.HTTP == nil || out.HTTP.Routers == nil || out.HTTP.Services == nil {
			return v, errors.New("route provider response is incomplete")
		}
		rows := []map[string]any{}
		seen := map[string]bool{}
		hostRule := regexp.MustCompile("^Host\\(`([a-zA-Z0-9.-]+)`\\)$")
		for _, r := range out.HTTP.Routers {
			m := hostRule.FindStringSubmatch(r.Rule)
			if m == nil {
				continue
			}
			service := out.HTTP.Services[r.Service]
			if service.LoadBalancer == nil {
				continue
			}
			for _, target := range service.LoadBalancer.Servers {
				u, e := url.Parse(target.URL)
				if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
					continue
				}
				key := m[1] + target.URL
				if seen[key] {
					continue
				}
				seen[key] = true
				rows = append(rows, map[string]any{"host": n.text(m[1]), "target": n.text(target.URL), "sso": "Unknown"})
			}
		}
		sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]["host"]) < fmt.Sprint(rows[j]["host"]) })
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Routes", Columns: []plugin.Column{{Key: "host", Label: "Hostname"}, {Key: "target", Label: "Target"}, {Key: "sso", Label: "SSO"}}, Rows: rows})
	case "seeding":
		base := n.extraText("qbit_url")
		if base == "" {
			return v, errors.New("qBittorrent is not configured")
		}
		jar, _ := cookiejar.New(nil)
		client := plugin.WithCookieJar(n.host.HTTP(), jar)
		values := url.Values{"username": {n.extraText("qbit_user")}, "password": {n.extraText("qbit_password")}}
		resp, e := n.fixed(ctx, client, base, "/api/v2/auth/login", "POST", strings.NewReader(values.Encode()))
		if e != nil {
			return v, e
		}
		b, e := io.ReadAll(resp.Body)
		resp.Body.Close()
		if e != nil || (resp.StatusCode != 204 && strings.TrimSpace(string(b)) != "Ok.") {
			return v, errors.New("qBittorrent login refused")
		}
		var health struct {
			Download   int64  `json:"dl_info_speed"`
			Upload     int64  `json:"up_info_speed"`
			Connection string `json:"connection_status"`
			DHT        int64  `json:"dht_nodes"`
		}
		if e = n.extraJSON(ctx, client, base, "/api/v2/transfer/info", &health); e != nil {
			return v, e
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "stats", Items: []plugin.Item{{Label: "Download bytes/s", Value: fmt.Sprint(health.Download)}, {Label: "Upload bytes/s", Value: fmt.Sprint(health.Upload)}, {Label: "Connection", Value: n.text(health.Connection)}, {Label: "DHT nodes", Value: fmt.Sprint(health.DHT)}}})
		var torrents []struct {
			Name    string  `json:"name"`
			Size    int64   `json:"size"`
			Seconds int64   `json:"seeding_time"`
			Ratio   float64 `json:"ratio"`
			Speed   int64   `json:"upspeed"`
			State   string  `json:"state"`
		}
		if e = n.extraJSON(ctx, client, base, "/api/v2/torrents/info?filter=seeding", &torrents); e != nil {
			return v, e
		}
		hours, _ := n.extra["seed_hours"].(float64)
		rows := []map[string]any{}
		for _, t := range torrents {
			if t.State != "uploading" && t.State != "stalledUP" && t.State != "forcedUP" && t.State != "queuedUP" {
				continue
			}
			remaining := hours*3600 - float64(t.Seconds)
			if remaining < 0 {
				remaining = 0
			}
			rows = append(rows, map[string]any{"name": n.text(t.Name), "size": t.Size, "seconds": t.Seconds, "remaining": remaining, "ratio": t.Ratio, "speed": t.Speed})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Sharing torrents", Columns: []plugin.Column{{Key: "name", Label: "Name"}, {Key: "size", Label: "Size", Format: "bytes"}, {Key: "seconds", Label: "Seed time", Format: "duration_s"}, {Key: "remaining", Label: "Requirement remaining", Format: "duration_s"}, {Key: "ratio", Label: "Ratio", Format: "number"}, {Key: "speed", Label: "Upload", Format: "bytes_s"}}, Rows: rows})
	case "speedtest":
		if e := n.ensureSpeed(); e != nil {
			return v, e
		}
		snap := n.speed.Snapshot()
		v.Actions = []plugin.ActionSpec{speedAction()}
		v.RefreshSeconds = 2
		items := []plugin.Item{{Label: "Running", Value: fmt.Sprint(snap.Running)}}
		if snap.Current != nil {
			items = append(items, plugin.Item{Label: n.text(snap.Current.Phase), Value: fmt.Sprintf("%.2f Mbps", snap.Current.Mbps)})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "stats", Items: items})
		rows := []map[string]any{}
		for i := 0; i < len(snap.History); i++ {
			r := snap.History[i]
			scalar := func(f *float64) any {
				if f == nil {
					return nil
				}
				return *f
			}
			rows = append(rows, map[string]any{"time": r.Time.Format(time.RFC3339), "down": scalar(r.DownMbps), "up": scalar(r.UpMbps), "latency": scalar(r.LatencyMs), "jitter": scalar(r.JitterMs), "error": n.text(r.Error)})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Speed history", Empty: "No measurements yet", Columns: []plugin.Column{{Key: "time", Label: "Time", Format: "time"}, {Key: "down", Label: "Down Mbps", Format: "number"}, {Key: "up", Label: "Up Mbps", Format: "number"}, {Key: "latency", Label: "Latency ms", Format: "number"}, {Key: "jitter", Label: "Jitter ms", Format: "number"}, {Key: "error", Label: "Status"}}, Rows: rows})

	}
	return v, nil
}

func (n *Network) ensureSpeed() error {
	if n.speed != nil {
		return nil
	}
	for _, key := range []string{"speedtest_url", "speed_download_url", "speed_fallback_url"} {
		if n.extraText(key) == "" {
			return errors.New("configure upload/latency and two range-download endpoints before measuring")
		}
	}
	size := func(key string) (int64, error) {
		v, ok := n.extra[key].(float64)
		if !ok || v < 128<<20 || v > 1e13 || v != float64(int64(v)) {
			return 0, errors.New("configure valid range-download file sizes of at least 128 MiB")
		}
		return int64(v), nil
	}
	a, e := size("speed_download_size")
	if e != nil {
		return e
	}
	b, e := size("speed_fallback_size")
	if e != nil {
		return e
	}
	n.speed, e = speed.NewSpeedtester(n.extraText("speedtest_url"), n.extraText("speed_download_url"), n.extraText("speed_fallback_url"), a, b, filepath.Join(n.host.CacheDir(), "speedtest-history.jsonl"), plugin.WithoutRedirects(plugin.StreamingHTTP(n.host.HTTP())))
	return e
}
func (n *Network) Action(_ context.Context, id string, _ plugin.Config) (plugin.View, error) {
	if id != "speedtest-run" {
		return plugin.View{}, plugin.ErrNotFound
	}
	if e := n.ensureSpeed(); e != nil {
		return plugin.View{}, e
	}
	if _, e := n.speed.Start(n.host.Now()); e != nil {
		return plugin.View{}, e
	}
	return n.extraView(context.Background(), "speedtest")
}
