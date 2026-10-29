// Package network renders optional exporter metrics using fixed PromQL only.
package network

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

const defaultExclude = `lo|veth.*|docker.*|br-.*|virbr.*|cali.*|flannel.*|cni.*|tap.*|fwbr.*|fwpr.*|fwln.*|tailscale.*|wg.*`
const containerQuery = `topk(10, sum by (name) (rate(container_network_receive_bytes_total{name!=""}[5m]) + rate(container_network_transmit_bytes_total{name!=""}[5m])))`
const downQuery = `max_over_time(probe_success[10m]) == 0`

type Network struct {
	host                         plugin.Host
	base, token, exclude, window string
	cache                        map[string]cached
	cacheBytes                   int
}

var _ plugin.AlertSource = (*Network)(nil)

func New(h plugin.Host) plugin.Plugin { return &Network{host: h} }
func (*Network) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "network", Name: "Network", Description: "Read network traffic and probe health from an existing Prometheus.", Version: "1", Tab: &plugin.Tab{Title: "Network", Icon: "network", Views: []plugin.ViewRef{{Name: "overview", Title: "Overview"}, {Name: "traffic", Title: "Traffic"}}}, Settings: []plugin.Field{
		{Key: "prometheus_url", Label: "Prometheus URL", Type: plugin.URL, Required: true},
		{Key: "prometheus_token", Label: "Bearer token", Type: plugin.Secret},
		{Key: "exclude_interfaces", Label: "Exclude interfaces", Type: plugin.Text, Default: defaultExclude, Help: "Regular expression matched against the whole device name."},
		{Key: "window", Label: "History window", Type: plugin.Select, Default: "6h", Options: []plugin.Option{{Value: "1h", Label: "1 hour"}, {Value: "6h", Label: "6 hours"}, {Value: "24h", Label: "24 hours"}}},
	}}
}
func (n *Network) Configure(c plugin.Config) error {
	base, _ := c["prometheus_url"].(string)
	parsed, err := url.Parse(base)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" {
		return &plugin.FieldError{Key: "prometheus_url", Message: "Use an http(s) base URL without credentials, query or fragment"}
	}
	exclude := defaultExclude
	if value, ok := c["exclude_interfaces"].(string); ok {
		exclude = value
	}
	if len(exclude) > 2048 || strings.ContainsAny(exclude, "\"\r\n`") {
		return &plugin.FieldError{Key: "exclude_interfaces", Message: "Exclude expression must not contain quotes, line breaks or backticks"}
	}
	if _, err := regexp.Compile("^(" + exclude + ")$"); err != nil {
		return &plugin.FieldError{Key: "exclude_interfaces", Message: "Use a valid regular expression"}
	}
	token, _ := c["prometheus_token"].(string)
	if strings.ContainsAny(token, "\r\n") {
		return &plugin.FieldError{Key: "prometheus_token", Message: "Token must not contain line breaks"}
	}
	window := "6h"
	if value, ok := c["window"].(string); ok {
		window = value
	}
	if window != "1h" && window != "6h" && window != "24h" {
		return &plugin.FieldError{Key: "window", Message: "Choose 1h, 6h or 24h"}
	}
	base = strings.TrimRight(base, "/")
	if n.base != base || n.token != token || n.exclude != exclude || n.window != window {
		n.cache = map[string]cached{}
		n.cacheBytes = 0
	}
	n.base, n.token, n.exclude, n.window = base, token, exclude, window
	return nil
}
func (n *Network) Close() error {
	n.base, n.token = "", ""
	n.cache = nil
	n.cacheBytes = 0
	return nil
}
func (n *Network) deviceMatcher() string {
	// PromQL string literals require backslashes escaped once more. Quotes and
	// newlines were rejected before any interpolation; users cannot add PromQL.
	return `device!~"^(` + strings.ReplaceAll(n.exclude, `\`, `\\`) + `)$"`
}
func (n *Network) hostQuery(kind string) string {
	matcher := n.deviceMatcher()
	switch kind {
	case "rx", "tx":
		direction := "receive"
		if kind == "tx" {
			direction = "transmit"
		}
		rate := `sum by (instance) (rate(node_network_` + direction + `_bytes_total{` + matcher + `}[5m]))`
		// Preserve instances without uname metrics while adding nodename where present.
		return `(` + rate + ` * on(instance) group_left(nodename) node_uname_info) or on(instance) ` + rate
	case "errors", "drops":
		metric := "errs"
		if kind == "drops" {
			metric = "drop"
		}
		return `sum by (instance) (rate(node_network_receive_` + metric + `_total{` + matcher + `}[5m]) + rate(node_network_transmit_` + metric + `_total{` + matcher + `}[5m]))`
	}
	panic("unknown server-owned query")
}
func (n *Network) Test(ctx context.Context) plugin.TestResult {
	result := plugin.TestResult{OK: true, Checks: []plugin.Check{}}
	var build struct {
		Status string `json:"status"`
		Data   struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	err := n.get(ctx, "/api/v1/status/buildinfo", nil, &build, false)
	check := plugin.Check{Name: "Prometheus", OK: err == nil && build.Status == "success" && build.Data.Version != "", Message: "Build information unavailable"}
	if check.OK {
		check.Message = "Version " + n.text(build.Data.Version)
	}
	result.Checks = append(result.Checks, check)
	result.OK = check.OK
	for _, source := range [][2]string{{"node_exporter", n.hostQuery("rx")}, {"blackbox_exporter", "probe_success"}, {"cAdvisor", containerQuery}} {
		samples, err := n.query(ctx, source[1], false, false)
		check := plugin.Check{Name: source[0], OK: err == nil, Message: "not found — optional"}
		if err != nil {
			check.Message = "Prometheus query failed"
		} else if len(samples) > 0 {
			check.Message = "Data present"
		}
		result.Checks = append(result.Checks, check)
		result.OK = result.OK && check.OK
	}
	if result.OK {
		result.Message = "Prometheus responded; exporters are optional"
	} else {
		result.Message = "Prometheus could not be fully checked"
	}
	return result
}
func (n *Network) Alerts(ctx context.Context) ([]plugin.Alert, error) {
	samples, err := n.query(ctx, downQuery, false, true)
	if err != nil {
		return nil, err
	}
	alerts := []plugin.Alert{}
	seen := map[string]bool{}
	for _, sample := range samples {
		subject := n.text(sample.Metric["instance"])
		if sample.Current[1] != nil && *sample.Current[1] == 0 && subject != "" && !seen[subject] {
			alerts = append(alerts, plugin.Alert{Rule: "probe.down", Subject: subject, Severity: "warning", Summary: "Probe has been down for 10 minutes", Detail: "Every probe_success sample in the last 10 minutes was zero."})
			seen[subject] = true
		}
	}
	return alerts, nil
}
func (n *Network) text(s string) string {
	if n.token != "" {
		s = strings.ReplaceAll(s, n.token, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < ' ' {
			return ' '
		}
		return r
	}, s)
	runes := []rune(s)
	if len(runes) > 300 {
		s = string(runes[:300])
	}
	return strings.TrimSpace(s)
}
func (n *Network) windowDuration() time.Duration { d, _ := time.ParseDuration(n.window); return d }
func (n *Network) View(ctx context.Context, name string, _ url.Values) (plugin.View, error) {
	if name != "overview" && name != "traffic" {
		return plugin.View{}, plugin.ErrNotFound
	}
	if n.base == "" {
		return plugin.View{}, errors.New("Prometheus is not configured")
	}
	view := plugin.View{Title: "Network", UpdatedAt: n.host.Now().UTC(), RefreshSeconds: 30, Blocks: []plugin.Block{}}
	// Leave time for the core to validate the notice-only response even when
	// every upstream request is unavailable.
	viewCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	n.buildView(viewCtx, name, &view)
	return view, nil
}
func notice(source string, err error) plugin.Block {
	message := source + " data not found — optional"
	tone := "info"
	if err != nil {
		message = fmt.Sprintf("%s data is unavailable; check Prometheus and the exporter.", source)
		tone = "warn"
	}
	return plugin.Block{Type: "notice", Tone: tone, Text: message}
}
