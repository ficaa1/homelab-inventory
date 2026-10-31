package network

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// The page's metrics report: a fixed list of server-owned queries, each read as
// a current value, the raw sample's own timestamp and a history range. Only
// validated exporter names from the settings are interpolated; the caller
// never sends a query.

type metricDef struct {
	id, unit, query, stamp string
	// section names the exporter collector whose health gates this panel;
	// optional panels may be absent without making the report partial.
	section  string
	optional bool
	scale    float64
}

type report struct {
	Status        string           `json:"status"`
	Source        string           `json:"source"`
	Stale         bool             `json:"stale"`
	GeneratedAt   time.Time        `json:"generatedAt"`
	WindowSeconds int              `json:"windowSeconds"`
	StepSeconds   int              `json:"stepSeconds"`
	Sources       map[string]bool  `json:"sources"`
	Panels        map[string]panel `json:"panels"`
}
type panel struct {
	Unit   string    `json:"unit"`
	Stale  bool      `json:"stale"`
	Series []mseries `json:"series"`
}
type mseries struct {
	Labels    map[string]string `json:"labels"`
	Value     *float64          `json:"value"`
	SampledAt *time.Time        `json:"sampledAt"`
	Points    []plugin.Point    `json:"points,omitempty"`
}

// freshFor is how old a raw sample may be before its value counts as unknown.
const freshFor = 120

func (n *Network) metricDefs() []metricDef {
	stamp := func(q string) string { return "timestamp(" + q + ")" }
	defs := []metricDef{
		{id: "probeUp", unit: "state", query: "probe_success", stamp: stamp("probe_success"), optional: true, scale: 1},
		{id: "probeDuration", unit: "ms", query: "probe_duration_seconds", stamp: stamp("probe_duration_seconds"), optional: true, scale: 1000},
	}
	if c := n.vpnContainer; c != "" {
		for _, d := range [][2]string{{"vpnRx", "receive"}, {"vpnTx", "transmit"}} {
			metric := `container_network_` + d[1] + `_bytes_total{name="` + c + `"}`
			defs = append(defs, metricDef{id: d[0], unit: "bits/s", query: `sum by (name) (rate(` + metric + `[5m])) * 8`, stamp: `min by (name) (timestamp(` + metric + `))`, scale: 1})
		}
	}
	if p := n.diagPrefix; p != "" {
		defs = append(defs,
			metricDef{id: "pingRtt", unit: "ms", query: p + "_ping_rtt_seconds", stamp: stamp(p + "_ping_rtt_seconds"), section: "ping", scale: 1000},
			metricDef{id: "pingLoss", unit: "%", query: p + "_ping_loss_percent", stamp: stamp(p + "_ping_loss_percent"), section: "ping", scale: 1},
			metricDef{id: "dnsLatency", unit: "ms", query: p + "_dns_resolve_seconds", stamp: stamp(p + "_dns_resolve_seconds"), section: "dns", scale: 1000},
			metricDef{id: "collectionSuccess", unit: "state", query: p + "_collection_success", stamp: stamp(p + "_collection_success"), scale: 1},
		)
		if d := n.wanDevice; d != "" {
			w := p + "_" + d + "_wan"
			down, up := w+"_rate_down_bytes_per_second", w+"_rate_up_bytes_per_second"
			defs = append(defs,
				metricDef{id: "wanDown", unit: "bits/s", query: down + " * 8", stamp: stamp(down), section: d, scale: 1},
				metricDef{id: "wanUp", unit: "bits/s", query: up + " * 8", stamp: stamp(up), section: d, scale: 1},
				metricDef{id: "wanDownUtilization", unit: "%", query: "100 * " + down + " * 8 / " + w + "_bandwidth_down_bits_per_second", stamp: "min(" + stamp(down) + " and (" + stamp(w+"_bandwidth_down_bits_per_second") + " > time() - 120))", section: d, scale: 1},
				metricDef{id: "wanUpUtilization", unit: "%", query: "100 * " + up + " * 8 / " + w + "_bandwidth_up_bits_per_second", stamp: "min(" + stamp(up) + " and (" + stamp(w+"_bandwidth_up_bits_per_second") + " > time() - 120))", section: d, scale: 1},
				metricDef{id: "wanState", unit: "state", query: w + "_state", stamp: stamp(w + "_state"), section: d, scale: 1},
				metricDef{id: "ponRxPower", unit: "dBm", query: p + "_" + d + "_pon_rx_power_dbm", stamp: stamp(p + "_" + d + "_pon_rx_power_dbm"), section: d, optional: true, scale: 1},
			)
		}
	}
	return defs
}

var keptLabels = []string{"target", "server", "device", "section", "instance", "job", "name"}

func labelKey(m map[string]string) string {
	kept := map[string]string{}
	for k, v := range m {
		if k != "__name__" {
			kept[k] = v
		}
	}
	b, _ := json.Marshal(kept)
	return string(b)
}

func (n *Network) metricsReport(ctx context.Context) report {
	now := n.host.Now().UTC()
	step := int(math.Ceil(n.windowDuration().Seconds() / 299))
	r := report{Status: "not configured", Source: "prometheus", Stale: true, GeneratedAt: now, WindowSeconds: int(n.windowDuration().Seconds()), StepSeconds: step,
		Sources: map[string]bool{"vpn": n.vpnContainer != "", "quality": n.diagPrefix != "", "wan": n.wanDevice != ""}, Panels: map[string]panel{}}
	if n.base == "" {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	defs := n.metricDefs()
	type result struct {
		values, stamps, spark []sample
		err                   bool
	}
	results := make([]result, len(defs))
	var wg sync.WaitGroup
	for i, d := range defs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var e1, e2, e3 error
			results[i].values, e1 = n.query(ctx, d.query, false, true)
			results[i].stamps, e2 = n.query(ctx, d.stamp, false, true)
			results[i].spark, e3 = n.query(ctx, d.query, true, true)
			results[i].err = e1 != nil || e2 != nil || e3 != nil
		}()
	}
	wg.Wait()
	r.Status, r.Stale = "ok", false
	for i, d := range defs {
		x := results[i]
		p := panel{Unit: d.unit, Stale: x.err || (len(x.values) == 0 && !d.optional), Series: []mseries{}}
		stamps := map[string]float64{}
		for _, s := range x.stamps {
			if s.Current[1] != nil {
				stamps[labelKey(s.Metric)] = *s.Current[1]
			}
		}
		for j, s := range x.values {
			if j == 50 {
				break
			}
			key := labelKey(s.Metric)
			v := mseries{Labels: map[string]string{}}
			for _, k := range keptLabels {
				if value, ok := s.Metric[k]; ok {
					v.Labels[k] = n.text(value)
				}
			}
			stamp, has := stamps[key]
			// A ratio's stamp is one aggregate across both inputs.
			if !has && len(x.stamps) == 1 && x.stamps[0].Current[1] != nil {
				stamp, has = *x.stamps[0].Current[1], true
			}
			if has = has && stamp >= 0 && stamp <= float64(now.Unix())+5; has {
				t := time.Unix(int64(stamp), 0).UTC()
				v.SampledAt = &t
			}
			if value := s.Current[1]; value != nil && has {
				scaled := *value * d.scale
				if !math.IsInf(scaled, 0) && !math.IsNaN(scaled) {
					v.Value = &scaled
				}
			}
			for _, sp := range x.spark {
				if labelKey(sp.Metric) != key {
					continue
				}
				for _, point := range sp.Points {
					item := plugin.Point{point[0], nil}
					if point[1] != nil {
						scaled := *point[1] * d.scale
						if !math.IsInf(scaled, 0) && !math.IsNaN(scaled) {
							item[1] = &scaled
						}
					}
					v.Points = append(v.Points, item)
				}
			}
			p.Series = append(p.Series, v)
		}
		r.Panels[d.id] = p
	}
	return checkedFreshness(r, defs, now)
}

// checkedFreshness nulls values whose raw sample is older than two minutes, and
// every panel of an exporter collector that reported failure: a failed
// collector can leave old gauges that are still freshly scraped.
func checkedFreshness(r report, defs []metricDef, now time.Time) report {
	healthy := map[string]bool{}
	for _, s := range r.Panels["collectionSuccess"].Series {
		section := s.Labels["device"]
		if section == "" {
			section = s.Labels["section"]
		}
		good := s.Value != nil && *s.Value == 1
		if prior, seen := healthy[section]; seen {
			good = good && prior
		}
		healthy[section] = good
	}
	for _, d := range defs {
		p, ok := r.Panels[d.id]
		if !ok {
			continue
		}
		for i := range p.Series {
			s := &p.Series[i]
			age := float64(now.Unix())
			if s.SampledAt != nil {
				age -= float64(s.SampledAt.Unix())
			}
			if s.SampledAt == nil || age < -5 || age > freshFor || (d.section != "" && !healthy[d.section]) {
				s.Value = nil
				p.Stale = true
			}
		}
		if d.id == "collectionSuccess" && len(p.Series) == 0 {
			p.Stale = true
		}
		r.Panels[d.id] = p
		if p.Stale {
			r.Stale = true
			r.Status = "partial"
		}
	}
	return r
}
