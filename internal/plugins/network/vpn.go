package network

import (
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (n *Network) SetNetworkSource(source func() ([]plugin.NetworkHost, error)) { n.inventory = source }
func (n *Network) vpnView() (plugin.View, error) {
	v := plugin.View{Title: "VPN", UpdatedAt: n.host.Now(), RefreshSeconds: 30, Blocks: []plugin.Block{}}
	if n.inventory == nil {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Inventory projection is unavailable"})
		return v, nil
	}
	hosts, e := n.inventory()
	if e != nil {
		return v, e
	}
	rows := []map[string]any{}
	for _, host := range hosts {
		scalar := func(s *string) any {
			if s == nil {
				return nil
			}
			return n.text(*s)
		}
		counter := func(s *uint64) any {
			if s == nil {
				return nil
			}
			return *s
		}
		var handshake any
		if host.LatestHandshake != nil {
			handshake = host.LatestHandshake.Format("2006-01-02T15:04:05Z07:00")
		}
		age := n.host.Now().Sub(host.CollectedAt).Seconds()
		rows = append(rows, map[string]any{"host": n.text(host.Host), "up": host.InterfaceUp, "stale": age > 2700 || age < -5, "handshake": handshake, "exit": scalar(host.ExitIP), "location": fmt.Sprint(scalar(host.City), " ", scalar(host.Country)), "endpoint": scalar(host.Endpoint), "rx": counter(host.RXBytes), "tx": counter(host.TXBytes)})
	}
	v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Inventory tunnel facts", Empty: "No VPN facts collected", Columns: []plugin.Column{{Key: "host", Label: "Host"}, {Key: "up", Label: "Interface up"}, {Key: "stale", Label: "Stale"}, {Key: "handshake", Label: "Latest handshake", Format: "time"}, {Key: "exit", Label: "Exit IP"}, {Key: "location", Label: "Location"}, {Key: "endpoint", Label: "Peer endpoint"}, {Key: "rx", Label: "Received", Format: "bytes"}, {Key: "tx", Label: "Sent", Format: "bytes"}}, Rows: rows})
	return v, nil
}
