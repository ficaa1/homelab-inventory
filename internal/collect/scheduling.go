package collect

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/redact"
)

// SchedulingCollector reads the cron half of "what runs on a schedule here".
// systemd timers are collected by SystemdCollector; between the two you have
// the full picture.
//
// User crontabs live in /var/spool/cron and are mode 0600, so this collector
// returns noticeably more when run as root. It says so in a diagnostic rather
// than quietly returning less.
type SchedulingCollector struct{}

func (SchedulingCollector) Name() string { return "scheduling" }

// cronSpoolDirs covers the Debian/Ubuntu layout and the RHEL one; Proxmox is
// Debian-based but LXCs on this host may not be.
var cronSpoolDirs = []string{
	"/var/spool/cron/crontabs",
	"/var/spool/cron",
}

var periodicDirs = []string{
	"/etc/cron.hourly",
	"/etc/cron.daily",
	"/etc/cron.weekly",
	"/etc/cron.monthly",
}

func (c SchedulingCollector) Collect(ctx context.Context, env *Env, inv *inventory.Inventory) error {
	s := &inventory.Scheduling{
		CronD:        []inventory.CronFile{},
		UserCrontabs: []inventory.CronFile{},
		PeriodicDirs: []inventory.PeriodicDir{},
	}
	inv.Scheduling = s

	// /etc/crontab and /etc/cron.d entries carry an explicit user column.
	if f, ok := readCronFile(env, "/etc/crontab", true, ""); ok {
		s.SystemCrontab = &f
	}

	for _, path := range listDirFiles(env, "/etc/cron.d") {
		// Debian's run-parts conventions: dotfiles and .dpkg-* backups are
		// ignored by cron itself, so ignoring them here keeps the inventory
		// honest about what actually runs.
		if isIgnoredDropIn(filepath.Base(path)) {
			continue
		}
		if f, ok := readCronFile(env, path, true, ""); ok {
			s.CronD = append(s.CronD, f)
		}
	}

	unreadable := 0
	for _, dir := range cronSpoolDirs {
		files := listDirFiles(env, dir)
		for _, path := range files {
			owner := filepath.Base(path)
			f, ok := readCronFile(env, path, false, owner)
			if !ok {
				continue
			}
			if f.ReadError != "" {
				unreadable++
			}
			s.UserCrontabs = append(s.UserCrontabs, f)
		}
		if len(files) > 0 {
			break // first layout that exists wins; the other is a symlink or absent
		}
	}
	if unreadable > 0 && os.Geteuid() != 0 {
		env.Warnf("%d user crontab(s) unreadable; re-run as root to capture them", unreadable)
	}

	for _, dir := range periodicDirs {
		scripts := []string{}
		for _, path := range listDirFiles(env, dir) {
			base := filepath.Base(path)
			if isIgnoredDropIn(base) {
				continue
			}
			scripts = append(scripts, base)
		}
		if len(scripts) > 0 {
			s.PeriodicDirs = append(s.PeriodicDirs, inventory.PeriodicDir{Path: dir, Scripts: scripts})
		}
	}

	return nil
}

func readCronFile(env *Env, path string, withUser bool, owner string) (inventory.CronFile, bool) {
	info, err := os.Stat(env.Path(path))
	if err != nil {
		if os.IsNotExist(err) {
			return inventory.CronFile{}, false
		}
		// A nil Entries slice would marshal as null. The schema promises that
		// null means "did not look" and [] means "looked, found nothing", so
		// every exit path from here has to carry a non-nil slice.
		return inventory.CronFile{
			Path:      path,
			Owner:     owner,
			ReadError: err.Error(),
			Entries:   []inventory.CronEntry{},
		}, true
	}
	if info.IsDir() {
		return inventory.CronFile{}, false
	}

	f := inventory.CronFile{
		Path:      path,
		Owner:     owner,
		SizeBytes: info.Size(),
		ModTime:   info.ModTime().UTC(),
		Entries:   []inventory.CronEntry{},
	}
	content, err := env.ReadFile(path)
	if err != nil {
		f.ReadError = err.Error()
		return f, true
	}
	if entries := parseCrontab(content, withUser); entries != nil {
		f.Entries = entries
	}
	return f, true
}

// parseCrontab extracts schedule lines.
//
// Environment assignment lines (PATH=, MAILTO=, and anything else people
// stash there) are intentionally not stored. They are the most likely place
// for a credential to appear in a crontab, and they say little about what is
// scheduled. If that turns out to matter, add them redacted rather than raw.
func parseCrontab(content string, withUser bool) []inventory.CronEntry {
	var entries []inventory.CronEntry

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// KEY=value with no leading schedule is an environment assignment.
		if k, _, ok := strings.Cut(line, "="); ok && !strings.ContainsAny(strings.TrimSpace(k), " \t") {
			continue
		}

		var schedule, rest string
		if strings.HasPrefix(line, "@") {
			f := splitFieldsN(line, 2)
			if len(f) < 2 {
				continue
			}
			schedule, rest = f[0], f[1]
		} else {
			f := splitFieldsN(line, 6)
			if len(f) < 6 {
				continue
			}
			schedule = strings.Join(f[:5], " ")
			rest = f[5]
		}

		e := inventory.CronEntry{
			Raw:      redact.String(line),
			Schedule: schedule,
		}
		if withUser {
			uf := splitFieldsN(rest, 2)
			if len(uf) < 2 {
				continue
			}
			e.User = uf[0]
			e.Command = redact.String(uf[1])
		} else {
			e.Command = redact.String(rest)
		}
		entries = append(entries, e)
	}
	return entries
}

// listDirFiles returns sorted absolute host paths of the regular files in dir.
// A missing directory is normal and silent; an unreadable one is not, but the
// caller decides how loudly to complain.
func listDirFiles(env *Env, dir string) []string {
	ents, err := os.ReadDir(env.Path(dir))
	if err != nil {
		if !os.IsNotExist(err) {
			env.Warnf("could not list %s: %v", dir, err)
		}
		return nil
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// isIgnoredDropIn mirrors run-parts' filename rules: cron skips these, so an
// inventory that lists them would be reporting jobs that never run.
func isIgnoredDropIn(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, suffix := range []string{".dpkg-old", ".dpkg-dist", ".dpkg-new", ".dpkg-bak", ".rpmsave", ".rpmnew", "~", ".bak", ".disabled"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
