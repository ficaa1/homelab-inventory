package collect

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

func TestParseGuestNIC(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, value string
		want        inventory.GuestNIC
	}{
		{"net0", "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
			inventory.GuestNIC{Name: "net0", Model: "virtio", MAC: "AA:BB:CC:DD:EE:FF", Bridge: "vmbr0"}},
		{"net0", "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1,tag=20",
			inventory.GuestNIC{Name: "net0", Model: "virtio", MAC: "AA:BB:CC:DD:EE:FF", Bridge: "vmbr0", Firewall: true, VLANTag: 20}},
		{"net1", "e1000=12:34:56:78:9A:BC,bridge=vmbr1",
			inventory.GuestNIC{Name: "net1", Model: "e1000", MAC: "12:34:56:78:9A:BC", Bridge: "vmbr1"}},
		// LXC spells the fields out and uses hwaddr rather than model=MAC.
		{"net0", "name=eth0,bridge=vmbr0,hwaddr=DE:AD:BE:EF:00:01,ip=192.0.2.23/24,type=veth",
			inventory.GuestNIC{Name: "net0", MAC: "DE:AD:BE:EF:00:01", Bridge: "vmbr0", IP: "192.0.2.23/24"}},
	}
	for _, c := range cases {
		if got := parseGuestNIC(c.name, c.value); !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseGuestNIC(%q, %q) = %+v, want %+v", c.name, c.value, got, c.want)
		}
	}
}

func TestParseGuestDisk(t *testing.T) {
	t.Parallel()
	yes, no := true, false
	cases := []struct {
		name, value string
		want        inventory.GuestDisk
	}{
		{"scsi0", "local-lvm:vm-100-disk-0,size=32G",
			inventory.GuestDisk{Name: "scsi0", Storage: "local-lvm", Volume: "vm-100-disk-0", Size: "32G"}},
		{"scsi1", "tank:vm-100-disk-1,size=500G,backup=0",
			inventory.GuestDisk{Name: "scsi1", Storage: "tank", Volume: "vm-100-disk-1", Size: "500G", Backup: &no}},
		{"rootfs", "local-lvm:subvol-101-disk-0,size=8G,backup=1",
			inventory.GuestDisk{Name: "rootfs", Storage: "local-lvm", Volume: "subvol-101-disk-0", Size: "8G", Backup: &yes}},
		// An LXC bind mount is a bare host path: no storage, no colon.
		{"mp0", "/mnt/share,mp=/media,backup=0",
			inventory.GuestDisk{Name: "mp0", Volume: "/mnt/share", Backup: &no}},
	}
	for _, c := range cases {
		got := parseGuestDisk(c.name, c.value)
		if got.Name != c.want.Name || got.Storage != c.want.Storage ||
			got.Volume != c.want.Volume || got.Size != c.want.Size {
			t.Errorf("parseGuestDisk(%q, %q) = %+v, want %+v", c.name, c.value, got, c.want)
		}
		switch {
		case (got.Backup == nil) != (c.want.Backup == nil):
			t.Errorf("parseGuestDisk(%q) backup presence mismatch: %v", c.name, got.Backup)
		case got.Backup != nil && *got.Backup != *c.want.Backup:
			t.Errorf("parseGuestDisk(%q) backup = %v, want %v", c.name, *got.Backup, *c.want.Backup)
		}
	}
}

func TestIsDiskKey(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"scsi0": true, "sata1": true, "virtio0": true, "ide2": true,
		"mp0": true, "rootfs": true, "unused0": true, "efidisk0": true,
		// scsihw is the controller model, not a disk. Without the digit check
		// in isIndexed it would be parsed as one.
		"scsihw": false, "net0": false, "onboot": false, "memory": false,
		"cores": false, "virtiofs0": false,
	}
	for k, want := range cases {
		if got := isDiskKey(k); got != want {
			t.Errorf("isDiskKey(%q) = %v, want %v", k, got, want)
		}
	}
}

func TestLooksLikeMAC(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"AA:BB:CC:DD:EE:FF", "12:34:56:78:9a:bc", "de-ad-be-ef-00-01"} {
		if !looksLikeMAC(s) {
			t.Errorf("looksLikeMAC(%q) = false", s)
		}
	}
	for _, s := range []string{"vmbr0", "1", "192.0.2.23/24", "AA:BB:CC:DD:EE", "", "not-a-mac-at-al"} {
		if looksLikeMAC(s) {
			t.Errorf("looksLikeMAC(%q) = true", s)
		}
	}
}

// TestGuestConfigSecretsAreRedacted pins the Proxmox-specific keys. Neither
// runs words together with a separator, so the general key regex missed both
// until they were named explicitly.
func TestGuestConfigSecretsAreRedacted(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"cipassword", "sshkeys"} {
		if !redact.IsSensitiveKey(key) {
			t.Errorf("%q is not treated as sensitive; cloud-init guests would leak it", key)
		}
		got := redact.Value(key, "s3cr3t-material")
		if strings.Contains(got, "s3cr3t-material") {
			t.Errorf("%q value not redacted: %q", key, got)
		}
	}
	// Neighbouring keys stay readable.
	for _, key := range []string{"ciuser", "ipconfig0", "nameserver", "searchdomain"} {
		if redact.IsSensitiveKey(key) {
			t.Errorf("%q was redacted but is not a secret", key)
		}
	}
}

// TestQMConfigParse walks a realistic `qm config` document end to end.
func TestQMConfigParse(t *testing.T) {
	t.Parallel()
	out := `agent: 1
boot: order=scsi0;net0
cores: 4
cpu: host
memory: 16384
name: host-a
net0: virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1
onboot: 1
ostype: l26
scsi0: local-lvm:vm-102-disk-0,size=64G
scsi1: tank:vm-102-disk-1,size=2000G,backup=0
scsihw: virtio-scsi-pci
smbios1: uuid=6f2d1c9e-0000-0000-0000-000000000000
sockets: 1
cipassword: hunter2
sshkeys: ssh-ed25519%20AAAAC3Nza
`
	cfg := parseKeyValue(out, ":")

	if cfg["onboot"] != "1" {
		t.Errorf("onboot = %q", cfg["onboot"])
	}
	// The first colon splits; the rest of the value keeps its colons.
	if cfg["scsi0"] != "local-lvm:vm-102-disk-0,size=64G" {
		t.Errorf("scsi0 = %q", cfg["scsi0"])
	}
	if cfg["net0"] != "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1" {
		t.Errorf("net0 = %q", cfg["net0"])
	}

	var disks []inventory.GuestDisk
	var nics []inventory.GuestNIC
	for k, v := range cfg {
		if isDiskKey(k) {
			disks = append(disks, parseGuestDisk(k, v))
		}
		if strings.HasPrefix(k, "net") && isIndexed(k, "net") {
			nics = append(nics, parseGuestNIC(k, v))
		}
	}
	if len(disks) != 2 {
		t.Errorf("got %d disks, want 2 (scsihw must not count): %+v", len(disks), disks)
	}
	if len(nics) != 1 {
		t.Errorf("got %d nics, want 1: %+v", len(nics), nics)
	}

	// Whole-config redaction is what actually ships in the inventory.
	for k, v := range cfg {
		if r := redact.Value(k, v); k == "cipassword" && strings.Contains(r, "hunter2") {
			t.Errorf("cipassword leaked: %q", r)
		}
	}
}
