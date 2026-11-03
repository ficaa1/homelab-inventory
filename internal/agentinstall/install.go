// Package agentinstall owns the explicit, reversible installer transaction.
// It is never imported by the mutation-free collector or runtime worker.
package agentinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentboot"
	"github.com/ficaa1/homelab-inventory/internal/agentmount"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/agentupdate"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

type Options struct {
	Dir, Prefix, URL, Scheduler, ReplaceSchedule, ExpectedHost string
	TokenFile, Enroll, Public, Manifest, Input                 string
	AllowHTTP                                                  bool
	Scheduled                                                  bool `json:"-"`
}
type backup struct {
	Exists bool   `json:"exists"`
	Data   []byte `json:"data,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
}
type intent struct {
	Format       int               `json:"format"`
	Phase        string            `json:"phase"`
	Options      Options           `json:"options"`
	LauncherHash string            `json:"launcher_sha256"`
	CodeHash     string            `json:"code_hash,omitempty"`
	Files        map[string]backup `json:"files"`
	Cron         *CronSnapshot     `json:"cron,omitempty"`
	HadSchedule  bool              `json:"had_schedule"`
	TimerEnabled bool              `json:"timer_enabled"`
	TimerActive  bool              `json:"timer_active"`
}

func safe(msg string) error { return errors.New(msg) }
func privateDir(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, " \t\r\n'\"\\") {
		return safe("state path must be absolute with plain characters")
	}
	// Refuse symlinks in every existing path component, including ancestors.
	current := string(filepath.Separator)
	for _, p := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		current = filepath.Join(current, p)
		st, e := os.Lstat(current)
		if e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			return safe("state path must contain only real directories")
		}
		if e != nil && !os.IsNotExist(e) {
			return safe("cannot inspect state directory")
		}
	}
	if e := os.MkdirAll(path, 0700); e != nil {
		return safe("cannot create private installation directory")
	}
	if os.Geteuid() == 0 {
		if e := os.Chown(path, 0, 0); e != nil {
			return e
		}
	}
	return os.Chmod(path, 0700)
}
func writeJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(path, b, 0600)
}
func readFile(path string, limit int64) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		return nil, safe("installer requires regular files")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, safe("installer file exceeds size limit")
	}
	return b, nil
}
func copyFile(src, dst string, mode os.FileMode) error {
	b, e := readFile(src, agentrelease.MaxArtifact)
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(dst, b, mode)
}
func digest(s string) string    { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:]) }
func journal(dir string) string { return filepath.Join(dir, ".migration", "intent.json") }
func load(dir string) (intent, error) {
	var in intent
	b, e := readFile(journal(dir), 96<<20)
	if e == nil {
		e = json.Unmarshal(b, &in)
	}
	if e == nil && (in.Format != 1 || in.Options.Dir != dir) {
		e = safe("invalid migration intent")
	}
	return in, e
}
func system(ctx context.Context, args ...string) error {
	cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	c := exec.CommandContext(cctx, "systemctl", args...)
	c.Stdout = io.Discard
	c.Stderr = io.Discard
	if c.Run() != nil {
		return safe("systemd operation failed; old schedule restored where possible")
	}
	return nil
}
func runner(o Options, dir string) agentboot.Runner {
	h, _ := os.Hostname()
	return agentboot.Runner{Config: agentboot.Config{Dir: dir, Public: filepath.Join(dir, "public.key"), URL: o.URL, TokenFile: filepath.Join(o.Dir, ".migration", "enroll.token"), AllowHTTP: o.AllowHTTP}, Host: h}
}
func request(ctx context.Context, o Options, token, path string, limit int64) ([]byte, error) {
	t, e := agentupdate.ReadToken(token)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.URL, "/")+path, nil)
	if e != nil {
		return nil, safe("invalid download URL")
	}
	req.Header.Set("Authorization", "Bearer "+t)
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: httpx.RefuseRedirects()}
	resp, e := client.Do(req)
	if e != nil {
		return nil, safe("authenticated bootstrap download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// Only known status codes are surfaced, never server text or credentials.
		if pathHasBootstrap(path) {
			return nil, safe("signed enrollment refused: release held, no promoted baseline, or runtime unavailable; check Add a host readiness")
		}
		return nil, safe("authenticated agent download refused")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, safe("bootstrap download oversized or interrupted")
	}
	return b, nil
}
func pathHasBootstrap(path string) bool { return strings.HasPrefix(path, "/agent/bootstrap") }
func snapshot(path string) (backup, error) {
	b, e := readFile(path, agentrelease.MaxArtifact)
	if os.IsNotExist(e) {
		return backup{}, nil
	}
	if e != nil {
		return backup{}, e
	}
	st, e := os.Stat(path)
	if e != nil {
		return backup{}, e
	}
	return backup{true, b, uint32(st.Mode().Perm())}, nil
}
func restore(in *intent) error {
	var errs []error
	for path, b := range in.Files {
		var e error
		if b.Exists {
			e = fsx.WriteAtomic(path, b.Data, os.FileMode(b.Mode))
		} else {
			e = os.Remove(path)
			if os.IsNotExist(e) {
				e = nil
			}
		}
		if e != nil {
			errs = append(errs, safe("restoring previous installation files failed"))
		}
	}
	return errors.Join(errs...)
}
func rollback(ctx context.Context, in *intent) error {
	var errs []error
	if in.Options.Scheduler == "truenas-cron" {
		if in.Cron != nil {
			plan, e := RestoreCronPlan(*in.Cron)
			if e != nil {
				errs = append(errs, e)
			} else {
				errs = append(errs, plan.Rollback(ctx))
			}
		}
	} else {
		if manageSystemd(in.Options) {
			_ = system(ctx, "stop", "homelab-discover.timer")
			if !in.Options.Scheduled {
				_ = system(ctx, "stop", "homelab-discover.service")
			}
		}
	}
	errs = append(errs, restore(in))
	if e := errors.Join(errs...); e != nil {
		return e
	}
	// Runtime entry points must see a restored transaction before restart; an
	// existing launcher must not recursively resume while this installer is locked.
	in.Phase = "prepared"
	if e := writeJSON(journal(in.Options.Dir), in); e != nil {
		return e
	}
	if in.Options.Scheduler != "truenas-cron" && manageSystemd(in.Options) {
		errs = append(errs, system(ctx, "daemon-reload"))
		if in.TimerEnabled {
			errs = append(errs, system(ctx, "enable", "homelab-discover.timer"))
		} else {
			_ = system(ctx, "disable", "homelab-discover.timer")
		}
		if in.HadSchedule && !in.Options.Scheduled {
			errs = append(errs, system(ctx, "start", "homelab-discover.service"))
		}
		if in.TimerActive {
			errs = append(errs, system(ctx, "start", "homelab-discover.timer"))
		}
	}
	if e := errors.Join(errs...); e != nil {
		return e
	}
	in.Phase = "prepared"
	return writeJSON(journal(in.Options.Dir), in)
}

// Resume is called by the installed launcher before reading config. If power
// failed after stopping the old schedule, the next scheduled invocation finishes
// the exact prepared transaction using its already-redeemed credential.
func Resume(ctx context.Context, dir string) (bool, error) {
	in, e := load(dir)
	if os.IsNotExist(e) {
		return false, nil
	}
	if e != nil {
		return true, safe("migration intent unreadable; refusing an ambiguous cutover")
	}
	if in.Phase != "switching" {
		return false, nil
	}
	in.Options.Scheduled = true
	return true, Install(ctx, in.Options)
}

// Install validates and stages everything before changing a working schedule.
// Credentials and intent survive failure; old files remain in root-only backups.
func Install(ctx context.Context, o Options) (retErr error) {
	if h, e := os.Hostname(); e != nil || o.ExpectedHost != "" && o.ExpectedHost != h {
		return safe("hostname differs from the generated command; request a command for this host")
	}
	if e := agentupdate.ValidateURL(o.URL, o.AllowHTTP); e != nil {
		return e
	}
	if o.Scheduler == "" {
		o.Scheduler = "systemd"
	}
	if o.Scheduler != "systemd" && o.Scheduler != "truenas-cron" {
		return safe("unknown scheduler")
	}
	if o.Scheduler == "truenas-cron" {
		if e := agentmount.CheckDataset(o.Dir); e != nil {
			return e
		}
	} else if o.ReplaceSchedule != "" {
		return safe("--replace-schedule requires truenas-cron")
	}
	if e := privateDir(o.Dir); e != nil {
		return e
	}
	migration := filepath.Join(o.Dir, ".migration")
	if e := privateDir(migration); e != nil {
		return e
	}
	unlock, e := fsx.LockDir(migration)
	if e != nil {
		return safe("another installer is running")
	}
	defer unlock()
	in, e := load(o.Dir)
	if e != nil && !os.IsNotExist(e) {
		return safe("migration intent unreadable; refusing to reset replay state")
	}
	if e == nil && in.Phase == "switching" {
		if in.Options.URL != o.URL || in.Options.Scheduler != o.Scheduler || in.Options.Prefix != o.Prefix {
			return safe("resume with the original migration settings")
		}
		in.Options.Scheduled = o.Scheduled
		// Revert interrupted file replacement first. Keep controls/floor and token.
		if e = rollback(ctx, &in); e != nil {
			return e
		}
	}
	oldUnlock, e := fsx.LockDir(o.Dir)
	if e != nil {
		return safe("agent is running; retry after it finishes")
	}
	defer func() {
		if oldUnlock != nil {
			oldUnlock()
		}
	}()
	stage := filepath.Join(migration, "stage")
	currentKey := filepath.Join(o.Dir, "public.key")
	token := filepath.Join(migration, "enroll.token")
	h, _ := os.Hostname()
	codeHash := ""
	if o.Enroll != "" {
		codeHash = digest(o.Enroll)
	}
	previous, loadErr := load(o.Dir)
	reuse := loadErr == nil && previous.Phase == "prepared" && (o.Enroll == "" || previous.CodeHash == codeHash) && previous.Options.URL == o.URL && previous.Options.Scheduler == o.Scheduler && previous.Options.ReplaceSchedule == o.ReplaceSchedule
	if reuse {
		in = previous
		in.Options.Scheduled = o.Scheduled
		// Every rerun rechecks signed bytes and every executable; no stale stage trust.
		if _, e = runner(o, stage).Load(); e != nil {
			return safe("prepared migration controls unreadable; use a fresh enrollment code")
		}
	} else {
		// A fresh code is the escape hatch. Existing slots controls are never removed.
		if e = removeStage(stage); e != nil {
			return e
		}
		if e = privateDir(stage); e != nil {
			return e
		}
		sourceToken := o.TokenFile
		if sourceToken == "" {
			if _, e = agentupdate.ReadToken(filepath.Join(o.Dir, "token")); e == nil {
				sourceToken = filepath.Join(o.Dir, "token")
			}
		}
		// New hosts must redeem to authenticate their downloads. Existing installs
		// validate with their old credential and redeem only after preparation.
		redeemed := false
		if sourceToken == "" && o.Enroll != "" {
			if e = agentupdate.Enroll(ctx, o.URL, o.Enroll, h, token, o.AllowHTTP); e != nil {
				return e
			}
			sourceToken = token
			redeemed = true
		}
		if sourceToken == "" {
			return safe("--enroll or --token-file required")
		}
		if _, e = agentupdate.ReadToken(sourceToken); e != nil {
			return e
		}
		public := []byte(nil)
		if o.Public != "" {
			public, e = readFile(o.Public, 4096)
		} else {
			public, e = request(ctx, o, sourceToken, "/agent/public-key", 4096)
		}
		if e != nil && o.Enroll != "" && !redeemed && o.Public == "" {
			if e = agentupdate.Enroll(ctx, o.URL, o.Enroll, h, token, o.AllowHTTP); e != nil {
				return e
			}
			sourceToken = token
			redeemed = true
			public, e = request(ctx, o, sourceToken, "/agent/public-key", 4096)
		}
		if e != nil {
			return e
		}
		if pinned, e := readFile(currentKey, 4096); e == nil {
			if strings.TrimSpace(string(pinned)) != strings.TrimSpace(string(public)) {
				return safe("pinned release key differs; explicit trust rotation required")
			}
		} else if !os.IsNotExist(e) {
			return safe("pinned release key unreadable")
		}
		if e = fsx.WriteAtomic(filepath.Join(stage, "public.key"), public, 0600); e != nil {
			return e
		}
		var envelope []byte
		if o.Manifest != "" {
			envelope, e = readFile(o.Manifest, 65536)
		} else {
			envelope, e = request(ctx, o, sourceToken, "/agent/bootstrap?host="+url.QueryEscape(h), 65536)
		}
		if e != nil {
			return e
		}
		m, e := agentrelease.Verify(envelope, public)
		if e != nil || m.LauncherProtocol != 1 {
			return safe("invalid signed baseline runtime")
		}
		// A legacy floor must be independently readable. A previously rejected or
		// ambiguous legacy sequence cannot be reactivated by converting its layout.
		if b, e := readFile(filepath.Join(o.Dir, "state.json"), 65536); e == nil {
			var s agentupdate.State
			if json.Unmarshal(b, &s) != nil || s.High == 0 || s.Current == 0 || s.Current > s.High || s.Format > 1 {
				return safe("legacy signed floor unreadable; refusing conversion")
			}
			if _, e = readFile(currentKey, 4096); e != nil {
				return safe("legacy signed key unreadable; refusing conversion")
			}
			if m.Sequence < s.High || m.Sequence == s.High && (s.Pending || s.Current != s.High || s.Update.Rejected == s.High) {
				return safe("baseline cannot replay the legacy high-water mark; publish a higher signed release")
			}
			if m.Sequence == s.High {
				meta, e := readFile(filepath.Join(o.Dir, "releases", fmt.Sprint(s.Current), "metadata.json"), 65536)
				var old agentrelease.Manifest
				if e != nil || json.Unmarshal(meta, &old) != nil || old.Sequence != s.Current || old.Artifacts[runtime.GOOS+"-"+runtime.GOARCH+"/discover"].SHA256 != m.Artifacts[runtime.GOOS+"-"+runtime.GOARCH+"/discover"].SHA256 {
					return safe("legacy release identity differs or is unreadable; publish a higher signed baseline")
				}
			}
		} else if !os.IsNotExist(e) {
			return safe("legacy signed floor unreadable; refusing conversion")
		}
		// Re-download the installed launcher over the authenticated channel. The
		// initial bootstrap executable has the same trust as the installer itself.
		self, e := os.Executable()
		if e != nil {
			return e
		}
		launcher, e := readFile(self, agentrelease.MaxArtifact)
		if e != nil {
			return e
		}
		if o.Manifest == "" {
			sums, e := request(ctx, o, sourceToken, "/agent/bin/SHA256SUMS", 65536)
			if e != nil {
				return e
			}
			launcher, e = request(ctx, o, sourceToken, "/agent/bin/"+runtime.GOOS+"-"+runtime.GOARCH+"/agentboot", agentrelease.MaxArtifact)
			if e != nil {
				return e
			}
			want := ""
			for _, line := range strings.Split(string(sums), "\n") {
				f := strings.Fields(line)
				if len(f) == 2 && f[1] == runtime.GOOS+"-"+runtime.GOARCH+"/agentboot" {
					if want != "" {
						return safe("ambiguous launcher checksum")
					}
					want = f[0]
				}
			}
			if want == "" || want != agentrelease.Digest(launcher) {
				return safe("launcher checksum mismatch")
			}
		}
		if e = fsx.WriteAtomic(filepath.Join(stage, "agentboot"), launcher, 0755); e != nil {
			return e
		}
		if e = fsx.WriteAtomic(filepath.Join(stage, "runtime.json"), envelope, 0400); e != nil {
			return e
		}
		r := runner(o, stage)
		r.TokenFile = sourceToken
		// Initialization streams and verifies artifacts and executes both bounded
		// self-tests in a private staging directory on the target filesystem.
		if e = r.Initialize(ctx, envelope, o.Input); e != nil {
			return safe("signed runtime preparation failed; previous schedule kept")
		}
		if !redeemed && o.Enroll != "" {
			if e = agentupdate.Enroll(ctx, o.URL, o.Enroll, h, token, o.AllowHTTP); e != nil {
				return e
			}
		} else if !redeemed {
			if e = copyFile(sourceToken, token, 0600); e != nil {
				return e
			}
		}
		clean := o
		clean.Enroll = ""
		clean.TokenFile = ""
		clean.Public = ""
		clean.Manifest = ""
		clean.Input = ""
		in = intent{Format: 1, Phase: "prepared", Options: clean, CodeHash: codeHash, LauncherHash: agentrelease.Digest(launcher), Files: map[string]backup{}}
		files := []string{filepath.Join(o.Dir, "config.json"), filepath.Join(o.Dir, "token"), currentKey, filepath.Join(o.Dir, "agentboot")}
		if o.Scheduler == "systemd" {
			bindir := filepath.Join(o.Prefix, "/usr/local/bin")
			units := filepath.Join(o.Prefix, "/etc/systemd/system")
			if e = os.MkdirAll(bindir, 0755); e != nil {
				return e
			}
			if e = os.MkdirAll(units, 0755); e != nil {
				return e
			}
			files = append(files, filepath.Join(bindir, "homelab-discover"), filepath.Join(units, "homelab-discover.service"), filepath.Join(units, "homelab-discover.timer"))
			_, e = os.Stat(filepath.Join(units, "homelab-discover.service"))
			in.HadSchedule = e == nil
			if manageSystemd(o) {
				in.TimerEnabled = system(ctx, "is-enabled", "--quiet", "homelab-discover.timer") == nil
				in.TimerActive = system(ctx, "is-active", "--quiet", "homelab-discover.timer") == nil
			}
		} else {
			plan, e := PrepareCron(ctx, o.Dir, o.ReplaceSchedule)
			if e != nil {
				return e
			}
			snap := plan.Snapshot()
			in.Cron = &snap
		}
		for _, f := range files {
			b, e := snapshot(f)
			if e != nil {
				return safe("cannot snapshot previous installation")
			}
			in.Files[f] = b
		}
		if e = writeJSON(journal(o.Dir), in); e != nil {
			return e
		}
	}
	// If controls exist, prove the original floor and runtime before cutover.
	// A failed migration can leave valid new controls with the old schedule.
	for _, name := range []string{"control.0", "control.1"} {
		if _, se := os.Lstat(filepath.Join(o.Dir, name)); se == nil {
			existing := runner(o, o.Dir)
			existing.Public = filepath.Join(stage, "public.key")
			if e = existing.VerifyInstalled(); e != nil {
				return safe("existing slots floor or runtime unreadable; refusing reinstall")
			}
			break
		} else if !os.IsNotExist(se) {
			return safe("existing slots controls unreadable")
		}
	}
	if b, e := readFile(filepath.Join(stage, "agentboot"), agentrelease.MaxArtifact); e != nil || agentrelease.Digest(b) != in.LauncherHash {
		return safe("prepared launcher checksum mismatch; use a fresh code")
	}
	// Legacy scheduling may have run since a rolled-back preparation. Recheck
	// its floor on every attempt under the same stable state lock.
	staged, e := runner(o, stage).Load()
	if e != nil {
		return safe("prepared migration floor unreadable")
	}
	selected := staged
	existing, existingErr := runner(o, o.Dir).Load()
	if existingErr == nil {
		selected = existing
	}
	replaceControls := false
	if _, le := os.Stat(filepath.Join(o.Dir, "state.json")); le == nil && existingErr == nil && staged.Floor > existing.Floor {
		selected = staged
		replaceControls = true
	}
	if b, le := readFile(filepath.Join(o.Dir, "state.json"), 65536); le == nil {
		var legacy agentupdate.State
		if json.Unmarshal(b, &legacy) != nil || legacy.High == 0 || legacy.Current == 0 || legacy.Current > legacy.High || legacy.Format > 1 {
			return safe("legacy signed floor unreadable; refusing conversion")
		}
		key, ke := readFile(currentKey, 4096)
		stageKey, se := readFile(filepath.Join(stage, "public.key"), 4096)
		if ke != nil || se != nil || strings.TrimSpace(string(key)) != strings.TrimSpace(string(stageKey)) {
			return safe("legacy pinned key differs or is unreadable")
		}
		if selected.Floor < legacy.High || selected.Active.Sequence == legacy.High && (legacy.Pending || legacy.Current != legacy.High || legacy.Update.Rejected == legacy.High) {
			return safe("legacy floor advanced since preparation; use a fresh code after publishing a higher baseline")
		}
	} else if !os.IsNotExist(le) {
		return safe("legacy signed floor unreadable")
	}
	// Reverify staged immutable files on every resume before stopping anything.
	if e = runner(o, stage).VerifyInstalled(); e != nil {
		return safe("prepared runtime verification failed; previous schedule kept")
	}
	if _, e = agentupdate.ReadToken(token); e != nil {
		return e
	}
	// From the first installed mutation onward, every failure restores backups.
	defer func() {
		if retErr != nil {
			if oldUnlock != nil {
				oldUnlock()
				oldUnlock = nil
			}
			if e := rollback(context.WithoutCancel(ctx), &in); e != nil {
				retErr = errors.Join(retErr, e)
			}
		}
	}()
	// The long-lived token is durable before a working schedule is stopped.
	if e = copyFile(token, filepath.Join(o.Dir, "token"), 0600); e != nil {
		return e
	}
	in.Phase = "switching"
	if e = writeJSON(journal(o.Dir), in); e != nil {
		return e
	}
	if o.Scheduler == "systemd" && manageSystemd(o) {
		if e = system(ctx, "stop", "homelab-discover.timer"); e != nil && in.HadSchedule {
			return e
		}
		if !o.Scheduled {
			if e = system(ctx, "stop", "homelab-discover.service"); e != nil && in.HadSchedule {
				return e
			}
		}
	}
	// An explicit higher legacy migration may repair a previous rolled-back
	// cutover. Every replacement strictly raises the existing floor.
	if replaceControls {
		if e = publishSlots(stage, o.Dir); e != nil {
			return e
		}
	}
	// Publish controls exactly once. A failed cutover never erases or lowers them.
	if _, e = os.Stat(filepath.Join(o.Dir, "control.0")); os.IsNotExist(e) {
		if _, e = os.Stat(filepath.Join(o.Dir, "control.1")); os.IsNotExist(e) {
			if e = publishSlots(stage, o.Dir); e != nil {
				return e
			}
		}
	}
	if e = copyFile(filepath.Join(stage, "public.key"), currentKey, 0600); e != nil {
		return e
	}
	if e = copyFile(filepath.Join(stage, "agentboot"), filepath.Join(o.Dir, "agentboot"), 0755); e != nil {
		return e
	}
	// Install recovery entry points before replacing config: a reboot after this
	// boundary invokes Resume and reconciles the exact durable intent first.
	if o.Scheduler == "systemd" {
		if e = writeSystemd(o); e != nil {
			return e
		}
	}
	config := agentboot.Config{Dir: o.Dir, Public: currentKey, URL: o.URL, TokenFile: filepath.Join(o.Dir, "token"), AllowHTTP: o.AllowHTTP}
	if o.Scheduler == "truenas-cron" {
		config.Dataset = o.Dir
	}
	if e = writeJSON(filepath.Join(o.Dir, "config.json"), config); e != nil {
		return e
	}
	oldUnlock()
	oldUnlock = nil
	r := agentboot.Runner{Config: config, Host: h}
	if e = r.VerifyInstalled(); e != nil {
		return safe("installed runtime verification failed")
	}
	// A real accepted report, independently witnessed by the launcher, precedes
	// schedule adoption. Exit zero from a worker is never sufficient.
	if e = r.Run(ctx); e != nil {
		return safe("first signed report failed; restoring previous schedule")
	}
	if o.Scheduler == "truenas-cron" {
		plan, e := RestoreCronPlan(*in.Cron)
		if e != nil {
			return e
		}
		ce := plan.Commit(ctx)
		snap := plan.Snapshot()
		in.Cron = &snap
		if e = writeJSON(journal(o.Dir), in); e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	} else if manageSystemd(o) {
		if e = system(ctx, "daemon-reload"); e != nil {
			return e
		}
		if e = system(ctx, "enable", "--now", "homelab-discover.timer"); e != nil {
			return e
		}
	}
	in.Phase = "committed"
	if e = writeJSON(journal(o.Dir), in); e != nil {
		return e
	}
	return nil
}
func publishSlots(stage, dir string) error {
	// Destination directories are immutable. Copy via durable atomic publication;
	// the stage is retained so any interrupted publication can be resumed.
	if err := filepath.WalkDir(filepath.Join(stage, "slots"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, e := filepath.Rel(stage, path)
		if e != nil {
			return e
		}
		dst := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0700)
		}
		data, e := readFile(path, agentrelease.MaxArtifact)
		if e != nil {
			return e
		}
		if prior, e := readFile(dst, agentrelease.MaxArtifact); e == nil {
			if agentrelease.Digest(prior) != agentrelease.Digest(data) {
				return safe("immutable installed slot differs; refusing replacement")
			}
			return nil
		} else if !os.IsNotExist(e) {
			return e
		}
		if e = fsx.CreateAtomic(dst, data, 0555); e != nil {
			return e
		}
		return nil
	}); err != nil {
		return err
	}
	// Sync all newly-created directory links before publishing either floor.
	var dirs []string
	if e := filepath.WalkDir(filepath.Join(dir, "slots"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	}); e != nil {
		return e
	}
	dirs = append([]string{dir}, dirs...)
	for i := len(dirs) - 1; i >= 0; i-- {
		f, e := os.Open(dirs[i])
		if e != nil {
			return e
		}
		e = f.Sync()
		f.Close()
		if e != nil {
			return e
		}
	}
	source := agentboot.Runner{Config: agentboot.Config{Dir: stage, Public: filepath.Join(stage, "public.key")}}
	c, e := source.Load()
	if e != nil {
		return e
	}
	installed := agentboot.Runner{Config: agentboot.Config{Dir: dir, Public: source.Public}}
	return installed.AdoptBaseline(c)
}
func writeSystemd(o Options) error {
	bin := filepath.Join(o.Prefix, "/usr/local/bin/homelab-discover")
	units := filepath.Join(o.Prefix, "/etc/systemd/system")
	wrapper := fmt.Sprintf("#!/bin/sh\nexec \"%s/agentboot\" -config \"%s/config.json\"\n", o.Dir, o.Dir)
	if e := fsx.WriteAtomic(bin, []byte(wrapper), 0755); e != nil {
		return e
	}
	service := fmt.Sprintf("[Unit]\nDescription=Supervise and report signed inventory runtime\nWants=network-online.target\nAfter=network-online.target\n[Service]\nType=oneshot\nExecStart=%s/agentboot -config %s/config.json\nTimeoutStartSec=14min\nKillMode=control-group\nUMask=0077\n", o.Dir, o.Dir)
	if e := fsx.WriteAtomic(filepath.Join(units, "homelab-discover.service"), []byte(service), 0644); e != nil {
		return e
	}
	return fsx.WriteAtomic(filepath.Join(units, "homelab-discover.timer"), []byte("[Unit]\nDescription=Run signed inventory launcher\n[Timer]\nOnBootSec=2min\nOnUnitActiveSec=15min\n[Install]\nWantedBy=timers.target\n"), 0644)
}

// Staged runtime directories are intentionally read-only. Make only our private
// staging directories writable before pruning a superseded installer attempt.
func removeStage(stage string) error {
	if _, e := os.Lstat(stage); os.IsNotExist(e) {
		return nil
	}
	if e := filepath.WalkDir(stage, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return os.Chmod(path, 0700)
		}
		return nil
	}); e != nil {
		return e
	}
	return os.RemoveAll(stage)
}
