package capacity

import (
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// testInventory builds an inventory with the storage sections shaped like the
// real fleet: host-b carries Proxmox storages, host-c carries ZFS pools and
// zfs-mounted filesystems, and every host has plain real filesystems plus
// Docker layer mounts that must be filtered out.
func testInventory(host string, used uint64) *inventory.Inventory {
	return &inventory.Inventory{
		Host: inventory.Host{
			Hostname: host,
			Filesystems: []inventory.Filesystem{
				{Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
					TotalBytes: 100 << 30, UsedBytes: used, FreeBytes: (100 << 30) - used, UsedPct: float64(used) / float64(100<<30) * 100},
				{Device: "overlay", MountPoint: "/var/lib/docker/overlay2/x", FSType: "overlay", Kind: inventory.FSKindContainer,
					TotalBytes: 100 << 30, UsedBytes: 20 << 30, FreeBytes: 80 << 30, UsedPct: 20},
				{Device: "media", MountPoint: "/mnt/media", FSType: "zfs", Kind: inventory.FSKindReal,
					TotalBytes: 4 << 40, UsedBytes: 3 << 40, FreeBytes: 1 << 40, UsedPct: 75},
			},
		},
		Proxmox: &inventory.Proxmox{
			Storages: []inventory.Storage{
				{Name: "local-lvm", Type: "lvmthin", TotalBytes: 100 << 30, UsedBytes: used, AvailBytes: (100 << 30) - used},
				{Name: "backup-nfs", Type: "nfs", TotalBytes: 2 << 40, UsedBytes: 1 << 40, AvailBytes: 1 << 40},
			},
		},
		ZFS: &inventory.ZFS{
			Pools: []inventory.ZPool{
				{Name: "media", Health: "ONLINE", SizeBytes: 6 << 40, AllocBytes: 4 << 40, FreeBytes: 2 << 40, CapacityPct: 66},
			},
		},
	}
}

func TestBuildSelectsAndSortsRows(t *testing.T) {
	t.Parallel()
	latest := []*inventory.Inventory{testInventory("host-b", 46<<30)}
	sum := Build(latest, nil)

	if len(sum.Hosts) != 1 || sum.Hosts[0] != "host-b" {
		t.Fatalf("hosts = %v", sum.Hosts)
	}

	byName := map[string]Row{}
	for _, r := range sum.Rows {
		byName[string(r.Kind)+":"+r.Name] = r
	}

	// The overlay (container) mount and the zfs dataset must be filtered out;
	// the zfs pool is reported instead, at pool level.
	if _, ok := byName["filesystem:/"]; !ok {
		t.Error("real root filesystem missing")
	}
	if _, ok := byName["filesystem:/var/lib/docker/overlay2/x"]; ok {
		t.Error("container mount leaked into the summary")
	}
	if _, ok := byName["filesystem:/mnt/media"]; ok {
		t.Error("zfs dataset leaked into the summary as a filesystem")
	}
	if _, ok := byName["zpool:media"]; !ok {
		t.Error("zfs pool missing")
	}
	if _, ok := byName["pve-storage:local-lvm"]; !ok {
		t.Error("host-b storage missing")
	}

	// Rows sort by used_pct descending: the fullest thing in the fleet first.
	if sum.Rows[0].Kind != KindPool || sum.Rows[0].Name != "media" {
		t.Errorf("first row = %s %s, want the fullest (zpool media at 66%%)", sum.Rows[0].Kind, sum.Rows[0].Name)
	}
	for i := 1; i < len(sum.Rows); i++ {
		if sum.Rows[i].UsedPct > sum.Rows[i-1].UsedPct {
			t.Errorf("rows not sorted by used_pct: %v before %v", sum.Rows[i-1].UsedPct, sum.Rows[i].UsedPct)
			break
		}
	}
}

func TestBuildComputesDeltas(t *testing.T) {
	t.Parallel()
	latest := []*inventory.Inventory{testInventory("host-b", 50<<30)}
	prev := map[string]*inventory.Inventory{"host-b": testInventory("host-b", 46<<30)}
	sum := Build(latest, prev)

	var lvm *Row
	for i := range sum.Rows {
		if sum.Rows[i].Kind == KindStorage && sum.Rows[i].Name == "local-lvm" {
			lvm = &sum.Rows[i]
		}
	}
	if lvm == nil {
		t.Fatal("local-lvm row missing")
	}
	if !lvm.HasPrev {
		t.Error("HasPrev false with a previous run present")
	}
	if lvm.DeltaPct != 4.0 {
		t.Errorf("delta_pct = %v, want 4.0 points", lvm.DeltaPct)
	}
	if lvm.DeltaUsedBytes != 4<<30 {
		t.Errorf("delta_used_bytes = %d, want 4 GiB", lvm.DeltaUsedBytes)
	}
}

func TestBuildWithoutPreviousRun(t *testing.T) {
	t.Parallel()
	latest := []*inventory.Inventory{testInventory("host-b", 50<<30)}
	sum := Build(latest, nil)
	for _, r := range sum.Rows {
		if r.HasPrev {
			t.Errorf("%s %s claims a previous run that does not exist", r.Kind, r.Name)
		}
		if r.DeltaPct != 0 {
			t.Errorf("delta on first run: %s %s = %v", r.Kind, r.Name, r.DeltaPct)
		}
	}
}

func TestZeroTotalFilesystemExcluded(t *testing.T) {
	t.Parallel()
	inv := &inventory.Inventory{
		Host: inventory.Host{
			Hostname: "lxc",
			Filesystems: []inventory.Filesystem{
				// A pseudo-mount statfs reports with no capacity.
				{Device: "none", MountPoint: "/proc", FSType: "proc", Kind: inventory.FSKindReal,
					TotalBytes: 0, UsedBytes: 0, FreeBytes: 0},
				{Device: "/dev/vda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal,
					TotalBytes: 8 << 30, UsedBytes: 4 << 30, FreeBytes: 4 << 30, UsedPct: 50},
			},
		},
	}
	sum := Build([]*inventory.Inventory{inv}, nil)
	if len(sum.Rows) != 1 || sum.Rows[0].Name != "/" {
		t.Errorf("rows = %+v, want only the real filesystem", sum.Rows)
	}
}
