package collect

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// SystemdCollector inventories service units and timers.
//
// Parsing strategy note: `systemctl list-timers` prints human-formatted dates
// with embedded spaces, which makes column splitting genuinely unreliable.
// So timers are enumerated with list-units (stable columns) and then detailed
// with a single `systemctl show`, whose KEY=VALUE output is machine-oriented
// and has been stable for years. When a tool offers both a pretty and a
// scriptable output mode, always take the scriptable one.
type SystemdCollector struct{}

func (SystemdCollector) Name() string { return "systemd" }

func (c SystemdCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	sd := &inventory.Systemd{
		Units:  []inventory.Unit{},
		Timers: []inventory.Timer{},
	}
	inv.Systemd = sd

	if !env.Run.Available("systemctl") {
		env.Infof("systemctl not found; host does not use systemd")
		return nil
	}
	sd.Available = true

	if res, err := env.Run.Run(ctx, "systemctl", "--version"); err == nil && res.OK() {
		if lines := res.Lines(); len(lines) > 0 {
			sd.Version = strings.TrimSpace(lines[0])
		}
	}

	// Enabled/disabled state lives in a different subcommand than
	// running/stopped state, so we collect both and join on unit name.
	states := map[string]unitFileState{}
	res, err := env.Run.Run(ctx, "systemctl", "list-unit-files", "--type=service", "--no-pager", "--no-legend", "--plain")
	if err != nil {
		env.WarnErr("systemctl list-unit-files failed", err)
	} else if res.OK() {
		for _, line := range res.Lines() {
			f := strings.Fields(stripBullet(line))
			if len(f) < 2 {
				continue
			}
			s := unitFileState{state: f[1]}
			if len(f) >= 3 {
				s.preset = f[2]
			}
			states[f[0]] = s
		}
	}

	res, err = env.Run.Run(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "--no-legend", "--plain")
	if err != nil {
		env.WarnErr("systemctl list-units failed", err)
	} else if res.OK() {
		for _, line := range res.Lines() {
			f := splitFieldsN(stripBullet(line), 5)
			if len(f) < 4 {
				continue
			}
			u := inventory.Unit{
				Name:   f[0],
				Load:   f[1],
				Active: f[2],
				Sub:    f[3],
			}
			if len(f) >= 5 {
				u.Description = redact.String(strings.TrimSpace(f[4]))
			}
			// Instantiated units (getty@tty1.service) have no unit file of
			// their own; their state lives on the template (getty@.service).
			// Without this fallback, 13 of 168 units on the main VM reported
			// an empty enabled/disabled state.
			s, ok := states[u.Name]
			if !ok {
				if tmpl := templateUnitName(u.Name); tmpl != "" {
					s, ok = states[tmpl]
					if ok {
						u.FromTemplate = tmpl
					}
				}
			}
			if ok {
				u.UnitFileState = s.state
				u.Preset = s.preset
			}
			if u.Active == "failed" || u.Sub == "failed" {
				sd.FailedCount++
			}
			sd.Units = append(sd.Units, u)
		}
	} else {
		env.Warnf("systemctl list-units exited %d: %s", res.ExitCode, res.Stderr)
	}
	sort.Slice(sd.Units, func(a, b int) bool { return sd.Units[a].Name < sd.Units[b].Name })

	sd.Timers = collectTimers(ctx, env)
	return nil
}

type unitFileState struct{ state, preset string }

func collectTimers(ctx context.Context, env *Env) []inventory.Timer {
	res, err := env.Run.Run(ctx, "systemctl", "list-units", "--type=timer", "--all", "--no-pager", "--no-legend", "--plain")
	if err != nil {
		env.WarnErr("listing systemd timers failed", err)
		return []inventory.Timer{}
	}
	var names []string
	for _, line := range res.Lines() {
		f := strings.Fields(stripBullet(line))
		if len(f) > 0 && strings.HasSuffix(f[0], ".timer") {
			names = append(names, f[0])
		}
	}
	if len(names) == 0 {
		return []inventory.Timer{}
	}
	sort.Strings(names)

	args := append([]string{
		"show", "--no-pager",
		"--property=Id,Unit,NextElapseUSecRealtime,LastTriggerUSec,Description",
	}, names...)
	res, err = env.Run.Run(ctx, "systemctl", args...)
	if err != nil {
		env.WarnErr("systemctl show for timers failed", err)
		return []inventory.Timer{}
	}

	timers := make([]inventory.Timer, 0, len(names))
	for _, block := range strings.Split(res.Stdout, "\n\n") {
		props := parseKeyValue(block, "=")
		id := props["Id"]
		if id == "" {
			continue
		}
		timers = append(timers, inventory.Timer{
			Name:      id,
			Activates: props["Unit"],
			Next:      formatUSec(props["NextElapseUSecRealtime"]),
			Last:      formatUSec(props["LastTriggerUSec"]),
		})
	}
	return timers
}

// templateUnitName maps an instantiated unit to its template unit file:
// getty@tty1.service -> getty@.service. It returns "" for non-instantiated
// names and for templates themselves.
func templateUnitName(name string) string {
	at := strings.Index(name, "@")
	if at < 0 {
		return ""
	}
	dot := strings.LastIndex(name, ".")
	if dot < at {
		return ""
	}
	if dot == at+1 {
		return "" // already a template: foo@.service
	}
	return name[:at+1] + name[dot:]
}

// humanTimeLayouts are the formats systemd uses when it prints a timestamp as
// text. Debian 12's systemd does this for NextElapseUSecRealtime even in
// `show` output, so the same field arrives as a number on one host and as
// prose on another. Normalising here keeps the schema honest.
var humanTimeLayouts = []string{
	"Mon 2006-01-02 15:04:05 MST",
	"Mon 2006-01-02 15:04:05",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
}

// formatUSec converts a systemd timestamp property into RFC3339.
//
// It handles all three shapes seen in the wild: microseconds since the epoch,
// the "never"/"not scheduled" sentinels (0 and max-uint64), and a
// human-formatted string. An unrecognised string is passed through rather than
// dropped — losing data to a parser you did not anticipate is worse than
// carrying an odd-looking value.
func formatUSec(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" || s == "n/a" {
		return ""
	}
	if usec, err := strconv.ParseUint(s, 10, 64); err == nil {
		if usec == 0 || usec >= uint64(1)<<63 {
			return ""
		}
		return time.UnixMicro(int64(usec)).UTC().Format(time.RFC3339)
	}
	// ParseInLocation, not Parse: Go resolves a zone abbreviation like CEST
	// to a zero offset unless it matches the location it is given.
	for _, layout := range humanTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t.UTC().Format(time.RFC3339)
		}
	}
	return s
}

// stripBullet removes the status glyph systemctl prefixes to failed units.
func stripBullet(line string) string {
	return strings.TrimLeft(line, " \t●*→")
}
