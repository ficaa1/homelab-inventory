package main

import (
	"context"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/seen"
)

type diskMessages struct{ messages []notify.Message }

func (c *diskMessages) Send(_ context.Context, m notify.Message) error {
	c.messages = append(c.messages, m)
	return nil
}

func TestDiskHealthUnknownAndQuietRecoveryTransitions(t *testing.T) {
	s := testServer(t, "")
	base := time.Now().UTC()
	failed, healthy := false, true
	one, zero := uint64(1), uint64(0)
	cap := &diskMessages{}
	notifier := &notify.Notifier{State: s.store, Sender: cap, Min: alert.LevelWarning, Log: s.log}
	ledger := &seen.Book{}
	disk := inventory.Disk{Name: "/dev/sdb", ReadStatus: "ok", Passed: &failed, PendingSectors: &one}
	inv := &inventory.Inventory{CollectedAt: base, Host: inventory.Host{Hostname: "host-a"}}
	run := func(offset time.Duration, count int) {
		t.Helper()
		inv.CollectedAt = base.Add(offset)
		rep, err := s.evaluateAlerts([]*inventory.Inventory{inv}, inv.CollectedAt)
		if err != nil {
			t.Fatal(err)
		}
		alerts := []alert.Alert{}
		for _, a := range rep.Alerts {
			if a.Rule == "host.kernel.io_errors" || a.Rule == "host.disk.smart.failed" || a.Rule == "host.disk.smart.errors" {
				alerts = append(alerts, a)
			}
		}
		if len(alerts) != count {
			t.Fatalf("at %s got %+v want %d", offset, alerts, count)
		}
		ledger.Stamp(alerts, inv.CollectedAt)
		if _, err = notifier.Run(context.Background(), alerts, inv.CollectedAt); err != nil {
			t.Fatal(err)
		}
	}
	run(0, 0) // Silent notifier seed.
	inv.Disks = &inventory.DiskHealth{Available: true, Devices: []inventory.Disk{disk}}
	inv.KernelIO = &inventory.KernelIO{Available: true, BootID: "boot-a", ObservedAt: base, Devices: []inventory.KernelIODevice{{Device: "sdb", Count: 1, LastErrorAt: base}}}
	run(time.Minute, 3)
	if len(cap.messages) != 1 {
		t.Fatal(cap.messages)
	}
	inv.Disks = &inventory.DiskHealth{Available: true, Devices: []inventory.Disk{{Name: disk.Name, ReadStatus: "failed"}}}
	inv.KernelIO = &inventory.KernelIO{Available: false, Devices: []inventory.KernelIODevice{}}
	run(3*time.Hour, 3)
	// Unavailable data cannot clear even beyond the quiet interval.
	inv.Disks = nil
	inv.KernelIO = nil
	run(8*time.Hour, 3)
	if len(cap.messages) != 1 {
		t.Fatalf("false transition: %+v", cap.messages)
	}
	// Known healthy data clears at the next successful observation.
	inv.Disks = &inventory.DiskHealth{Available: true, Devices: []inventory.Disk{{Name: disk.Name, ReadStatus: "ok", Passed: &healthy, PendingSectors: &zero}}}
	inv.KernelIO = &inventory.KernelIO{Available: true, BootID: "boot-a", ObservedAt: base.Add(9 * time.Hour), Devices: []inventory.KernelIODevice{}}
	run(9*time.Hour, 0)
	if len(cap.messages) != 2 {
		t.Fatal(cap.messages)
	}
	// Raw collection data was never filled with held metrics.
	if len(inv.KernelIO.Devices) != 0 {
		t.Fatal("raw kernel observation mutated")
	}
}
