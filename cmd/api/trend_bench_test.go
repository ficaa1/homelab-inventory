package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

// trendBenchRuns is roughly what a host holds at the trend endpoint's sample
// cap: the endpoint reads 200 of them.
const trendBenchRuns = 200

// benchInventory is shaped like a busy Ubuntu host: storage and filesystem
// series for the trend, plus the bulk (listeners, containers, units) that makes
// a real run a few hundred kilobytes of JSON.
func benchInventory(host string, at time.Time, step int) *inventory.Inventory {
	used := func(base uint64) uint64 { return base + uint64(step)*(1<<26) }
	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		CollectedAt:   at,
		Host:          inventory.Host{Hostname: host, OS: inventory.OSRelease{Pretty: "Ubuntu 22.04.4 LTS"}},
	}
	for i := 0; i < 12; i++ {
		inv.Host.Filesystems = append(inv.Host.Filesystems, inventory.Filesystem{
			Device: fmt.Sprintf("/dev/sd%c1", 'a'+i), MountPoint: fmt.Sprintf("/mnt/disk%02d", i),
			FSType: "ext4", Kind: inventory.FSKindReal,
			TotalBytes: 4 << 40, UsedBytes: used(1 << 40), FreeBytes: 3 << 40, UsedPct: 25,
		})
	}
	inv.ZFS = &inventory.ZFS{Available: true}
	for i := 0; i < 3; i++ {
		inv.ZFS.Pools = append(inv.ZFS.Pools, inventory.ZPool{
			Name: fmt.Sprintf("pool%d", i), Health: "ONLINE",
			SizeBytes: 8 << 40, AllocBytes: used(2 << 40), FreeBytes: 6 << 40, CapacityPct: 25,
		})
		inv.ZFS.Datasets = append(inv.ZFS.Datasets, inventory.ZDataset{Name: fmt.Sprintf("pool%d/data", i)})
	}
	inv.Proxmox = &inventory.Proxmox{Available: true}
	for i := 0; i < 9; i++ {
		inv.Proxmox.Storages = append(inv.Proxmox.Storages, inventory.Storage{
			Name: fmt.Sprintf("store%d", i), Type: "dir", Active: true, Enabled: true,
			TotalBytes: 2 << 40, UsedBytes: used(1 << 39),
		})
	}
	inv.Network = &inventory.Network{Source: "ss", EphemeralRange: [2]int{32768, 60999}}
	for i := 0; i < 200; i++ {
		inv.Network.Listeners = append(inv.Network.Listeners, inventory.Listener{
			Protocol: "tcp", Family: "inet", Address: "0.0.0.0", Port: 1000 + i,
			Process: "service", Command: "/usr/bin/service --config /etc/service/config.yaml --verbose",
		})
	}
	inv.Docker = &inventory.Docker{Available: true, Version: "27.0.3"}
	for i := 0; i < 150; i++ {
		c := inventory.Container{
			ID: fmt.Sprintf("%012x", i+step), Name: fmt.Sprintf("app-%02d", i),
			Image: "ghcr.io/example/app:1.2.3", State: "running", Status: "Up 3 days",
			Ports:  []string{"0.0.0.0:8080->8080/tcp"},
			Labels: map[string]string{"com.docker.compose.project": "stack", "com.docker.compose.service": "app"},
		}
		for m := 0; m < 3; m++ {
			c.Mounts = append(c.Mounts, inventory.Mount{Type: "bind", Source: "/srv/app/data", Destination: "/data", ReadWrite: true})
		}
		inv.Docker.Containers = append(inv.Docker.Containers, c)
	}
	inv.Systemd = &inventory.Systemd{Available: true, Version: "249"}
	for i := 0; i < 300; i++ {
		inv.Systemd.Units = append(inv.Systemd.Units, inventory.Unit{
			Name: fmt.Sprintf("unit-%03d.service", i), Load: "loaded", Active: "active", Sub: "running",
			Description: "A service that does something useful for the homelab", UnitFileState: "enabled",
		})
	}
	return inv
}

// trendBenchStore holds one host with trendBenchRuns runs, 25 minutes apart, so
// the forecast gates have a real window to work with.
func trendBenchStore(b *testing.B) *store.Store {
	b.Helper()
	st, err := store.Open(filepath.Join(b.TempDir(), "data"))
	if err != nil {
		b.Fatal(err)
	}
	start := time.Now().UTC().Add(-time.Duration(trendBenchRuns) * 25 * time.Minute)
	for i := 0; i < trendBenchRuns; i++ {
		at := start.Add(time.Duration(i) * 25 * time.Minute)
		if _, err := st.Save(benchInventory("host-a", at, i)); err != nil {
			b.Fatal(err)
		}
	}
	records, err := st.History("host-a")
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("%d runs, about %d KiB each", len(records), records[0].SizeBytes/1024)
	return st
}

func trendRequest(b *testing.B, h http.Handler) {
	b.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts/host-a/trend", nil))
	if rec.Code != http.StatusOK {
		b.Fatalf("trend: %d %s", rec.Code, rec.Body.String())
	}
}

// BenchmarkHostTrendCold is a request against a server with nothing cached:
// every sampled run is read and parsed, which is what each request costs
// without a cache.
func BenchmarkHostTrendCold(b *testing.B) {
	st := trendBenchStore(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := &server{store: st, log: log.New(io.Discard, "", 0)}
		trendRequest(b, s.routes())
	}
}

// BenchmarkHostTrendWarm is the same request on one long-lived server, the way
// the dashboard hits it: the first request fills whatever a cache keeps, and
// the timed requests run against that.
func BenchmarkHostTrendWarm(b *testing.B) {
	st := trendBenchStore(b)
	h := (&server{store: st, log: log.New(io.Discard, "", 0)}).routes()
	trendRequest(b, h)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		trendRequest(b, h)
	}
}
