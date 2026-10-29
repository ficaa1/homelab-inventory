package alert

import (
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"testing"
	"time"
)

func TestUnexpectedReadOnly(t *testing.T) {
	root := inventory.Filesystem{Device: "/dev/sda1", MountPoint: "/", FSType: "ext4", Kind: inventory.FSKindReal, Options: "ro,relatime"}
	with := func(f func(*inventory.Filesystem)) inventory.Filesystem { c := root; f(&c); return c }
	for _, tc := range []struct {
		name string
		fs   inventory.Filesystem
		virt string
		want bool
	}{
		{"failed remount", root, "kvm", true},
		{"option substring", with(func(f *inventory.Filesystem) { f.Options = "rw,errors=remount-ro" }), "kvm", false},
		{"pseudo path", with(func(f *inventory.Filesystem) { f.MountPoint = "/snap/data" }), "kvm", false},
		{"fstab ro", with(func(f *inventory.Filesystem) { f.ConfiguredReadOnly = true }), "kvm", false},
		{"bind", with(func(f *inventory.Filesystem) { f.BindMount = true }), "kvm", false},
		{"lxc bind", with(func(f *inventory.Filesystem) { f.MountPoint = "/data" }), "lxc", false},
	} {
		if got := unexpectedReadOnly(tc.fs, tc.virt); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}

func TestDiskHealthRulesAndClear(t *testing.T) {
	failed, n, wear := false, uint64(1), uint64(90)
	inv := &inventory.Inventory{CollectedAt: time.Now(), Host: inventory.Host{Hostname: "host-a", Filesystems: []inventory.Filesystem{{Device: "/dev/sdb", MountPoint: "/photos", FSType: "ext4", Kind: inventory.FSKindReal, Options: "ro"}}}, Disks: &inventory.DiskHealth{Available: true, Devices: []inventory.Disk{{Name: "/dev/sdb", Passed: &failed, PendingSectors: &n, PercentageUsed: &wear}}}, KernelIO: &inventory.KernelIO{Available: true, Devices: []inventory.KernelIODevice{{Device: "sdb", Count: 2}, {Device: "unknown", Count: 1}}}}
	count := func() (c int) {
		for _, a := range Evaluate([]*inventory.Inventory{inv}, DefaultThresholds(), time.Now()).Alerts {
			switch a.Rule {
			case "host.filesystem.readonly", "host.kernel.io_errors", "host.disk.smart.failed", "host.disk.smart.errors", "host.disk.nvme.wear":
				c++
			}
		}
		return
	}
	if c := count(); c != 6 {
		t.Fatal(c)
	}
	inv.Host.Filesystems[0].Options = "rw"
	failed, n, wear = true, 0, 89
	inv.KernelIO.Devices = []inventory.KernelIODevice{}
	if c := count(); c != 0 {
		t.Fatal(c)
	}
}
