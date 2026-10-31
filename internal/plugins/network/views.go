package network

import (
	"context"
	"fmt"
	"math"
	"net"
	"regexp"
	"sort"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type hostTraffic struct {
	instance, name        string
	rx, tx, errors, drops *float64
	receive, transmit     []plugin.Point
}

func hostName(instance string) string {
	host, _, err := net.SplitHostPort(instance)
	if err == nil {
		return host
	}
	return instance
}
func number(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}
func scalar(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

var containerID = regexp.MustCompile(`^[0-9a-f]{12}([0-9a-f]{52})?$`)

// trafficSet is node_exporter traffic per host, busiest first, with the range
// samples behind the fleet chart. Each source's error is kept separately.
type trafficSet struct {
	hosts            []*hostTraffic
	rxRange, txRange []sample
	nodeErr, rxErr   error
	txErr            error
}

func (n *Network) collectTraffic(ctx context.Context) trafficSet {
	hosts := map[string]*hostTraffic{}
	host := func(s sample) *hostTraffic {
		instance := s.Metric["instance"]
		if hosts[instance] == nil {
			hosts[instance] = &hostTraffic{instance: instance, name: hostName(instance)}
		}
		h := hosts[instance]
		// A containerised node_exporter reports its container ID as nodename.
		if nodename := s.Metric["nodename"]; nodename != "" && !containerID.MatchString(nodename) {
			h.name = nodename
		}
		return h
	}
	var set trafficSet
	for _, kind := range []string{"rx", "tx", "errors", "drops"} {
		samples, err := n.query(ctx, n.hostQuery(kind), false, true)
		if err != nil {
			if set.nodeErr == nil {
				set.nodeErr = err
			}
			continue
		}
		for _, s := range samples {
			h := host(s)
			switch kind {
			case "rx":
				h.rx = s.Current[1]
			case "tx":
				h.tx = s.Current[1]
			case "errors":
				h.errors = s.Current[1]
			case "drops":
				h.drops = s.Current[1]
			}
		}
	}
	if len(hosts) > 0 {
		set.rxRange, set.rxErr = n.query(ctx, n.hostQuery("rx"), true, true)
		set.txRange, set.txErr = n.query(ctx, n.hostQuery("tx"), true, true)
		for _, s := range set.rxRange {
			host(s).receive = s.Points
		}
		for _, s := range set.txRange {
			host(s).transmit = s.Points
		}
	}
	ordered := make([]*hostTraffic, 0, len(hosts))
	for _, h := range hosts {
		if h.receive == nil {
			h.receive = []plugin.Point{}
		}
		if h.transmit == nil {
			h.transmit = []plugin.Point{}
		}
		ordered = append(ordered, h)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := number(ordered[i].rx)+number(ordered[i].tx), number(ordered[j].rx)+number(ordered[j].tx)
		if a != b {
			return a > b
		}
		return ordered[i].instance < ordered[j].instance
	})
	set.hosts = ordered
	return set
}

func (n *Network) buildView(ctx context.Context, name string, view *plugin.View) {
	set := n.collectTraffic(ctx)
	ordered, rxRange, txRange, nodeErr, rxErr, txErr := set.hosts, set.rxRange, set.txRange, set.nodeErr, set.rxErr, set.txErr
	if name == "traffic" {
		view.Title = "Traffic"
		for i, h := range ordered {
			if i >= 20 {
				break
			}
			if len(h.receive)+len(h.transmit) == 0 {
				continue
			}
			view.Blocks = append(view.Blocks, plugin.Block{Type: "chart", Title: n.text(h.name), Unit: "bytes_s", Empty: "No traffic history yet", Series: []plugin.Series{{Name: "Receive", Tone: "info", Points: h.receive}, {Name: "Transmit", Tone: "ok", Points: h.transmit}}})
		}
		if len(view.Blocks) == 0 {
			view.Blocks = append(view.Blocks, notice("node_exporter", nodeErr))
		}
		if rxErr != nil || txErr != nil {
			view.Blocks = append(view.Blocks, notice("Traffic history", firstError(rxErr, txErr)))
		}
		return
	}
	// Independent sources degrade separately so one absent exporter cannot hide
	// the rest of the page. Every series/row is bounded before view validation.
	stats := plugin.Block{Type: "stats", Items: []plugin.Item{}}
	if len(ordered) > 0 {
		var receive, transmit, errorRate float64
		haveRX, haveTX, haveErrors := false, false, false
		rows := []map[string]any{}
		for _, h := range ordered {
			receive += number(h.rx)
			transmit += number(h.tx)
			errorRate += number(h.errors)
			haveRX = haveRX || h.rx != nil
			haveTX = haveTX || h.tx != nil
			haveErrors = haveErrors || h.errors != nil
			if len(rows) < 500 {
				rows = append(rows, map[string]any{"host": n.text(h.name), "rx": scalar(h.rx), "tx": scalar(h.tx), "errors": scalar(h.errors), "drops": scalar(h.drops)})
			}
		}
		if len(ordered) > 500 {
			view.Blocks = append(view.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Showing the 500 busiest hosts."})
		}
		if haveRX {
			stats.Items = append(stats.Items, plugin.Item{Label: "Fleet receive", Value: rateText(receive), Tone: "info"})
		}
		if haveTX {
			stats.Items = append(stats.Items, plugin.Item{Label: "Fleet transmit", Value: rateText(transmit), Tone: "ok"})
		}
		if haveErrors {
			stats.Items = append(stats.Items, plugin.Item{Label: "Error rate", Value: strconv.FormatFloat(errorRate, 'f', 2, 64) + " /s"})
		}
		if len(rxRange)+len(txRange) > 0 {
			view.Blocks = append(view.Blocks, plugin.Block{Type: "chart", Title: "Fleet traffic", Unit: "bytes_s", Empty: "No traffic history yet", Series: []plugin.Series{{Name: "Receive", Tone: "info", Points: fleetPoints(rxRange)}, {Name: "Transmit", Tone: "ok", Points: fleetPoints(txRange)}}})
		}
		view.Blocks = append(view.Blocks, plugin.Block{Type: "table", Title: "Hosts", Columns: []plugin.Column{{Key: "host", Label: "Host"}, {Key: "rx", Label: "Receive", Format: "bytes_s", Align: "right"}, {Key: "tx", Label: "Transmit", Format: "bytes_s", Align: "right"}, {Key: "errors", Label: "Errors/s", Format: "number", Align: "right"}, {Key: "drops", Label: "Drops/s", Format: "number", Align: "right"}}, Rows: rows})
	} else {
		view.Blocks = append(view.Blocks, notice("node_exporter", nodeErr))
	}
	if len(ordered) > 0 && nodeErr != nil {
		view.Blocks = append(view.Blocks, notice("Some node_exporter metrics", nodeErr))
	}
	if rxErr != nil || txErr != nil {
		view.Blocks = append(view.Blocks, notice("Traffic history", firstError(rxErr, txErr)))
	}
	probes, probeErr := n.query(ctx, "probe_success", false, true)
	if len(probes) > 0 {
		duration, durationErr := n.query(ctx, "probe_duration_seconds", false, true)
		latencies := map[string]*float64{}
		for _, s := range duration {
			latencies[probeKey(s)] = s.Current[1]
		}
		items := []plugin.Item{}
		up := 0
		for _, s := range probes {
			tone := "crit"
			if number(s.Current[1]) == 1 {
				tone = "ok"
				up++
			}
			meta := "Latency unavailable"
			if latency := latencies[probeKey(s)]; latency != nil {
				meta = fmt.Sprintf("%.1f ms", *latency*1000)
			}
			items = append(items, plugin.Item{Title: n.text(s.Metric["instance"]), Subtitle: n.text(s.Metric["job"]), Meta: meta, Tone: tone})
		}
		tone := "ok"
		if up != len(probes) {
			tone = "crit"
		}
		stats.Items = append(stats.Items, plugin.Item{Label: "Probes up", Value: fmt.Sprintf("%d / %d", up, len(probes)), Tone: tone})
		view.Blocks = append(view.Blocks, plugin.Block{Type: "list", Title: "Probes", Items: items})
		if durationErr != nil {
			view.Blocks = append(view.Blocks, notice("Probe latency", durationErr))
		}
	} else {
		view.Blocks = append(view.Blocks, notice("blackbox_exporter", probeErr))
	}
	containers, containerErr := n.query(ctx, containerQuery, false, true)
	if len(containers) > 0 {
		sort.SliceStable(containers, func(i, j int) bool { return number(containers[i].Current[1]) > number(containers[j].Current[1]) })
		rows := []map[string]any{}
		for i, s := range containers {
			if i == 10 {
				break
			}
			rows = append(rows, map[string]any{"name": n.text(s.Metric["name"]), "traffic": scalar(s.Current[1])})
		}
		view.Blocks = append(view.Blocks, plugin.Block{Type: "table", Title: "Busiest containers", Columns: []plugin.Column{{Key: "name", Label: "Container"}, {Key: "traffic", Label: "Traffic", Format: "bytes_s", Align: "right"}}, Rows: rows})
	} else {
		view.Blocks = append(view.Blocks, notice("cAdvisor", containerErr))
	}
	if len(stats.Items) > 0 {
		view.Blocks = append([]plugin.Block{stats}, view.Blocks...)
	}
}
func firstError(a, b error) error {
	if a != nil {
		return a
	}
	return b
}
func probeKey(s sample) string { return s.Metric["instance"] + "\x00" + s.Metric["job"] }
func rateText(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return "Unavailable"
	}
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s", "TiB/s"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
func fleetPoints(samples []sample) []plugin.Point {
	// A gap from every source remains null, rather than becoming a zero line.
	sums := map[float64]*float64{}
	for _, s := range samples {
		for _, p := range s.Points {
			timestamp := *p[0]
			if _, exists := sums[timestamp]; !exists {
				sums[timestamp] = nil
			}
			if p[1] != nil {
				if sums[timestamp] == nil {
					sums[timestamp] = new(float64)
				}
				*sums[timestamp] += *p[1]
			}
		}
	}
	times := make([]float64, 0, len(sums))
	for timestamp := range sums {
		times = append(times, timestamp)
	}
	sort.Float64s(times)
	if len(times) > 300 {
		times = times[len(times)-300:]
	}
	points := make([]plugin.Point, 0, len(times))
	for _, timestamp := range times {
		value := sums[timestamp]
		if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
			value = nil
		}
		points = append(points, plugin.Point{&timestamp, value})
	}
	return points
}
