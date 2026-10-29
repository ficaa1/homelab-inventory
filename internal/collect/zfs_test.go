package collect

import (
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestParsePoolStatusHealthy(t *testing.T) {
	t.Parallel()
	out := "  pool: secure\n" +
		" state: ONLINE\n" +
		"  scan: scrub repaired 0B in 04:12:33 with 0 errors on Sun Jul 13 04:12:35 2026\n" +
		"config:\n" +
		"\n" +
		"\tNAME        STATE     READ WRITE CKSUM\n" +
		"\tsecure      ONLINE       0     0     0\n" +
		"\t  mirror-0  ONLINE       0     0     0\n" +
		"\t    sda2    ONLINE       0     0     0\n" +
		"\t    sdb2    ONLINE       0     0     0\n" +
		"\n" +
		"errors: No known data errors\n"

	var p inventory.ZPool
	parsePoolStatus(out, &p)

	if !strings.HasPrefix(p.Scan, "scrub repaired 0B") {
		t.Errorf("scan = %q", p.Scan)
	}
	if p.Errors != "No known data errors" {
		t.Errorf("errors = %q", p.Errors)
	}
	if p.Status != "" || p.Action != "" {
		t.Errorf("a healthy pool should have no status/action: %q / %q", p.Status, p.Action)
	}
	if len(p.Vdevs) != 4 {
		t.Fatalf("got %d vdevs, want 4 (header excluded): %+v", len(p.Vdevs), p.Vdevs)
	}
	// Indentation encodes the tree: pool, mirror, then its two disks.
	wantDepth := []int{0, 1, 2, 2}
	for i, w := range wantDepth {
		if p.Vdevs[i].Depth != w {
			t.Errorf("vdev %q depth = %d, want %d", p.Vdevs[i].Name, p.Vdevs[i].Depth, w)
		}
	}
	if p.Vdevs[0].Name != "secure" || p.Vdevs[1].Name != "mirror-0" {
		t.Errorf("vdev names wrong: %+v", p.Vdevs)
	}
}

func TestParsePoolStatusDegraded(t *testing.T) {
	t.Parallel()
	// A degraded pool: multi-line status and action advisories, a resilver in
	// the scan line, and non-zero error counters.
	out := "  pool: tank\n" +
		" state: DEGRADED\n" +
		"status: One or more devices could not be opened.  Sufficient replicas\n" +
		"\texist for the pool to continue functioning in a degraded state.\n" +
		"action: Attach the missing device and online it using 'zpool online'.\n" +
		"   see: https://openzfs.github.io/openzfs-docs/msg/ZFS-8000-2Q\n" +
		"  scan: resilver in progress since Fri Jul 24 21:00:00 2026\n" +
		"\t1.20T scanned at 400M/s, 900G issued at 300M/s, 2.40T total\n" +
		"config:\n" +
		"\n" +
		"\tNAME        STATE     READ WRITE CKSUM\n" +
		"\ttank        DEGRADED     0     0     0\n" +
		"\t  mirror-0  DEGRADED     0     0     0\n" +
		"\t    sda2    ONLINE       0     0     2\n" +
		"\t    sdb2    UNAVAIL      0     0     0  cannot open\n" +
		"\n" +
		"errors: No known data errors\n"

	var p inventory.ZPool
	parsePoolStatus(out, &p)

	// A continuation line must join the advisory it belongs to, not start a
	// new one or get dropped.
	if !strings.Contains(p.Status, "degraded state") {
		t.Errorf("status continuation lost: %q", p.Status)
	}
	if !strings.Contains(p.Scan, "2.40T total") {
		t.Errorf("scan continuation lost: %q", p.Scan)
	}
	if !strings.HasPrefix(p.Action, "Attach the missing device") {
		t.Errorf("action = %q", p.Action)
	}

	byName := map[string]inventory.Vdev{}
	for _, v := range p.Vdevs {
		byName[v.Name] = v
	}
	if got := byName["sda2"]; got.Cksum != 2 {
		t.Errorf("checksum errors on sda2 = %d, want 2", got.Cksum)
	}
	if got := byName["sdb2"]; got.State != "UNAVAIL" || got.Note != "cannot open" {
		t.Errorf("failed device parsed wrong: %+v", got)
	}
}

func TestParsePoolStatusIgnoresDeviceNamesAsKeys(t *testing.T) {
	t.Parallel()
	// A device path containing a colon must not be mistaken for an advisory.
	out := "  pool: rpool\n state: ONLINE\nconfig:\n\n" +
		"\tNAME                        STATE     READ WRITE CKSUM\n" +
		"\trpool                       ONLINE       0     0     0\n" +
		"\t  wwn-0x5000c500:abcd-part3  ONLINE      0     0     0\n" +
		"\nerrors: No known data errors\n"

	var p inventory.ZPool
	parsePoolStatus(out, &p)
	if len(p.Vdevs) != 2 {
		t.Errorf("got %d vdevs, want 2: %+v", len(p.Vdevs), p.Vdevs)
	}
	if p.Errors != "No known data errors" {
		t.Errorf("errors = %q", p.Errors)
	}
}

func TestParsePctAndUint(t *testing.T) {
	t.Parallel()
	if got := parsePct("43"); got != 43 {
		t.Errorf("parsePct(43) = %d", got)
	}
	// -p suppresses the sign on most versions but not all.
	if got := parsePct("43%"); got != 43 {
		t.Errorf("parsePct(43%%) = %d", got)
	}
	if got := parseUint("5860522584064"); got != 5860522584064 {
		t.Errorf("parseUint = %d", got)
	}
	if got := parseUint("-"); got != 0 {
		t.Errorf("parseUint(-) = %d, want 0", got)
	}
}

func TestDashToEmpty(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"-": "", "none": "", "lz4": "lz4", "off": "off"} {
		if got := dashToEmpty(in); got != want {
			t.Errorf("dashToEmpty(%q) = %q, want %q", in, got, want)
		}
	}
}
