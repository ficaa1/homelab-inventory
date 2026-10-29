package sysexec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAllowlistBlocksUnknownBinaries is the load-bearing test in this package:
// it asserts that the read-only guarantee is enforced by code, not convention.
func TestAllowlistBlocksUnknownBinaries(t *testing.T) {
	t.Parallel()
	r := New()
	for _, name := range []string{"rm", "sh", "bash", "dd", "mkfs", "iptables", "kill", "reboot"} {
		if _, err := r.Run(context.Background(), name); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Run(%q) error = %v, want ErrNotAllowed", name, err)
		}
		if r.Available(name) {
			t.Errorf("Available(%q) = true, want false for a non-allowlisted binary", name)
		}
	}
}

// TestSubcommandGuard is the second half of the read-only guarantee. Proxmox's
// tools put destructive verbs next to read-only ones on the same binary, so
// allowlisting the binary alone would let `qm destroy` through.
func TestSubcommandGuard(t *testing.T) {
	t.Parallel()
	r := New()

	blocked := []struct {
		name string
		args []string
	}{
		{"ss", []string{"-K", "dst", "192.0.2.10"}},
		{"ss", []string{"-tulpn", "--output=/tmp/sockets"}},
		{"netstat", []string{"--file=/tmp/output"}},
		{"iptables-save", []string{"-f", "/tmp/firewall"}},
		{"ip6tables-save", []string{"-f", "/tmp/firewall"}},
		{"uname", []string{"--definitely-not-a-flag"}},
		{"systemctl", []string{"-H", "show", "stop", "app"}},
		{"pvesh", []string{"--output-format", "get", "delete", "/nodes/host-a"}},

		{"pvesh", []string{"set", "/nodes/host-b/qemu/100/config", "-memory", "1024"}},
		{"pvesh", []string{"create", "/nodes/host-b/qemu"}},
		{"pvesh", []string{"delete", "/nodes/host-b/qemu/100"}},
		{"qm", []string{"destroy", "100"}},
		{"qm", []string{"stop", "100"}},
		{"pct", []string{"destroy", "101"}},
		{"docker", []string{"rm", "-f", "host-a"}},
		{"docker", []string{"stop", "host-a"}},
		{"systemctl", []string{"stop", "nginx"}},
		{"systemctl", []string{"disable", "--now", "nginx"}},
		{"systemctl", []string{"mask", "nginx"}},
		// nft is the sharpest tool on the list: `nft flush ruleset` on a
		// remote box takes the machine off the network, and every one of
		// these sits one word away from `list`.
		{"nft", []string{"flush", "ruleset"}},
		{"nft", []string{"delete", "table", "ip", "filter"}},
		{"nft", []string{"add", "rule", "ip", "filter", "INPUT", "drop"}},
		{"nft", []string{"insert", "rule", "ip", "filter", "INPUT", "drop"}},
		{"nft", []string{"replace", "rule", "ip", "filter", "INPUT", "handle", "4", "drop"}},
		{"pve-firewall", []string{"stop"}},
		{"pve-firewall", []string{"restart"}},
	}
	for _, c := range blocked {
		if _, err := r.Run(context.Background(), c.name, c.args...); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("Run(%q, %v) error = %v, want ErrNotAllowed", c.name, c.args, err)
		}
	}

	// The read-only forms must still be permitted. These fail with
	// ErrNotFound on a machine without Proxmox, which is fine — the point is
	// that they are not rejected by the guard.
	allowedCalls := []struct {
		name string
		args []string
	}{
		{"pvesh", []string{"get", "/nodes/host-b/qemu", "--output-format=json"}},
		{"qm", []string{"config", "100"}},
		{"pct", []string{"config", "101"}},
		{"docker", []string{"ps", "--all", "--format", "{{json .}}"}},
		{"docker", []string{"inspect", "--format", "{{.Id}}", "abc"}},
		{"systemctl", []string{"--version"}},
		{"systemctl", []string{"list-units", "--type=service", "--all"}},
	}
	for _, c := range allowedCalls {
		_, err := r.Run(context.Background(), c.name, c.args...)
		if errors.Is(err, ErrNotAllowed) {
			t.Errorf("Run(%q, %v) was refused by the subcommand guard", c.name, c.args)
		}
	}
}

func TestAllowlistContainsOnlyReadOnlyTools(t *testing.T) {
	t.Parallel()
	// Binaries that are mutating end to end, with no read-only mode worth
	// having. This test is a tripwire: adding one fails the build instead of
	// slipping through review.
	//
	// qm, pct and nft are deliberately absent: they have genuinely useful
	// read-only subcommands and are constrained by a subcommand guard, checked
	// below. nft was here until the firewall collector needed `nft list
	// ruleset`; it is now in exactly the same category as `qm config`, and the
	// blocked-call list above pins `flush`, `delete`, `add`, `insert` and
	// `replace` as refused.
	//
	// `iptables` stays forbidden, and the difference is worth stating because
	// it is the reason the collector uses a different binary. iptables
	// distinguishes reading from writing by *flag* — `-L` versus `-A` — and
	// this guard only understands subcommands, so it could not tell them
	// apart. `iptables-save` is a separate, inherently read-only binary whose
	// write counterpart is the separate `iptables-restore`, which is not
	// allowlisted. Same for ip6tables.
	forbidden := map[string]bool{
		"sh": true, "bash": true, "rm": true, "dd": true, "mv": true,
		"cp": true, "chmod": true, "chown": true, "mount": true,
		"umount": true, "iptables": true, "ip6tables": true,
		"iptables-restore": true, "ip6tables-restore": true,
		"crontab": true, "pvecm": true, "reboot": true, "kill": true,
	}
	for name := range allowed {
		if forbidden[name] {
			t.Errorf("allowlist contains a mutating or shell binary: %q", name)
		}
	}

	// Binaries that do have destructive verbs must carry a subcommand guard;
	// listing one without a rule silently reopens the hole.
	needGuard := []string{"docker", "pvesh", "qm", "pct", "systemctl", "nft", "pve-firewall"}
	for _, name := range needGuard {
		if _, ok := allowed[name]; !ok {
			continue
		}
		if len(allowed[name].subcommands) == 0 {
			t.Errorf("%q is allowlisted with no subcommand restriction", name)
		}
	}
}

func TestNonZeroExitIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "uname"), []byte("#!/bin/sh\nexit 3\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	res, err := New().Run(context.Background(), "uname", "-s")
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("nonzero exit: %+v %v", res, err)
	}
}

func TestRunSucceeds(t *testing.T) {
	t.Parallel()
	r := New()
	if !r.Available("uname") {
		t.Skip("uname not present")
	}
	res, err := r.Run(context.Background(), "uname", "-s")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.OK() || len(res.Lines()) != 1 {
		t.Errorf("unexpected result: %+v", res)
	}
}

func TestContextCancellationIsAnError(t *testing.T) {
	t.Parallel()
	r := New()
	if !r.Available("uname") {
		t.Skip("uname not present")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if _, err := r.Run(ctx, "uname", "-a"); err == nil {
		t.Error("expected an error when the context is already expired")
	}
}

// Docker exec is never part of read-only discovery, including fixed probes.
func TestDockerExecRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, args := range [][]string{
		{"exec", "vpnservice", "wg", "show", "wg0", "dump"},
		{"exec", "service", "curl", "https://example.org"},
		{"exec", "service", "sh", "-c", "exit 0"},
	} {
		if _, err := New().Run(context.Background(), "docker", args...); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("exec accepted: %v", err)
		}
	}
}

func TestDockerGlobalOptionsCannotHideExec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for _, option := range []string{"--context", "-c", "--host", "-H", "--config"} {
		for _, value := range []string{"inspect", "ps", "version"} {
			args := []string{option, value, "exec", "vpnservice", "sh", "-c", "exit 0"}
			if _, err := New().Run(context.Background(), "docker", args...); !errors.Is(err, ErrNotAllowed) {
				t.Errorf("global option %s with value %s hid exec", option, value)
			}
		}
	}
}
