package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// The page API takes no caller input beyond an empty start request, and a
// metric from a failed or silent collector never reads as a current value.
func TestPageAPIStartAndFreshness(t *testing.T) {
	n := New(extraHost{}).(*Network)
	if e := n.Configure(plugin.Config{}); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	for body, want := range map[string]int{`{"url":"http://192.0.2.1"}`: 400, `{}`: 409, ``: 409} {
		out, e := n.API(ctx, plugin.APIRequest{Method: "POST", Path: "speedtest", Body: []byte(body)})
		if e != nil || out.Status != want {
			t.Fatalf("POST %q: %d %v, want %d", body, out.Status, e, want)
		}
	}
	if _, e := n.API(ctx, plugin.APIRequest{Method: "POST", Path: "metrics"}); !errors.Is(e, plugin.ErrNotFound) {
		t.Fatal("reads must not accept POST")
	}

	now := time.Unix(1_800_000_000, 0).UTC()
	at := func(age time.Duration) *time.Time { t := now.Add(-age); return &t }
	one, ok := 1.0, 0.0
	v := func() *float64 { x := 5.0; return &x }
	defs := []metricDef{{id: "collectionSuccess"}, {id: "pingRtt", section: "ping"}, {id: "dnsLatency", section: "dns"}, {id: "probeUp", optional: true}}
	r := report{Status: "ok", Panels: map[string]panel{
		"collectionSuccess": {Series: []mseries{{Labels: map[string]string{"section": "ping"}, Value: &one, SampledAt: at(0)}, {Labels: map[string]string{"section": "dns"}, Value: &ok, SampledAt: at(0)}}},
		"pingRtt":           {Series: []mseries{{Value: v(), SampledAt: at(30 * time.Second)}}},
		"dnsLatency":        {Series: []mseries{{Value: v(), SampledAt: at(0)}}},
		"probeUp":           {Series: []mseries{{Value: v(), SampledAt: at(10 * time.Minute)}}},
	}}
	r = checkedFreshness(r, defs, now)
	if r.Panels["pingRtt"].Series[0].Value == nil || r.Panels["pingRtt"].Stale {
		t.Fatal("a fresh sample from a healthy collector must stay")
	}
	if r.Panels["dnsLatency"].Series[0].Value != nil || r.Panels["probeUp"].Series[0].Value != nil || r.Status != "partial" {
		t.Fatalf("failed collector or old sample read as current: %+v", r)
	}
}
