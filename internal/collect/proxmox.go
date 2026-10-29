package collect

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"

	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
	"time"
)

// ProxmoxCollector inventories the guests on a PVE node.
//
// It uses `pvesh get`, which returns the same JSON the web UI consumes, rather
// than scraping `qm list`'s human table. Guest configuration comes from
// `qm config` / `pct config`, whose "key: value" output is stable and is the
// same text the API returns.
//
// Safety note: pvesh, qm and pct all have destructive verbs sitting next to
// the read-only ones — `pvesh set`, `qm destroy`, `pct stop`. The allowlist in
// internal/sysexec now restricts each of these binaries to specific
// subcommands, so a mistake here fails before a process is created rather than
// reconfiguring a VM.
type ProxmoxCollector struct{}

func (ProxmoxCollector) Name() string { return "proxmox" }

func (c ProxmoxCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	p := &inventory.Proxmox{
		Guests:   []inventory.Guest{},
		Storages: []inventory.Storage{}, Backups: []inventory.Backup{}, BackupJobs: []inventory.BackupJob{},
	}

	// Only attach the section on an actual PVE node. Emitting an empty
	// proxmox block on the Ubuntu VM would imply we looked and found no
	// guests, when the truth is that the question does not apply.
	if !env.Run.Available("pvesh") {
		if _, err := os.Stat(env.Path("/etc/pve")); err != nil {
			return nil
		}
	}
	inv.Proxmox = p
	p.Available = true
	p.Node = inv.Host.Hostname

	if env.Run.Available("pveversion") {
		if res, err := env.Run.Run(ctx, "pveversion"); err == nil && res.OK() {
			if lines := res.Lines(); len(lines) > 0 {
				p.Version = strings.TrimSpace(lines[0])
			}
		}
	}
	if p.Node == "" {
		env.Warnf("node name unknown; guest queries need it")
		return nil
	}

	p.Guests = append(p.Guests, collectGuests(ctx, env, p.Node, inventory.GuestQEMU)...)
	p.Guests = append(p.Guests, collectGuests(ctx, env, p.Node, inventory.GuestLXC)...)
	sort.Slice(p.Guests, func(a, b int) bool { return p.Guests[a].VMID < p.Guests[b].VMID })

	p.Storages = collectStorages(ctx, env, p.Node)
	p.Backups = collectBackups(ctx, env, p.Node, p.Storages)
	p.BackupJobs = collectBackupJobs(ctx, env)

	var running, stopped, noAutostart int
	for _, g := range p.Guests {
		if g.Status == "running" {
			running++
			if !g.OnBoot {
				noAutostart++
			}
		} else {
			stopped++
		}
	}
	env.Infof("%d guest(s): %d running, %d stopped", len(p.Guests), running, stopped)
	if noAutostart > 0 {
		// Worth saying out loud: these are the services that quietly do not
		// come back after a reboot.
		env.Warnf("%d running guest(s) are not set to start on boot", noAutostart)
	}
	return nil
}

// pveGuest is the subset of /nodes/<node>/{qemu,lxc} we use.
type pveGuest struct {
	VMID    int    `json:"vmid"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	CPUs    int    `json:"cpus"`
	MaxMem  uint64 `json:"maxmem"`
	MaxDisk uint64 `json:"maxdisk"`
	Uptime  int64  `json:"uptime"`
	Tags    string `json:"tags"`
}

func collectGuests(ctx context.Context, env *Env, node string, kind inventory.GuestType) []inventory.Guest {
	path := "/nodes/" + node + "/" + string(kind)
	res, err := env.Run.Run(ctx, "pvesh", "get", path, "--output-format=json")
	if err != nil {
		env.WarnErr("listing "+string(kind)+" guests failed", err)
		return nil
	}
	if !res.OK() {
		env.Warnf("pvesh get %s exited %d: %s", path, res.ExitCode, firstLine(res.Stderr))
		return nil
	}

	var raw []pveGuest
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		env.WarnErr("parsing the "+string(kind)+" guest list failed", err)
		return nil
	}

	out := make([]inventory.Guest, 0, len(raw))
	for _, r := range raw {
		g := inventory.Guest{
			VMID:          r.VMID,
			Name:          r.Name,
			Type:          kind,
			Status:        r.Status,
			CPUs:          r.CPUs,
			MemoryBytes:   r.MaxMem,
			DiskBytes:     r.MaxDisk,
			UptimeSeconds: r.Uptime,
			Networks:      []inventory.GuestNIC{},
			Disks:         []inventory.GuestDisk{},
		}
		if r.Tags != "" {
			g.Tags = strings.FieldsFunc(r.Tags, func(c rune) bool { return c == ';' || c == ',' })
		}
		readGuestConfig(ctx, env, &g)
		out = append(out, g)
	}
	return out
}

// readGuestConfig fills in the structured and raw configuration for a guest.
func readGuestConfig(ctx context.Context, env *Env, g *inventory.Guest) {
	bin := "qm"
	if g.Type == inventory.GuestLXC {
		bin = "pct"
	}
	if !env.Run.Available(bin) {
		g.ConfigError = bin + " not available"
		return
	}

	res, err := env.Run.Run(ctx, bin, "config", strconv.Itoa(g.VMID))
	if err != nil {
		g.ConfigError = err.Error()
		env.WarnErr("reading config for guest "+strconv.Itoa(g.VMID)+" failed", err)
		return
	}
	if !res.OK() {
		g.ConfigError = firstLine(res.Stderr)
		env.Warnf("%s config %d exited %d: %s", bin, g.VMID, res.ExitCode, firstLine(res.Stderr))
		return
	}

	cfg := parseKeyValue(res.Stdout, ":")
	g.Config = map[string]string{}
	for k, v := range cfg {
		switch {
		case k == "onboot":
			g.OnBoot = v == "1"
		case k == "description":
			g.Description = redact.String(v)
		case k == "cores" && g.CPUs == 0:
			g.CPUs = atoi(v)
		case strings.HasPrefix(k, "net") && isIndexed(k, "net"):
			g.Networks = append(g.Networks, parseGuestNIC(k, v))
		case isDiskKey(k):
			g.Disks = append(g.Disks, parseGuestDisk(k, v))
		}
		// Every key is kept, redacted. cipassword and sshkeys live here on
		// cloud-init guests and are exactly what must not reach the JSON.
		g.Config[k] = redact.Value(k, v)
	}

	sort.Slice(g.Networks, func(a, b int) bool { return g.Networks[a].Name < g.Networks[b].Name })
	sort.Slice(g.Disks, func(a, b int) bool { return g.Disks[a].Name < g.Disks[b].Name })
}

// diskPrefixes are the config keys that describe storage. rootfs and mpN are
// LXC; the rest are QEMU bus names.
var diskPrefixes = []string{"scsi", "sata", "virtio", "ide", "mp", "unused", "efidisk", "tpmstate"}

func isDiskKey(k string) bool {
	if k == "rootfs" {
		return true
	}
	for _, p := range diskPrefixes {
		if isIndexed(k, p) {
			return true
		}
	}
	return false
}

// isIndexed reports whether k is prefix followed by digits — net0, scsi1, mp2.
// Without the digit check, "scsihw" (the controller model) would be mistaken
// for a disk.
func isIndexed(k, prefix string) bool {
	rest, ok := strings.CutPrefix(k, prefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseGuestNIC reads a netN value:
//
//	virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0,firewall=1,tag=20
//
// The model is the one field expressed as a bare key=value rather than a named
// option, which is why it is handled separately from the rest.
func parseGuestNIC(name, value string) inventory.GuestNIC {
	nic := inventory.GuestNIC{Name: name}
	for _, part := range strings.Split(value, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "bridge":
			nic.Bridge = v
		case "firewall":
			nic.Firewall = v == "1"
		case "tag":
			nic.VLANTag = atoi(v)
		case "ip":
			nic.IP = v
		case "hwaddr": // LXC spells it out
			nic.MAC = v
		case "name", "type", "mtu", "rate", "link_down", "queues", "gw", "ip6", "gw6":
			// Recognised but not promoted to a field; still in Config.
		default:
			// A bare model=MAC pair: virtio=AA:BB:...
			if looksLikeMAC(v) {
				nic.Model = k
				nic.MAC = v
			}
		}
	}
	return nic
}

func looksLikeMAC(s string) bool {
	if len(s) != 17 {
		return false
	}
	for i, r := range s {
		if (i+1)%3 == 0 {
			if r != ':' && r != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// parseGuestDisk reads a disk value:
//
//	local-lvm:vm-100-disk-0,size=32G,backup=0
//	/mnt/share,mp=/media,backup=0        (an LXC bind mount: no storage)
func parseGuestDisk(name, value string) inventory.GuestDisk {
	d := inventory.GuestDisk{Name: name}
	parts := strings.Split(value, ",")
	if len(parts) > 0 {
		// The first field is storage:volume, or a bare host path for a bind
		// mount, which has no colon.
		if storage, volume, ok := strings.Cut(parts[0], ":"); ok {
			d.Storage, d.Volume = storage, volume
		} else {
			d.Volume = parts[0]
		}
	}
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		switch k {
		case "size":
			d.Size = v
		case "backup":
			b := v == "1"
			d.Backup = &b
		}
	}
	return d
}

// pveStorage is the subset of /nodes/<node>/storage we use.
type pveStorage struct {
	Storage string `json:"storage"`
	Type    string `json:"type"`
	Content string `json:"content"`
	Active  int    `json:"active"`
	Enabled int    `json:"enabled"`
	Total   uint64 `json:"total"`
	Used    uint64 `json:"used"`
	Avail   uint64 `json:"avail"`
}

func collectStorages(ctx context.Context, env *Env, node string) []inventory.Storage {
	path := "/nodes/" + node + "/storage"
	res, err := env.Run.Run(ctx, "pvesh", "get", path, "--output-format=json")
	if err != nil {
		env.WarnErr("listing storages failed", err)
		return []inventory.Storage{}
	}
	if !res.OK() {
		env.Warnf("pvesh get %s exited %d: %s", path, res.ExitCode, firstLine(res.Stderr))
		return []inventory.Storage{}
	}

	var raw []pveStorage
	if err := json.Unmarshal([]byte(res.Stdout), &raw); err != nil {
		env.WarnErr("parsing the storage list failed", err)
		return []inventory.Storage{}
	}

	out := make([]inventory.Storage, 0, len(raw))
	for _, r := range raw {
		s := inventory.Storage{
			Name:       r.Storage,
			Type:       r.Type,
			Active:     r.Active == 1,
			Enabled:    r.Enabled == 1,
			TotalBytes: r.Total,
			UsedBytes:  r.Used,
			AvailBytes: r.Avail,
		}
		if r.Content != "" {
			s.Content = strings.Split(r.Content, ",")
			sort.Strings(s.Content)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// collectBackups lists every backup archive on storages that hold them.
//
// Backup timestamps answer "when was this guest last backed up". A job that stopped two
// years ago looks exactly like one that ran last night, right up until the
// moment it matters.
//
// Only storages whose content list includes "backup" are queried, so this adds
// one API call per backup storage rather than one per storage.
func collectBackups(ctx context.Context, env *Env, node string, storages []inventory.Storage) []inventory.Backup {
	out := []inventory.Backup{}

	for _, st := range storages {
		if !holdsContent(st.Content, "backup") {
			continue
		}
		path := fmt.Sprintf("/nodes/%s/storage/%s/content", node, st.Name)
		res, err := env.Run.Run(ctx, "pvesh", "get", path, "--output-format=json")
		if err != nil {
			env.WarnErr("could not list backups on "+st.Name, err)
			continue
		}
		if res.ExitCode != 0 {
			env.Warnf("pvesh get %s exited %d: %s", path, res.ExitCode, firstLine(res.Stderr))
			continue
		}

		var items []struct {
			VolID     string          `json:"volid"`
			Content   string          `json:"content"`
			Format    string          `json:"format"`
			Size      uint64          `json:"size"`
			CTime     int64           `json:"ctime"`
			VMID      json.RawMessage `json:"vmid"`
			Notes     string          `json:"notes"`
			Protected int             `json:"protected"`
		}
		if err := json.Unmarshal([]byte(res.Stdout), &items); err != nil {
			env.WarnErr("could not parse backup list for "+st.Name, err)
			continue
		}

		for _, it := range items {
			if it.Content != "backup" {
				continue
			}
			b := inventory.Backup{
				Storage: st.Name, VolID: it.VolID, Format: it.Format,
				Size: it.Size, Notes: redact.String(it.Notes),
				Protected: it.Protected != 0,
			}
			if it.CTime > 0 {
				b.Created = time.Unix(it.CTime, 0)
			}
			// vmid arrives as a number from some endpoints and a string from
			// others, which is why it is decoded as raw JSON first.
			b.VMID = parseFlexibleInt(it.VMID)
			out = append(out, b)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].VMID != out[j].VMID {
			return out[i].VMID < out[j].VMID
		}
		return out[i].Created.After(out[j].Created)
	})
	return out
}

// parseFlexibleInt reads a JSON value that may be a number or a quoted number.
func parseFlexibleInt(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.Atoi(s); err == nil {
			return v
		}
	}
	return 0
}

// holdsContent reports whether a storage is declared to hold a content type.
func holdsContent(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// collectBackupJobs reads the scheduled vzdump jobs.
//
// This answers a different question from collectBackups, and the difference
// turned out to be the whole story on this node. An archive that is two years
// old can mean a job that has been failing, or a job that never included the
// guest at all — and only the schedule distinguishes them. Here it was the
// second: one enabled job, running every Sunday, covering vmid 102 and nothing
// else. Nothing was broken; six guests were simply never in it.
func collectBackupJobs(ctx context.Context, env *Env) []inventory.BackupJob {
	out := []inventory.BackupJob{}

	res, err := env.Run.Run(ctx, "pvesh", "get", "/cluster/backup", "--output-format=json")
	if err != nil {
		env.WarnErr("could not list backup jobs", err)
		return out
	}
	if res.ExitCode != 0 {
		env.Warnf("pvesh get /cluster/backup exited %d: %s", res.ExitCode, firstLine(res.Stderr))
		return out
	}

	var items []struct {
		ID       string          `json:"id"`
		Enabled  json.RawMessage `json:"enabled"`
		Schedule string          `json:"schedule"`
		Storage  string          `json:"storage"`
		Mode     string          `json:"mode"`
		Node     string          `json:"node"`
		All      json.RawMessage `json:"all"`
		VMID     json.RawMessage `json:"vmid"`
		Exclude  json.RawMessage `json:"exclude"`
		Pool     string          `json:"pool"`
		NextRun  int64           `json:"next-run"`
		Prune    json.RawMessage `json:"prune-backups"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &items); err != nil {
		env.WarnErr("could not parse backup jobs", err)
		return out
	}

	for _, it := range items {
		j := inventory.BackupJob{
			ID: it.ID, Schedule: it.Schedule, Storage: it.Storage,
			Mode: it.Mode, Node: it.Node, Pool: it.Pool,
			// Proxmox returns these as 1/0, sometimes quoted.
			Enabled: parseFlexibleInt(it.Enabled) != 0,
			All:     parseFlexibleInt(it.All) != 0,
			VMIDs:   parseVMIDList(it.VMID),
			Exclude: parseVMIDList(it.Exclude),
		}
		if it.NextRun > 0 {
			j.NextRun = time.Unix(it.NextRun, 0)
		}
		j.Retention = flattenRetention(it.Prune)
		out = append(out, j)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// parseVMIDList reads Proxmox's guest selector, which arrives as a
// comma-separated string ("100,101,102"), a bare number, or a JSON array
// depending on the endpoint and the version.
func parseVMIDList(raw json.RawMessage) []int {
	if len(raw) == 0 {
		return nil
	}
	var out []int
	add := func(s string) {
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if n, err := strconv.Atoi(part); err == nil {
				out = append(out, n)
			}
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		add(s)
		return out
	}
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return []int{n}
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		for _, e := range arr {
			out = append(out, parseFlexibleInt(e))
		}
	}
	return out
}

// flattenRetention renders the prune policy as "keep-monthly=1", because the
// difference between that and "keep-daily=14" is a materially different
// promise and deserves to be visible rather than buried in a nested object.
func flattenRetention(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}
