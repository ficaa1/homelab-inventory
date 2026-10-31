package dusk

import (
	"crypto/sha256"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

const discoveryType = "urn:homelab-inventory:service:dusk:1"

type advertiser struct {
	conn *net.UDPConn
	done chan struct{}
}

func (a *advertiser) close() { a.conn.Close(); <-a.done }
func discoveryResponse(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if strings.IndexFunc(raw, func(r rune) bool { return r < 33 || r == 127 }) >= 0 {
		return "", fmt.Errorf("invalid URL characters")
	}
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\r\n\\ \t") || len(raw) > 2048 {
		return "", fmt.Errorf("use a reachable HTTP(S) base URL without credentials, query or fragment")
	}
	if len(name) > 100 || strings.ContainsAny(name, "\r\n<>") || strings.IndexFunc(name, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return "", fmt.Errorf("invalid server name")
	}
	raw = strings.TrimRight(raw, "/")
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=60\r\nST: %s\r\nUSN: uuid:%x::%s\r\nLOCATION: %s\r\nX-Dusk-Name: %s\r\nEXT:\r\n\r\n", discoveryType, sum[:16], discoveryType, raw, name), nil
}
func isSearch(b []byte) bool {
	if len(b) > 2048 {
		return false
	}
	lines := strings.Split(string(b), "\r\n")
	if len(lines) < 4 || lines[0] != "M-SEARCH * HTTP/1.1" {
		return false
	}
	h := map[string]string{}
	for _, line := range lines[1:] {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return false
		}
		k = strings.ToLower(k)
		if _, ok := h[k]; ok {
			return false
		}
		h[k] = strings.TrimSpace(v)
	}
	return h["st"] == discoveryType && h["man"] == `"ssdp:discover"` && h["mx"] == "1" && h["host"] == "239.255.255.250:1900"
}
func (d *Dusk) configureDiscovery(c plugin.Config) error {
	enabled, _ := c["lan_discovery"].(bool)
	if !enabled {
		d.response = ""
		return nil
	}
	raw, _ := c["discovery_url"].(string)
	if raw == "" {
		raw = os.Getenv("PUBLIC_URL")
	}
	name, _ := c["server_name"].(string)
	response, err := discoveryResponse(raw, name)
	if err != nil {
		return &plugin.FieldError{Key: "discovery_url", Message: err.Error()}
	}
	d.response = response
	return nil
}

// Start activates only committed enabled settings; Configure never opens sockets.
func (d *Dusk) Start() error {
	d.Close()
	if d.response == "" {
		return nil
	}
	response := d.response
	addr := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 1900}
	conn, err := net.ListenMulticastUDP("udp4", nil, addr)
	if err != nil {
		if d.host != nil {
			d.host.Logf("LAN discovery unavailable: check host multicast networking")
		}
		return nil
	}
	a := &advertiser{conn: conn, done: make(chan struct{})}
	d.discovery = a
	go func() {
		defer close(a.done)
		buf := make([]byte, 2049)
		var last time.Time
		for {
			n, peer, e := conn.ReadFromUDP(buf)
			if e != nil {
				return
			}
			if (!peer.IP.IsPrivate() && !peer.IP.IsLoopback() && !peer.IP.IsLinkLocalUnicast()) || !isSearch(buf[:n]) || time.Since(last) < 100*time.Millisecond {
				continue
			}
			last = time.Now()
			conn.WriteToUDP([]byte(response), peer)
		}
	}()
	return nil
}
