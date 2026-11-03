// Package vpnprobe collects public VPN facts through fixed allowlisted commands.
package vpnprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

type Facts struct {
	LatestHandshake            *time.Time
	Endpoint                   *string
	RXBytes, TXBytes           *uint64
	ExitIP, Country, City, Org *string
}
type Container struct {
	Name, State, Health string
	StartedAt           time.Time
}

func probe(ctx context.Context, r *execallow.Runner, operation, name string) (string, error) {
	if r == nil {
		return "", errors.New("container probe unavailable")
	}
	res := r.Run(ctx, operation, map[string]string{"container": name})
	if res.ExitCode != 0 || res.Error != "" {
		return "", errors.New("container probe failed")
	}
	return res.Stdout, nil
}
func WireGuard(ctx context.Context, r *execallow.Runner, name string) (Facts, error) {
	raw, err := probe(ctx, r, "vpn-wg-dump", name)
	if err != nil {
		return Facts{}, err
	}
	return parseWGDump(raw)
}
func Exit(ctx context.Context, r *execallow.Runner, name string) (Facts, error) {
	raw, err := probe(ctx, r, "vpn-exit-check", name)
	if err != nil {
		return Facts{}, err
	}
	return parseExit(raw)
}
func Inspect(ctx context.Context, r *execallow.Runner, name string) (Container, error) {
	raw, err := probe(ctx, r, "container-state", name)
	if err != nil {
		return Container{}, err
	}
	var c Container
	if json.Unmarshal([]byte(raw), &c) != nil || c.Name != "/"+name || c.State == "" || (c.State == "running" && c.StartedAt.IsZero()) {
		return Container{}, errors.New("invalid container state")
	}
	c.Name = name
	return c, nil
}

// The all-interfaces dump adds an interface column to each row. Counters
// cover all peers; endpoint and handshake describe the most recent peer.
// The runner has already discarded every key before these facts are parsed.
func parseWGDump(raw string) (Facts, error) {
	f := Facts{}
	interfaces := map[string]bool{}
	var rxTotal, txTotal uint64
	peers := 0
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		p := strings.Split(line, "\t")
		if len(p) == 5 {
			if p[0] == "" || interfaces[p[0]] {
				return Facts{}, errors.New("invalid WireGuard interface")
			}
			interfaces[p[0]] = true
			continue
		}
		if len(p) != 9 || !interfaces[p[0]] {
			return Facts{}, errors.New("invalid WireGuard peer")
		}
		stamp, err := strconv.ParseInt(p[5], 10, 64)
		if err != nil || stamp < 0 {
			return Facts{}, errors.New("invalid handshake")
		}
		rx, err := strconv.ParseUint(p[6], 10, 64)
		if err != nil || ^uint64(0)-rxTotal < rx {
			return Facts{}, errors.New("invalid receive count")
		}
		tx, err := strconv.ParseUint(p[7], 10, 64)
		if err != nil || ^uint64(0)-txTotal < tx {
			return Facts{}, errors.New("invalid transmit count")
		}
		rxTotal += rx
		txTotal += tx
		var endpoint *string
		if p[3] != "(none)" {
			host, port, e := net.SplitHostPort(p[3])
			n, pe := strconv.ParseUint(port, 10, 16)
			if e != nil || net.ParseIP(host) == nil || pe != nil || n == 0 {
				return Facts{}, errors.New("invalid endpoint")
			}
			value := p[3]
			endpoint = &value
		}
		if peers == 0 {
			f.Endpoint = endpoint
		}
		if stamp != 0 && (f.LatestHandshake == nil || stamp > f.LatestHandshake.Unix()) {
			t := time.Unix(stamp, 0).UTC()
			f.LatestHandshake = &t
			f.Endpoint = endpoint
		}
		peers++
	}
	if len(interfaces) == 0 {
		return Facts{}, errors.New("invalid WireGuard dump")
	}
	if peers > 0 {
		f.RXBytes = &rxTotal
		f.TXBytes = &txTotal
	}
	return f, nil
}
func parseExit(raw string) (Facts, error) {
	var d struct{ IP, Country, City, Org string }
	if ip := net.ParseIP(strings.TrimSpace(raw)); ip != nil {
		value := ip.String()
		return Facts{ExitIP: &value}, nil
	}
	if json.Unmarshal([]byte(raw), &d) != nil || net.ParseIP(d.IP) == nil {
		return Facts{}, errors.New("invalid VPN exit")
	}
	f := Facts{}
	for _, field := range []struct {
		src string
		dst **string
	}{{d.IP, &f.ExitIP}, {d.Country, &f.Country}, {d.City, &f.City}, {d.Org, &f.Org}} {
		if field.src != "" {
			s := redact.Text(field.src)
			if len(s) > 256 {
				s = s[:256]
			}
			*field.dst = &s
		}
	}
	return f, nil
}
