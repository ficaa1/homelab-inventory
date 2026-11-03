package inventory

import "time"

// VPN contains only public tunnel facts, never WireGuard keys.
type VPN struct {
	Container       string         `json:"container"`
	State           string         `json:"state"`
	Health          string         `json:"health"`
	Interface       string         `json:"interface"`
	StartedAt       *time.Time     `json:"startedAt"`
	Dependents      []VPNDependent `json:"dependents"`
	InterfaceUp     bool           `json:"interfaceUp"`
	LatestHandshake *time.Time     `json:"latestHandshake"`
	Endpoint        *string        `json:"endpoint"`
	RXBytes         *uint64        `json:"rxBytes"`
	TXBytes         *uint64        `json:"txBytes"`
	ExitIP          *string        `json:"exitIp"`
	Country         *string        `json:"country"`
	City            *string        `json:"city"`
	Org             *string        `json:"org"`
}

// VPNDependent is a configured network-namespace consumer. Attached is nil
// when either process namespace could not be observed, including stopped
// containers. Configuration alone cannot prove attachment after a restart.
type VPNDependent struct {
	Name      string     `json:"name"`
	State     string     `json:"state"`
	StartedAt *time.Time `json:"startedAt"`
	Attached  *bool      `json:"attached"`
}
