package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network/speed"
)

// The JSON surface behind the Network page (internal/webui/pages/network.js).
// Every route is a fixed read except POST speedtest, which starts one bounded
// run under the normal dashboard login. Upstream reads are cached for 20s so
// the page's sections can load independently without repeating them.

var (
	_ plugin.APISource = (*Network)(nil)
)

type answer struct {
	at    time.Time
	value any
}

// remember answers from a 20-second cache. A read cut short by the request's
// own deadline is returned but not kept.
func (n *Network) remember(ctx context.Context, key string, read func() any) any {
	now := n.host.Now()
	if a, ok := n.answers[key]; ok && now.Sub(a.at) >= 0 && now.Sub(a.at) < 20*time.Second {
		return a.value
	}
	value := read()
	if ctx.Err() == nil {
		if n.answers == nil {
			n.answers = map[string]answer{}
		}
		n.answers[key] = answer{at: now, value: value}
	}
	return value
}

func jsonAnswer(status int, v any) (plugin.APIResponse, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return plugin.APIResponse{}, err
	}
	return plugin.APIResponse{Status: status, Body: b}, nil
}

func (n *Network) API(ctx context.Context, req plugin.APIRequest) (plugin.APIResponse, error) {
	switch req.Method + " " + req.Path {
	case "GET summary":
		return jsonAnswer(200, n.summary(ctx))
	case "GET metrics":
		return jsonAnswer(200, n.remember(ctx, "metrics", func() any { return n.metricsReport(ctx) }))
	case "GET routes":
		return jsonAnswer(200, n.remember(ctx, "routes", func() any { return n.routesAnswer(ctx) }))
	case "GET torrents":
		return jsonAnswer(200, n.remember(ctx, "torrents", func() any { return n.torrentsAnswer(ctx) }))
	case "GET traffic":
		return jsonAnswer(200, n.remember(ctx, "traffic", func() any { return n.trafficAnswer(ctx) }))
	case "GET speedtest":
		return jsonAnswer(200, n.speedAnswer())
	case "POST speedtest":
		if body := bytes.TrimSpace(req.Body); len(body) > 0 && !bytes.Equal(body, []byte("{}")) {
			return jsonAnswer(400, map[string]string{"error": "expected an empty JSON object"})
		}
		return n.startSpeed()
	}
	return plugin.APIResponse{}, plugin.ErrNotFound
}

// ------------------------------------------------------------ summary

type vpnData struct {
	InterfaceUp     bool       `json:"interfaceUp"`
	LatestHandshake *time.Time `json:"latestHandshake"`
	Endpoint        *string    `json:"endpoint"`
	RXBytes         *uint64    `json:"rxBytes"`
	TXBytes         *uint64    `json:"txBytes"`
	ExitIP          *string    `json:"exitIp"`
	Country         *string    `json:"country"`
	City            *string    `json:"city"`
	Org             *string    `json:"org"`
}
type vpnView struct {
	Host        string     `json:"host"`
	InventoryAt *time.Time `json:"inventoryAt"`
	AgeSeconds  *float64   `json:"ageSeconds"`
	Stale       bool       `json:"stale"`
	Data        *vpnData   `json:"data"`
}
type qbitHealth struct {
	Status     string  `json:"status"`
	Version    *string `json:"version"`
	Download   *int64  `json:"downloadBytesPerSecond"`
	Upload     *int64  `json:"uploadBytesPerSecond"`
	Connection *string `json:"connectionStatus"`
	DHT        *int64  `json:"dhtNodes"`
}
type routeCounts struct {
	Status string `json:"status"`
	Stale  bool   `json:"stale"`
	Total  int    `json:"total"`
}
type summaryAnswer struct {
	GeneratedAt time.Time   `json:"generatedAt"`
	VPN         vpnView     `json:"vpn"`
	VPNs        []vpnView   `json:"vpns"`
	Metrics     report      `json:"metrics"`
	Routes      routeCounts `json:"routes"`
	Qbit        qbitHealth  `json:"qbit"`
}

func (n *Network) summary(ctx context.Context) summaryAnswer {
	now := n.host.Now().UTC()
	out := summaryAnswer{GeneratedAt: now, VPN: vpnView{Stale: true}, VPNs: n.vpns(now)}
	if len(out.VPNs) > 0 {
		out.VPN = out.VPNs[0]
	}
	// The poll summary omits sparklines; GET metrics carries them.
	m := n.remember(ctx, "metrics", func() any { return n.metricsReport(ctx) }).(report)
	panels := map[string]panel{}
	for id, p := range m.Panels {
		series := make([]mseries, len(p.Series))
		copy(series, p.Series)
		for i := range series {
			series[i].Points = nil
		}
		p.Series = series
		panels[id] = p
	}
	m.Panels = panels
	out.Metrics = m
	routes := n.remember(ctx, "routes", func() any { return n.routesAnswer(ctx) }).(routesAnswer)
	out.Routes = routeCounts{Status: routes.Status, Stale: routes.Stale, Total: len(routes.Routes)}
	out.Qbit = n.remember(ctx, "qbit", func() any { return n.qbitHealth(ctx) }).(qbitHealth)
	return out
}

// vpns lists every host reporting tunnel facts, the most recently collected first.
func (n *Network) vpns(now time.Time) []vpnView {
	out := []vpnView{}
	if n.inventory == nil {
		return out
	}
	hosts, err := n.inventory()
	if err != nil {
		return out
	}
	text := func(s *string) *string {
		if s == nil {
			return nil
		}
		t := n.text(*s)
		return &t
	}
	for _, h := range hosts {
		at := h.CollectedAt.UTC()
		age := now.Sub(at).Seconds()
		v := vpnView{Host: n.text(h.Host), InventoryAt: &at, AgeSeconds: &age, Stale: age > 2700 || age < -5,
			Data: &vpnData{InterfaceUp: h.InterfaceUp, LatestHandshake: h.LatestHandshake, Endpoint: text(h.Endpoint), RXBytes: h.RXBytes, TXBytes: h.TXBytes, ExitIP: text(h.ExitIP), Country: text(h.Country), City: text(h.City), Org: text(h.Org)}}
		out = append(out, v)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].InventoryAt.After(*out[j-1].InventoryAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

func (n *Network) qbitHealth(ctx context.Context) qbitHealth {
	h := qbitHealth{Status: "not configured"}
	if n.extraText("qbit_url") == "" {
		return h
	}
	h.Status = "unavailable"
	client, err := n.qbitLogin(ctx)
	if err != nil {
		return h
	}
	version, err := n.qbitVersion(ctx, client)
	if err != nil {
		return h
	}
	t, err := n.qbitTransfer(ctx, client)
	if err != nil {
		return h
	}
	connection := n.text(*t.Connection)
	return qbitHealth{Status: "ok", Version: &version, Download: t.Download, Upload: t.Upload, Connection: &connection, DHT: t.DHT}
}

// ------------------------------------------------------------ routes and torrents

type routesAnswer struct {
	Status        string             `json:"status"`
	Stale         bool               `json:"stale"`
	GeneratedAt   time.Time          `json:"generatedAt"`
	Routes        []route            `json:"routes"`
	CloudflareDNS string             `json:"cloudflareDns,omitempty"`
	DNS           []plugin.DNSRecord `json:"dns,omitempty"`
}

func (n *Network) routesAnswer(ctx context.Context) routesAnswer {
	r := routesAnswer{Status: "not configured", Stale: true, GeneratedAt: n.host.Now().UTC(), Routes: []route{}, CloudflareDNS: "not configured", DNS: []plugin.DNSRecord{}}
	if n.extraText("routes_url") != "" {
		r.Status = "unavailable"
		if routes, err := n.fetchRoutes(ctx); err == nil {
			r.Status, r.Stale, r.Routes = "ok", false, routes
		}
	}
	if n.dnsRecords != nil {
		r.CloudflareDNS = "unavailable"
		if records, err := n.dnsRecords(ctx); err == nil {
			r.CloudflareDNS, r.DNS = "ok", records
			for i := range r.DNS {
				for _, route := range r.Routes {
					if strings.EqualFold(strings.TrimSuffix(r.DNS[i].Name, "."), strings.TrimSuffix(route.Hostname, ".")) {
						r.DNS[i].MatchesRoute = true
						break
					}
				}
			}
		}
	}
	return r
}

type torrentsAnswer struct {
	Status             string    `json:"status"`
	GeneratedAt        time.Time `json:"generatedAt"`
	RequirementSeconds *int64    `json:"requirementSeconds"`
	Count              *int      `json:"count"`
	TotalSizeBytes     *int64    `json:"totalSizeBytes"`
	Torrents           []torrent `json:"torrents"`
}

func (n *Network) torrentsAnswer(ctx context.Context) torrentsAnswer {
	list := torrentsAnswer{Status: "not configured", GeneratedAt: n.host.Now().UTC()}
	if required := n.seedRequirement(); required > 0 {
		list.RequirementSeconds = &required
	}
	if n.extraText("qbit_url") == "" {
		return list
	}
	list.Status = "unavailable"
	client, err := n.qbitLogin(ctx)
	if err != nil {
		return list
	}
	torrents, err := n.qbitSeeding(ctx, client)
	if err != nil {
		return list
	}
	count, total := len(torrents), int64(0)
	for _, t := range torrents {
		total += t.SizeBytes
	}
	list.Status, list.Count, list.TotalSizeBytes, list.Torrents = "ok", &count, &total, torrents
	return list
}

// ------------------------------------------------------------ traffic

type trafficHost struct {
	Name     string         `json:"name"`
	RX       *float64       `json:"rx"`
	TX       *float64       `json:"tx"`
	Errors   *float64       `json:"errors"`
	Drops    *float64       `json:"drops"`
	Receive  []plugin.Point `json:"receive"`
	Transmit []plugin.Point `json:"transmit"`
}
type trafficContainer struct {
	Name           string   `json:"name"`
	BytesPerSecond *float64 `json:"bytesPerSecond"`
}
type trafficAnswer struct {
	Status      string             `json:"status"`
	GeneratedAt time.Time          `json:"generatedAt"`
	Hosts       []trafficHost      `json:"hosts"`
	Fleet       [2][]plugin.Point  `json:"fleet"`
	Containers  []trafficContainer `json:"containers"`
	Notes       []string           `json:"notes"`
}

func (n *Network) trafficAnswer(ctx context.Context) trafficAnswer {
	out := trafficAnswer{Status: "not configured", GeneratedAt: n.host.Now().UTC(), Hosts: []trafficHost{}, Fleet: [2][]plugin.Point{{}, {}}, Containers: []trafficContainer{}, Notes: []string{}}
	if n.base == "" {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	out.Status = "ok"
	set := n.collectTraffic(ctx)
	for i, h := range set.hosts {
		if i == 20 {
			break
		}
		out.Hosts = append(out.Hosts, trafficHost{Name: n.text(h.name), RX: h.rx, TX: h.tx, Errors: h.errors, Drops: h.drops, Receive: h.receive, Transmit: h.transmit})
	}
	if len(set.rxRange)+len(set.txRange) > 0 {
		out.Fleet = [2][]plugin.Point{fleetPoints(set.rxRange), fleetPoints(set.txRange)}
	}
	if set.nodeErr != nil {
		out.Notes = append(out.Notes, "node_exporter data is unavailable; check Prometheus and the exporter.")
	} else if len(set.hosts) == 0 {
		out.Notes = append(out.Notes, "node_exporter data not found — optional.")
	}
	if set.rxErr != nil || set.txErr != nil {
		out.Notes = append(out.Notes, "Traffic history is unavailable.")
	}
	containers, err := n.query(ctx, containerQuery, false, true)
	if err != nil {
		out.Notes = append(out.Notes, "cAdvisor data is unavailable.")
	}
	for i, s := range containers {
		if i == 10 {
			break
		}
		out.Containers = append(out.Containers, trafficContainer{Name: n.text(s.Metric["name"]), BytesPerSecond: s.Current[1]})
	}
	sortContainers(out.Containers)
	if len(out.Notes) > 0 && len(out.Hosts) == 0 {
		out.Status = "unavailable"
	}
	return out
}

func sortContainers(c []trafficContainer) {
	for i := 1; i < len(c); i++ {
		for j := i; j > 0 && number(c[j].BytesPerSecond) > number(c[j-1].BytesPerSecond); j-- {
			c[j], c[j-1] = c[j-1], c[j]
		}
	}
}

// ------------------------------------------------------------ speed test

type speedAnswer struct {
	speed.SpeedSnapshot
	Configured bool   `json:"configured"`
	Message    string `json:"message,omitempty"`
}

func (n *Network) speedAnswer() speedAnswer {
	if err := n.ensureSpeed(); err != nil {
		return speedAnswer{SpeedSnapshot: speed.SpeedSnapshot{History: []speed.SpeedResult{}}, Message: err.Error()}
	}
	snap := n.speed.Snapshot()
	if snap.History == nil {
		snap.History = []speed.SpeedResult{}
	}
	return speedAnswer{SpeedSnapshot: snap, Configured: true}
}

// startSpeed starts one run and returns at once; the page polls GET speedtest.
// Single flight and the two-minute cooldown live in the speed tester.
func (n *Network) startSpeed() (plugin.APIResponse, error) {
	if err := n.ensureSpeed(); err != nil {
		return jsonAnswer(409, map[string]string{"error": err.Error()})
	}
	id, err := n.speed.Start(n.host.Now())
	var cool speed.SpeedtestCooldown
	switch {
	case errors.Is(err, speed.ErrSpeedtestRunning):
		return jsonAnswer(409, map[string]string{"error": "A speed test is already running"})
	case errors.As(err, &cool):
		wait := int(math.Ceil(cool.Wait.Seconds()))
		return jsonAnswer(429, map[string]any{"error": fmt.Sprintf("Try again in %ds", wait), "retryAfter": wait})
	case err != nil:
		return plugin.APIResponse{}, errors.New("speed test did not start")
	}
	return jsonAnswer(202, map[string]string{"id": id})
}
