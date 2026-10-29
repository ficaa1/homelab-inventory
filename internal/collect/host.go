package collect

import (
	"context"
	"net"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// HostCollector gathers identity and health facts.
//
// It prefers reading /proc and /sys over shelling out. Files are faster, have
// no locale or version-dependent formatting, and cannot be tricked into doing
// something destructive. `systemd-detect-virt` is the one exception, because
// there is no clean file equivalent.
type HostCollector struct{}

func (HostCollector) Name() string { return "host" }

func (c HostCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	h := inventory.Host{
		Architecture: runtime.GOARCH,
		CPUCount:     runtime.NumCPU(),
	}

	if name, err := os.Hostname(); err == nil {
		h.Hostname = name
		if fqdn, err := net.LookupCNAME(name); err == nil {
			h.FQDN = strings.TrimSuffix(fqdn, ".")
		}
	} else {
		env.WarnErr("could not determine hostname", err)
	}

	if s, err := env.ReadFile("/etc/machine-id"); err == nil {
		h.MachineID = strings.TrimSpace(s)
	}

	if s, err := env.ReadFile("/etc/os-release"); err == nil {
		h.OS = parseOSRelease(s)
	} else {
		env.WarnErr("could not read /etc/os-release", err)
	}

	if s, err := env.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		h.Kernel = strings.TrimSpace(s)
	}

	if s, err := env.ReadFile("/proc/uptime"); err == nil {
		if secs, ok := parseUptime(s); ok {
			h.UptimeSeconds = secs
			h.BootTime = time.Now().Add(-time.Duration(secs * float64(time.Second))).UTC().Truncate(time.Second)
		}
	} else {
		env.WarnErr("could not read /proc/uptime", err)
	}

	if s, err := env.ReadFile("/proc/loadavg"); err == nil {
		h.LoadAverage = parseLoadAvg(s)
	}

	if s, err := env.ReadFile("/proc/meminfo"); err == nil {
		h.Memory = parseMemInfo(s)
	} else {
		env.WarnErr("could not read /proc/meminfo", err)
	}

	if s, err := env.ReadFile("/proc/cpuinfo"); err == nil {
		h.CPUModel = parseCPUModel(s)
	}

	if s, err := env.ReadFile("/proc/mounts"); err == nil {
		h.Filesystems = parseMounts(s)
		annotateMounts(env, h.Filesystems)
	} else {
		env.WarnErr("could not read /proc/mounts", err)
	}

	// Virtualisation tells you whether you are looking at the Proxmox host,
	// a KVM guest, or an LXC container — which changes how you read
	// everything else in this document.
	if env.Run.Available("systemd-detect-virt") {
		if res, err := env.Run.Run(ctx, "systemd-detect-virt"); err == nil {
			v := strings.TrimSpace(res.Stdout)
			if v != "" && v != "none" {
				h.Virtualization = v
			} else {
				h.Virtualization = "none"
			}
		} else {
			env.WarnErr("systemd-detect-virt failed", err)
		}
	}

	h.Interfaces = collectInterfaces(env)

	// Same null-versus-empty contract as everywhere else: these collectors ran,
	// so their slices must serialise as [] even when they found nothing.
	if h.Filesystems == nil {
		h.Filesystems = []inventory.Filesystem{}
	}
	if h.Interfaces == nil {
		h.Interfaces = []inventory.Interface{}
	}

	inv.Host = h
	return nil
}

func collectInterfaces(env *Env) []inventory.Interface {
	ifaces, err := net.Interfaces()
	if err != nil {
		env.WarnErr("could not enumerate network interfaces", err)
		return nil
	}
	out := make([]inventory.Interface, 0, len(ifaces))
	for _, i := range ifaces {
		entry := inventory.Interface{
			Name:      i.Name,
			Kind:      classifyInterface(i.Name, i.Flags),
			MAC:       i.HardwareAddr.String(),
			MTU:       i.MTU,
			Flags:     i.Flags.String(),
			Addresses: []string{},
		}
		addrs, err := i.Addrs()
		if err != nil {
			env.WarnErr("could not read addresses for "+i.Name, err)
		}
		for _, a := range addrs {
			entry.Addresses = append(entry.Addresses, a.String())
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// classifyInterface labels an interface by naming convention.
//
// Naming conventions are not a specification, but they are what every tool on
// a Linux box relies on and they are stable in practice. The alternative —
// reading /sys/class/net/<if>/{bridge,tun_flags,device} — is more rigorous and
// can be swapped in behind this function if a name ever misleads us.
func classifyInterface(name string, flags net.Flags) inventory.InterfaceKind {
	switch {
	case name == "lo" || flags&net.FlagLoopback != 0:
		return inventory.IfKindLoopback

	// Proxmox firewall bridges sit between a VM's tap device and vmbr0.
	case hasAnyPrefix(name, "fwbr"):
		return inventory.IfKindBridge
	// Guest attachments: docker veth pairs, Proxmox tap NICs, and the
	// fwln/fwpr pair that links a VM into its firewall bridge.
	case hasAnyPrefix(name, "veth", "tap", "fwln", "fwpr"):
		return inventory.IfKindVeth

	case hasAnyPrefix(name, "docker", "br-", "vmbr", "virbr", "lxcbr", "cni", "kube-bridge"):
		return inventory.IfKindBridge
	case hasAnyPrefix(name, "tailscale", "wg", "tun", "zt", "ppp", "gre", "wt"):
		return inventory.IfKindTunnel

	// Predictable NIC names (en*, eth*, wl*, ib*) plus bonds and VLANs, which
	// are configuration over a physical link rather than virtual devices.
	case hasAnyPrefix(name, "en", "eth", "wl", "ib", "bond", "vlan"):
		return inventory.IfKindPhysical
	default:
		return inventory.IfKindOther
	}
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// networkFS are filesystems backed by another machine. They are classified
// by type before any path rule, because a
// network mount under /var/lib/docker is still a network mount.
var networkFS = map[string]bool{
	"nfs": true, "nfs3": true, "nfs4": true, "cifs": true, "smbfs": true,
	"smb3": true, "fuse.sshfs": true, "glusterfs": true, "ceph": true,
	"9p": true, "afs": true,
}

// realFS are the types that hold durable local data.
var realFS = map[string]bool{
	"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true,
	"zfs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true,
	"f2fs": true, "jfs": true, "reiserfs": true, "bcachefs": true, "ufs": true,
}

var memoryFS = map[string]bool{"tmpfs": true, "ramfs": true, "devtmpfs": true}

// containerPaths are layer and runtime directories. Note that
// /var/lib/docker/volumes is deliberately absent: a named volume holds your
// actual data and should be classified by its filesystem type, not written off
// as container plumbing.
var containerPaths = []string{
	"/var/lib/docker/overlay2/",
	"/var/lib/docker/containers/",
	"/var/lib/docker/plugins/",
	"/var/lib/docker/btrfs/",
	"/var/lib/containers/storage/",
	"/var/lib/lxc/",
	"/var/lib/lxcfs/",
	"/run/docker/",
}

func classifyFilesystem(fsType, mountPoint string) inventory.FilesystemKind {
	switch {
	case networkFS[fsType]:
		return inventory.FSKindNetwork
	case fsType == "overlay" || fsType == "aufs":
		return inventory.FSKindContainer
	case fsType == "squashfs" && strings.HasPrefix(mountPoint, "/snap/"):
		return inventory.FSKindSnap
	case hasAnyPrefix(mountPoint, containerPaths...):
		return inventory.FSKindContainer
	case memoryFS[fsType]:
		return inventory.FSKindMemory
	case realFS[fsType]:
		return inventory.FSKindReal
	default:
		return inventory.FSKindOther
	}
}

// parseOSRelease handles the shell-ish KEY="value" format of /etc/os-release.
func parseOSRelease(s string) inventory.OSRelease {
	kv := parseKeyValue(s, "=")
	return inventory.OSRelease{
		ID:        unquote(kv["ID"]),
		Name:      unquote(kv["NAME"]),
		Version:   unquote(kv["VERSION"]),
		VersionID: unquote(kv["VERSION_ID"]),
		Pretty:    unquote(kv["PRETTY_NAME"]),
	}
}

func parseUptime(s string) (float64, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	return v, err == nil
}

func parseLoadAvg(s string) [3]float64 {
	var out [3]float64
	fields := strings.Fields(s)
	for i := 0; i < 3 && i < len(fields); i++ {
		out[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return out
}

// parseMemInfo converts the kB-denominated /proc/meminfo into bytes.
func parseMemInfo(s string) inventory.Memory {
	get := func(key string) uint64 {
		for _, line := range strings.Split(s, "\n") {
			name, val, ok := strings.Cut(line, ":")
			if !ok || strings.TrimSpace(name) != key {
				continue
			}
			fields := strings.Fields(val)
			if len(fields) == 0 {
				return 0
			}
			n, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0
			}
			if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
				return n * 1024
			}
			return n
		}
		return 0
	}
	return inventory.Memory{
		TotalBytes:     get("MemTotal"),
		AvailableBytes: get("MemAvailable"),
		FreeBytes:      get("MemFree"),
		SwapTotalBytes: get("SwapTotal"),
		SwapFreeBytes:  get("SwapFree"),
	}
}

func parseCPUModel(s string) string {
	for _, line := range strings.Split(s, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name", "Model", "Hardware", "cpu model":
			return strings.TrimSpace(val)
		}
	}
	return ""
}

// pseudoFS lists filesystem types that are kernel bookkeeping rather than
// storage. Including them would bury the three mounts you actually care about
// under forty cgroup entries.
var pseudoFS = map[string]bool{
	"autofs": true, "bpf": true, "cgroup": true, "cgroup2": true,
	"configfs": true, "debugfs": true, "devpts": true, "devtmpfs": true,
	"efivarfs": true, "fuse.gvfsd-fuse": true, "fusectl": true,
	"hugetlbfs": true, "mqueue": true, "nsfs": true, "proc": true,
	"pstore": true, "ramfs": true, "rpc_pipefs": true, "securityfs": true,
	"selinuxfs": true, "sysfs": true, "tracefs": true,
}

// parseMounts reads /proc/mounts and attaches usage figures.
//
// Network mounts (nfs, cifs) are kept: on this host they are exactly the
// TrueNAS shares we are trying to map. tmpfs is kept too, since an
// undersized /tmp is a real failure mode.
func parseMounts(s string) []inventory.Filesystem {
	var out []inventory.Filesystem
	seen := make(map[string]bool)

	for _, line := range strings.Split(s, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		device, mount, fstype, opts := fields[0], unescapeMount(fields[1]), fields[2], fields[3]
		if pseudoFS[fstype] || seen[mount] {
			continue
		}
		seen[mount] = true

		fs := inventory.Filesystem{
			Device:     device,
			MountPoint: mount,
			FSType:     fstype,
			Kind:       classifyFilesystem(fstype, mount),
			Options:    opts,
		}
		if total, free, avail, ok := diskUsage(mount); ok {
			fs.TotalBytes = total
			fs.FreeBytes = avail
			fs.UsedBytes = total - free
			if total > 0 {
				fs.UsedPct = float64(fs.UsedBytes) / float64(total) * 100
				fs.UsedPct = float64(int(fs.UsedPct*100+0.5)) / 100
			}
		}
		out = append(out, fs)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].MountPoint < out[b].MountPoint })
	return out
}

// unescapeMount decodes the octal escapes the kernel uses for spaces and
// friends in /proc/mounts (a space becomes \040).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
