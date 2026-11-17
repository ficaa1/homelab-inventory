package cve

import (
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"os"
	"testing"
	"time"
)

func TestEcosystemsAndEOL(t *testing.T) {
	for _, tt := range []struct{ id, v, want string }{
		{"debian", "12.1", "Debian:12"}, {"ubuntu", "22.04", "Ubuntu:22.04:LTS"}, {"ubuntu", "24.10", "Ubuntu:24.10"}, {"ubuntu", "23.04", "Ubuntu:23.04"}, {"alpine", "3.20.3", "Alpine:v3.20"}, {"rocky", "9.4", "Rocky Linux:9"}, {"almalinux", "8.10", "AlmaLinux:8"}, {"fedora", "40", ""}, {"debian", "", ""}, {"ubuntu", "22", ""}, {"ubuntu", "22.99", ""},
	} {
		t.Run(tt.id+tt.v, func(t *testing.T) {
			got, reason := ecosystem(plugin.PackageDistro{ID: tt.id, VersionID: tt.v})
			if got != tt.want || (got == "") != (reason != "") {
				t.Fatalf("got %q, %q", got, reason)
			}
		})
	}
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	if got := distroEOL("Debian:10", now); got == nil || got.Date != "2024-06-30" {
		t.Fatal(got)
	}
	if distroEOL("Debian:12", now) != nil || distroEOL("Ubuntu:22.04:LTS", now) != nil {
		t.Fatal("supported release marked EOL")
	}
	if distroEOL("Ubuntu:20.04:LTS", now) == nil || distroEOL("Alpine:v3.19", now) == nil {
		t.Fatal("missing EOL")
	}
}
func TestDebianVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"1:1.0-1", "2.0-1", 1}, {"2:0", "1:99", 1}, {"1.0~rc1-1", "1.0-1", -1}, {"1.0~~", "1.0~", -1}, {"1.0", "1.0-0", 0},
		{"3.0.11-1~deb12u2", "3.0.15-1~deb12u1", -1}, {"1.0-2", "1.0-10", -1}, {"1.01", "1.1", 0}, {"1.0+b1", "1.0", 1}, {"1.0a", "1.0+", -1}, {"0:1", "1", 0},
	} {
		if got := debianCompare(tt.a, tt.b); sign(got) != tt.want || sign(debianCompare(tt.b, tt.a)) != -tt.want {
			t.Errorf("compare %q %q = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
func TestAPKVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"3.1.4-r0", "3.1.4-r1", -1}, {"1.0_alpha1", "1.0_beta1", -1}, {"1.0_beta1", "1.0_pre1", -1}, {"1.0_pre1", "1.0_rc1", -1}, {"1.0_rc1", "1.0", -1},
		{"1.0", "1.0_p1", -1}, {"1.0_p1", "1.0_p2", -1}, {"1.0", "1.0-r0", -1}, {"1.0-r9", "1.0-r10", -1}, {"1.01", "1.1", -1}, {"1.1a", "1.1b", -1}, {"1.1a", "1.1", 1}, {"2.0.0", "2.0", 1}, {"1.0_git1", "1.0_p1", -1}, {"1.0", "1.0", 0},
	} {
		if got := apkCompare(tt.a, tt.b); sign(got) != tt.want || sign(apkCompare(tt.b, tt.a)) != -tt.want {
			t.Errorf("compare %q %q = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
func TestRPMVersions(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"1:3.0.1-41.el9_0", "1:3.0.1-43.el9_0", -1}, {"1.0~rc1", "1.0", -1}, {"1.0^git1", "1.0", 1}, {"1.0^git1", "1.0.1", -1}, {"1.9", "1.10", -1}, {"1:1", "9", 1}, {"1.01", "1.1", 0}} {
		if got := rpmCompare(tt.a, tt.b); sign(got) != tt.want {
			t.Errorf("%q %q: %d", tt.a, tt.b, got)
		}
	}
}
func sign(v int) int {
	if v < 0 {
		return -1
	}
	if v > 0 {
		return 1
	}
	return 0
}
func TestCVSSv3(t *testing.T) {
	for _, tt := range []struct {
		vector string
		score  float64
	}{
		{"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8}, {"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:N/A:H", 9.1},
		{"CVSS:3.0/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", 9.8}, {"CVSS:3.1/AV:N/AC:L/PR:L/UI:N/S:C/C:L/I:L/A:N", 6.4},
		{"CVSS:3.1/AV:N/AC:H/PR:H/UI:R/S:U/C:L/I:L/A:N", 3.1}, {"CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:N/I:N/A:N", 0},
	} {
		got := cvssV3(tt.vector)
		if got == nil || got.Score != tt.score {
			t.Errorf("%s = %+v, want %v", tt.vector, got, tt.score)
		}
	}
	for _, v := range []string{"CVSS:4.0/AV:N", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H", "CVSS:3.1/AV:X/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H", "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H/C:L"} {
		if cvssV3(v) != nil {
			t.Errorf("accepted invalid vector %s", v)
		}
	}
}

func TestVendorSeverityAndFixedBranches(t *testing.T) {
	for _, tt := range []struct {
		eco, priority, urgency string
		want, source           string
	}{
		{"Ubuntu:22.04:LTS", "negligible", "", "low", "ubuntu"},
		{"Ubuntu:22.04:LTS", "critical", "", "critical", "ubuntu"},
		{"Debian:12", "", "unimportant", "low", "debian"},
		{"Debian:12", "", "not yet assigned", "unknown", "none"},
	} {
		v := Vulnerability{VulnRow: VulnRow{Severity: "unknown", SeveritySource: "none"}}
		applySeverity(&v, nil, tt.eco, vendorInfo{Priority: tt.priority, Urgency: tt.urgency})
		finishSeverity(&v)
		if v.Severity != tt.want || v.SeveritySource != tt.source {
			t.Fatalf("%+v: %+v", tt, v)
		}
	}
	// A distro rating outranks a higher NVD score; CVSS fills in when the distro has none.
	v := Vulnerability{}
	applySeverity(&v, []severity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}}, "Ubuntu:22.04:LTS", vendorInfo{Priority: "negligible"})
	finishSeverity(&v)
	if v.Severity != "low" || v.SeveritySource != "ubuntu" || v.CVSS == nil || v.CVSS.Score != 9.8 {
		t.Fatalf("vendor first: %+v", v.VulnRow)
	}
	v = Vulnerability{}
	applySeverity(&v, []severity{{Type: "CVSS_V3", Score: "CVSS:3.1/AV:N/AC:L/PR:N/UI:N/S:U/C:H/I:H/A:H"}}, "Debian:12", vendorInfo{Urgency: "not yet assigned"})
	finishSeverity(&v)
	if v.Severity != "critical" || v.SeveritySource != "cvss_v3" {
		t.Fatalf("cvss fallback: %+v", v.VulnRow)
	}
	a := osvAffected{Package: osvPackage{Name: "example", Ecosystem: "Debian:12"}, Ranges: []osvRange{{Type: "ECOSYSTEM", Events: []osvEvent{{Introduced: "0"}, {Fixed: "2"}, {Introduced: "4"}, {Fixed: "5"}}}}}
	for _, tt := range []struct{ installed, want string }{{"1", "2"}, {"4.1", "5"}, {"2.5", ""}, {"5", ""}} {
		got := fixedVersion(a, tt.installed)
		if tt.want == "" {
			if got != nil {
				t.Fatal(tt, *got)
			}
		} else if got == nil || *got != tt.want {
			t.Fatal(tt, got)
		}
	}
}

func TestTrimRecordKeepsQueriedPackagesOnly(t *testing.T) {
	jammy := osvPackage{Name: "linux", Ecosystem: "Ubuntu:22.04:LTS"}
	a := advisory{ID: "UBUNTU-CVE-2026-1", Affected: []osvAffected{
		{Package: jammy, Ranges: []osvRange{{Type: "ECOSYSTEM"}, {Type: "GIT"}}},
		{Package: osvPackage{Name: "linux-aws", Ecosystem: "Ubuntu:Pro:14.04:LTS"}},
	}}
	got := trimRecord(a, map[osvPackage]bool{jammy: true})
	if len(got.Affected) != 1 || got.Affected[0].Package != jammy || len(got.Affected[0].Ranges) != 1 {
		t.Fatalf("%+v", got.Affected)
	}
	if !covers(got, map[osvPackage]bool{jammy: true}) {
		t.Fatal("trimmed record should cover its own packages")
	}
	if covers(got, map[osvPackage]bool{jammy: true, {Name: "linux-hwe", Ecosystem: "Ubuntu:22.04:LTS"}: true}) {
		t.Fatal("a newly matched package must refetch")
	}
	for id, want := range map[string]bool{"USN-9022-1": true, "DSA-5700-1": true, "DLA-3800-1": true, "DEBIAN-CVE-2024-1": false, "UBUNTU-CVE-2024-1": false, "ALPINE-CVE-2024-1": false} {
		if aggregateNotice(id) != want {
			t.Fatal(id)
		}
	}
}

func TestAdvisoryStoreShardsPersistAndPrune(t *testing.T) {
	dir := t.TempDir()
	s, err := openAdvisoryStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"DEBIAN-CVE-2024-1", "UBUNTU-CVE-2024-2", "ALPINE-CVE-2024-3"} {
		s.put(advisory{ID: id, Modified: "m"})
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	again, _ := openAdvisoryStore(dir)
	if a, ok := again.get("UBUNTU-CVE-2024-2"); !ok || a.Modified != "m" {
		t.Fatal("record not persisted", a)
	}
	again.prune(map[string]string{"DEBIAN-CVE-2024-1": "m"})
	if err := again.flush(); err != nil {
		t.Fatal(err)
	}
	third, _ := openAdvisoryStore(dir)
	if _, ok := third.get("UBUNTU-CVE-2024-2"); ok {
		t.Fatal("pruned record kept")
	}
	if _, ok := third.get("DEBIAN-CVE-2024-1"); !ok {
		t.Fatal("kept record lost")
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("empty shards should be removed", len(files))
	}
}

func TestSameModifiedIgnoresSubMicrosecondPrecision(t *testing.T) {
	if !sameModified("2026-11-11T09:47:35.859961408Z", "2026-11-11T09:47:35.859961Z") {
		t.Fatal("querybatch and vulns timestamps for one record must match")
	}
	if sameModified("2026-11-11T09:47:35.859962Z", "2026-11-11T09:47:35.859961Z") || sameModified("", "2026-11-11T09:47:35Z") {
		t.Fatal("a newer modification must refetch")
	}
}
