// Package dusk configures the optional TV integration. Core owns device credentials.
package dusk

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net"
	"net/url"
	"strings"
)

type Dusk struct {
	discovery *advertiser
	response  string
	host      plugin.Host
	listen    func() (*net.UDPConn, error)
}

func New(h plugin.Host) plugin.Plugin { return &Dusk{host: h} }
func (*Dusk) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "dusk", Name: "Dusk", Description: "Pair household TVs with scoped access to shared Media connections.", Version: "1", Settings: []plugin.Field{{Key: "lan_discovery", Label: "Advertise to TVs on this LAN", Type: plugin.Bool, Default: false}, {Key: "discovery_url", Label: "TV reachable base URL", Type: plugin.URL, Help: "Required for LAN advertising. Include any reverse proxy prefix; never use a container-only address."}, {Key: "server_name", Label: "Server name", Type: plugin.Text, Default: "Homelab"}, {Key: "discovery", Label: "Discovery and search", Type: plugin.Bool, Default: true}, {Key: "taste", Label: "Taste and local feedback", Type: plugin.Bool, Default: true}, {Key: "requests", Label: "Allow configured request policies", Type: plugin.Bool, Default: false}}}
}
func (d *Dusk) Configure(c plugin.Config) error {
	n, _ := c["server_name"].(string)
	if len(n) > 100 || strings.ContainsAny(n, "<>\r\n") {
		return &plugin.FieldError{Key: "server_name", Message: "Use a plain server name up to 100 characters"}
	}
	return d.configureDiscovery(c)
}
func (d *Dusk) Close() error {
	if d.discovery != nil {
		d.discovery.close()
		d.discovery = nil
	}
	return nil
}
func (*Dusk) Test(context.Context) plugin.TestResult {
	return plugin.TestResult{OK: true, Message: "Use household setup below to select profiles and pair devices. Media connections are reused."}
}
func (*Dusk) View(context.Context, string, url.Values) (plugin.View, error) {
	return plugin.View{}, nil
}
