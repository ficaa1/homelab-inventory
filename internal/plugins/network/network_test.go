package network

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func TestNetworkViewsOptionalExportersAndProbeAlert(t *testing.T) {
	empty := false
	redirect := false
	calls := 0
	const secret = "fixture-prometheus-token"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("unguarded request")
		}
		if r.URL.Path == "/metrics/api/v1/status/buildinfo" {
			fmt.Fprint(w, `{"status":"success","data":{"version":"3.0.0"}}`)
			return
		}
		if r.URL.Path != "/metrics/api/v1/query" && r.URL.Path != "/metrics/api/v1/query_range" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		if redirect {
			http.Redirect(w, r, "/forbidden", 302)
			return
		}
		query := r.URL.Query().Get("query")
		ranged := strings.HasSuffix(r.URL.Path, "query_range")
		samples := []map[string]any{}
		appendSample := func(labels map[string]string, value string) {
			sample := map[string]any{"metric": labels, "value": []any{1761743100, value}}
			if ranged {
				delete(sample, "value")
				sample["values"] = [][]any{{1761742800, value}, {1761743100, "NaN"}}
			}
			samples = append(samples, sample)
		}
		if !empty {
			switch {
			case query == downQuery:
				appendSample(map[string]string{"instance": "https://target.example.org", "job": "http"}, "0")
			case query == "probe_success":
				appendSample(map[string]string{"instance": "https://target.example.org", "job": "http"}, "0")
				appendSample(map[string]string{"instance": "https://other.example.org", "job": "http"}, "1")
			case query == "probe_duration_seconds":
				appendSample(map[string]string{"instance": "https://target.example.org", "job": "http"}, "0.025")
			case query == containerQuery:
				appendSample(map[string]string{"name": "worker"}, "512")
			case strings.Contains(query, "node_network_"):
				if !strings.Contains(query, `device!~"^(`) || strings.Contains(query, "gluetun") {
					t.Error("unexpected query")
				}
				value := "1"
				if strings.Contains(query, "receive_bytes") {
					value = "1024"
				}
				if strings.Contains(query, "transmit_bytes") {
					value = "2048"
				}
				appendSample(map[string]string{"instance": "host-a.example.org:9100", "nodename": "host-a"}, value)
			}
		}
		if ranged {
			step, _ := strconv.Atoi(r.URL.Query().Get("step"))
			start, _ := strconv.ParseInt(r.URL.Query().Get("start"), 10, 64)
			end, _ := strconv.ParseInt(r.URL.Query().Get("end"), 10, 64)
			if step <= 0 || int64(math.Floor(float64(end-start)/float64(step)))+1 > 300 {
				t.Error("range too dense")
			}
		}
		resultType := "vector"
		if ranged {
			resultType = "matrix"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{"resultType": resultType, "result": samples}})
	}))
	defer fake.Close()
	var n *Network
	registry, err := plugin.Open(t.TempDir(), []plugin.Builtin{{ID: "network", New: func(h plugin.Host) plugin.Plugin { n = New(h).(*Network); return n }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	on := true
	if _, err = registry.Update("network", plugin.Update{Enabled: &on, Settings: plugin.Config{"prometheus_url": fake.URL + "/metrics", "prometheus_token": secret}}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	raw, err := registry.View(ctx, "network", "overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Fleet traffic", "Hosts", "Probes", "Busiest containers", "1 / 2", "host-a", "bytes_s"} {
		if !strings.Contains(string(raw), text) {
			t.Fatalf("view missing %s", text)
		}
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "null") {
		t.Fatal("secret or invalid gap")
	}
	before := calls
	if _, err = registry.View(ctx, "network", "overview", nil); err != nil || calls != before {
		t.Fatal("30 second cache not used")
	}
	raw, err = registry.View(ctx, "network", "traffic", nil)
	if err != nil || !strings.Contains(string(raw), `"title":"host-a"`) {
		t.Fatal("traffic chart missing")
	}
	alerts, err := n.Alerts(ctx)
	if err != nil || len(alerts) != 1 || alerts[0].Rule != "probe.down" || alerts[0].Subject != "https://target.example.org" || alerts[0].Severity != "warning" {
		t.Fatal(alerts, err)
	}
	result := n.Test(ctx)
	if !result.OK || len(result.Checks) != 4 {
		t.Fatal(result)
	}
	empty = true
	n.cache = map[string]cached{}
	n.cacheBytes = 0
	raw, err = registry.View(ctx, "network", "overview", nil)
	if err != nil {
		t.Fatal(err)
	}
	var view plugin.View
	if json.Unmarshal(raw, &view) != nil || len(view.Blocks) != 3 {
		t.Fatal("expected three optional-source notices")
	}
	for _, b := range view.Blocks {
		if b.Type != "notice" {
			t.Fatal("missing source was not a notice")
		}
	}
	result = n.Test(ctx)
	if !result.OK {
		t.Fatal("missing exporters failed test")
	}
	for _, c := range result.Checks[1:] {
		if c.Message != "not found — optional" {
			t.Fatal(c)
		}
	}
	redirect = true
	n.cache = map[string]cached{}
	n.cacheBytes = 0
	raw, err = registry.View(ctx, "network", "overview", nil)
	if err != nil || !strings.Contains(string(raw), "unavailable") {
		t.Fatal("failed sources did not degrade to notices")
	}
	if _, err = n.Alerts(ctx); err == nil {
		t.Fatal("failed alerts query silently resolved alerts")
	}

}
func TestExcludeRegexRejectsQueryInjection(t *testing.T) {
	n := &Network{}
	for _, exclude := range []string{`lo"} or up #`, `lo\"}`, "lo\nup", "lo`up", "["} {
		if err := n.Configure(plugin.Config{"prometheus_url": "http://example.org", "exclude_interfaces": exclude}); err == nil {
			t.Fatal("unsafe regular expression accepted")
		}
	}
	if err := n.Configure(plugin.Config{"prometheus_url": "http://example.org", "exclude_interfaces": `lo|eth\.[0-9]+`}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(n.deviceMatcher(), `eth\\.`) {
		t.Fatal("PromQL backslash was not escaped")
	}
}
