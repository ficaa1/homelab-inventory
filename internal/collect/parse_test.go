package collect

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestParseSS(t *testing.T) {
	t.Parallel()
	// Synthetic `ss -tulpn` output, including the header and an unattributed
	// socket (what you get when not running as root).
	out := `Netid State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
udp   UNCONN 0      0      127.0.0.53%lo:53    0.0.0.0:*         users:(("systemd-resolve",pid=700,fd=12))
tcp   LISTEN 0      4096   0.0.0.0:22          0.0.0.0:*         users:(("sshd",pid=812,fd=3))
tcp   LISTEN 0      511    [::]:80             [::]:*            users:(("nginx",pid=1201,fd=6),("nginx",pid=1202,fd=6))
tcp   LISTEN 0      4096   127.0.0.1:9090      0.0.0.0:*
tcp   ESTAB  0      0      192.0.2.5:22         192.0.2.9:51234    users:(("sshd",pid=999,fd=4))
`
	got := parseSS(out)
	if len(got) != 4 {
		t.Fatalf("got %d listeners, want 4 (established connections must be excluded): %+v", len(got), got)
	}
	sortListeners(got)

	if got[0].Port != 22 || got[0].Address != "0.0.0.0" || !got[0].Exposed || got[0].Process != "sshd" || got[0].PID != 812 {
		t.Errorf("ssh listener parsed wrong: %+v", got[0])
	}
	if got[1].Port != 53 || got[1].Family != "ipv4" || got[1].Exposed {
		t.Errorf("loopback resolver should not be marked exposed: %+v", got[1])
	}
	if got[2].Port != 80 || got[2].Address != "::" || got[2].Family != "ipv6" || !got[2].Exposed {
		t.Errorf("ipv6 wildcard parsed wrong: %+v", got[2])
	}
	if got[3].Port != 9090 || got[3].PID != 0 || got[3].Process != "" {
		t.Errorf("unattributed socket should have no process: %+v", got[3])
	}
}

// TestNewListenerAddressShapes pins the address parsing, including the
// bracketed-IPv6-with-zone form that a naive bracket trim mangles into
// "ff02::fb]%veth1a2b3c".
func TestNewListenerAddressShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in         string
		addr, zone string
		port       int
		family     string
		exposed    bool
	}{
		{"0.0.0.0:22", "0.0.0.0", "", 22, "ipv4", true},
		{"127.0.0.1:9090", "127.0.0.1", "", 9090, "ipv4", false},
		{"[::]:80", "::", "", 80, "ipv6", true},
		{"[::1]:25", "::1", "", 25, "ipv6", false},
		{"*:8006", "*", "", 8006, "any", true},
		{"127.0.0.53%lo:53", "127.0.0.53", "lo", 53, "ipv4", false},
		{"[ff02::fb]%veth1a2b3c:5353", "ff02::fb", "veth1a2b3c", 5353, "ipv6", false},
		{"[fe80::1]%ens18:546", "fe80::1", "ens18", 546, "ipv6", false},
		{"224.0.0.251:5353", "224.0.0.251", "", 5353, "ipv4", false},
	}
	for _, c := range cases {
		got := newListener("tcp", c.in)
		if got.Address != c.addr || got.Zone != c.zone || got.Port != c.port ||
			got.Family != c.family || got.Exposed != c.exposed {
			t.Errorf("newListener(%q) = {addr:%q zone:%q port:%d family:%q exposed:%v}, want {%q %q %d %q %v}",
				c.in, got.Address, got.Zone, got.Port, got.Family, got.Exposed,
				c.addr, c.zone, c.port, c.family, c.exposed)
		}
	}
}

func TestParseNetstat(t *testing.T) {
	t.Parallel()
	out := `Active Internet connections (only servers)
Proto Recv-Q Send-Q Local Address           Foreign Address         State       PID/Program name
tcp        0      0 0.0.0.0:22              0.0.0.0:*               LISTEN      812/sshd
tcp6       0      0 :::80                   :::*                    LISTEN      1201/nginx
tcp        0      0 192.0.2.5:22             192.0.2.9:51234          ESTABLISHED 999/sshd
udp        0      0 127.0.0.1:323           0.0.0.0:*                           900/chronyd
`
	got := parseNetstat(out)
	if len(got) != 3 {
		t.Fatalf("got %d listeners, want 3: %+v", len(got), got)
	}
	sortListeners(got)

	if got[0].Port != 22 || got[0].Protocol != "tcp" || got[0].Process != "sshd" {
		t.Errorf("ssh listener parsed wrong: %+v", got[0])
	}
	if got[1].Port != 80 || got[1].Address != "::" || got[1].Family != "ipv6" || !got[1].Exposed {
		t.Errorf("tcp6 listener parsed wrong: %+v", got[1])
	}
	if got[2].Port != 323 || got[2].Protocol != "udp" || got[2].Process != "chronyd" {
		t.Errorf("udp listener parsed wrong: %+v", got[2])
	}
}

func TestParseMemInfo(t *testing.T) {
	t.Parallel()
	out := `MemTotal:       16316716 kB
MemFree:         2107500 kB
MemAvailable:   12904312 kB
Buffers:          301560 kB
SwapTotal:       4194300 kB
SwapFree:        4194300 kB
`
	m := parseMemInfo(out)
	if m.TotalBytes != 16316716*1024 {
		t.Errorf("TotalBytes = %d, want kB converted to bytes", m.TotalBytes)
	}
	if m.AvailableBytes != 12904312*1024 {
		t.Errorf("AvailableBytes = %d", m.AvailableBytes)
	}
	if m.SwapTotalBytes != 4194300*1024 || m.SwapFreeBytes != 4194300*1024 {
		t.Errorf("swap parsed wrong: %+v", m)
	}
}

func TestParseOSRelease(t *testing.T) {
	t.Parallel()
	out := `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
ID=ubuntu
ID_LIKE=debian
`
	rel := parseOSRelease(out)
	if rel.ID != "ubuntu" || rel.VersionID != "24.04" || rel.Pretty != "Ubuntu 24.04.1 LTS" {
		t.Errorf("os-release parsed wrong: %+v", rel)
	}
}

func TestParseMountsSkipsPseudoFilesystems(t *testing.T) {
	t.Parallel()
	out := `sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0
proc /proc proc rw,nosuid,nodev,noexec,relatime 0 0
/dev/mapper/vg0-root / ext4 rw,relatime,errors=remount-ro 0 0
cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime 0 0
host-c:/mnt/tank/media /mnt/media nfs4 rw,relatime 0 0
/dev/sda1 /boot/efi vfat rw,relatime 0 0
tmpfs /run/user/1000 tmpfs rw,nosuid,nodev,relatime 0 0
`
	got := parseMounts(out)
	var mounts []string
	for _, fs := range got {
		mounts = append(mounts, fs.MountPoint)
	}
	want := []string{"/", "/boot/efi", "/mnt/media", "/run/user/1000"}
	if !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
	for _, fs := range got {
		if fs.MountPoint == "/mnt/media" && fs.FSType != "nfs4" {
			t.Errorf("network mount must be retained with its type: %+v", fs)
		}
	}
}

func TestUnescapeMount(t *testing.T) {
	t.Parallel()
	if got := unescapeMount(`/mnt/my\040share`); got != "/mnt/my share" {
		t.Errorf("unescapeMount = %q, want %q", got, "/mnt/my share")
	}
	if got := unescapeMount("/mnt/plain"); got != "/mnt/plain" {
		t.Errorf("unescapeMount altered a plain path: %q", got)
	}
}

func TestParseCrontabWithUserColumn(t *testing.T) {
	t.Parallel()
	// /etc/crontab style: five schedule fields, then a user, then a command.
	content := "SHELL=/bin/sh\n" +
		"PATH=/usr/local/sbin:/usr/local/bin:/sbin:/bin\n" +
		"MAILTO=\"\"\n" +
		"# m h dom mon dow user command\n" +
		"17 *\t* * *\troot\tcd / && run-parts --report /etc/cron.hourly\n" +
		"@reboot root /opt/stack/start.sh\n" +
		"*/5 * * * * backup /usr/local/bin/sync.sh --dest=/mnt/media\n"

	got := parseCrontab(content, true)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (env assignments and comments must be skipped): %+v", len(got), got)
	}
	if got[0].Schedule != "17 * * * *" || got[0].User != "root" {
		t.Errorf("hourly entry parsed wrong: %+v", got[0])
	}
	if got[0].Command != "cd / && run-parts --report /etc/cron.hourly" {
		t.Errorf("command lost: %q", got[0].Command)
	}
	if got[1].Schedule != "@reboot" || got[1].User != "root" || got[1].Command != "/opt/stack/start.sh" {
		t.Errorf("@reboot entry parsed wrong: %+v", got[1])
	}
	if got[2].User != "backup" || !strings.Contains(got[2].Command, "--dest=/mnt/media") {
		t.Errorf("entry with = in the command must not be treated as an env assignment: %+v", got[2])
	}
}

func TestParseCrontabUserFileHasNoUserColumn(t *testing.T) {
	t.Parallel()
	content := "0 3 * * * /usr/bin/borg create /mnt/backup::daily /home\n"
	got := parseCrontab(content, false)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].User != "" {
		t.Errorf("user crontabs have no user column, got %q", got[0].User)
	}
	if got[0].Command != "/usr/bin/borg create /mnt/backup::daily /home" {
		t.Errorf("command parsed wrong: %q", got[0].Command)
	}
}

func TestParseCrontabRedactsSecrets(t *testing.T) {
	t.Parallel()
	content := "0 * * * * root /usr/bin/curl https://user:fake-password@api.example.org/ping\n"
	got := parseCrontab(content, true)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if strings.Contains(got[0].Command, "fake-password") || strings.Contains(got[0].Raw, "fake-password") {
		t.Errorf("credential leaked into inventory: %+v", got[0])
	}
}

func TestClassifyFilesystem(t *testing.T) {
	t.Parallel()
	cases := []struct {
		fsType, mount string
		want          inventory.FilesystemKind
	}{
		{"ext4", "/", inventory.FSKindReal},
		{"ext4", "/mnt/storage", inventory.FSKindReal},
		{"vfat", "/boot/efi", inventory.FSKindReal},
		{"nfs", "/mnt/share", inventory.FSKindNetwork},
		{"overlay", "/var/lib/docker/overlay2/abc/merged", inventory.FSKindContainer},
		{"squashfs", "/snap/core24/1643", inventory.FSKindSnap},
		{"tmpfs", "/run/user/1000", inventory.FSKindMemory},
		{"binfmt_misc", "/proc/sys/fs/binfmt_misc", inventory.FSKindOther},
		// A network mount under /var/lib/docker is still a network mount:
		// type wins over path.
		{"nfs", "/var/lib/docker/volumes/data/_data", inventory.FSKindNetwork},
		// A named volume on local disk holds real data and must not be
		// written off as container plumbing.
		{"ext4", "/var/lib/docker/volumes/nvme_volume/_data", inventory.FSKindReal},
		// Layer and plugin directories are plumbing.
		{"ext4", "/var/lib/docker/plugins/abc/propagated-mount", inventory.FSKindContainer},
	}
	for _, c := range cases {
		if got := classifyFilesystem(c.fsType, c.mount); got != c.want {
			t.Errorf("classifyFilesystem(%q, %q) = %q, want %q", c.fsType, c.mount, got, c.want)
		}
	}
}

func TestClassifyInterface(t *testing.T) {
	t.Parallel()
	cases := map[string]inventory.InterfaceKind{
		"lo":              inventory.IfKindLoopback,
		"ens18":           inventory.IfKindPhysical,
		"enp11s0":         inventory.IfKindPhysical,
		"eth0":            inventory.IfKindPhysical,
		"docker0":         inventory.IfKindBridge,
		"br-03753c94793e": inventory.IfKindBridge,
		"vmbr0":           inventory.IfKindBridge,
		"veth1a2b3c4":     inventory.IfKindVeth,
		"tailscale0":      inventory.IfKindTunnel,
		"wg0":             inventory.IfKindTunnel,
		// Proxmox per-VM firewall plumbing: fwbr is the bridge, fwln/fwpr the
		// pair that attaches a VM's tap device to it.
		"fwbr101i0": inventory.IfKindBridge,
		"fwln101i0": inventory.IfKindVeth,
		"fwpr101p0": inventory.IfKindVeth,
		"tap100i0":  inventory.IfKindVeth,
	}
	for name, want := range cases {
		if got := classifyInterface(name, 0); got != want {
			t.Errorf("classifyInterface(%q) = %q, want %q", name, got, want)
		}
	}
	// The loopback flag wins even if the name is unconventional.
	if got := classifyInterface("lo0", net.FlagLoopback); got != inventory.IfKindLoopback {
		t.Errorf("loopback flag ignored: %q", got)
	}
}

func TestTemplateUnitName(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"getty@tty1.service":            "getty@.service",
		"lvm2-pvscan@8:17.service":      "lvm2-pvscan@.service",
		"modprobe@configfs.service":     "modprobe@.service",
		"user@1000.service":             "user@.service",
		"systemd-fsck@dev-sda1.service": "systemd-fsck@.service",
		"nginx.service":                 "", // not instantiated
		"getty@.service":                "", // already a template
	}
	for in, want := range cases {
		if got := templateUnitName(in); got != want {
			t.Errorf("templateUnitName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMountTableResolve(t *testing.T) {
	t.Parallel()
	// Reproduces the Portainer case: the compose file is recorded at a path
	// that only exists inside the Portainer container.
	root := t.TempDir()
	realDir := filepath.Join(root, "var/lib/docker/volumes/portainer_data/_data/compose/6")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "docker-compose.yml"), []byte("services: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := &Env{RootFS: root}
	table := newMountTable([]inventory.Container{{
		Name: "portainer",
		Mounts: []inventory.Mount{
			{Type: "bind", Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"},
			{Type: "volume", Name: "portainer_data", Source: "/var/lib/docker/volumes/portainer_data/_data", Destination: "/data"},
		},
	}})

	got, via, ok := table.resolve(env, "/data/compose/6/docker-compose.yml")
	if !ok {
		t.Fatal("compose path was not resolved through the portainer volume")
	}
	if got != "/var/lib/docker/volumes/portainer_data/_data/compose/6/docker-compose.yml" {
		t.Errorf("resolved to %q", got)
	}
	if via != "mount portainer:/data" {
		t.Errorf("provenance = %q", via)
	}

	// A path that maps onto the mount but does not exist must not resolve:
	// a coincidental prefix pointing at the wrong file is worse than
	// reporting the file as unreachable.
	if _, _, ok := table.resolve(env, "/data/compose/999/docker-compose.yml"); ok {
		t.Error("resolved a path that does not exist on the host")
	}
	// Unrelated paths are left alone.
	if _, _, ok := table.resolve(env, "/opt/example-stack/docker-compose.yml"); ok {
		t.Error("resolved a path that no mount covers")
	}
}

func TestMountTableIgnoresRootMounts(t *testing.T) {
	t.Parallel()
	// A container that mounts / would otherwise match every path and rewrite
	// entries that were already correct.
	table := newMountTable([]inventory.Container{{
		Name:   "rootful",
		Mounts: []inventory.Mount{{Source: "/mnt/host", Destination: "/"}},
	}})
	if len(table) != 0 {
		t.Errorf("root mount was not excluded: %+v", table)
	}
}

func TestFormatUSec(t *testing.T) {
	t.Parallel()
	if got := formatUSec("0"); got != "" {
		t.Errorf("zero sentinel should be empty, got %q", got)
	}
	if got := formatUSec("n/a"); got != "" {
		t.Errorf("n/a sentinel should be empty, got %q", got)
	}
	if got := formatUSec("18446744073709551615"); got != "" {
		t.Errorf("never-scheduled sentinel should be empty, got %q", got)
	}
	// Debian 12's systemd prints a human string here; it must be normalised
	// to RFC3339 so the field has one format across hosts.
	if got := formatUSec("Sat 2026-07-25 06:25:53 UTC"); got != "2026-07-25T06:25:53Z" {
		t.Errorf("human timestamp not normalised: %q", got)
	}
	// Anything genuinely unparseable is passed through rather than dropped.
	if got := formatUSec("sometime next tuesday"); got != "sometime next tuesday" {
		t.Errorf("unrecognised timestamp dropped: %q", got)
	}
	want := time.UnixMicro(1753401600000000).UTC().Format(time.RFC3339)
	if got := formatUSec("1753401600000000"); got != want {
		t.Errorf("formatUSec = %q, want %q", got, want)
	}
}

// TestResolveProcessesRepairsTruncatedNames covers the kernel's 15-character
// comm limit, which produced "Plex Media Serv" and "pvedaemon worke" in the
// first real run.
func TestResolveProcessesRepairsTruncatedNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeCmdline := func(pid int, argv ...string) {
		dir := filepath.Join(root, "proc", strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		data := strings.Join(argv, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeCmdline(1000, "/usr/lib/plexmediaserver/Plex Media Server")
	writeCmdline(2000, "/usr/sbin/sshd", "-D")
	writeCmdline(3000, "/usr/bin/curl", "https://user:fake-password@api.example.org/ping")

	listeners := []inventory.Listener{
		{Port: 32400, Process: "Plex Media Serv", PID: 1000},
		{Port: 22, Process: "sshd", PID: 2000},
		{Port: 8080, Process: "curl", PID: 3000},
		{Port: 9090}, // unattributed: no PID, must be left alone
	}
	resolveProcesses(&Env{RootFS: root}, listeners)

	if listeners[0].Process != "Plex Media Server" {
		t.Errorf("truncated name not repaired: %q", listeners[0].Process)
	}
	// A name that was not truncated is left as comm, which is more
	// trustworthy than an argv a process can rewrite at will.
	if listeners[1].Process != "sshd" {
		t.Errorf("untruncated name was overwritten: %q", listeners[1].Process)
	}
	if listeners[1].Command != "/usr/sbin/sshd -D" {
		t.Errorf("command line wrong: %q", listeners[1].Command)
	}
	if strings.Contains(listeners[2].Command, "fake-password") {
		t.Errorf("credential leaked from a command line: %q", listeners[2].Command)
	}
	if listeners[3].Process != "" || listeners[3].Command != "" {
		t.Errorf("unattributed socket was modified: %+v", listeners[3])
	}
}

func TestSplitFieldsN(t *testing.T) {
	t.Parallel()
	got := splitFieldsN("a  b\tc d e f", 3)
	want := []string{"a", "b", "c d e f"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitFieldsN = %v, want %v", got, want)
	}
}

func TestStripBullet(t *testing.T) {
	t.Parallel()
	if got := stripBullet("● nginx.service loaded failed failed nginx"); !strings.HasPrefix(got, "nginx.service") {
		t.Errorf("bullet not stripped: %q", got)
	}
}
