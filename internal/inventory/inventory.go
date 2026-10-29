// Package inventory defines the wire format produced by the discovery binary.
//
// This package intentionally contains only data types and trivial helpers: no
// collection logic, no I/O. The ingest API will import this same
// package so that producer and consumer share one compile-checked schema.
//
// Compatibility rules for this package:
//
//   - Adding a field is a minor change; keep SchemaVersion the same.
//   - Renaming, removing, or changing the meaning of a field requires bumping
//     SchemaVersion so ingest can branch on it.
//   - Every optional section is a pointer with `omitempty` so that "we did not
//     look" (null) is distinguishable from "we looked and found nothing" ([]).
//     That distinction matters a lot when you are auditing an untidy server.
package inventory

import "time"

// SchemaVersion identifies the shape of Inventory. Bump on breaking changes.
//
// v2: Container.Mounts became []Mount (was []string); Filesystem and
// Interface gained Kind; ComposeFile gained ResolvedPath/ResolvedVia.
// v3: Listener gained Owner and Ephemeral; Network gained EphemeralRange.
// v4: Listener.Address no longer carries a "%zone" suffix; the zone moved to
// its own field. Address changed meaning, so this is a bump rather than an
// additive change. Also adds the Proxmox section, which is additive.
// v5: adds the ZFS and Shares sections. Both additive; the bump exists so an
// ingest can tell whether a host predates them.
// Software adds package and runtime inventory without changing schema v5.
const SchemaVersion = "5"

// AgentRevision identifies discover's collected output and behaviour. Bump it
// whenever the agent source changes, then refresh agentSourceHash as directed
// by TestAgentSourceRevision. Older agents omit it and report revision zero.
// Revision 1 starts the extracted project after the collector and module cut.
const AgentRevision = 3

// Inventory is the root document emitted by one run of the discovery binary on
// one machine.
type Inventory struct {
	SchemaVersion string    `json:"schema_version"`
	CollectedAt   time.Time `json:"collected_at"`
	DurationMS    int64     `json:"duration_ms"`

	Tool        Tool         `json:"tool"`
	AgentUpdate *AgentUpdate `json:"agent_update,omitempty"`
	Host        Host         `json:"host"`

	// Optional sections. nil means the collector did not run (disabled, or
	// filtered out with -only). A non-nil section with empty slices means the
	// collector ran and genuinely found nothing.
	Disks      *DiskHealth `json:"disks,omitempty"`
	KernelIO   *KernelIO   `json:"kernel_io,omitempty"`
	Systemd    *Systemd    `json:"systemd,omitempty"`
	Network    *Network    `json:"network,omitempty"`
	Docker     *Docker     `json:"docker,omitempty"`
	Proxmox    *Proxmox    `json:"proxmox,omitempty"`
	ZFS        *ZFS        `json:"zfs,omitempty"`
	Shares     *Shares     `json:"shares,omitempty"`
	Scheduling *Scheduling `json:"scheduling,omitempty"`
	VPN        *VPN        `json:"vpn,omitempty"`
	Software   *Software   `json:"software,omitempty"`
	Firewall   *Firewall   `json:"firewall,omitempty"`

	// Diagnostics records everything that went sideways during collection.
	// A partial inventory with loud diagnostics is far more useful than a
	// hard failure, so collectors almost never abort the run.
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// AgentUpdate is added by the privileged posting runner, never discover.
// Additive in schema v5. Sequence is the installed signed release; failures
// counts consecutive failed update checks/trials. Result contains no remote text.
type AgentUpdate struct {
	Sequence    uint64    `json:"sequence"`
	AttemptedAt time.Time `json:"attempted_at"`
	Result      string    `json:"result"`
	Failures    int       `json:"failures"`
}

// Tool records provenance: which binary produced this document, how, and as
// whom. Permission-sensitive collectors behave differently as root, so
// recording Elevated is essential for interpreting the results later.
type Tool struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Revision  int      `json:"revision,omitempty"`
	GoVersion string   `json:"go_version"`
	Platform  string   `json:"platform"`
	Args      []string `json:"args"`
	User      string   `json:"user"`
	UID       int      `json:"uid"`
	Elevated  bool     `json:"elevated"`
}

// Severity classifies a Diagnostic.
type Severity string

const (
	// SeverityInfo notes an expected absence, e.g. "docker not installed".
	SeverityInfo Severity = "info"
	// SeverityWarn means the section is present but incomplete, e.g. a file
	// could not be read because we are not root.
	SeverityWarn Severity = "warn"
	// SeverityError means the collector failed outright.
	SeverityError Severity = "error"
)

// Diagnostic is a machine-readable note about collection quality.
type Diagnostic struct {
	Collector string    `json:"collector"`
	Severity  Severity  `json:"severity"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// ---------------------------------------------------------------- host

// Host holds basic identity and health facts about the machine.
type Host struct {
	Hostname       string       `json:"hostname"`
	FQDN           string       `json:"fqdn,omitempty"`
	MachineID      string       `json:"machine_id,omitempty"`
	OS             OSRelease    `json:"os"`
	Kernel         string       `json:"kernel,omitempty"`
	Architecture   string       `json:"architecture"`
	Virtualization string       `json:"virtualization,omitempty"`
	CPUCount       int          `json:"cpu_count"`
	CPUModel       string       `json:"cpu_model,omitempty"`
	BootTime       time.Time    `json:"boot_time,omitempty"`
	UptimeSeconds  float64      `json:"uptime_seconds"`
	LoadAverage    [3]float64   `json:"load_average"`
	Memory         Memory       `json:"memory"`
	Filesystems    []Filesystem `json:"filesystems"`
	Interfaces     []Interface  `json:"interfaces"`
}

// OSRelease mirrors the interesting subset of /etc/os-release.
type OSRelease struct {
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Version   string `json:"version,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Pretty    string `json:"pretty,omitempty"`
}

// Memory is reported in bytes rather than kB so consumers never have to guess
// at units.
type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
	FreeBytes      uint64 `json:"free_bytes"`
	SwapTotalBytes uint64 `json:"swap_total_bytes"`
	SwapFreeBytes  uint64 `json:"swap_free_bytes"`
}

// FilesystemKind classifies a mount so consumers can filter without having to
// know that overlay2 means Docker and squashfs means snap.
//
// On a container host the raw mount list is dominated by layer and snap
// mounts. They are kept rather than
// dropped — "which snap revisions are still mounted?" is a real question — but
// a dashboard should show FSKindReal by default.
type FilesystemKind string

const (
	// FSKindReal is durable storage you care about: root, /boot, data disks.
	FSKindReal FilesystemKind = "real"
	// FSKindNetwork is NFS/SMB, i.e. the TrueNAS shares.
	FSKindNetwork FilesystemKind = "network"
	// FSKindContainer is a Docker/LXC layer mount.
	FSKindContainer FilesystemKind = "container"
	// FSKindSnap is a squashfs snap revision.
	FSKindSnap FilesystemKind = "snap"
	// FSKindMemory is tmpfs/ramfs.
	FSKindMemory FilesystemKind = "memory"
	// FSKindOther is anything unrecognised, kept so nothing disappears.
	FSKindOther FilesystemKind = "other"
)

// Filesystem is one mounted, non-pseudo filesystem.
type Filesystem struct {
	ConfiguredReadOnly bool           `json:"configured_read_only,omitempty"`
	BindMount          bool           `json:"bind_mount,omitempty"`
	Device             string         `json:"device"`
	MountPoint         string         `json:"mount_point"`
	FSType             string         `json:"fs_type"`
	Kind               FilesystemKind `json:"kind"`
	Options            string         `json:"options,omitempty"`
	TotalBytes         uint64         `json:"total_bytes"`
	UsedBytes          uint64         `json:"used_bytes"`
	FreeBytes          uint64         `json:"free_bytes"`
	UsedPct            float64        `json:"used_pct"`
}

// InterfaceKind classifies an interface. Same rationale as FilesystemKind:
// 45 of 49 interfaces on the main VM are Docker veth pairs and bridges.
type InterfaceKind string

const (
	// IfKindPhysical is a real NIC (or the VM's virtio equivalent).
	IfKindPhysical InterfaceKind = "physical"
	// IfKindLoopback is lo.
	IfKindLoopback InterfaceKind = "loopback"
	// IfKindBridge is a Linux bridge: docker0, br-*, vmbr* on Proxmox.
	IfKindBridge InterfaceKind = "bridge"
	// IfKindVeth is a guest attachment: one end of a container veth pair, or
	// a Proxmox tap/fwln/fwpr device belonging to a VM.
	IfKindVeth InterfaceKind = "veth"
	// IfKindTunnel is a VPN or tunnel device: tailscale0, wg*, tun*.
	IfKindTunnel InterfaceKind = "tunnel"
	// IfKindOther is anything unrecognised.
	IfKindOther InterfaceKind = "other"
)

// Interface is a network interface and its addresses.
type Interface struct {
	Name      string        `json:"name"`
	Kind      InterfaceKind `json:"kind"`
	MAC       string        `json:"mac,omitempty"`
	MTU       int           `json:"mtu"`
	Flags     string        `json:"flags"`
	Addresses []string      `json:"addresses"`
}

// ---------------------------------------------------------------- systemd

// Systemd describes the service manager's view of the world.
type Systemd struct {
	Available   bool    `json:"available"`
	Version     string  `json:"version,omitempty"`
	Units       []Unit  `json:"units"`
	Timers      []Timer `json:"timers"`
	FailedCount int     `json:"failed_count"`
}

// Unit is one service unit. Load/Active/Sub are systemd's own vocabulary and
// are kept verbatim rather than normalised, so nothing is lost in translation.
type Unit struct {
	Name          string `json:"name"`
	Load          string `json:"load"`
	Active        string `json:"active"`
	Sub           string `json:"sub"`
	Description   string `json:"description,omitempty"`
	UnitFileState string `json:"unit_file_state,omitempty"`
	Preset        string `json:"preset,omitempty"`
	// FromTemplate is set on instantiated units (getty@tty1.service) whose
	// enabled/disabled state was read from their template (getty@.service).
	// Recording it means a state that did not come from the unit's own file is
	// visible as such rather than silently attributed.
	FromTemplate string `json:"from_template,omitempty"`
}

// Timer is a systemd timer, i.e. the modern replacement for cron. Anything
// scheduled on the box shows up either here or in Scheduling.
type Timer struct {
	Name      string `json:"name"`
	Next      string `json:"next,omitempty"`
	Left      string `json:"left,omitempty"`
	Last      string `json:"last,omitempty"`
	Passed    string `json:"passed,omitempty"`
	Activates string `json:"activates,omitempty"`
}

// ---------------------------------------------------------------- network

// Network currently covers listening sockets. Firewall rules land here in a
// later increment; the field is deliberately absent rather than empty so its
// arrival is an additive change.
type Network struct {
	// Source records which tool produced Listeners ("ss" or "netstat"), since
	// their parsing quirks differ and you will want to know which you got.
	Source    string     `json:"source"`
	Listeners []Listener `json:"listeners"`

	// Unattributed is true when the listing came from the fallback without
	// process owners (ss -tuln after ss -tulpn failed). Ports are real; owners
	// are unknown, not changed.
	Unattributed bool `json:"unattributed,omitempty"`

	// EphemeralRange is the kernel's dynamic port range, read from
	// /proc/sys/net/ipv4/ip_local_port_range. Anything you assign to a new
	// service should sit below it, or the kernel may hand the same port to an
	// outbound connection before your service binds after a reboot.
	EphemeralRange [2]int `json:"ephemeral_range"`
}

// NetworkFailed reports a run whose network collector errored. Agents before
// the fallback stored an empty listener list in that case rather than null,
// so the diagnostic is the reliable signal.
func (inv *Inventory) NetworkFailed() bool {
	for _, d := range inv.Diagnostics {
		if d.Collector == "network" && d.Severity == SeverityError {
			return true
		}
	}
	return false
}

// Listener is one socket in LISTEN (tcp) or UNCONN (udp) state.
type Listener struct {
	Protocol string `json:"protocol"`
	Family   string `json:"family"`
	Address  string `json:"address"`
	// Zone is the scope identifier for link-local and multicast binds — the
	// "%eth0" in fe80::1%eth0. It is split out rather than left in Address so
	// that a service bound to one multicast group on fifty interfaces reads as
	// one address, not fifty.
	Zone string `json:"zone,omitempty"`
	Port int    `json:"port"`
	// Process is the owning process name. ss and netstat report the kernel's
	// comm, which is capped at 15 characters; when it hits that cap the name
	// is repaired from /proc/<pid>/cmdline.
	Process string `json:"process,omitempty"`
	// Command is the full command line, redacted and length-capped. It is the
	// difference between "python3" and knowing which of your stacks that is.
	Command string `json:"command,omitempty"`
	PID     int    `json:"pid,omitempty"`
	// Exposed is true when the socket is bound to a wildcard address and is
	// therefore reachable from outside the host. This is the single most
	// useful derived field for auditing an old server.
	Exposed bool `json:"exposed"`

	// Ephemeral marks a port the kernel handed out dynamically rather than
	// one a service reserved. RPC services and outbound-heavy daemons pick up
	// these constantly and they change on every restart, so they must not be
	// treated as allocations when you are hunting for a free port.
	Ephemeral bool `json:"ephemeral"`

	// Owner is what runs on this port, resolved during the correlate step.
	// nil means it could not be attributed — usually because the run was
	// unprivileged and there was no PID to work from.
	Owner *ListenerOwner `json:"owner,omitempty"`
}

// OwnerKind distinguishes how a listener was attributed.
type OwnerKind string

const (
	// OwnerContainer means the listening process runs inside a container,
	// determined from its cgroup.
	OwnerContainer OwnerKind = "container"
	// OwnerPublished means the socket belongs to docker-proxy, which forwards
	// a published port into a container. The process is on the host but the
	// service is not.
	OwnerPublished OwnerKind = "published"
	// OwnerUnit means the process belongs to a systemd service.
	OwnerUnit OwnerKind = "unit"
	// OwnerProcess means we identified the process but nothing above it.
	OwnerProcess OwnerKind = "process"
)

// ListenerOwner names what is behind a listening socket.
//
// This is the join that makes the inventory answer "what is on :8096?" — the
// question you actually ask before adding a service. Three mechanisms feed it:
// a process's cgroup names its container, a cgroup under system.slice names
// its systemd unit, and docker's published-port table maps a docker-proxy
// socket to the container it forwards to.
type ListenerOwner struct {
	Kind OwnerKind `json:"kind"`
	// Name is the container name, unit name, or process name.
	Name           string `json:"name"`
	ContainerID    string `json:"container_id,omitempty"`
	ComposeProject string `json:"compose_project,omitempty"`
	ComposeService string `json:"compose_service,omitempty"`
	// ContainerPort is the port inside the container, when it differs from the
	// host port because of a published mapping.
	ContainerPort int `json:"container_port,omitempty"`
}

// ---------------------------------------------------------------- docker

// Docker describes the container runtime, if present.
type Docker struct {
	Available       bool             `json:"available"`
	Version         string           `json:"version,omitempty"`
	Containers      []Container      `json:"containers"`
	ComposeProjects []ComposeProject `json:"compose_projects"`
}

// Container is one container, running or not.
//
// Note the deliberate omission: we do not collect container environment
// variables. They are the single richest source of secrets on a homelab box
// and are almost never needed for inventory purposes. Labels are collected
// because compose relies on them, and are redacted like everything else.
type Container struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	State          string            `json:"state"`
	Status         string            `json:"status"`
	Command        string            `json:"command,omitempty"`
	CreatedAt      string            `json:"created_at,omitempty"`
	Ports          []string          `json:"ports,omitempty"`
	Networks       []string          `json:"networks,omitempty"`
	Mounts         []Mount           `json:"mounts"`
	Labels         map[string]string `json:"labels,omitempty"`
	ComposeProject string            `json:"compose_project,omitempty"`
	ComposeService string            `json:"compose_service,omitempty"`

	// RestartCount is Docker's own tally, which turns the crash-loop rule from
	// an inference into a fact. Uptime-versus-age says "this looks like it
	// restarted"; this says how many times.
	RestartCount int `json:"restart_count,omitempty"`
	// NetworkMode is "host", "bridge", a network name, or "container:<id>"
	// when this container shares another's network namespace. The last is the
	// one that records network namespace sharing.
	NetworkMode string `json:"network_mode,omitempty"`
	// DependsOn is the compose dependency, taken from the label compose writes.
	DependsOn []string `json:"depends_on,omitempty"`
}

// Mount is one bind or volume attached to a container.
//
// Source is the host path, Destination the container path. Keeping both is
// what lets the collector translate container-visible paths (Portainer's
// /data/compose/<id>) back into something readable on the host.
type Mount struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ReadWrite   bool   `json:"read_write"`
}

// ComposeProject groups containers that share a compose project label and
// points at the compose files that defined them.
type ComposeProject struct {
	Name        string        `json:"name"`
	WorkingDir  string        `json:"working_dir,omitempty"`
	Services    []string      `json:"services"`
	ConfigFiles []ComposeFile `json:"config_files"`
}

// ComposeFile is a compose YAML found via container labels. Content is stored
// post-redaction; SHA256 is of the redacted content so that comparing two runs
// tells you whether the file changed without ever storing the plaintext.
type ComposeFile struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// ResolvedPath is set when Path was container-visible only and had to be
	// translated through a mount to be read. ResolvedVia names the mount used,
	// so a surprising result is traceable rather than magic.
	ResolvedPath string `json:"resolved_path,omitempty"`
	ResolvedVia  string `json:"resolved_via,omitempty"`

	SizeBytes int64     `json:"size_bytes,omitempty"`
	ModTime   time.Time `json:"mod_time,omitempty"`
	SHA256    string    `json:"sha256,omitempty"`
	Content   string    `json:"content,omitempty"`
	ReadError string    `json:"read_error,omitempty"`
}

// ---------------------------------------------------------------- proxmox

// Proxmox describes a PVE node and the guests it hosts.
//
// This is the section that answers "what is actually on this box" for the
// hypervisor itself. Everything else in the inventory describes one machine;
// this describes the machines inside it.
type Proxmox struct {
	Available bool      `json:"available"`
	Version   string    `json:"version,omitempty"`
	Node      string    `json:"node,omitempty"`
	Guests    []Guest   `json:"guests"`
	Storages  []Storage `json:"storages"`
	// Backups is every archive found on storages that hold them. Empty means
	// looked-and-found-none, which on a node with running guests is itself the
	// finding rather than the absence of one.
	Backups []Backup `json:"backups"`
	// BackupJobs is what is *scheduled*, which is a different question from
	// what exists. An old archive can mean a job that is failing or a job that
	// never included the guest, and only the schedule tells you which. On this
	// node the answer was the second: one enabled job covering one of nine
	// guests.
	BackupJobs []BackupJob `json:"backup_jobs"`
}

// GuestType distinguishes a full VM from a container.
type GuestType string

const (
	// GuestQEMU is a full virtual machine.
	GuestQEMU GuestType = "qemu"
	// GuestLXC is a system container.
	GuestLXC GuestType = "lxc"
)

// Guest is one VM or LXC on the node.
type Guest struct {
	VMID   int       `json:"vmid"`
	Name   string    `json:"name"`
	Type   GuestType `json:"type"`
	Status string    `json:"status"`
	// OnBoot mirrors the guest's autostart setting. A guest that is running
	// but not set to start on boot is the classic homelab landmine: it comes
	// back from a reboot missing a service nobody remembers configuring.
	OnBoot        bool     `json:"on_boot"`
	CPUs          int      `json:"cpus,omitempty"`
	MemoryBytes   uint64   `json:"memory_bytes,omitempty"`
	DiskBytes     uint64   `json:"disk_bytes,omitempty"`
	UptimeSeconds int64    `json:"uptime_seconds,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Description   string   `json:"description,omitempty"`

	Networks []GuestNIC  `json:"networks"`
	Disks    []GuestDisk `json:"disks"`

	// Config is the guest's full configuration, redacted. Structured fields
	// above cover the common questions; this is here so that an oddity you did
	// not anticipate is still visible rather than silently dropped.
	Config map[string]string `json:"config,omitempty"`
	// ConfigError records why the configuration could not be read, if it
	// could not. A guest that appears with no config is otherwise
	// indistinguishable from one with an empty config.
	ConfigError string `json:"config_error,omitempty"`
}

// GuestNIC is one virtual network interface, parsed from a netN config line.
type GuestNIC struct {
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	MAC      string `json:"mac,omitempty"`
	Bridge   string `json:"bridge,omitempty"`
	VLANTag  int    `json:"vlan_tag,omitempty"`
	Firewall bool   `json:"firewall"`
	IP       string `json:"ip,omitempty"`
}

// GuestDisk is one virtual disk or mount point.
type GuestDisk struct {
	Name    string `json:"name"`
	Storage string `json:"storage,omitempty"`
	Volume  string `json:"volume,omitempty"`
	Size    string `json:"size,omitempty"`
	// Backup is nil when the guest does not say, which is not the same as
	// "excluded from backups".
	Backup *bool `json:"backup,omitempty"`
}

// Storage is one configured PVE storage backend.
type Storage struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Content    []string `json:"content,omitempty"`
	Active     bool     `json:"active"`
	Enabled    bool     `json:"enabled"`
	TotalBytes uint64   `json:"total_bytes,omitempty"`
	UsedBytes  uint64   `json:"used_bytes,omitempty"`
	AvailBytes uint64   `json:"avail_bytes,omitempty"`
}

// ---------------------------------------------------------------- zfs

// ZFS describes pools and datasets.
//
// This section exists because the Filesystems list cannot answer capacity
// questions about ZFS. Datasets in a pool share free space, so a per-dataset
// statfs repeats the same pool free space for each dataset. Pool-level numbers come from zpool,
// which is the only source that gets this right.
type ZFS struct {
	Available bool       `json:"available"`
	Pools     []ZPool    `json:"pools"`
	Datasets  []ZDataset `json:"datasets"`
	// Snapshots is a per-dataset summary rather than a list.
	//
	// Individual snapshots are deliberately not collected — a homelab can have
	// tens of thousands and their space is already counted in a dataset's
	// used. But "does this dataset have snapshots at all, and how old is the
	// newest" is a different question, and it is the one that distinguishes a
	// pool that is being protected from one that merely holds data.
	Snapshots []SnapshotSummary `json:"snapshots"`
}

// SnapshotSummary is what a dataset's snapshots look like in aggregate.
type SnapshotSummary struct {
	Dataset string `json:"dataset"`
	Count   int    `json:"count"`
	// Newest and Oldest bound the retention window actually in effect, which
	// is usually more informative than whatever the policy claims.
	Newest time.Time `json:"newest,omitempty"`
	Oldest time.Time `json:"oldest,omitempty"`
	// UsedBytes is the space the snapshots themselves hold down.
	UsedBytes uint64 `json:"used_bytes,omitempty"`
}

// ZPool is one pool with its health and capacity.
type ZPool struct {
	Name   string `json:"name"`
	Health string `json:"health"`

	SizeBytes  uint64 `json:"size_bytes"`
	AllocBytes uint64 `json:"alloc_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
	// CapacityPct is what `zpool list` calls CAP. ZFS performance degrades
	// well before 100%, so this is the number to alert on, not free bytes.
	CapacityPct      int `json:"capacity_pct"`
	FragmentationPct int `json:"fragmentation_pct"`

	// Status and Action are the human-readable advisories zpool status prints
	// when something is wrong. They are empty on a healthy pool.
	Status string `json:"status,omitempty"`
	Action string `json:"action,omitempty"`
	// Scan is the raw "scan:" line: scrub and resilver state, verbatim.
	// Parsing it into fields would lose the several formats ZFS uses.
	Scan string `json:"scan,omitempty"`
	// Errors is the "errors:" line, normally "No known data errors".
	Errors string `json:"errors,omitempty"`

	Vdevs []Vdev `json:"vdevs"`
}

// Vdev is one row of the zpool status config table: a pool, a mirror, or a
// leaf device. Depth preserves the tree without needing a nested structure.
type Vdev struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Depth int    `json:"depth"`
	Read  int    `json:"read_errors"`
	Write int    `json:"write_errors"`
	Cksum int    `json:"cksum_errors"`
	Note  string `json:"note,omitempty"`
}

// ZDataset is one filesystem, volume or snapshot in a pool.
type ZDataset struct {
	Name            string `json:"name"`
	Type            string `json:"type"`
	MountPoint      string `json:"mount_point,omitempty"`
	UsedBytes       uint64 `json:"used_bytes"`
	AvailableBytes  uint64 `json:"available_bytes"`
	ReferencedBytes uint64 `json:"referenced_bytes"`
	QuotaBytes      uint64 `json:"quota_bytes,omitempty"`
	Compression     string `json:"compression,omitempty"`
	Encryption      string `json:"encryption,omitempty"`
}

// ---------------------------------------------------------------- shares

// Shares is what this host exports to others — the server side of the NFS and
// SMB mounts that show up as Filesystems elsewhere in the fleet.
type Shares struct {
	NFS []NFSExport `json:"nfs"`
	SMB []SMBShare  `json:"smb"`
}

// NFSExport is one line of /etc/exports.
type NFSExport struct {
	Path    string      `json:"path"`
	Source  string      `json:"source"`
	Clients []NFSClient `json:"clients"`
}

// NFSClient is one "host(options)" grouping on an export line.
type NFSClient struct {
	// Spec is the host, network or netgroup: 192.0.2.24/24, *, @trusted.
	Spec string `json:"spec"`
	// ReadOnly and Options are derived from the same option list; ReadOnly is
	// promoted because it is the one that matters at a glance.
	ReadOnly bool     `json:"read_only"`
	Options  []string `json:"options,omitempty"`
}

// SMBShare is one non-global section of smb.conf.
type SMBShare struct {
	Name    string            `json:"name"`
	Path    string            `json:"path,omitempty"`
	Comment string            `json:"comment,omitempty"`
	Source  string            `json:"source"`
	Options map[string]string `json:"options,omitempty"`
}

// ---------------------------------------------------------------- scheduling

// Scheduling covers the cron side of "what runs on a timer here". systemd
// timers live in Systemd.Timers.
type Scheduling struct {
	SystemCrontab *CronFile     `json:"system_crontab,omitempty"`
	CronD         []CronFile    `json:"cron_d"`
	UserCrontabs  []CronFile    `json:"user_crontabs"`
	PeriodicDirs  []PeriodicDir `json:"periodic_dirs"`
}

// CronFile is a parsed crontab-format file.
type CronFile struct {
	Path      string      `json:"path"`
	Owner     string      `json:"owner,omitempty"`
	SizeBytes int64       `json:"size_bytes,omitempty"`
	ModTime   time.Time   `json:"mod_time,omitempty"`
	Entries   []CronEntry `json:"entries"`
	ReadError string      `json:"read_error,omitempty"`
}

// CronEntry is one schedule line. Raw is kept alongside the parsed fields
// because crontab syntax has enough dialects that you want the original when
// the parse looks wrong.
type CronEntry struct {
	Raw      string `json:"raw"`
	Schedule string `json:"schedule"`
	User     string `json:"user,omitempty"`
	Command  string `json:"command"`
}

// PeriodicDir is one of /etc/cron.{hourly,daily,weekly,monthly} and the
// scripts dropped into it.
type PeriodicDir struct {
	Path    string   `json:"path"`
	Scripts []string `json:"scripts"`
}

// Firewall is what is filtered, which is the other half of the answer
// `network.listeners[].exposed` gives.
//
// `exposed` says a socket is bound to a wildcard address, and therefore what
// the *process* asked for. It cannot say whether anything can actually reach
// it, because that is decided by the kernel's packet filter, not by the bind.
// A wildcard bind behind a default-deny INPUT chain is unreachable; the same
// bind with an empty ruleset is reachable by anything that can route to the
// machine. Without this section, "exposed" is a fact about intent being read
// as a fact about risk.
type Firewall struct {
	// Available is false when no packet filter could be read at all — no nft,
	// no iptables-save, or neither was readable unprivileged. It is not the
	// same as "no rules", which is Available with an empty Tables.
	Available bool `json:"available"`
	// Sources names the tools that answered: "nft", "iptables-save",
	// "pve-firewall". Recorded because "nftables is empty" means something
	// quite different on a host whose rules live in legacy iptables.
	Sources []string `json:"sources"`

	Tables []FirewallTable `json:"tables"`

	// Proxmox is the node's own firewall layer, which sits above nftables and
	// has its own enable switch. Absent on non-PVE hosts.
	Proxmox *ProxmoxFirewall `json:"proxmox,omitempty"`

	// InboundPosture is the derived headline, and the reason this section
	// exists. It is deliberately not a boolean.
	//
	// The question worth answering is "is my exposed port reachable?", and
	// "are there rules?" does not answer it. host-a has exactly one rule on its
	// INPUT chain — a jump into Tailscale's chain, which accepts tailnet
	// traffic and drops spoofed tailnet source addresses. Counting that as
	// "filtered" would report a host whose forty exposed ports are reachable
	// from the whole LAN as protected. Only the default verdict settles it:
	// under an accept policy a port is reachable unless some rule specifically
	// says otherwise, and deciding that would mean interpreting rule
	// semantics, which this deliberately does not do.
	InboundPosture Posture `json:"inbound_posture"`
	// FiltersInbound is true only for a default-deny posture. Kept as a
	// boolean for consumers that want the simple question, but it is the
	// strict reading: "there is a default-deny inbound", not "there are rules".
	FiltersInbound bool `json:"filters_inbound"`
	// InboundSummary explains the verdict in a sentence, because the bare
	// value invites the reader to assume a bug rather than a finding.
	InboundSummary string `json:"inbound_summary,omitempty"`
}

// Posture is how a host treats inbound traffic nothing matched.
type Posture string

const (
	// PostureDefaultDeny means an input base chain defaults to drop or reject:
	// a port is unreachable unless a rule opens it.
	PostureDefaultDeny Posture = "default-deny"
	// PostureDefaultAllow means every input base chain accepts by default. A
	// port is reachable unless a rule closes it, and rules may well exist —
	// but the burden is the other way round.
	PostureDefaultAllow Posture = "default-allow"
	// PostureUnknown means the packet filter could not be read, usually
	// because the collector was not running as root. This is emphatically not
	// the same as "no rules".
	PostureUnknown Posture = "unknown"
)

// FirewallTable is one nftables table, or the synthesised equivalent for a
// legacy iptables table.
type FirewallTable struct {
	// Family is nftables' address family: ip, ip6, inet, arp, bridge, netdev.
	Family string          `json:"family"`
	Name   string          `json:"name"`
	Chains []FirewallChain `json:"chains"`
}

// FirewallChain is one chain and its rules.
type FirewallChain struct {
	Name string `json:"name"`
	// Hook is where the chain attaches: input, forward, output, prerouting,
	// postrouting. Empty for a regular chain that only runs when jumped to,
	// which matters — a chain full of drop rules that nothing jumps to filters
	// nothing.
	Hook string `json:"hook,omitempty"`
	// Policy is the default verdict for packets no rule matched: accept or
	// drop. On a base chain this is the single most important field here.
	Policy   string `json:"policy,omitempty"`
	Priority string `json:"priority,omitempty"`
	// Rules are kept verbatim rather than parsed into a structure.
	//
	// nftables' expression grammar is large and evolving, and a half-parsed
	// rule is worse than an unparsed one: it invites a consumer to reason
	// about a match it did not actually understand. The structure above —
	// which chain, which hook, what policy — is parsed because it is stable
	// and is what the derived verdict needs; the bodies are recorded as text
	// so a human can read exactly what the kernel was told.
	Rules []string `json:"rules"`
}

// Backup is one stored backup archive, as Proxmox reports it.
//
// This answers "when was this guest
// last backed up". A backup that stopped running six weeks ago looks exactly
// like one that ran last night, right up until the moment it matters.
type Backup struct {
	// Storage is the PVE storage it lives on.
	Storage string `json:"storage"`
	VolID   string `json:"volid"`
	// VMID is the guest it belongs to, 0 when it could not be determined.
	VMID    int       `json:"vmid,omitempty"`
	Format  string    `json:"format,omitempty"`
	Size    uint64    `json:"size_bytes,omitempty"`
	Created time.Time `json:"created,omitempty"`
	Notes   string    `json:"notes,omitempty"`
	// Protected backups are exempt from retention pruning.
	Protected bool `json:"protected,omitempty"`
}

// BackupJob is a scheduled vzdump job as Proxmox defines it.
type BackupJob struct {
	ID       string `json:"id"`
	Enabled  bool   `json:"enabled"`
	Schedule string `json:"schedule,omitempty"`
	Storage  string `json:"storage,omitempty"`
	Mode     string `json:"mode,omitempty"`
	Node     string `json:"node,omitempty"`
	// All means the job covers every guest, in which case VMIDs is empty and
	// Exclude may list exceptions. Getting this wrong in either direction
	// would invert the coverage answer, which is the whole point of collecting
	// it.
	All     bool  `json:"all,omitempty"`
	VMIDs   []int `json:"vmids,omitempty"`
	Exclude []int `json:"exclude,omitempty"`
	// Pool selects guests by resource pool, which this cannot expand without
	// another API call. Recorded so a consumer knows the coverage answer is
	// incomplete rather than silently treating the job as covering nothing.
	Pool string `json:"pool,omitempty"`
	// NextRun is when Proxmox says it will fire next.
	NextRun time.Time `json:"next_run,omitempty"`
	// Retention is the prune policy, verbatim, because "keep-monthly=1" is a
	// materially different promise from "keep-daily=14".
	Retention string `json:"retention,omitempty"`
}

// Covers reports whether this job includes a guest.
func (j BackupJob) Covers(vmid int) bool {
	for _, e := range j.Exclude {
		if e == vmid {
			return false
		}
	}
	if j.All {
		return true
	}
	for _, v := range j.VMIDs {
		if v == vmid {
			return true
		}
	}
	return false
}

// ProxmoxFirewall is the PVE firewall's own state.
type ProxmoxFirewall struct {
	// Enabled is the datacenter/node switch. Proxmox can be running its
	// firewall service while the firewall itself is disabled, which is exactly
	// the state this node is in, and which `pve-firewall status` reports as
	// "disabled/running" — a string that reads like good news at a glance.
	Enabled bool   `json:"enabled"`
	Status  string `json:"status,omitempty"`
}
