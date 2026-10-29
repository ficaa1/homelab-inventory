package collect

import (
	"context"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPackageDatabases(t *testing.T) {
	for _, tc := range []struct {
		manager, file string
		count         int
	}{{"dpkg", "dpkg-status", 3}, {"apk", "apk-installed", 1}} {
		t.Run(tc.manager, func(t *testing.T) {
			f, err := os.Open("testdata/packages/" + tc.file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			ps, err := parsePackageDB(context.Background(), f, tc.manager, 20000, false)
			if err != nil || len(ps) != tc.count {
				t.Fatalf("packages=%+v err=%v", ps, err)
			}
			if tc.manager == "dpkg" {
				if !strings.HasPrefix(ps[0].Version, "1:") {
					t.Fatalf("epoch lost: %+v", ps[0])
				}
				if ps[1].Source != "example" {
					t.Fatalf("source mapping lost: %+v", ps[1])
				}
			} else if ps[0].Source != "example" || ps[0].Version != "2.0-r1" || ps[0].Arch != "x86_64" {
				t.Fatalf("apk: %+v", ps)
			}
		})
	}
	// Installed-only filtering, source revisions, distroless fragments, and limits.
	input := "Package: libexample\nStatus: install ok installed\nVersion: 1:2.0-1+b1\nSource: example (1:2.0-1)\nArchitecture: amd64\n\nPackage: removed\nStatus: deinstall ok config-files\nVersion: 1\n\nPackage: broken\nStatus: install reinstreq half-installed\nVersion: 2\n"
	ps, err := parsePackageDB(context.Background(), strings.NewReader(input), "dpkg", 1, false)
	if err != nil || len(ps) != 1 || ps[0].SourceVersion != "1:2.0-1" {
		t.Fatalf("source/status: %+v %v", ps, err)
	}
	_, err = parsePackageDB(context.Background(), strings.NewReader(input), "dpkg", 0, false)
	if err == nil {
		t.Fatal("package cap ignored")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "var/lib/dpkg/status.d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("Package: base\nVersion: 2\nArchitecture: amd64\n"), 0644); err != nil {
		t.Fatal(err)
	}
	set := collectPackageSet(context.Background(), root)
	if set.Status != "ok" || set.Count != 1 {
		t.Fatalf("fragment: %+v", set)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	set = collectPackageSet(ctx, root)
	if set.Status != "failed" {
		t.Fatalf("cancelled: %+v", set)
	}
}

func TestPackageRootConfinement(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"etc", "usr/lib", "var/lib/dpkg"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, "usr/lib/os-release")
	if err := os.WriteFile(target, []byte("ID=debian\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/lib/os-release", filepath.Join(root, "etc/os-release")); err != nil {
		t.Fatal(err)
	}
	got, err := confinedPath(root, "/etc/os-release")
	if err != nil || got != target {
		t.Fatalf("absolute link: %q %v", got, err)
	}
	if err := os.Symlink("../../../../outside", filepath.Join(root, "var/lib/dpkg/status")); err != nil {
		t.Fatal(err)
	}
	if _, err := confinedPath(root, "/var/lib/dpkg/status"); err == nil {
		t.Fatal("symlink escaped root")
	}
	set := collectPackageSet(context.Background(), root)
	if set.Status != "failed" || set.Count != 0 {
		t.Fatalf("unsafe DB reported as success: %+v", set)
	}
	if err := os.Symlink("cycle", filepath.Join(root, "cycle")); err != nil {
		t.Fatal(err)
	}
	if _, err := confinedPath(root, "/cycle"); err == nil {
		t.Fatal("symlink loop accepted")
	}
}

func TestPackagesDailyWindow(t *testing.T) {
	for _, tc := range []struct {
		name, at, uptime string
		force, fresh     bool
		want             bool
	}{
		{name: "before", at: "03:59:59"}, {name: "start", at: "04:00:00", want: true},
		{name: "end", at: "04:19:59", want: true}, {name: "after", at: "04:20:00"},
		{name: "noon", at: "12:00:00", uptime: "86400.5 1.0"}, {name: "forced", at: "12:00:00", force: true, want: true},
		{name: "booted", at: "12:00:00", uptime: "300.25 10.0", want: true},
		{name: "installed", at: "12:00:00", fresh: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Fixed non-UTC zone catches accidental UTC window checks.
			day, _ := time.ParseInLocation("2006-01-02 15:04:05", "2026-10-21 "+tc.at, time.FixedZone("local", 2*3600))
			root := t.TempDir()
			if tc.uptime != "" {
				os.MkdirAll(filepath.Join(root, "proc"), 0o755)
				os.WriteFile(filepath.Join(root, "proc", "uptime"), []byte(tc.uptime+"\n"), 0o644)
			}
			exe := filepath.Join(t.TempDir(), "discover")
			os.WriteFile(exe, nil, 0o755)
			installed := day.Add(-48 * time.Hour)
			if tc.fresh {
				installed = day.Add(-5 * time.Minute)
			}
			os.Chtimes(exe, installed, installed)
			diags := []inventory.Diagnostic{}
			env := &Env{RootFS: root, Run: sysexec.New(), ForcePackages: tc.force, Now: func() time.Time { return day }, Executable: exe, diags: &diags}
			// Stop Docker probes; the host read still establishes that collection ran.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			inv := &inventory.Inventory{}
			if err := (PackagesCollector{}).Collect(ctx, env, inv); err != nil {
				t.Fatal(err)
			}
			if (inv.Software != nil) != tc.want {
				t.Fatalf("software=%+v", inv.Software)
			}
			if !tc.want && (len(diags) != 1 || diags[0].Severity != inventory.SeverityInfo) {
				t.Fatalf("diagnostics=%+v", diags)
			}
		})
	}
}
