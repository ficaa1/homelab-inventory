package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/capacity"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func TestStorageEndpoint(t *testing.T) {
	t.Parallel()
	st, err := store.Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: st, log: log.New(io.Discard, "", 0)}

	// Two runs so the delta path is exercised end to end through the store.
	first := capacityTestInventory("host-b", 46<<30)
	second := capacityTestInventory("host-b", 50<<30)
	for _, inv := range []*inventory.Inventory{first, second} {
		if _, err := st.Save(inv); err != nil {
			t.Fatal(err)
		}
	}

	h := s.routes()
	rec := doRequest(t, h, http.MethodGet, "/storage", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /storage = %d: %s", rec.Code, rec.Body.String())
	}
	var sum capacity.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if len(sum.Rows) == 0 {
		t.Fatal("no storage rows")
	}
	var lvm *capacity.Row
	for i := range sum.Rows {
		if sum.Rows[i].Kind == capacity.KindStorage && sum.Rows[i].Name == "local-lvm" {
			lvm = &sum.Rows[i]
		}
	}
	if lvm == nil {
		t.Fatal("local-lvm row missing from /storage")
	}
	if !lvm.HasPrev || lvm.DeltaPct != 4.0 {
		t.Errorf("delta through the store = has_prev %v delta %v, want true and 4.0", lvm.HasPrev, lvm.DeltaPct)
	}
}

func capacityTestInventory(host string, used uint64) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host: inventory.Host{
			Hostname: host,
			Filesystems: []inventory.Filesystem{
				{Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
					TotalBytes: 100 << 30, UsedBytes: used, FreeBytes: (100 << 30) - used, UsedPct: float64(used) / float64(100<<30) * 100},
			},
		},
		Proxmox: &inventory.Proxmox{
			Storages: []inventory.Storage{
				{Name: "local-lvm", Type: "lvmthin", TotalBytes: 100 << 30, UsedBytes: used, AvailBytes: (100 << 30) - used},
			},
		},
	}
}
