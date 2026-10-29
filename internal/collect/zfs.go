package collect

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"time"
)

// ZFSCollector inventories pools and datasets.
//
// Every command uses -H (no header) and -p (parsable: exact bytes, no "1.5T"
// rounding). Those two flags turn ZFS's human output into something a parser
// can trust, and they are the reason this collector does not have to guess at
// units. The one exception is `zpool status`, which has no machine format on
// some versions, so its output is parsed as text — and the
// fields most likely to change wording are kept verbatim rather than
// interpreted.
type ZFSCollector struct{}

func (ZFSCollector) Name() string { return "zfs" }

func (c ZFSCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	// Absent on hosts without ZFS, rather than present-and-empty: an empty
	// pool list would imply we looked and found no pools, when the question
	// does not apply.
	if !env.Run.Available("zpool") {
		return nil
	}
	z := &inventory.ZFS{
		Available: true,
		Pools:     []inventory.ZPool{},
		Datasets:  []inventory.ZDataset{},
		Snapshots: []inventory.SnapshotSummary{},
	}
	inv.ZFS = z

	z.Pools = collectPools(ctx, env)
	z.Datasets = collectDatasets(ctx, env)
	collectSnapshots(ctx, env, z)

	for _, p := range z.Pools {
		if p.Health != "ONLINE" {
			env.Warnf("pool %q is %s: %s", p.Name, p.Health, p.Status)
		}
		// ZFS slows down markedly as a pool fills, well before it is full.
		if p.CapacityPct >= 80 {
			env.Warnf("pool %q is %d%% full", p.Name, p.CapacityPct)
		}
		if p.Errors != "" && !strings.Contains(p.Errors, "No known data errors") {
			env.Warnf("pool %q reports errors: %s", p.Name, p.Errors)
		}
	}
	return nil
}

// zpoolFields is the column set requested from `zpool list`. Naming them
// explicitly means a future ZFS release adding a column cannot shift our
// parsing.
var zpoolFields = []string{"name", "size", "alloc", "free", "frag", "cap", "health"}

func collectPools(ctx context.Context, env *Env) []inventory.ZPool {
	res, err := env.Run.Run(ctx, "zpool", "list", "-Hp", "-o", strings.Join(zpoolFields, ","))
	if err != nil {
		env.WarnErr("zpool list failed", err)
		return []inventory.ZPool{}
	}
	if !res.OK() {
		env.Warnf("zpool list exited %d: %s", res.ExitCode, firstLine(res.Stderr))
		return []inventory.ZPool{}
	}

	pools := []inventory.ZPool{}
	for _, line := range res.Lines() {
		f := strings.Split(line, "\t")
		if len(f) < len(zpoolFields) {
			continue
		}
		p := inventory.ZPool{
			Name:             f[0],
			SizeBytes:        parseUint(f[1]),
			AllocBytes:       parseUint(f[2]),
			FreeBytes:        parseUint(f[3]),
			FragmentationPct: parsePct(f[4]),
			CapacityPct:      parsePct(f[5]),
			Health:           f[6],
			Vdevs:            []inventory.Vdev{},
		}
		readPoolStatus(ctx, env, &p)
		pools = append(pools, p)
	}
	sort.Slice(pools, func(a, b int) bool { return pools[a].Name < pools[b].Name })
	return pools
}

func readPoolStatus(ctx context.Context, env *Env, p *inventory.ZPool) {
	res, err := env.Run.Run(ctx, "zpool", "status", p.Name)
	if err != nil {
		env.WarnErr("zpool status "+p.Name+" failed", err)
		return
	}
	if !res.OK() {
		env.Warnf("zpool status %s exited %d", p.Name, res.ExitCode)
		return
	}
	parsePoolStatus(res.Stdout, p)
}

// parsePoolStatus reads `zpool status` output into an existing pool.
//
// The format is a set of "key: value" advisories, some spanning several
// indented lines, followed by a config table:
//
//	  pool: secure
//	 state: DEGRADED
//	status: One or more devices could not be opened...
//	action: Attach the missing device and online it using 'zpool online'.
//	  scan: scrub repaired 0B in 04:12:33 with 0 errors on Sun Jul 13 2026
//	config:
//
//		NAME        STATE     READ WRITE CKSUM
//		secure      DEGRADED     0     0     0
//		  mirror-0  DEGRADED     0     0     0
//		    sda2    ONLINE       0     0     0
//		    sdb2    UNAVAIL      0     0     0
//
//	errors: No known data errors
func parsePoolStatus(out string, p *inventory.ZPool) {
	var section string
	var inConfig bool

	appendTo := func(field *string, text string) {
		text = strings.TrimSpace(text)
		if text == "" {
			return
		}
		if *field == "" {
			*field = text
		} else {
			*field += " " + text
		}
	}

	for _, raw := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(raw)

		if trimmed == "" {
			continue
		}
		if trimmed == "config:" {
			inConfig, section = true, ""
			continue
		}

		// A new advisory starts with "key:" at the start of the trimmed line.
		if key, rest, ok := strings.Cut(trimmed, ":"); ok && isStatusKey(key) {
			section = strings.TrimSpace(key)
			inConfig = false
			switch section {
			case "status":
				appendTo(&p.Status, rest)
			case "action":
				appendTo(&p.Action, rest)
			case "scan":
				appendTo(&p.Scan, rest)
			case "errors":
				appendTo(&p.Errors, rest)
			}
			continue
		}

		if inConfig {
			if v, ok := parseVdevRow(raw); ok {
				p.Vdevs = append(p.Vdevs, v)
			}
			continue
		}

		// A continuation line of the advisory we are inside.
		switch section {
		case "status":
			appendTo(&p.Status, trimmed)
		case "action":
			appendTo(&p.Action, trimmed)
		case "scan":
			appendTo(&p.Scan, trimmed)
		case "errors":
			appendTo(&p.Errors, trimmed)
		}
	}
}

// statusKeys are the advisory labels zpool status emits. Restricting to a
// known set stops a device named "sda2" or a scan line containing a colon from
// being mistaken for a new section.
var statusKeys = map[string]bool{
	"pool": true, "state": true, "status": true, "action": true,
	"scan": true, "scrub": true, "see": true, "errors": true, "config": true,
	"remove": true, "checkpoint": true,
}

func isStatusKey(k string) bool { return statusKeys[strings.TrimSpace(k)] }

// parseVdevRow reads one line of the config table. Leading whitespace encodes
// tree depth, which is how zpool shows that a disk belongs to a mirror.
func parseVdevRow(raw string) (inventory.Vdev, bool) {
	line := strings.TrimRight(raw, " \t")
	if strings.TrimSpace(line) == "" {
		return inventory.Vdev{}, false
	}
	f := strings.Fields(line)
	if len(f) < 2 || f[0] == "NAME" {
		return inventory.Vdev{}, false
	}

	// Depth: zpool indents two spaces per level, after a leading tab.
	indent := len(line) - len(strings.TrimLeft(line, " \t"))
	trimmedLeading := strings.TrimLeft(line, "\t")
	spaces := len(trimmedLeading) - len(strings.TrimLeft(trimmedLeading, " "))
	if indent == 0 {
		return inventory.Vdev{}, false
	}

	v := inventory.Vdev{Name: f[0], State: f[1], Depth: spaces / 2}
	if len(f) >= 5 {
		v.Read = atoi(f[2])
		v.Write = atoi(f[3])
		v.Cksum = atoi(f[4])
	}
	if len(f) > 5 {
		v.Note = strings.Join(f[5:], " ")
	}
	return v, true
}

// zfsFields is the column set requested from `zfs list`.
var zfsFields = []string{
	"name", "type", "used", "available", "referenced",
	"mountpoint", "quota", "compression", "encryption",
}

func collectDatasets(ctx context.Context, env *Env) []inventory.ZDataset {
	if !env.Run.Available("zfs") {
		return []inventory.ZDataset{}
	}
	// -t filesystem,volume deliberately excludes snapshots: a homelab can have
	// tens of thousands of them and they would swamp the inventory. Their
	// space is already accounted for in a dataset's used bytes.
	res, err := env.Run.Run(ctx, "zfs", "list", "-Hp", "-t", "filesystem,volume",
		"-o", strings.Join(zfsFields, ","))
	if err != nil {
		env.WarnErr("zfs list failed", err)
		return []inventory.ZDataset{}
	}
	if !res.OK() {
		env.Warnf("zfs list exited %d: %s", res.ExitCode, firstLine(res.Stderr))
		return []inventory.ZDataset{}
	}

	out := []inventory.ZDataset{}
	for _, line := range res.Lines() {
		f := strings.Split(line, "\t")
		if len(f) < len(zfsFields) {
			continue
		}
		d := inventory.ZDataset{
			Name:            f[0],
			Type:            f[1],
			UsedBytes:       parseUint(f[2]),
			AvailableBytes:  parseUint(f[3]),
			ReferencedBytes: parseUint(f[4]),
			MountPoint:      dashToEmpty(f[5]),
			QuotaBytes:      parseUint(f[6]),
			Compression:     dashToEmpty(f[7]),
			Encryption:      dashToEmpty(f[8]),
		}
		out = append(out, d)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func parseUint(s string) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parsePct accepts both "43" and "43%": -p suppresses the sign on most ZFS
// versions but not all.
func parsePct(s string) int {
	return atoi(strings.TrimSuffix(strings.TrimSpace(s), "%"))
}

// dashToEmpty converts ZFS's "-" placeholder for an unset property.
func dashToEmpty(s string) string {
	if s == "-" || s == "none" {
		return ""
	}
	return s
}

// collectSnapshots summarises snapshots per dataset.
//
// Individual snapshots are deliberately not stored: a homelab can have tens of
// thousands and their space is already counted in a dataset's used. But "does
// this dataset have any, and how old is the newest" is a different question,
// and it is the one that separates a pool being protected from one merely
// holding data.
//
// The aggregation happens here rather than in a consumer so that the inventory
// stays small no matter how many snapshots exist.
func collectSnapshots(ctx context.Context, env *Env, z *inventory.ZFS) {
	res, err := env.Run.Run(ctx, "zfs", "list", "-Hp", "-t", "snapshot",
		"-o", "name,creation,used")
	if err != nil {
		env.WarnErr("could not list snapshots", err)
		return
	}
	if res.ExitCode != 0 {
		// A pool with no snapshots at all exits non-zero on some versions,
		// which is a finding rather than a failure.
		env.Infof("zfs list -t snapshot exited %d; treating as no snapshots", res.ExitCode)
		return
	}

	type agg struct {
		count          int
		newest, oldest time.Time
		used           uint64
	}
	byDataset := map[string]*agg{}

	for _, line := range strings.Split(res.Stdout, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 {
			continue
		}
		dataset, _, ok := strings.Cut(fields[0], "@")
		if !ok || dataset == "" {
			continue
		}
		secs, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		at := time.Unix(secs, 0)

		a := byDataset[dataset]
		if a == nil {
			a = &agg{newest: at, oldest: at}
			byDataset[dataset] = a
		}
		a.count++
		if at.After(a.newest) {
			a.newest = at
		}
		if at.Before(a.oldest) {
			a.oldest = at
		}
		if len(fields) >= 3 {
			if used, err := strconv.ParseUint(fields[2], 10, 64); err == nil {
				a.used += used
			}
		}
	}

	for name, a := range byDataset {
		z.Snapshots = append(z.Snapshots, inventory.SnapshotSummary{
			Dataset: name, Count: a.count,
			Newest: a.newest, Oldest: a.oldest, UsedBytes: a.used,
		})
	}
	sort.Slice(z.Snapshots, func(i, j int) bool { return z.Snapshots[i].Dataset < z.Snapshots[j].Dataset })
}
