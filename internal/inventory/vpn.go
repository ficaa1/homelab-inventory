package inventory

import "time"

// VPN contains only public tunnel facts, never WireGuard keys.
type VPN struct {
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
