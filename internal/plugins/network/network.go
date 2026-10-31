// Package network renders optional exporter metrics using fixed PromQL only.
package network

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network/plexload"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network/speed"
)

const defaultExclude = `lo|veth.*|docker.*|br-.*|virbr.*|cali.*|flannel.*|cni.*|tap.*|fwbr.*|fwpr.*|fwln.*|tailscale.*|wg.*`
const containerQuery = `topk(10, sum by (name) (rate(container_network_receive_bytes_total{name!=""}[5m]) + rate(container_network_transmit_bytes_total{name!=""}[5m])))`
const downQuery = `max_over_time(probe_success[10m]) == 0`

// Names interpolated into server-owned PromQL. Neither can close a string or
// a selector.
var (
	metricName    = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)
	containerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
)

type Network struct {
	load                         *plexload.Client
	inventory                    func() ([]plugin.NetworkHost, error)
	dnsRecords                   func(context.Context) ([]plugin.DNSRecord, error)
	speed                        *speed.Speedtester
	extra                        plugin.Config
	host                         plugin.Host
	base, token, exclude, window string
	cache                        map[string]cached
	cacheBytes                   int
	cacheMu                      sync.Mutex // the page's metrics report queries in parallel
	// Optional exporter names for the page's WAN, quality and VPN panels.
	vpnContainer, diagPrefix, wanDevice string
	answers                             map[string]answer
}

func (n *Network) SetDNSRecordsSource(source func(context.Context) ([]plugin.DNSRecord, error)) {
	n.dnsRecords = source
}

var _ plugin.AlertSource = (*Network)(nil)

func New(h plugin.Host) plugin.Plugin { return &Network{host: h} }
func (*Network) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "network", Name: "Network", Description: "Read optional network metrics, routes and seeding health, and run bounded speed measurements.", Version: "1", Actions: []plugin.ActionSpec{speedAction()}, Tab: &plugin.Tab{Title: "Network", Icon: "globe", Views: []plugin.ViewRef{{Name: "overview", Title: "Overview"}, {Name: "vpn", Title: "VPN"}, {Name: "traffic", Title: "Traffic"}, {Name: "routes", Title: "Routes"}, {Name: "seeding", Title: "Seeding"}, {Name: "speedtest", Title: "Speed test"}}}, Settings: []plugin.Field{
		{Key: "routes_url", Label: "HTTP route-provider URL", Type: plugin.URL},
		{Key: "qbit_url", Label: "qBittorrent URL", Type: plugin.URL},
		{Key: "qbit_user", Label: "qBittorrent username", Type: plugin.Text},
		{Key: "qbit_password", Label: "qBittorrent password", Type: plugin.Secret},
		{Key: "seed_hours", Label: "Required seeding hours", Type: plugin.Number, Default: 0, Help: "Zero means no configured tracker requirement."},
		{Key: "speedtest_url", Label: "Upload and latency service URL", Type: plugin.URL, Help: "Cloudflare speed-test protocol (__down and __up)."},
		{Key: "speed_download_url", Label: "Primary range-download URL", Type: plugin.URL},
		{Key: "speed_fallback_url", Label: "Fallback range-download URL", Type: plugin.URL},
		{Key: "speed_download_size", Label: "Primary file bytes", Type: plugin.Number},
		{Key: "speed_fallback_size", Label: "Fallback file bytes", Type: plugin.Number},
		{Key: "prometheus_url", Label: "Prometheus URL", Type: plugin.URL},
		{Key: "prometheus_token", Label: "Bearer token", Type: plugin.Secret},
		{Key: "load_job", Label: "Load exporter job", Type: plugin.Text, Default: "node_exporter", Group: "Overview load"},
		{Key: "load_instance", Label: "Load exporter instance", Type: plugin.Text, Help: "Exact Prometheus instance label. Empty disables the Overview load tile.", Group: "Overview load"},
		{Key: "load_label", Label: "Load tile label", Type: plugin.Text, Default: "Host load", Help: "Host name shown before load; a label ending in load is used as written.", Group: "Overview load"},
		{Key: "exclude_interfaces", Label: "Exclude interfaces", Type: plugin.Text, Default: defaultExclude, Help: "Regular expression matched against the whole device name."},
		{Key: "vpn_container", Label: "VPN container", Type: plugin.Text, Help: "cAdvisor container name whose traffic is the VPN tunnel. Empty hides VPN traffic."},
		{Key: "diag_prefix", Label: "Diagnostics metric prefix", Type: plugin.Text, Help: "Prefix of a network-diagnostics exporter: <prefix>_ping_rtt_seconds, _ping_loss_percent, _dns_resolve_seconds and _collection_success. Empty hides ping and DNS quality."},
		{Key: "wan_device", Label: "Router metric name", Type: plugin.Text, Help: "Router segment of the exporter's WAN gauges: <prefix>_<router>_wan_rate_down_bytes_per_second, _wan_bandwidth_down_bits_per_second, _wan_state and <prefix>_<router>_pon_rx_power_dbm. Empty hides the WAN card."},
		{Key: "window", Label: "History window", Type: plugin.Select, Default: "6h", Options: []plugin.Option{{Value: "1h", Label: "1 hour"}, {Value: "6h", Label: "6 hours"}, {Value: "24h", Label: "24 hours"}}},
	}}
}
func (n *Network) Configure(c plugin.Config) error {
	if err := n.configureExtra(c); err != nil {
		return err
	}
	base, _ := c["prometheus_url"].(string)
	parsed, err := url.Parse(base)
	if base != "" && (err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "") {
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
	names := map[string]string{}
	for _, key := range []string{"vpn_container", "diag_prefix", "wan_device"} {
		value, _ := c[key].(string)
		pattern := metricName
		if key == "vpn_container" {
			pattern = containerName
		}
		if value != "" && !pattern.MatchString(value) {
			return &plugin.FieldError{Key: key, Message: "Use a plain name without quotes, spaces or braces"}
		}
		names[key] = value
	}
	if names["wan_device"] != "" && names["diag_prefix"] == "" {
		return &plugin.FieldError{Key: "wan_device", Message: "Set the diagnostics metric prefix as well"}
	}
	n.vpnContainer, n.diagPrefix, n.wanDevice = names["vpn_container"], names["diag_prefix"], names["wan_device"]
	if err := n.configureLoad(c, base, token); err != nil {
		return err
	}
	n.answers = nil
	base = strings.TrimRight(base, "/")
	if n.base != base || n.token != token || n.exclude != exclude || n.window != window {
		n.cache = map[string]cached{}
		n.cacheBytes = 0
	}
	n.base, n.token, n.exclude, n.window = base, token, exclude, window
	return nil
}
func (n *Network) Close() error {
	n.load = nil
	if n.speed != nil {
		n.speed.Close()
		n.speed = nil
	}
	n.base, n.token = "", ""
	n.extra = nil
	n.answers = nil
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
	if n.base == "" {
		return plugin.TestResult{OK: len(n.extra) > 0, Message: "Optional services configured; open their views to read status"}
	}
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
	if n.base == "" {
		return []plugin.Alert{}, nil
	}
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
	for _, key := range []string{"qbit_password"} {
		if secret := n.extraText(key); secret != "" {
			s = strings.ReplaceAll(s, secret, "[redacted]")
		}
	}
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
	if name == "vpn" {
		return n.vpnView()
	}
	if name == "routes" || name == "seeding" || name == "speedtest" {
		return n.extraView(ctx, name)
	}
	if name != "overview" && name != "traffic" {
		return plugin.View{}, plugin.ErrNotFound
	}
	if n.base == "" {
		return plugin.View{Title: "Network", UpdatedAt: n.host.Now(), Blocks: []plugin.Block{{Type: "notice", Tone: "info", Text: "Prometheus is not configured. Optional routes, seeding and speed tests have separate views."}}}, nil
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
