package overview

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/capacity"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"github.com/ficaa1/homelab-inventory/internal/trend"
)

// InventoryStore is the read-only inventory/history data needed by this view.
type InventoryStore interface {
	Hosts() ([]string, error)
	Latest(string) (*inventory.Inventory, error)
	History(string) ([]store.Record, error)
	At(string, string) (*inventory.Inventory, error)
	ReadState(string, any) error
}
type Builder struct {
	// Contributions reads optional plugin sections alongside inventory history.
	Contributions func(context.Context) map[string]any
	Store         InventoryStore
	Cache         *StorageCache
	// Runs memoizes per-run storage reads across requests. Nil parses every run.
	Runs *RunMemo
	// Evaluate, when set, produces the alert report the Alerts tab uses.
	Evaluate func([]*inventory.Inventory, time.Time) (*alert.Report, error)
}

func unavailable(err string) Unavailable { return Unavailable{"unavailable", err} }
func (b Builder) Build(ctx context.Context, now time.Time) Report {
	now = now.UTC()
	out := Report{GeneratedAt: now}
	var contributions <-chan map[string]any
	if b.Contributions != nil {
		ch := make(chan map[string]any, 1)
		contributions = ch
		go func() { ch <- b.Contributions(ctx) }()
	}
	b.local(ctx, &out, now)
	if contributions != nil {
		select {
		case sections := <-contributions:
			out.Streams, out.Arr = sections["streams"], sections["arr"]
			out.Archive, out.PlexLoad = sections["archive"], sections["plex_load"]
		case <-ctx.Done():
		}
	}
	return out
}
func (b Builder) local(ctx context.Context, out *Report, now time.Time) {
	fail := func(msg string) {
		u := unavailable(msg)
		out.Fleet = u
		out.Attention = u
		out.Storage = u
		out.ZFS = u
		out.Backups = u
		out.UnitsFailed = u
	}
	if b.Store == nil {
		fail("inventory store unavailable")
		return
	}
	if ctx.Err() != nil {
		fail("request canceled")
		return
	}
	hosts, err := b.Store.Hosts()
	if err == nil && b.Runs != nil {
		b.Runs.retainHosts(hosts)
	}
	invs := []*inventory.Inventory{}
	if err == nil {
		for _, host := range hosts {
			if ctx.Err() != nil {
				fail("request canceled")
				return
			}
			v, e := b.Store.Latest(host)
			if errors.Is(e, store.ErrNotFound) {
				continue
			}
			if e != nil {
				err = e
				break
			}
			invs = append(invs, v)
		}
	}
	if ctx.Err() != nil {
		fail("request canceled")
		return
	}
	if err != nil {
		fail("inventories could not be read")
		return
	}
	if len(invs) == 0 {
		fail("no inventories reported")
		return
	}
	var sb silence.Book
	var ledger seen.Book
	se := b.Store.ReadState(silence.StateName, &sb)
	if ctx.Err() != nil {
		fail("request canceled")
		return
	}
	le := b.Store.ReadState(seen.StateName, &ledger)
	if ctx.Err() != nil {
		fail("request canceled")
		return
	}
	rep := alert.Evaluate(invs, alert.DefaultThresholds(), now)
	if b.Evaluate != nil {
		var eerr error
		if rep, eerr = b.Evaluate(invs, now); eerr != nil {
			fail("alerts could not be evaluated")
			return
		}
	}
	rep = silence.Apply(rep, &sb, now)
	// Read the persisted ages without changing dashboard state on this GET.
	for i := range rep.Alerts {
		a := &rep.Alerts[i]
		if e, ok := ledger.Entries[seen.Key(*a)]; ok && validAge(e, now) {
			t := e.First.UTC()
			a.FirstSeen = &t
		}
	}
	f := Fleet{HostsTotal: len(invs), Stale: []Stale{}, AgentsOutdated: []string{}, Alerts: Counts{Critical: rep.Counts.Critical, Warning: rep.Counts.Warning, Info: rep.Counts.Info, Silenced: rep.Silenced, Recent: recentAlerts(rep.Alerts, le, now)}}
	attention := []Attention{}
	for _, a := range rep.Alerts {
		if a.Rule == "agent.outdated" {
			f.AgentsOutdated = append(f.AgentsOutdated, a.Host)
		}
		if a.Rule == "host.stale" {
			age := int64(now.Sub(*a.Since).Seconds())
			f.Stale = append(f.Stale, Stale{a.Host, utc(a.Since), &age, string(a.Level)})
		}
		if !a.Silenced && a.Level.AtLeast(alert.LevelWarning) {
			attention = append(attention, Attention{a.Host, a.Rule, a.Subject, string(a.Level), utc(a.FirstSeen), a.Message})
		}
	}
	for _, inv := range invs {
		if inv.CollectedAt.IsZero() {
			f.Stale = append(f.Stale, Stale{Host: inv.Host.Hostname, Severity: "warning"})
		} else if now.Sub(inv.CollectedAt) < alert.DefaultThresholds().StaleAfter {
			f.HostsReporting++
		}
	}
	sort.Strings(f.AgentsOutdated)
	sort.Slice(attention, func(i, j int) bool {
		a, c := attention[i], attention[j]
		if a.Severity != c.Severity {
			return a.Severity == "critical"
		}
		if a.Since != nil && c.Since != nil && !a.Since.Equal(*c.Since) {
			return a.Since.Before(*c.Since)
		}
		if (a.Since == nil) != (c.Since == nil) {
			return a.Since != nil
		}
		return a.Host+a.Rule+a.Subject < c.Host+c.Rule+c.Subject
	})
	if len(attention) > 12 {
		attention = attention[:12]
	}
	out.Fleet = f
	out.Attention = attention
	out.Storage = b.cachedStorage(ctx, invs, now)
	if ctx.Err() != nil {
		fail("request canceled")
		return
	}
	out.ZFS = buildZFS(invs, now)
	out.Backups = buildBackups(invs, rep, now, &sb)
	out.UnitsFailed = buildUnits(invs, rep)
	if se != nil {
		out.Fleet = unavailable("silences could not be read")
		out.Attention = unavailable("silences could not be read")
		out.Backups = unavailable("silences could not be read")
		out.UnitsFailed = unavailable("silences could not be read")
	}
	if le != nil {
		out.Attention = unavailable("alert ages could not be read")
	}
}
func utc(t *time.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	v := t.UTC()
	return &v
}
func days(t time.Time, now time.Time) *int {
	if t.IsZero() {
		return nil
	}
	d := int(now.Sub(t).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return &d
}

func (b Builder) storage(ctx context.Context, invs []*inventory.Inventory, now time.Time) any {
	rows := []Storage{}
	looked := false
	for _, inv := range invs {
		if ctx.Err() != nil {
			return unavailable("request canceled")
		}
		// Do not let capacity's legacy empty/unavailable sections become zeros.
		copy := *inv
		if copy.ZFS != nil && !copy.ZFS.Available {
			return unavailable("ZFS capacity source unavailable")
		}
		if copy.Proxmox != nil && !copy.Proxmox.Available {
			return unavailable("Proxmox capacity source unavailable")
		}
		looked = looked || copy.Host.Filesystems != nil || (copy.ZFS != nil && copy.ZFS.Pools != nil) || (copy.Proxmox != nil && copy.Proxmox.Storages != nil)
		host := inv.Host.Hostname
		records, err := b.Store.History(host)
		if err != nil {
			return unavailable("storage history could not be listed")
		}
		if b.Runs != nil {
			b.Runs.retain(host, records)
		}
		var samples []trend.Sample
		for _, idx := range sampledIndexes(len(records)) {
			s, err := b.summaryOf(ctx, host, records[idx].ID)
			if err != nil {
				return readFailure(ctx)
			}
			samples = append(samples, s.samples...)
		}
		tr := trend.Assemble(samples, now)
		target := inv.CollectedAt.Add(-24 * time.Hour)
		var prior *runSummary
		priorLoaded := false
		for _, r := range capacity.Build([]*inventory.Inventory{&copy}, nil).Rows {
			kind := map[capacity.Kind]string{capacity.KindPool: "zfs", capacity.KindStorage: "pve", capacity.KindFilesystem: "fs"}[r.Kind]
			row := Storage{Host: r.Host, Kind: kind, Name: r.Name, Status: "ok", UsedBytes: &r.UsedBytes, SizeBytes: &r.TotalBytes, Pct: &r.UsedPct, Spark: []float64{}, Forecast: Forecast{Status: "insufficient", Reason: "no readable capacity history"}}
			if r.TotalBytes <= 0 {
				row.Status, row.Error = "unavailable", "capacity could not be read"
			}
			if r.Kind == capacity.KindStorage {
				for _, st := range inv.Proxmox.Storages {
					if st.Name == r.Name && !st.Active {
						row.Status, row.Error = "unavailable", "Proxmox storage is inactive"
					}
				}
			}
			if row.Status == "unavailable" {
				row.UsedBytes, row.SizeBytes, row.Pct = nil, nil, nil
				row.Spark = nil
				row.Forecast = Forecast{Status: "unavailable", Reason: row.Error}
				rows = append(rows, row)
				continue
			}
			for _, s := range tr.Series {
				if s.Subject != r.Name || !sameKind(r.Kind, s.Kind) {
					continue
				}
				row.Spark = spark(s.Points)
				row.Forecast.Reason = s.Note
				if s.Forecast != nil {
					if s.Forecast.PctPerDay <= 0 {
						row.Forecast.Status = "flat"
					} else if s.Forecast.FullAt != nil {
						row.Forecast.Status = "ok"
						row.Forecast.FullAt = utc(s.Forecast.FullAt)
						if s.Forecast.Confidence == "low" {
							row.Forecast.Reason = fmt.Sprintf("low confidence: trend explains %.0f%% of variation", s.Forecast.R2*100)
						}
					}
				}
			}
			// A 24-hour delta uses an actual observation near that boundary, not
			// the previous 15-minute run, and refuses a changed capacity denominator.
			// The prior run depends only on the host, so it is read once.
			if !priorLoaded {
				priorLoaded = true
				prior, err = b.priorRun(ctx, host, windowRecords(records, target), target)
				if err != nil {
					return readFailure(ctx)
				}
			}
			if prior != nil {
				for _, p := range prior.capacity {
					if p.kind == r.Kind && p.name == r.Name && p.total == r.TotalBytes && p.readable {
						d := r.UsedBytes - p.used
						row.Delta24hBytes = &d
					}
				}
			}
			rows = append(rows, row)
		}
	}
	if ctx.Err() != nil {
		return unavailable("request canceled")
	}
	if !looked {
		return unavailable("capacity was not collected")
	}
	sort.Slice(rows, func(i, j int) bool {
		if (rows[i].Pct == nil) != (rows[j].Pct == nil) {
			return rows[i].Pct != nil
		}
		if rows[i].Pct != nil && *rows[i].Pct != *rows[j].Pct {
			return *rows[i].Pct > *rows[j].Pct
		}
		return rows[i].Host+rows[i].Kind+rows[i].Name < rows[j].Host+rows[j].Kind+rows[j].Name
	})
	return rows
}

// summaryOf reads one run through the memo when there is one, and directly
// otherwise. Both paths make the same store calls for the same run.
func (b Builder) summaryOf(ctx context.Context, host, id string) (*runSummary, error) {
	if b.Runs != nil {
		return b.Runs.summary(ctx, b.Store, host, id)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, err := b.Store.At(host, id)
	if err != nil {
		return nil, err
	}
	return summarize(v), nil
}

// priorRun returns the window run nearest target, or nil when there is none.
func (b Builder) priorRun(ctx context.Context, host string, window []store.Record, target time.Time) (*runSummary, error) {
	var prior *runSummary
	for _, rec := range window {
		s, err := b.summaryOf(ctx, host, rec.ID)
		if err != nil {
			return nil, err
		}
		if prior == nil || abs(s.collectedAt.Sub(target)) < abs(prior.collectedAt.Sub(target)) {
			prior = s
		}
	}
	return prior, nil
}

func readFailure(ctx context.Context) any {
	if ctx.Err() != nil {
		return unavailable("request canceled")
	}
	return unavailable("storage history sample could not be read")
}

// Inactive historical PVE rows cannot establish a delta.
func readablePVE(inv *inventory.Inventory, r capacity.Row) bool {
	if r.Kind != capacity.KindStorage {
		return true
	}
	if inv.Proxmox == nil || !inv.Proxmox.Available {
		return false
	}
	for _, st := range inv.Proxmox.Storages {
		if st.Name == r.Name {
			return st.Active && st.TotalBytes > 0
		}
	}
	return false
}
func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
func sameKind(c capacity.Kind, t trend.Kind) bool {
	return c == capacity.KindPool && t == trend.KindZFSPool || c == capacity.KindStorage && t == trend.KindPVEStorage || c == capacity.KindFilesystem && t == trend.KindFilesystem
}

// Pick actual observations nearest evenly spaced times; never synthesize
// capacity readings. Sparse windows may return fewer than thirty samples.
func spark(pts []trend.Point) []float64 {
	out := []float64{}
	if len(pts) == 0 {
		return out
	}
	n := len(pts)
	if n > 30 {
		n = 30
	}
	if n == 1 {
		return []float64{pts[0].Pct}
	}
	start, end := pts[0].At, pts[len(pts)-1].At
	j, last := 0, -1
	for i := 0; i < n; i++ {
		at := start.Add(time.Duration(float64(end.Sub(start)) * float64(i) / float64(n-1)))
		for j+1 < len(pts) && abs(pts[j+1].At.Sub(at)) <= abs(pts[j].At.Sub(at)) {
			j++
		}
		if j != last {
			out = append(out, pts[j].Pct)
			last = j
		}
	}
	return out
}
func buildZFS(invs []*inventory.Inventory, now time.Time) any {
	out := []Pool{}
	looked := false
	uncollected := false
	for _, inv := range invs {
		if inv.ZFS == nil {
			continue
		}
		if !inv.ZFS.Available {
			return unavailable("ZFS source unavailable")
		}
		if inv.ZFS.Pools == nil {
			uncollected = true
			continue
		}
		looked = true
		for _, p := range inv.ZFS.Pools {
			row := Pool{Scan: p.Scan, Status: poolStatus(p.Health), Host: inv.Host.Hostname, Pool: p.Name, Health: p.Health, Pct: float64(p.CapacityPct), Scrub: parseScrub(p.Scan, now)}
			if inv.ZFS.Snapshots != nil {
				ss := Snapshots{}
				var newest time.Time
				for _, s := range inv.ZFS.Snapshots {
					if s.Dataset == p.Name || strings.HasPrefix(s.Dataset, p.Name+"/") {
						ss.Count += s.Count
						if s.Newest.After(newest) {
							newest = s.Newest
						}
					}
				}
				ss.NewestAgeDays = days(newest, now)
				ss.Status = snapshotStatus(ss, newest, now)
				row.Snapshots = &ss
			}
			out = append(out, row)
		}
	}
	if uncollected {
		return nil
	}
	if !looked {
		return unavailable("ZFS was not collected")
	}
	return out
}
func parseScrub(raw string, now time.Time) Scrub {
	out := Scrub{Status: "unknown"}
	s := strings.ToLower(raw)
	state := ""
	switch {
	case strings.HasPrefix(s, "none requested"):
		state = "none"
	case strings.HasPrefix(s, "scrub") && strings.Contains(s, "in progress"):
		state = "running"
	case strings.HasPrefix(s, "scrub") && (strings.Contains(s, "canceled") || strings.Contains(s, "cancelled")):
		state = "canceled"
	case strings.HasPrefix(s, "scrub repaired"):
		state = "completed"
	}
	if state == "" {
		return out
	}
	out.State = &state
	if state == "completed" || state == "canceled" {
		if i := strings.LastIndex(raw, " on "); i >= 0 {
			if t, e := time.Parse("Mon Jan _2 15:04:05 2006", strings.TrimSpace(raw[i+4:])); e == nil {
				out.FinishedAt = utc(&t)
				out.AgeDays = days(t, now)
			}
		}
	}
	fields := strings.Fields(raw)
	for i, v := range fields {
		if v == "errors" && i > 0 {
			n, e := strconv.Atoi(fields[i-1])
			if e == nil {
				out.Errors = &n
			}
		}
	}
	if state == "canceled" || state == "none" || (out.FinishedAt != nil && now.Sub(*out.FinishedAt) > alert.DefaultThresholds().ScrubMaxAge) {
		out.Status = "stale"
	} else if state == "running" {
		out.Status = "running"
	} else if state == "completed" && out.AgeDays != nil {
		out.Status = "ok"
	}
	if out.Errors != nil && *out.Errors > 0 {
		out.Status = "errors"
	}
	return out
}
func poolStatus(health string) string {
	switch health {
	case "ONLINE":
		return "ok"
	case "":
		return "unknown"
	default:
		return "warning"
	}
}
func snapshotStatus(s Snapshots, newest, now time.Time) string {
	if s.Count == 0 {
		return "none"
	}
	if newest.IsZero() || newest.After(now) {
		return "unknown"
	}
	if now.Sub(newest) > alert.DefaultThresholds().SnapshotMaxAge {
		return "stale"
	}
	return "ok"
}
func buildBackups(invs []*inventory.Inventory, rep *alert.Report, now time.Time, books ...*silence.Book) any {
	var out *Backups
	for _, inv := range invs {
		p := inv.Proxmox
		if p == nil {
			continue
		}
		if !p.Available {
			return unavailable("Proxmox source unavailable")
		}
		if p.BackupJobs == nil || p.Backups == nil || p.Guests == nil {
			return unavailable("backup inventory incomplete")
		}
		if out == nil {
			out = &Backups{Jobs: []BackupJob{}, Guests: []Guest{}}
		}
		for _, j := range p.BackupJobs {
			out.Jobs = append(out.Jobs, BackupJob{Storage: j.Storage, Host: inv.Host.Hostname, ID: j.ID, Schedule: j.Schedule, NextRun: utc(&j.NextRun), Retention: j.Retention, Enabled: j.Enabled})
		}
		for _, g := range p.Guests {
			row := Guest{ExcludedDisks: []string{}, Host: inv.Host.Hostname, VMID: g.VMID, Name: g.Name, Coverage: "uncovered"}
			for _, disk := range g.Disks {
				if disk.Backup != nil && !*disk.Backup {
					row.ExcludedDisks = append(row.ExcludedDisks, disk.Name)
				}
			}
			jobs := []string{}
			uncertain := false
			for _, j := range p.BackupJobs {
				if j.Enabled && j.Pool != "" {
					uncertain = true
				}
				if j.Enabled && j.Pool == "" && j.Covers(g.VMID) {
					jobs = append(jobs, j.ID)
				}
			}
			sort.Strings(jobs)
			row.Jobs = jobs
			if len(jobs) > 0 {
				row.Coverage = "covered"
			} else if uncertain {
				row.Coverage = "unknown"
			}
			if len(jobs) == 1 {
				v := jobs[0]
				row.Job = &v
			}
			var newest time.Time
			for _, bk := range p.Backups {
				if bk.VMID == g.VMID && bk.Created.After(newest) {
					newest = bk.Created
					if bk.Size > 0 {
						size := bk.Size
						row.NewestArchiveSizeBytes = &size
					} else {
						row.NewestArchiveSizeBytes = nil
					}
				}
			}
			row.NewestArchiveAgeDays = days(newest, now)
			for _, a := range rep.Alerts {
				if a.Host == inv.Host.Hostname && a.Rule == "proxmox.backup.unscheduled" && a.Subject == fmt.Sprint(g.VMID) && a.Silenced {
					row.Silenced = true
					row.SilenceReason = a.SilenceReason
				}
			}
			// A stored exclusion remains reviewable even if the alert evaluator
			// emits only a node-level finding (for example, no backup storage).
			if len(books) > 0 && books[0] != nil {
				match, ok := books[0].Covers(alert.Alert{Host: row.Host, Rule: "proxmox.backup.unscheduled", Subject: fmt.Sprint(row.VMID)}, now)
				row.Silenced = ok
				row.SilenceReason = match.Reason
			}
			if row.Coverage == "uncovered" && row.Silenced && strings.TrimSpace(row.SilenceReason) != "" {
				row.Coverage = "skipped"
			}
			switch row.Coverage {
			case "covered":
				out.Counts.Covered++
			case "skipped":
				out.Counts.Skipped++
			case "uncovered":
				out.Counts.Uncovered++
			default:
				out.Counts.Unknown++
			}
			out.Guests = append(out.Guests, row)
		}
	}
	if out == nil {
		return nil
	}
	return out
}
func buildUnits(invs []*inventory.Inventory, rep *alert.Report) any {
	out := []Unit{}
	looked := false
	for _, inv := range invs {
		if inv.Systemd == nil {
			continue
		}
		if !inv.Systemd.Available {
			return unavailable("systemd source unavailable")
		}
		if inv.Systemd.Units == nil {
			continue
		}
		looked = true
		for _, u := range inv.Systemd.Units {
			if u.Active != "failed" {
				continue
			}
			row := Unit{Host: inv.Host.Hostname, Unit: u.Name}
			for _, a := range rep.Alerts {
				if a.Host == row.Host && a.Rule == "systemd.unit.failed" && a.Subject == row.Unit {
					row.Silenced = a.Silenced
				}
			}
			out = append(out, row)
		}
	}
	if !looked {
		return unavailable("units were not collected")
	}
	return out
}
