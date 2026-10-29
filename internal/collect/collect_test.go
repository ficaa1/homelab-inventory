package collect

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/sysexec"
)

func TestSelectFiltersCollectors(t *testing.T) {
	t.Parallel()

	got, err := Select(Options{Only: []string{"host", "network"}})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d collectors, want 2", len(got))
	}

	got, err = Select(Options{Skip: []string{"docker"}})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	for _, c := range got {
		if c.Name() == "docker" {
			t.Error("skipped collector was still selected")
		}
	}

	// Typos must fail loudly. Silently running everything because -only was
	// misspelled is the kind of thing you only notice three inventories later.
	if _, err := Select(Options{Only: []string{"dokcer"}}); err == nil {
		t.Error("expected an error for an unknown collector name")
	}
}

func TestEnvReadFileTruncates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}

	env := &Env{MaxFileBytes: 10}
	got, err := env.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("oversized file was not marked truncated: %q", got)
	}
	if len(got) > 40 {
		t.Errorf("truncation did not limit the read: %d bytes", len(got))
	}
}

func TestEnvPathHonoursRootFS(t *testing.T) {
	t.Parallel()
	env := &Env{RootFS: "/tmp/fake"}
	if got := env.Path("/etc/os-release"); got != "/tmp/fake/etc/os-release" {
		t.Errorf("Path = %q", got)
	}
	env = &Env{}
	if got := env.Path("/etc/os-release"); got != "/etc/os-release" {
		t.Errorf("Path with empty RootFS = %q", got)
	}
}

// TestRunProducesMarshalableInventory is the end-to-end smoke test: it runs
// the real collectors against this machine. It asserts nothing about the
// contents (they differ per host) beyond the invariants that must always hold.
func TestRunProducesMarshalableInventory(t *testing.T) {
	t.Parallel()

	inv, err := Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inv.SchemaVersion != inventory.SchemaVersion {
		t.Errorf("SchemaVersion = %q", inv.SchemaVersion)
	}
	if inv.CollectedAt.IsZero() {
		t.Error("CollectedAt not set")
	}
	// Every optional section must be non-nil after a full run: nil means
	// "did not look", and we looked.
	if inv.Systemd == nil || inv.Network == nil || inv.Docker == nil || inv.Scheduling == nil {
		t.Error("a collector left its section nil despite running")
	}
	if _, err := json.Marshal(inv); err != nil {
		t.Fatalf("inventory is not marshalable: %v", err)
	}
}

func TestRunRespectsOnly(t *testing.T) {
	t.Parallel()
	inv, err := Run(context.Background(), Options{Only: []string{"host"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if inv.Docker != nil || inv.Network != nil || inv.Systemd != nil {
		t.Error("sections were populated for collectors that were not selected")
	}
}

// A listing that fails (ss timing out on a busy host) must leave the section
// nil, "not looked at", rather than an empty list that reads as "nothing is
// listening".
func TestFailedNetworkListingLeavesSectionNil(t *testing.T) {
	t.Parallel()
	env := &Env{Run: sysexec.New()}
	if !env.Run.Available("ss") && !env.Run.Available("netstat") {
		t.Skip("neither ss nor netstat installed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inv := &inventory.Inventory{}
	if err := (NetworkCollector{}).Collect(ctx, env, inv); err == nil {
		t.Fatal("listing with a cancelled context succeeded")
	}
	if inv.Network != nil {
		t.Fatalf("failed listing left a network section: %+v", inv.Network)
	}
}

// A listing that exits non-zero is a failure, not a partial list.
func TestListSocketsRejectsNonZeroExit(t *testing.T) {
	t.Parallel()
	env := &Env{Run: sysexec.New()}
	if !env.Run.Available("ss") {
		t.Skip("ss not installed")
	}
	if _, err := listSockets(context.Background(), env, "ss", "--no-such-flag"); err == nil {
		t.Fatal("non-zero exit accepted")
	}
	if out, err := listSockets(context.Background(), env, "ss", "-tuln"); err != nil || out == "" {
		t.Fatalf("plain listing failed: %v", err)
	}
}
