package alert

import (
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"strings"
	"time"
)

// KernelClearWindow requires a successful same-boot observation six hours after the last error.
const KernelClearWindow = 6 * time.Hour

func hasMountOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}
func unexpectedReadOnly(fs inventory.Filesystem, virtualization string) bool {
	if fs.Kind != inventory.FSKindReal || !strings.HasPrefix(fs.Device, "/dev/") || fs.ConfiguredReadOnly || !hasMountOption(fs.Options, "ro") {
		return false
	}
	switch fs.FSType {
	case "ext4", "xfs", "btrfs":
	default:
		return false
	}
	for _, p := range []string{"/proc", "/sys", "/run", "/snap"} {
		if fs.MountPoint == p || strings.HasPrefix(fs.MountPoint, p+"/") {
			return false
		}
	}
	if fs.BindMount || hasMountOption(fs.Options, "bind") || hasMountOption(fs.Options, "rbind") {
		return false
	}
	// Old inventories lack mountinfo. LXC non-root paths may be intentional
	// host bind mounts; the container's own root still catches remount failures.
	if virtualization == "lxc" && fs.MountPoint != "/" {
		return false
	}
	return true
}
func (e *evaluator) diskHealth(inv *inventory.Inventory) {
	for _, fs := range inv.Host.Filesystems {
		if unexpectedReadOnly(fs, inv.Host.Virtualization) {
			e.add(Alert{Rule: "host.filesystem.readonly", Level: LevelCritical, Subject: fs.MountPoint, Message: fmt.Sprintf("%s is read-only", fs.MountPoint), Detail: fs.Device})
		}
	}
	if k := inv.KernelIO; k != nil && k.Available {
		for _, d := range k.Devices {
			observed := k.ObservedAt
			if observed.IsZero() {
				observed = inv.CollectedAt
			}
			if d.Count > 0 || (!d.LastErrorAt.IsZero() && observed.Sub(d.LastErrorAt) < KernelClearWindow) {
				level := LevelCritical
				if d.Device == "unknown" {
					level = LevelWarning
				}
				e.add(Alert{Rule: "host.kernel.io_errors", Level: level, Subject: d.Device, Value: float64(d.Count), Message: fmt.Sprintf("%s kernel storage errors", d.Device), Detail: d.NewestSample})
			}
		}
	}
	if inv.Disks == nil || !inv.Disks.Available {
		return
	}
	for _, d := range inv.Disks.Devices {
		if d.Passed != nil && !*d.Passed {
			e.add(Alert{Rule: "host.disk.smart.failed", Level: LevelCritical, Subject: d.Name, Message: fmt.Sprintf("%s SMART health failed", d.Name), Detail: d.Model})
		}
		if nonzero(d.PendingSectors) || nonzero(d.OfflineUncorrectable) || nonzero(d.MediaErrors) {
			e.add(Alert{Rule: "host.disk.smart.errors", Level: LevelWarning, Subject: d.Name, Message: fmt.Sprintf("%s SMART error counters are nonzero", d.Name), Detail: fmt.Sprintf("pending=%d uncorrectable=%d media=%d", metric(d.PendingSectors), metric(d.OfflineUncorrectable), metric(d.MediaErrors))})
		}
		if d.PercentageUsed != nil && *d.PercentageUsed >= 90 {
			e.add(Alert{Rule: "host.disk.nvme.wear", Level: LevelWarning, Subject: d.Name, Value: float64(*d.PercentageUsed), Threshold: 90, Unit: UnitPercent, Message: fmt.Sprintf("%s NVMe endurance used: %d%%", d.Name, *d.PercentageUsed)})
		}
	}
}
func metric(n *uint64) uint64 {
	if n == nil {
		return 0
	}
	return *n
}
func nonzero(n *uint64) bool { return metric(n) > 0 }
