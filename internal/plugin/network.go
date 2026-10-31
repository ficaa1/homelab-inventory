package plugin

import (
	"time"
)

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
