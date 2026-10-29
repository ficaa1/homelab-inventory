package alert

import (
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"time"
)

const DiskHealthStateName = "disk-health"

// DiskHealthBook retains evidence, independently of notification delivery state.
// Apply returns evaluation copies. Raw inventories and nil sections stay intact.
// Persist this book before publishing an evaluation to seen or notify.
type DiskHealthBook map[string]DiskHealthObservation

type DiskHealthObservation struct {
	At     time.Time             `json:"at"`
	Disks  *inventory.DiskHealth `json:"disks,omitempty"`
	Kernel *inventory.KernelIO   `json:"kernel,omitempty"`
}

func (b DiskHealthBook) Apply(invs []*inventory.Inventory) []*inventory.Inventory {
	out := make([]*inventory.Inventory, 0, len(invs))
	for _, inv := range invs {
		old := b[inv.Host.Hostname]
		next := old
		if !inv.CollectedAt.Before(old.At) {
			next = DiskHealthObservation{At: inv.CollectedAt, Disks: mergeDisks(inv.Disks, old.Disks), Kernel: mergeKernel(inv.KernelIO, old.Kernel, inv.CollectedAt)}
			b[inv.Host.Hostname] = next
		}
		copy := *inv
		copy.Disks, copy.KernelIO = next.Disks, next.Kernel
		out = append(out, &copy)
	}
	return out
}

func mergeDisks(current, old *inventory.DiskHealth) *inventory.DiskHealth {
	if current == nil || !current.Available {
		return old
	}
	result := *current
	result.Devices = append([]inventory.Disk{}, current.Devices...)
	by := map[string]int{}
	for i, d := range result.Devices {
		by[d.Name] = i
	}
	if old == nil {
		return &result
	}
	for _, prev := range old.Devices {
		i, found := by[prev.Name]
		if !found {
			result.Devices = append(result.Devices, prev)
			continue
		}
		d := &result.Devices[i]
		if d.ReadStatus != "" && d.ReadStatus != "ok" {
			status := d.ReadStatus
			*d = prev
			d.ReadStatus = status
			continue
		}
		// A missing metric is unknown even on an otherwise successful read.
		if d.Passed == nil {
			d.Passed = prev.Passed
		}
		if d.PendingSectors == nil {
			d.PendingSectors = prev.PendingSectors
		}
		if d.OfflineUncorrectable == nil {
			d.OfflineUncorrectable = prev.OfflineUncorrectable
		}
		if d.MediaErrors == nil {
			d.MediaErrors = prev.MediaErrors
		}
		if d.PercentageUsed == nil {
			d.PercentageUsed = prev.PercentageUsed
		}
	}
	return &result
}

func mergeKernel(current, old *inventory.KernelIO, at time.Time) *inventory.KernelIO {
	if current == nil || !current.Available {
		return old
	}
	result := *current
	result.Devices = append([]inventory.KernelIODevice{}, current.Devices...)
	if result.ObservedAt.IsZero() {
		result.ObservedAt = at
	}
	if old == nil {
		return &result
	}
	// A successful read in a new boot ends the previous boot's conditions.
	if current.BootID != "" && old.BootID != "" && current.BootID != old.BootID {
		return &result
	}
	sameBoot := current.BootID != "" && current.BootID == old.BootID
	if result.BootID == "" {
		result.BootID = old.BootID
	}
	by := map[string]int{}
	for i, d := range result.Devices {
		by[d.Device] = i
	}
	for _, prev := range old.Devices {
		if i, found := by[prev.Device]; found {
			if result.Devices[i].LastErrorAt.Before(prev.LastErrorAt) {
				prev.Count = result.Devices[i].Count
				result.Devices[i] = prev
			}
			if !sameBoot && result.Devices[i].Count == 0 {
				result.Devices[i].Count = 1
			}
			continue
		}
		if !sameBoot || prev.LastErrorAt.IsZero() || result.ObservedAt.Sub(prev.LastErrorAt) < KernelClearWindow {
			prev.Count = 0
			if prev.LastErrorAt.IsZero() || !sameBoot {
				prev.Count = 1
			}
			result.Devices = append(result.Devices, prev)
		}
	}
	return &result
}
