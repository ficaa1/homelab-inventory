package plugin

import (
	"context"
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
type NetworkHost struct {
	Host                                 string
	CollectedAt                          time.Time
	InterfaceUp                          bool
	LatestHandshake                      *time.Time
	Endpoint, ExitIP, Country, City, Org *string
	RXBytes, TXBytes                     *uint64
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
