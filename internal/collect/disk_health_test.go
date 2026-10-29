package collect

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
)

func TestParseSMART(t *testing.T) {
	attr := func(name, str string) string {
		return `{"smart_status":{"passed":true},"ata_smart_attributes":{"table":[{"id":198,"name":"` + name + `","raw":{"value":281474976710658,"string":"` + str + `"}}]}}`
	}
	for _, tc := range []struct {
		name, raw string
		want      *uint64
	}{
		{"packed 198", attr("Offline_Uncorrectable", "2 (0 0 1)"), ptr(2)},
		{"198 is a host-reads counter", attr("Host_Reads_GiB", "80000"), nil},
		{"unfamiliar string stays unknown", attr("Offline_Uncorrectable", "vendor: 2/1"), nil},
	} {
		d, _, err := parseSMART("/dev/sda", tc.raw)
		if err != nil || (tc.want == nil) != (d.OfflineUncorrectable == nil) || (tc.want != nil && *d.OfflineUncorrectable != *tc.want) {
			t.Errorf("%s: %+v %v", tc.name, d, err)
		}
	}
	d, ok, err := parseSMART("/dev/sda", `{"smart_status":{}}`)
	if err != nil || !ok || d.Passed != nil {
		t.Fatal("unknown health became a verdict", d)
	}
	if _, ok, _ := parseSMART("/dev/sda", `{"smartctl":{"exit_status":2}}`); ok {
		t.Fatal("exit-only output accepted")
	}
	if _, _, err := parseSMART("/dev/sda", "bad"); err == nil {
		t.Fatal("malformed JSON")
	}
}

func TestParseKernelIO(t *testing.T) {
	line := func(msg string) string {
		return `{"MESSAGE":` + strconv.Quote(msg) + `,"__REALTIME_TIMESTAMP":"1"}` + "\n"
	}
	for _, msg := range []string{
		"blk_update_request: I/O error, dev sr0, sector 0",
		"Buffer I/O error on dev sr0, logical block 1",
		"blk_update_request: I/O error, dev fd0, sector 0",
		"I/O error, dev loop3, sector 8",
		"EXT4-fs warning (device sda1): maximal mount count reached",
		"usb-storage 2-1: device disconnected",
		"amdgpu: GPU ring I/O error",
	} {
		if ds, ok := parseKernelIO(line(msg)); !ok || len(ds) != 0 {
			t.Errorf("matched %q: %+v", msg, ds)
		}
	}
	ds, ok := parseKernelIO(line("I/O error, dev sdb, sector 4") + line("EXT4-fs error (device sdb1): failed write"))
	if !ok || len(ds) != 2 {
		t.Fatalf("%+v", ds)
	}
	if _, ok = parseKernelIO("[truncated]"); ok {
		t.Fatal("partial output accepted")
	}
}

func TestSMARTCollectorStandbyAndFailure(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "--scan" ]; then printf '/dev/sda -d sat\n/dev/sdb -d sat\n'; exit 0; fi
if [ "$6" = "/dev/sda" ]; then
 printf '{"smartctl":{"exit_status":2,"messages":[{"string":"Device is in STANDBY mode, exit(2)"}]}}'; exit 2
fi
printf '{"smart_status":{"passed":false}}'; exit 8
`
	if err := os.WriteFile(filepath.Join(dir, "smartctl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	inv := &inventory.Inventory{}
	DiskHealthCollector{}.Collect(context.Background(), &Env{RootFS: t.TempDir(), Run: sysexec.New()}, inv)
	if len(inv.Disks.Devices) != 2 || inv.Disks.Devices[0].ReadStatus != "standby" || inv.Disks.Devices[0].Passed != nil || inv.Disks.Devices[1].Passed == nil || *inv.Disks.Devices[1].Passed {
		t.Fatalf("%+v", inv.Disks)
	}
}

func ptr(n uint64) *uint64 { return &n }
