package alert

import (
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"testing"
	"time"
)

func TestKernelQuietWindowBootAndClock(t *testing.T) {
	at := time.Now().UTC()
	for _, tc := range []struct {
		name, boot string
		available  bool
		offset     time.Duration
		want       bool
	}{
		{"short clean window", "boot", true, time.Hour, true},
		{"six hours same boot", "boot", true, 6 * time.Hour, false},
		{"unavailable", "boot", false, 24 * time.Hour, true},
		{"new boot", "next", true, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			book := DiskHealthBook{}
			inv := &inventory.Inventory{CollectedAt: at, Host: inventory.Host{Hostname: "host-a"}, KernelIO: &inventory.KernelIO{Available: true, BootID: "boot", ObservedAt: at, Devices: []inventory.KernelIODevice{{Device: "sdb", Count: 1, LastErrorAt: at}}}}
			book.Apply([]*inventory.Inventory{inv})
			inv.CollectedAt = at.Add(tc.offset)
			inv.KernelIO = &inventory.KernelIO{Available: tc.available, BootID: tc.boot, ObservedAt: inv.CollectedAt, Devices: []inventory.KernelIODevice{}}
			effective := book.Apply([]*inventory.Inventory{inv})
			found := false
			for _, a := range Evaluate(effective, DefaultThresholds(), inv.CollectedAt).Alerts {
				found = found || a.Rule == "host.kernel.io_errors"
			}
			if found != tc.want {
				t.Fatal(found, tc.want)
			}
		})
	}
}

func TestSMARTUnknownReadsHoldEvidence(t *testing.T) {
	failed, one := false, uint64(1)
	old := &inventory.DiskHealth{Available: true, Devices: []inventory.Disk{{Name: "/dev/sda", Passed: &failed, PendingSectors: &one}}}
	for _, current := range []*inventory.DiskHealth{nil, {Available: false}, {Available: true, Devices: []inventory.Disk{{Name: "/dev/sda", ReadStatus: "standby"}}}, {Available: true, Devices: []inventory.Disk{{Name: "/dev/sda", ReadStatus: "failed"}}}} {
		m := mergeDisks(current, old)
		if len(m.Devices) != 1 || m.Devices[0].Passed == nil || *m.Devices[0].Passed || *m.Devices[0].PendingSectors != 1 {
			t.Fatal(m)
		}
	}
}
