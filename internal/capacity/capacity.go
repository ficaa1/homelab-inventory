// Package capacity builds the cross-host storage snapshot behind the
// /storage endpoint and the daily digest: one flat, normalized row per
// capacity answer — a PVE storage, a ZFS pool, or a real filesystem — with
// the delta against the previous stored run so the digest can say "this pool
// is filling up".
//
// The selection rules mirror the dashboard's: ZFS pools are reported at
// pool level (per-dataset statfs double-counts shared free space), and
// real filesystems exclude zfs mounts for the same reason. PVE storages are
// included because on host-b they are the actual capacity story (local-lvm is a
// thin pool, not a filesystem).
package capacity

import (
	"sort"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Kind classifies a capacity row.
type Kind string

const (
	// KindStorage is a Proxmox storage backend (a pool, directory or LVM-thin
	// target as configured in /etc/pve/storage.cfg).
	KindStorage Kind = "pve-storage"
	// KindPool is a ZFS pool, reported at pool level.
	KindPool Kind = "zpool"
	// KindFilesystem is a mounted real filesystem (ext4, btrfs, xfs, …).
	KindFilesystem Kind = "filesystem"
)

// Row is one capacity answer: how full one thing is, and how it moved.
type Row struct {
	Host string `json:"host"`
	Kind Kind   `json:"kind"`
	Name string `json:"name"`
	// Detail is the secondary label: the storage type, pool health or device.
	Detail string `json:"detail,omitempty"`

	UsedBytes  int64   `json:"used_bytes"`
	AvailBytes int64   `json:"avail_bytes"`
	TotalBytes int64   `json:"total_bytes"`
	UsedPct    float64 `json:"used_pct"`

	// HasPrev is false when there was no previous run to compare against.
	HasPrev        bool    `json:"has_prev"`
	DeltaPct       float64 `json:"delta_pct"`        // percentage points vs previous
	DeltaUsedBytes int64   `json:"delta_used_bytes"` // bytes vs previous
}

// Summary is the fleet-wide snapshot: every row, sorted by how full it is,
// plus the hosts that contributed.
type Summary struct {
	Rows  []Row    `json:"rows"`
	Hosts []string `json:"hosts"`
}

// Build assembles the snapshot from the latest inventories and one previous
// run per host (previous[host] may be nil for a first run).
func Build(latest []*inventory.Inventory, previous map[string]*inventory.Inventory) Summary {
	var out Summary
	seen := map[string]bool{}
	for _, inv := range latest {
		if inv == nil {
			continue
		}
		host := inv.Host.Hostname
		if !seen[host] {
			seen[host] = true
			out.Hosts = append(out.Hosts, host)
		}
		prev := previous[host]
		out.Rows = append(out.Rows, fromInventory(host, inv, prev)...)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if out.Rows[i].UsedPct != out.Rows[j].UsedPct {
			return out.Rows[i].UsedPct > out.Rows[j].UsedPct
		}
		return out.Rows[i].Host < out.Rows[j].Host
	})
	sort.Strings(out.Hosts)
	return out
}

// fromInventory extracts the capacity rows of one host and annotates each
// with its delta against prev.
func fromInventory(host string, inv, prev *inventory.Inventory) []Row {
	var rows []Row
	if inv.Proxmox != nil {
		for _, st := range inv.Proxmox.Storages {
			rows = append(rows, Row{
				Host: host, Kind: KindStorage, Name: st.Name, Detail: st.Type,
				UsedBytes: int64(st.UsedBytes), AvailBytes: int64(st.AvailBytes),
				TotalBytes: int64(st.TotalBytes), UsedPct: pct(st.UsedBytes, st.TotalBytes),
			})
		}
	}
	if inv.ZFS != nil {
		for _, p := range inv.ZFS.Pools {
			rows = append(rows, Row{
				Host: host, Kind: KindPool, Name: p.Name, Detail: p.Health,
				UsedBytes: int64(p.AllocBytes), AvailBytes: int64(p.FreeBytes),
				TotalBytes: int64(p.SizeBytes), UsedPct: float64(p.CapacityPct),
			})
		}
	}
	for _, f := range inv.Host.Filesystems {
		// The dashboard's selection: real mounts that are not a ZFS dataset
		// (pool free space is shared and already counted at pool level) and
		// that have a real size. total_bytes > 0 also drops pseudo-mounts
		// that statfs reports with no capacity.
		if f.Kind != inventory.FSKindReal || f.FSType == "zfs" || f.TotalBytes == 0 {
			continue
		}
		detail := f.Device
		if f.FSType != "" && detail != "" {
			detail += " (" + f.FSType + ")"
		}
		rows = append(rows, Row{
			Host: host, Kind: KindFilesystem, Name: f.MountPoint, Detail: detail,
			UsedBytes: int64(f.UsedBytes), AvailBytes: int64(f.FreeBytes),
			TotalBytes: int64(f.TotalBytes), UsedPct: f.UsedPct,
		})
	}
	if prev == nil {
		return rows
	}
	prevRows := fromInventory(host, prev, nil)
	byKey := map[string]Row{}
	for _, r := range prevRows {
		byKey[string(r.Kind)+"\x00"+r.Name] = r
	}
	for i := range rows {
		p, ok := byKey[string(rows[i].Kind)+"\x00"+rows[i].Name]
		if !ok {
			continue
		}
		rows[i].HasPrev = true
		rows[i].DeltaPct = rows[i].UsedPct - p.UsedPct
		rows[i].DeltaUsedBytes = rows[i].UsedBytes - p.UsedBytes
	}
	return rows
}

func pct(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}
