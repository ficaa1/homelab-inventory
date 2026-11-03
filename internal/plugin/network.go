package plugin

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"time"
)

type DNSRecord struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Proxied      bool   `json:"proxied"`
	MatchesRoute bool   `json:"matchesRoute"`
}

type DNSRecordsSource interface {
	DNSRecords(context.Context) ([]DNSRecord, error)
}
type DNSConsumer interface {
	SetDNSRecordsSource(func(context.Context) ([]DNSRecord, error))
}

// NetworkHost is the narrow read-only projection supplied by inventory core.
// It contains public tunnel facts; no settings, credentials or full inventories.
type NetworkReconnect struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Result  string    `json:"result"`
	Reason  string    `json:"reason"`
	ExitIP  *string   `json:"exitIp"`
	Country *string   `json:"country"`
}
type NetworkAutofix struct {
	Enabled       bool       `json:"enabled"`
	AfterMinutes  int        `json:"afterMinutes"`
	State         string     `json:"state"`
	LastRun       *time.Time `json:"lastRun"`
	CooldownUntil *time.Time `json:"cooldownUntil"`
}
type NetworkHost struct {
	Container, State, Health, Interface, Status, Reason string
	StartedAt                                           *time.Time
	Dependents                                          []inventory.VPNDependent
	Since                                               *time.Time
	HandshakeAgeSeconds                                 *float64
	Live                                                bool
	ReconnectAllowed                                    bool
	LastReconnect                                       *NetworkReconnect
	Autofix                                             NetworkAutofix
	Host                                                string
	CollectedAt                                         time.Time
	InterfaceUp                                         bool
	LatestHandshake                                     *time.Time
	Endpoint, ExitIP, Country, City, Org                *string
	RXBytes, TXBytes                                    *uint64
}
type NetworkSource interface {
	SetNetworkSource(func() ([]NetworkHost, error))
}

func (r *Registry) BindNetworkSource(source func() ([]NetworkHost, error)) {
	e := r.entries["network"]
	if e == nil {
		return
	}
	e.op.Lock()
	defer e.op.Unlock()
	if p, ok := e.plugin.(NetworkSource); ok {
		p.SetNetworkSource(source)
	}
}

func (r *Registry) BindNetworkDNS() {
	n := r.entries["network"]
	c := r.entries["cloudflare"]
	if n == nil || c == nil {
		return
	}
	c.mu.Lock()
	cloudflareReady := c.stored.Enabled && c.ready
	c.mu.Unlock()
	n.mu.Lock()
	networkReady := n.stored.Enabled && n.ready
	n.mu.Unlock()
	consumer, ok := n.plugin.(DNSConsumer)
	source, ok2 := c.plugin.(DNSRecordsSource)
	if !ok || !ok2 || !networkReady || !cloudflareReady {
		if ok {
			consumer.SetDNSRecordsSource(nil)
		}
		return
	}
	consumer.SetDNSRecordsSource(source.DNSRecords)
}

// NetworkPanelSource requests an exit check through the shared five-minute cache.
type NetworkPanelSource interface{ SetVPNPanelSource(func(context.Context)) }

func (r *Registry) BindVPNPanelSource(source func(context.Context)) {
	e := r.entries["network"]
	if e == nil {
		return
	}
	e.op.Lock()
	defer e.op.Unlock()
	if p, ok := e.plugin.(NetworkPanelSource); ok {
		p.SetVPNPanelSource(source)
	}
}
