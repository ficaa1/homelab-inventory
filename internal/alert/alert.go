// Package alert turns inventories into a list of things that warrant
// attention.
//
// Nothing here collects anything. Every rule reads facts `discover` already
// gathers — pool capacity, scrub state, failed units, restarting containers,
// thin-pool usage — which is the whole reason alerting could be built without
// touching a single collector.
//
// Like internal/portmap, this lives in internal/ rather than inside a command
// because the CLI, the API and eventually the web UI all want the same
// answers. Rules produce structured fields (Value, Threshold, Subject) as well
// as a human-readable Message, so a dashboard can render a gauge rather than
// re-parsing prose.
package alert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Level is an alert's severity.
type Level string

const (
	// LevelCritical is something that is failing now, or is close enough to
	// failing that it will bite without intervention: a degraded pool, a thin
	// pool near 100%, checksum errors.
	LevelCritical Level = "critical"
	// LevelWarning is something wrong that is not yet doing damage: a failed
	// unit, a crash-looping container, a scrub that never ran.
	LevelWarning Level = "warning"
	// LevelInfo is worth knowing but not acting on tonight.
	LevelInfo Level = "info"
)

// rank orders levels for sorting and for the ?level= filter. Higher is worse.
func (l Level) rank() int {
	switch l {
	case LevelCritical:
		return 3
	case LevelWarning:
		return 2
	case LevelInfo:
		return 1
	}
	return 0
}

// AtLeast reports whether l is as severe as min.
func (l Level) AtLeast(min Level) bool { return l.rank() >= min.rank() }

// ParseLevel maps a string to a Level, for query parameters and flags.
func ParseLevel(s string) (Level, bool) {
	switch Level(strings.ToLower(strings.TrimSpace(s))) {
	case LevelCritical:
		return LevelCritical, true
	case LevelWarning:
		return LevelWarning, true
	case LevelInfo:
		return LevelInfo, true
	}
	return "", false
}

// Alert is one thing worth looking at on one host.
type Alert struct {
	Host   string `json:"host"`
	Source string `json:"source,omitempty"`
	// Rule is a stable identifier ("zfs.pool.scrub"). It is what a UI groups
	// on, or what a future silence/acknowledge feature would key off — the
	// Message is prose and will be reworded, the Rule should not be.
	Rule    string `json:"rule"`
	Level   Level  `json:"level"`
	Subject string `json:"subject,omitempty"`
	Message string `json:"message"`

	// Value and Threshold are set on numeric rules so a consumer can render a
	// gauge without parsing Message. Both are omitted when the rule is not
	// about a number.
	Value     float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	// Unit says how to read Value and Threshold, so a UI does not have to
	// infer it from the rule name. UnitPercent is meter-shaped (it has a
	// natural 0-100 scale); UnitHours is not — a scrub 369 days overdue
	// against a 35-day threshold would render as a meter filled ten times
	// over, which is why the distinction is carried rather than guessed.
	Unit Unit `json:"unit,omitempty"`

	// Since is the moment the condition dates from, when that is knowable: the
	// last scrub, or the last inventory received. It is what makes "stale"
	// actionable rather than merely true.
	Since *time.Time `json:"since,omitempty"`

	// FirstSeen is when *this tool* first observed the condition, stamped by
	// internal/seen after evaluation. It is deliberately separate from Since:
	// a scrub cancelled in July 2025 has a Since of last year and a FirstSeen
	// of whenever the agents went live, and conflating them would claim
	// knowledge the fleet did not have.
	FirstSeen *time.Time `json:"first_seen,omitempty"`

	Detail string `json:"detail,omitempty"`

	// Silenced and its two companions are stamped by internal/silence after
	// evaluation, never by a rule. No rule may consider whether something is
	// acknowledged: a rule's job is to say what is true, and a silence only
	// changes how loudly that is reported. Keeping the decision out here is
	// what makes it impossible for an acknowledgement to hide a finding.
	Silenced      bool       `json:"silenced,omitempty"`
	SilenceReason string     `json:"silence_reason,omitempty"`
	SilenceUntil  *time.Time `json:"silence_until,omitempty"`
}

// Key identifies one condition across evaluations. Subject is part of it because
// a rule can fire once per pool, unit or container. Persisted notify and seen
// state and silence IDs depend on these exact bytes.
func (a Alert) Key() string { return a.Host + "\x00" + a.Rule + "\x00" + a.Subject }

// Unit describes what a numeric alert's Value and Threshold are measured in.
type Unit string

const (
	// UnitPercent is a 0-100 proportion: pool and filesystem capacity.
	UnitPercent Unit = "percent"
	// UnitHours is an age, and is unbounded — it can exceed its threshold by
	// any multiple.
	UnitHours Unit = "hours"
)

// Counts summarises a Report.
type Counts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
	Total    int `json:"total"`
}

// Report is the evaluation of every rule against every host.
type Report struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Thresholds  Thresholds `json:"thresholds"`
	Counts      Counts     `json:"counts"`
	// Silenced is how many alerts were acknowledged and therefore left out of
	// Counts. Reported so that a quiet dashboard can never be confused with a
	// healthy one — "0 critical, 4 silenced" says something quite different
	// from "0 critical".
	Silenced int      `json:"silenced,omitempty"`
	Hosts    []string `json:"hosts"`
	Alerts   []Alert  `json:"alerts"`
}

// Thresholds are the numbers the rules compare against.
//
// They are a struct rather than constants so the API can expose them (a
// consumer that cannot see the threshold cannot tell a 76% pool from an 96%
// one without guessing) and so a future config file can override them.
type Thresholds struct {
	// Pool capacity. ZFS degrades well before it is full, which is why these
	// are lower than a filesystem's.
	PoolWarnPct float64 `json:"pool_warn_pct"`
	PoolCritPct float64 `json:"pool_crit_pct"`

	// Thin pools are the sharp edge: at 100% writes error, guests go
	// read-only, and metadata can corrupt. Deliberately the most aggressive
	// numbers here.
	ThinPoolWarnPct float64 `json:"thin_pool_warn_pct"`
	ThinPoolCritPct float64 `json:"thin_pool_crit_pct"`

	// Ordinary filesystems and file-backed PVE storages.
	FilesystemWarnPct float64 `json:"filesystem_warn_pct"`
	FilesystemCritPct float64 `json:"filesystem_crit_pct"`

	// ScrubMaxAge is how long a pool may go unscrubbed before it is worth
	// mentioning. A monthly scrub is the usual convention, so this allows a
	// little over that before complaining.
	ScrubMaxAge time.Duration `json:"scrub_max_age"`

	// StaleAfter / StaleCritAfter bound how long ago a host last reported.
	// This is the rule that only exists because ingest is push-based: a host
	// whose timer died stops appearing in the data rather than reporting a
	// problem, so silence has to be the alert.
	//
	// StaleAfter of 0 disables the rule, which is what a caller analysing a
	// snapshot it just collected by hand wants — there, every host being
	// "stale" is an artefact of how the data was gathered rather than a fault,
	// and it would bury the findings that matter.
	StaleAfter     time.Duration `json:"stale_after"`
	StaleCritAfter time.Duration `json:"stale_crit_after"`

	// BackupMaxAge is how old a guest's newest backup may be. A week is the
	// usual homelab cadence and leaves room for a job that runs on Sundays.
	BackupMaxAge time.Duration `json:"backup_max_age"`
	// SnapshotMaxAge is the same for ZFS snapshots. More generous, because a
	// snapshot policy of "weekly" is common and a slightly stale one is far
	// less alarming than a stale backup.
	SnapshotMaxAge time.Duration `json:"snapshot_max_age"`
}

// DefaultThresholds returns the numbers used when a caller does not care.
func DefaultThresholds() Thresholds {
	return Thresholds{
		PoolWarnPct:       80,
		PoolCritPct:       90,
		ThinPoolWarnPct:   75,
		ThinPoolCritPct:   85,
		FilesystemWarnPct: 85,
		FilesystemCritPct: 95,
		ScrubMaxAge:       35 * 24 * time.Hour,
		// The agent posts every 15 minutes, so three missed runs is a
		// meaningful silence rather than a slow boot.
		StaleAfter:     45 * time.Minute,
		StaleCritAfter: 2 * time.Hour,
		BackupMaxAge:   8 * 24 * time.Hour,
		SnapshotMaxAge: 14 * 24 * time.Hour,
	}
}

// Evaluate runs every rule against every inventory.
//
// now is a parameter rather than a call to time.Now() inside so that age-based
// rules are deterministic under test.
func Evaluate(invs []*inventory.Inventory, th Thresholds, now time.Time) *Report {
	rep := &Report{
		GeneratedAt: now.UTC(),
		Thresholds:  th,
		Hosts:       []string{},
		Alerts:      []Alert{},
	}

	for _, inv := range invs {
		if inv == nil {
			continue
		}
		host := inv.Host.Hostname
		if host == "" {
			host = "(unknown)"
		}
		rep.Hosts = append(rep.Hosts, host)

		e := &evaluator{host: host, th: th, now: now}
		e.staleness(inv)
		e.schema(inv)
		e.agent(inv)
		e.zfs(inv)
		e.proxmox(inv)
		e.systemd(inv)
		e.docker(inv)
		e.filesystems(inv)
		e.diskHealth(inv)
		e.firewall(inv)
		e.images(inv)
		e.backups(inv)
		e.snapshots(inv)
		rep.Alerts = append(rep.Alerts, e.out...)
	}

	sort.Strings(rep.Hosts)
	sortAlerts(rep.Alerts)

	rep.Counts = count(rep.Alerts)
	return rep
}

func sortAlerts(alerts []Alert) {
	sort.SliceStable(alerts, func(a, b int) bool {
		x, y := alerts[a], alerts[b]
		if x.Level != y.Level {
			return x.Level.rank() > y.Level.rank() // worst first
		}
		if x.Host != y.Host {
			return x.Host < y.Host
		}
		if x.Rule != y.Rule {
			return x.Rule < y.Rule
		}
		return x.Subject < y.Subject
	})
}

// Add appends alerts that do not come from an inventory (the API's own
// canary tripwires) and restores the report's ordering and counts.
func (r *Report) Add(extra ...Alert) {
	if len(extra) == 0 {
		return
	}
	r.Alerts = append(r.Alerts, extra...)
	sortAlerts(r.Alerts)
	r.Counts = count(r.Alerts)
}

// FilterHost returns a copy of rep containing only one host's alerts.
func (r *Report) FilterHost(name string) *Report {
	out := *r
	out.Hosts = nil
	out.Alerts = nil
	for _, h := range r.Hosts {
		if h == name {
			out.Hosts = append(out.Hosts, h)
		}
	}
	for _, a := range r.Alerts {
		if a.Host == name {
			out.Alerts = append(out.Alerts, a)
		}
	}
	out.Counts = count(out.Alerts)
	return &out
}

// FilterLevel returns a copy of rep keeping only alerts at least as severe as
// min. Hosts is left alone: a host with nothing wrong is still a host that
// reported.
func (r *Report) FilterLevel(min Level) *Report {
	out := *r
	out.Alerts = nil
	for _, a := range r.Alerts {
		if a.Level.AtLeast(min) {
			out.Alerts = append(out.Alerts, a)
		}
	}
	out.Counts = count(out.Alerts)
	return &out
}

func count(alerts []Alert) Counts {
	var c Counts
	for _, a := range alerts {
		switch a.Level {
		case LevelCritical:
			c.Critical++
		case LevelWarning:
			c.Warning++
		case LevelInfo:
			c.Info++
		}
	}
	c.Total = len(alerts)
	return c
}

// evaluator accumulates one host's alerts.
type evaluator struct {
	host string
	th   Thresholds
	now  time.Time
	out  []Alert
}

func (e *evaluator) add(a Alert) {
	a.Host = e.host
	e.out = append(e.out, a)
}

// ---------------------------------------------------------------- rules

func (e *evaluator) staleness(inv *inventory.Inventory) {
	if e.th.StaleAfter <= 0 || inv.CollectedAt.IsZero() {
		return
	}
	age := e.now.Sub(inv.CollectedAt)
	if age < e.th.StaleAfter {
		return
	}
	level := LevelWarning
	if age >= e.th.StaleCritAfter {
		level = LevelCritical
	}
	at := inv.CollectedAt
	e.add(Alert{
		Rule:      "host.stale",
		Level:     level,
		Subject:   e.host,
		Message:   fmt.Sprintf("last inventory was %s ago; the agent may have stopped posting", roundDuration(age)),
		Value:     age.Hours(),
		Threshold: e.th.StaleAfter.Hours(),
		Unit:      UnitHours,
		Since:     &at,
	})
}

func (e *evaluator) schema(inv *inventory.Inventory) {
	if inv.SchemaVersion == "" || inv.SchemaVersion == inventory.SchemaVersion {
		return
	}
	e.add(Alert{
		Rule:    "host.schema",
		Level:   LevelInfo,
		Subject: e.host,
		Message: fmt.Sprintf("inventory is schema v%s but this build expects v%s; re-run the agent",
			inv.SchemaVersion, inventory.SchemaVersion),
	})
}

// agent compares ordered revisions rather than git hashes because the API can
// be rebuilt without any change to discover's collected output or behaviour.
func (e *evaluator) agent(inv *inventory.Inventory) {
	if inv.Tool.Revision >= inventory.AgentRevision {
		return
	}
	revision := fmt.Sprintf("revision %d", inv.Tool.Revision)
	if inv.Tool.Revision == 0 {
		revision = fmt.Sprintf("unknown/pre-revision, version %q", inv.Tool.Version)
	}
	e.add(Alert{
		Rule:    "agent.outdated",
		Level:   LevelInfo,
		Subject: "agent",
		Message: fmt.Sprintf("discover is out of date (%s, latest %d); check the signed rollout in Hosts (pre-bootstrap hosts: re-run make install-agent)",
			revision, inventory.AgentRevision),
	})
}

func (e *evaluator) zfs(inv *inventory.Inventory) {
	if inv.ZFS == nil || !inv.ZFS.Available {
		return
	}
	for _, p := range inv.ZFS.Pools {
		if p.Health != "" && p.Health != "ONLINE" {
			e.add(Alert{
				Rule:    "zfs.pool.health",
				Level:   LevelCritical,
				Subject: p.Name,
				Message: fmt.Sprintf("pool %s is %s", p.Name, p.Health),
				Detail:  p.Status,
			})
		}

		if pct := float64(p.CapacityPct); pct >= e.th.PoolWarnPct {
			level, threshold := LevelWarning, e.th.PoolWarnPct
			if pct >= e.th.PoolCritPct {
				level, threshold = LevelCritical, e.th.PoolCritPct
			}
			e.add(Alert{
				Rule:      "zfs.pool.capacity",
				Level:     level,
				Subject:   p.Name,
				Message:   fmt.Sprintf("pool %s is %d%% full", p.Name, p.CapacityPct),
				Value:     pct,
				Threshold: threshold,
				Unit:      UnitPercent,
			})
		}

		// "No known data errors" is what a healthy pool says. Anything else is
		// the pool telling you it lost something.
		if p.Errors != "" && !strings.EqualFold(strings.TrimSpace(p.Errors), "No known data errors") {
			e.add(Alert{
				Rule:    "zfs.pool.errors",
				Level:   LevelCritical,
				Subject: p.Name,
				Message: fmt.Sprintf("pool %s reports data errors", p.Name),
				Detail:  p.Errors,
			})
		}

		e.scrub(p)

		for _, v := range p.Vdevs {
			if v.Read == 0 && v.Write == 0 && v.Cksum == 0 {
				continue
			}
			e.add(Alert{
				Rule:    "zfs.vdev.errors",
				Level:   LevelCritical,
				Subject: fmt.Sprintf("%s/%s", p.Name, v.Name),
				Message: fmt.Sprintf("vdev %s has read=%d write=%d cksum=%d errors",
					v.Name, v.Read, v.Write, v.Cksum),
				Detail: v.Note,
			})
		}
	}
}

// scrub alerts on a pool that is not being scrubbed.
//
// The Scan line is kept raw in the schema because ZFS prints it several ways,
// so this reads it rather than relying on parsed fields. A cancelled scrub is
// called out regardless of age: it means the mechanism ran and was stopped,
// which is a different and more misleading failure than never scrubbing at
// all — the pool looks tended and is not.
func (e *evaluator) scrub(p inventory.ZPool) {
	state, at, ok := parseScan(p.Scan)
	switch state {
	case scanCanceled:
		a := Alert{
			Rule:    "zfs.pool.scrub",
			Level:   LevelWarning,
			Subject: p.Name,
			Message: fmt.Sprintf("pool %s: last scrub was cancelled and never rescheduled", p.Name),
			Detail:  p.Scan,
		}
		if ok {
			a.Since = &at
			a.Message = fmt.Sprintf("pool %s: last scrub was cancelled %s ago and never rescheduled",
				p.Name, roundDuration(e.now.Sub(at)))
			a.Value = e.now.Sub(at).Hours()
			a.Threshold = e.th.ScrubMaxAge.Hours()
			a.Unit = UnitHours
		}
		e.add(a)
	case scanNone:
		e.add(Alert{
			Rule:    "zfs.pool.scrub",
			Level:   LevelWarning,
			Subject: p.Name,
			Message: fmt.Sprintf("pool %s has never been scrubbed", p.Name),
			Detail:  p.Scan,
		})
	case scanCompleted:
		if !ok {
			return
		}
		age := e.now.Sub(at)
		if age < e.th.ScrubMaxAge {
			return
		}
		e.add(Alert{
			Rule:      "zfs.pool.scrub",
			Level:     LevelWarning,
			Subject:   p.Name,
			Message:   fmt.Sprintf("pool %s has not been scrubbed in %s", p.Name, roundDuration(age)),
			Value:     age.Hours(),
			Threshold: e.th.ScrubMaxAge.Hours(),
			Unit:      UnitHours,
			Since:     &at,
			Detail:    p.Scan,
		})
	}
	// scanInProgress and an unrecognised line are both left alone: a scrub
	// running now is the good case, and inventing an alert from a line we did
	// not understand would be worse than silence.
}

func (e *evaluator) proxmox(inv *inventory.Inventory) {
	if inv.Proxmox == nil || !inv.Proxmox.Available {
		return
	}

	for _, g := range inv.Proxmox.Guests {
		// A guest that is running but not set to start on boot is the classic
		// homelab landmine: it survives until the next reboot and then
		// silently does not come back.
		if g.Status == "running" && !g.OnBoot {
			e.add(Alert{
				Rule:    "proxmox.guest.autostart",
				Level:   LevelWarning,
				Subject: fmt.Sprintf("%d/%s", g.VMID, g.Name),
				Message: fmt.Sprintf("guest %d (%s) is running but will not start on boot", g.VMID, g.Name),
			})
		}
	}

	for _, s := range inv.Proxmox.Storages {
		if s.TotalBytes == 0 || !s.Active {
			continue
		}
		pct := float64(s.UsedBytes) / float64(s.TotalBytes) * 100

		warn, crit, rule := e.storageThresholds(s.Type)
		if warn == 0 || pct < warn {
			continue
		}
		level, threshold := LevelWarning, warn
		if pct >= crit {
			level, threshold = LevelCritical, crit
		}
		// Plain LVM is the exception: "used" there is space allocated to
		// volumes, not data written, and allocating a whole volume group is
		// normal rather than dangerous. It caps out at info so a full VG shows
		// up as "you cannot create another guest here" and not as an outage.
		if rule == "proxmox.storage.allocation" {
			level = LevelInfo
		}
		e.add(Alert{
			Rule:      rule,
			Level:     level,
			Subject:   s.Name,
			Message:   fmt.Sprintf("%s storage %q is %.0f%% used", s.Type, s.Name, pct),
			Value:     pct,
			Threshold: threshold,
			Unit:      UnitPercent,
		})
	}
}

// storageThresholds picks the numbers for a PVE storage type, and the rule id
// that goes with them. The distinction matters: 96% on an lvmthin pool is an
// emergency, 96% on an lvm volume group is a Tuesday.
func (e *evaluator) storageThresholds(typ string) (warn, crit float64, rule string) {
	switch typ {
	case "lvmthin":
		return e.th.ThinPoolWarnPct, e.th.ThinPoolCritPct, "proxmox.storage.thinpool"
	case "lvm":
		return e.th.FilesystemCritPct, 101, "proxmox.storage.allocation"
	case "zfspool":
		// Already covered, and better, by the zfs section's own pool numbers.
		return 0, 0, ""
	default: // dir, nfs, cifs, and anything else backed by a real filesystem
		return e.th.FilesystemWarnPct, e.th.FilesystemCritPct, "proxmox.storage.capacity"
	}
}

func (e *evaluator) systemd(inv *inventory.Inventory) {
	if inv.Systemd == nil || !inv.Systemd.Available {
		return
	}
	for _, u := range inv.Systemd.Units {
		if u.Active != "failed" && u.Sub != "failed" {
			continue
		}
		e.add(Alert{
			Rule:    "systemd.unit.failed",
			Level:   LevelWarning,
			Subject: u.Name,
			Message: fmt.Sprintf("unit %s has failed", u.Name),
			Detail:  u.Description,
		})
	}
}

func (e *evaluator) docker(inv *inventory.Inventory) {
	if inv.Docker == nil || !inv.Docker.Available {
		return
	}
	for _, c := range inv.Docker.Containers {
		// Same three states cmd/discover's summary calls out. "exited" is
		// deliberately not among them: plenty of containers are meant to run
		// once and stop.
		switch c.State {
		case "restarting", "dead", "paused":
			e.add(Alert{
				Rule:    "docker.container.unhealthy",
				Level:   LevelWarning,
				Subject: c.Name,
				Message: fmt.Sprintf("container %s is %s", c.Name, c.State),
				Detail:  c.Status,
			})
			continue
		}

		// Docker's own healthcheck saying "unhealthy" is not a guess.
		if strings.Contains(strings.ToLower(c.Status), "unhealthy") {
			e.add(Alert{
				Rule:    "docker.container.unhealthy",
				Level:   LevelWarning,
				Subject: c.Name,
				Message: fmt.Sprintf("container %s reports unhealthy", c.Name),
				Detail:  c.Status,
			})
			continue
		}

		// A crash-looping container is only caught in state "restarting" if
		// the inventory happens to be taken during the gap between exit and
		// restart, which is a fraction of each cycle — so a container that
		// dies every few seconds usually reads as plain "running", and this
		// rule can miss it while status reports `running` / `Up 1 second`.
		//
		// Uptime alone would flag every fresh deploy, so it is paired with
		// age: a container created weeks ago that has been up for one second
		// has restarted, and there is no other way to read it.
		if up, ok := shortUptime(c.Status); ok {
			if age, ok := containerAge(c.CreatedAt, e.now); ok && age > containerSettledAfter {
				e.add(Alert{
					Rule:    "docker.container.restarting",
					Level:   LevelWarning,
					Subject: c.Name,
					Message: fmt.Sprintf("container %s has been up %s but was created %s ago; it is restarting",
						c.Name, roughDuration(up), roughDuration(age)),
					Detail: c.Status,
				})
			}
		}
	}
}

// containerSettledAfter is how old a container must be before a very short
// uptime means "it restarted" rather than "it was just deployed".
const containerSettledAfter = time.Hour

// dockerCreatedLayout is what `docker ps` emits for CreatedAt.
const dockerCreatedLayout = "2006-01-02 15:04:05 -0700 MST"

func containerAge(createdAt string, now time.Time) (time.Duration, bool) {
	if createdAt == "" {
		return 0, false
	}
	t, err := time.Parse(dockerCreatedLayout, createdAt)
	if err != nil {
		return 0, false
	}
	d := now.Sub(t)
	if d < 0 {
		return 0, false
	}
	return d, true
}

// shortUptime reads Docker's humanised Status ("Up 4 weeks (healthy)") and
// reports an uptime only when it is measured in seconds or minutes.
//
// Anything longer is not interesting here, so the coarser units are not parsed
// at all rather than being converted approximately — "About an hour" and "4
// weeks" both just mean "not short", and a rule that does not need the number
// should not invent one.
func shortUptime(status string) (time.Duration, bool) {
	const prefix = "Up "
	if !strings.HasPrefix(status, prefix) {
		return 0, false
	}
	rest := status[len(prefix):]
	if strings.HasPrefix(rest, "Less than a second") {
		return time.Second, true
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil || n < 0 {
		return 0, false
	}
	switch strings.TrimSuffix(fields[1], "s") {
	case "second":
		return time.Duration(n) * time.Second, true
	case "minute":
		if n > 5 {
			// Five minutes of uptime is a container that is running, not one
			// that is looping. Above this the signal is gone.
			return 0, false
		}
		return time.Duration(n) * time.Minute, true
	}
	return 0, false
}

// roughDuration renders a duration the way the message wants to read it.
func roughDuration(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "1 second"
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// backups checks that every guest actually has a recent one.
//
// This is the rule most likely to matter, because its failure mode is silent
// and total. A backup job that stopped two years ago produces exactly the same
// evidence as one that ran last night — a storage with archives on it — right
// up until the moment somebody needs to restore.
//
// Guests that are not set to start on boot are held to a gentler standard:
// something deliberately kept switched off is usually not something you are
// still protecting.
func (e *evaluator) backups(inv *inventory.Inventory) {
	if inv.Proxmox == nil || !inv.Proxmox.Available || len(inv.Proxmox.Guests) == 0 {
		return
	}
	// No backup storage at all is a different statement from stale backups,
	// and worth saying once rather than once per guest.
	hasBackupStorage := false
	for _, st := range inv.Proxmox.Storages {
		for _, c := range st.Content {
			if c == "backup" {
				hasBackupStorage = true
			}
		}
	}
	if !hasBackupStorage {
		e.add(Alert{
			Rule:    "proxmox.backup.no_storage",
			Level:   LevelWarning,
			Subject: "backups",
			Message: fmt.Sprintf("no storage on this node holds backups, and it runs %d guests",
				len(inv.Proxmox.Guests)),
		})
		return
	}

	newest := map[int]time.Time{}
	for _, b := range inv.Proxmox.Backups {
		if b.VMID == 0 || b.Created.IsZero() {
			continue
		}
		if b.Created.After(newest[b.VMID]) {
			newest[b.VMID] = b.Created
		}
	}

	// Whether a guest is *scheduled* is a different question from whether it
	// has an archive, and it is the more useful one. An old backup can mean a
	// job that is failing or a job that never included the guest; only the
	// schedule tells you which, and the fix is completely different.
	scheduled := map[int]bool{}
	jobs := 0
	poolJob := false
	for _, j := range inv.Proxmox.BackupJobs {
		if !j.Enabled {
			continue
		}
		jobs++
		if j.Pool != "" {
			// A pool-selected job cannot be expanded without another API call,
			// so coverage is unknown rather than absent. Saying "not backed
			// up" here would be a confident wrong answer.
			poolJob = true
		}
		for _, g := range inv.Proxmox.Guests {
			if j.Covers(g.VMID) {
				scheduled[g.VMID] = true
			}
		}
	}

	if jobs == 0 {
		e.add(Alert{
			Rule:    "proxmox.backup.no_job",
			Level:   LevelWarning,
			Subject: "backups",
			Message: fmt.Sprintf("no enabled backup job exists, and this node runs %d guests",
				len(inv.Proxmox.Guests)),
		})
	}

	for _, g := range inv.Proxmox.Guests {
		name := fmt.Sprintf("%d", g.VMID)
		if g.Name != "" {
			name = fmt.Sprintf("%d (%s)", g.VMID, g.Name)
		}
		subject := fmt.Sprintf("%d", g.VMID)
		level := LevelWarning
		if !g.OnBoot {
			// Something deliberately switched off is usually not something you
			// are still protecting.
			level = LevelInfo
		}
		last, hasArchive := newest[g.VMID]

		// An old archive does not mean a guest is scheduled for future backups.
		if jobs > 0 && !scheduled[g.VMID] && !poolJob {
			detail := "no archive either"
			if hasArchive {
				detail = fmt.Sprintf("the newest archive is from %s and nothing will replace it",
					last.Format("2006-01-02"))
			}
			e.add(Alert{
				Rule:    "proxmox.backup.unscheduled",
				Level:   level,
				Subject: subject,
				Message: fmt.Sprintf("guest %s is not included in any backup job", name),
				Detail:  detail,
			})
			continue
		}

		if !hasArchive {
			e.add(Alert{
				Rule:    "proxmox.backup.missing",
				Level:   level,
				Subject: subject,
				Message: fmt.Sprintf("guest %s has no backup at all", name),
			})
			continue
		}
		age := e.now.Sub(last)
		if age < e.th.BackupMaxAge {
			continue
		}
		// Only claim the guest is scheduled when it actually is. With no
		// enabled job at all, "scheduled for backup but the newest is 712 days
		// old" would send somebody hunting for a failing job that does not
		// exist — the same wrong diagnosis this rule was rewritten to avoid.
		since := last
		msg := fmt.Sprintf("guest %s was last backed up %s ago", name, roughDuration(age))
		detail := "no enabled job would replace it"
		if scheduled[g.VMID] {
			msg = fmt.Sprintf("guest %s is scheduled for backup but the newest is %s old",
				name, roughDuration(age))
			detail = "the job is defined, so this looks like it is failing rather than never running"
		}
		e.add(Alert{
			Rule:      "proxmox.backup.stale",
			Level:     level,
			Subject:   subject,
			Value:     age.Hours(),
			Threshold: e.th.BackupMaxAge.Hours(),
			Unit:      UnitHours,
			Since:     &since,
			Message:   msg,
			Detail:    detail,
		})
	}
}

// snapshots reports datasets whose newest snapshot is old, or absent.
//
// Snapshots are not backups — they live on the same disks — but they are the
// thing that saves you from a mistake rather than from a disk, and a snapshot
// task that quietly stopped is the same class of silent failure as a cancelled
// scrub.
//
// Only pool roots and their immediate children are considered interesting
// enough to alert on; alerting per dataset would produce dozens of lines on a
// fleet with many.
func (e *evaluator) snapshots(inv *inventory.Inventory) {
	if inv.ZFS == nil || !inv.ZFS.Available || len(inv.ZFS.Pools) == 0 {
		return
	}
	byDataset := map[string]inventory.SnapshotSummary{}
	for _, s := range inv.ZFS.Snapshots {
		byDataset[s.Dataset] = s
	}

	for _, p := range inv.ZFS.Pools {
		// A boot pool is rebuilt by the installer and is not something anyone
		// restores a file from.
		if strings.Contains(strings.ToLower(p.Name), "boot") {
			continue
		}
		// Does anything in this pool have a snapshot?
		var newest time.Time
		count := 0
		for name, s := range byDataset {
			if name == p.Name || strings.HasPrefix(name, p.Name+"/") {
				count += s.Count
				if s.Newest.After(newest) {
					newest = s.Newest
				}
			}
		}
		if count == 0 {
			e.add(Alert{
				Rule:    "zfs.snapshot.none",
				Level:   LevelWarning,
				Subject: p.Name,
				Message: fmt.Sprintf("pool %s has no snapshots at all", p.Name),
				Detail:  "a snapshot is not a backup, but it is what recovers a file deleted by mistake",
			})
			continue
		}
		age := e.now.Sub(newest)
		if age < e.th.SnapshotMaxAge {
			continue
		}
		since := newest
		e.add(Alert{
			Rule:      "zfs.snapshot.stale",
			Level:     LevelWarning,
			Subject:   p.Name,
			Value:     age.Hours(),
			Threshold: e.th.SnapshotMaxAge.Hours(),
			Unit:      UnitHours,
			Since:     &since,
			Message: fmt.Sprintf("pool %s: newest snapshot is %s old (%d snapshots)",
				p.Name, roughDuration(age), count),
		})
	}
}

// images notes containers on a moving image tag.
//
// Info, and deliberately not higher. Running :latest with an auto-updater is a
// legitimate choice — it is how security fixes arrive without anyone doing
// anything — and the opposite choice, pinning everything to a digest, has its
// own failure mode. This exists so the split is visible rather than assumed,
// which is worth exactly one line per host and no more.
func (e *evaluator) images(inv *inventory.Inventory) {
	if inv.Docker == nil || !inv.Docker.Available {
		return
	}
	floating := 0
	for _, c := range inv.Docker.Containers {
		ref := c.Image
		if i := strings.Index(ref, "@"); i >= 0 {
			continue // digest-pinned
		}
		base := ref
		if i := strings.LastIndex(ref, "/"); i >= 0 {
			base = ref[i+1:]
		}
		tag := ""
		if i := strings.LastIndex(base, ":"); i >= 0 {
			tag = base[i+1:]
		}
		if tag == "" || tag == "latest" {
			floating++
		}
	}
	if floating == 0 {
		return
	}
	e.add(Alert{
		Rule:    "docker.image.floating",
		Level:   LevelInfo,
		Subject: "images",
		Value:   float64(floating),
		Message: fmt.Sprintf("%d of %d containers run a moving image tag (:latest or untagged)",
			floating, len(inv.Docker.Containers)),
		Detail: "their image can change without being asked; internal/drift records it after the fact",
	})
}

// firewall alerts when a host filters nothing inbound while exposing ports.
//
// Neither half is a finding on its own. A host with no firewall and nothing
// listening is fine; a host with forty exposed ports behind a default-deny is
// fine. The pair is the finding, and it is the one the ports grid could not
// make on its own — "exposed" says what the process asked for, and only the
// packet filter says whether anyone can take it up.
//
// Rated info rather than warning, deliberately. A flat LAN with no host
// firewall is an ordinary and often deliberate homelab choice, and this rule
// would be crying wolf if it called that broken. What it is worth is saying
// the quiet part out loud once, with a number attached.
func (e *evaluator) firewall(inv *inventory.Inventory) {
	fw := inv.Firewall
	if fw == nil {
		return
	}
	if !fw.Available {
		e.add(Alert{
			Rule:    "firewall.unknown",
			Level:   LevelInfo,
			Subject: "firewall",
			Message: "the packet filter could not be read, so this host's exposure is unknown",
			Detail:  fw.InboundSummary,
		})
		return
	}
	if fw.FiltersInbound {
		return
	}

	exposed := 0
	if inv.Network != nil {
		seen := map[int]bool{}
		for _, l := range inv.Network.Listeners {
			if l.Exposed && !l.Ephemeral && l.Protocol == "tcp" && !seen[l.Port] {
				seen[l.Port] = true
				exposed++
			}
		}
	}
	if exposed == 0 {
		return
	}
	e.add(Alert{
		Rule:    "firewall.no_inbound_filtering",
		Level:   LevelInfo,
		Subject: "firewall",
		Value:   float64(exposed),
		Message: fmt.Sprintf(
			"no inbound filtering: %d exposed TCP port(s) are reachable from anything that can route here",
			exposed),
		Detail: fw.InboundSummary,
	})
}

// filesystems alerts on mounts filling up.
//
// ZFS is skipped on purpose. Datasets in a pool share free space, so a
// per-dataset statfs reports the same pool over and over. The zfs rules
// above use pool-level numbers instead.
func (e *evaluator) filesystems(inv *inventory.Inventory) {
	for _, fs := range inv.Host.Filesystems {
		if fs.Kind != inventory.FSKindReal && fs.Kind != inventory.FSKindNetwork {
			continue
		}
		if strings.EqualFold(fs.FSType, "zfs") {
			continue
		}
		if fs.TotalBytes == 0 || fs.UsedPct < e.th.FilesystemWarnPct {
			continue
		}
		level, threshold := LevelWarning, e.th.FilesystemWarnPct
		if fs.UsedPct >= e.th.FilesystemCritPct {
			level, threshold = LevelCritical, e.th.FilesystemCritPct
		}
		e.add(Alert{
			Rule:      "host.filesystem.capacity",
			Level:     level,
			Subject:   fs.MountPoint,
			Message:   fmt.Sprintf("%s is %.0f%% full", fs.MountPoint, fs.UsedPct),
			Value:     fs.UsedPct,
			Threshold: threshold,
			Unit:      UnitPercent,
			Detail:    fs.Device,
		})
	}
}

// ---------------------------------------------------------------- scan parsing

type scanState int

const (
	scanUnknown scanState = iota
	scanNone
	scanInProgress
	scanCompleted
	scanCanceled
)

// scanTimeLayout matches the timestamp zpool status puts at the end of a scan
// line: "Mon Jan  1 03:45:24 2024". The underscore in _2 is what handles the
// space-padded single-digit day ("Jul  3").
const scanTimeLayout = "Mon Jan _2 15:04:05 2006"

// parseScan reads the raw "scan:" line from zpool status.
//
// Example scan lines:
//
//	scrub repaired 0B in 00:00:23 with 0 errors on Mon Jan  1 03:45:24 2024
//	scrub canceled on Sun Jan  1 17:06:06 2023
//
// It deliberately does not try to understand every field — only enough to
// answer "was it scrubbed, and when". Anything unrecognised returns
// scanUnknown so the caller stays quiet rather than guessing.
func parseScan(scan string) (scanState, time.Time, bool) {
	s := strings.TrimSpace(scan)
	if s == "" {
		return scanUnknown, time.Time{}, false
	}
	lower := strings.ToLower(s)

	var state scanState
	switch {
	case strings.HasPrefix(lower, "none requested"):
		return scanNone, time.Time{}, false
	case strings.Contains(lower, "in progress"):
		state = scanInProgress
	// ZFS spells it "canceled"; accept the double-l spelling too rather than
	// depending on which it is.
	case strings.Contains(lower, "canceled"), strings.Contains(lower, "cancelled"):
		state = scanCanceled
	case strings.Contains(lower, "scrub repaired"), strings.Contains(lower, "resilvered"):
		state = scanCompleted
	default:
		state = scanUnknown
	}

	at, ok := parseTrailingTime(s)
	return state, at, ok
}

// parseTrailingTime pulls the timestamp off the end of a scan line.
//
// It walks the " on " separators from the right, because the timestamp always
// follows the last one and the earlier text may contain the word too.
func parseTrailingTime(s string) (time.Time, bool) {
	for idx := strings.LastIndex(s, " on "); idx >= 0; idx = strings.LastIndex(s[:idx], " on ") {
		candidate := strings.TrimSpace(s[idx+len(" on "):])
		if t, err := time.Parse(scanTimeLayout, candidate); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// roundDuration renders an age at a granularity that suits how old it is —
// days for anything over a day, which is every age these rules care about.
func roundDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
