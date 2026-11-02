package archivetier

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ArrAPI interface {
	Targets(context.Context) ([]Target, error)
	Current(context.Context, Target) (Target, error)
	Queue(context.Context) ([]QueueItem, error)
	Search(context.Context, Target) ([]Release, error)
	Grab(context.Context, Release) error
	Manual(context.Context, QueueItem) ([]ManualItem, error)
	Import(context.Context, []map[string]any) (int, error)
	Command(context.Context, int) (string, error)
	Preflight(context.Context) error
	Present(context.Context, Target) (bool, error)
	Opted(context.Context, Target) (bool, error)
	TargetPreflight(context.Context, Target, []ManualItem, int64) (string, []Evidence, error)
	Recovery(context.Context, *Job) ([]Evidence, error)
	Lifecycle(context.Context, *Job, Target) error
	GrabbedDownload(context.Context, *Job) (string, error)
}
type PlexAPI interface {
	Active(context.Context) (bool, error)
	Recent(context.Context, time.Time, int) (map[string]bool, error)
}
type Runner struct {
	// Interval bounds gaps between playback observations; zero defaults to 2m.
	Interval time.Duration
	// Kept in memory so a restart cannot reuse unobserved quiet time.
	playbackObservedAt time.Time
	// OwnerPaused is read again immediately before each new search, grab or import.
	OwnerPaused func() bool
	// Probe overrides the read-only ffprobe implementation in tests.
	Probe      func(context.Context, File) (*MediaInfo, error)
	Notify     func(context.Context, string) error
	LogSummary func(string)
	Config     Config
	Arr        map[string]ArrAPI
	Plex       PlexAPI
	Store      *Store
	DryRun     bool
	DState     func() (int, error)
	// DownloadFree reports free bytes on the download disk.
	DownloadFree func() (int64, error)
	Now          func() time.Time
	Acquire      func(Target) (func(), error)
	NoSearch     bool
	// RecycleCheck reports whether one recorded recycle original is still
	// there, through the read-only inspection view; nil leaves it unknown.
	RecycleCheck func(Evidence) (bool, error)
	// ignoreWindow permits explicit approvals outside the nightly window.
	// Cycles are serialized by the supervisor.
	ignoreWindow bool
	// lookupBudget is scoped to one Check now press; nil leaves nightly cycles unchanged.
	lookupBudget *int
}

// window opens outside Config.Window only for explicit approvals.
func (r *Runner) window(now time.Time) (bool, string) {
	ok, night := r.Config.Window(now)
	return ok || r.ignoreWindow, night
}

func CountDState(root string) (int, error) {
	entries, e := os.ReadDir(root)
	if e != nil {
		return 0, fmt.Errorf("cannot inspect host /proc")
	}
	n := 0
	seen := 0
	for _, v := range entries {
		if v.Name() == "" || v.Name()[0] < '0' || v.Name()[0] > '9' {
			continue
		}
		b, e := os.ReadFile(filepath.Join(root, v.Name(), "stat"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return 0, fmt.Errorf("cannot inspect host processes")
		}
		s := string(b)
		i := strings.LastIndex(s, ") ")
		if i < 0 || len(s) < i+3 {
			return 0, fmt.Errorf("invalid process stat")
		}
		seen++
		if s[i+2] == 'D' {
			n++
		}
	}
	if seen == 0 {
		return 0, fmt.Errorf("no host processes visible")
	}
	return n, nil
}

// Admission is the supervisor's decision on archive HTTP writes this cycle.
// Read-only reconciliation, verification and timeout handling of an existing
// job always run; NewWrites gates every grab and import submission.
type Admission struct {
	NewWrites bool
	Reason    string
	// CheckNow refreshes discovery outside the nightly window without opening
	// it for automatic grabs. Ready imports and explicit approvals run any time.
	CheckNow bool
}

// Check runs one cycle with writes admitted (subject to dry-run and guards).
func (r *Runner) Check(ctx context.Context, now time.Time) (State, error) {
	return r.CheckAdmission(ctx, now, Admission{NewWrites: true})
}

// CheckAdmission runs one cycle under the supervisor's admission decision.
func (r *Runner) CheckAdmission(ctx context.Context, now time.Time, adm Admission) (State, error) {
	if !adm.NewWrites && adm.Reason == "" {
		adm.Reason = "supervisor closed admission"
	}
	if adm.CheckNow {
		remaining := 5
		r.lookupBudget = &remaining
	}
	defer func() { r.lookupBudget = nil }()
	st, e := r.Store.Load()
	if e != nil {
		return st, e
	}
	activeJob := st.Job != nil && st.Job.Phase != Complete && st.Job.Phase != Abandoned && st.Job.Phase != Failed
	r.ignoreWindow = activeJob && st.Job.ManuallyApproved || !activeJob && st.runnableApproval()
	defer func() { r.ignoreWindow = false }()
	st.Checked = now
	st.DryRun = r.DryRun
	st.Error = ""
	st.Paused = ""
	if st.Job != nil && !st.Job.HasImportIntent() {
		st.Job.Message = preImportMessage(st.Job.Message)
	}
	_, _ = r.observePlayback(ctx, now, &st)
	if r.OwnerPaused != nil && r.OwnerPaused() {
		adm.NewWrites, adm.Reason = false, "Paused by owner"
	}
	e = r.cycle(ctx, now, &st, adm)
	if e != nil {
		st.Error = e.Error()
	}
	r.cleanupAbandoned(ctx, now, &st, adm)
	r.retryTagRemovals(ctx, now, &st, adm)
	r.recordHistory(&st, now)
	st.MigrateFailed()
	r.queueReports(&st, now)
	if saveErr := r.Store.Save(st); saveErr != nil {
		return st, saveErr
	}
	if err := r.sendReports(ctx, &st, now); err != nil {
		return st, err
	}
	return st, e
}
func (r *Runner) guards(ctx context.Context, now time.Time) (map[string]bool, error) {
	active, e := r.Plex.Active(ctx)
	if e != nil {
		return nil, e
	}
	if active {
		return nil, pauseError("Plex sessions active")
	}
	n, e := r.DState()
	if e != nil {
		return nil, e
	}
	if n >= r.Config.MaxDState {
		return nil, pauseError("host D-state threshold reached")
	}
	return r.Plex.Recent(ctx, now, r.Config.IdleDays)
}
func (r *Runner) cycle(ctx context.Context, now time.Time, st *State, adm Admission) error {
	if e := r.Config.Validate(); e != nil {
		return e
	}
	window, night := r.window(now)
	inside, _ := r.Config.Window(now)
	checkOnly := adm.CheckNow && !inside
	activeJob := false
	if st.Job != nil && st.Job.HoldsLease() {
		if st.Job.Phase == Failed {
			return fmt.Errorf("archive halted: %s", st.Job.Message)
		}
		release, e := r.acquire(st.Job.Target)
		if e != nil {
			st.Paused = "target lease conflict"
			return nil
		}
		defer release()
		// Verification and timeouts are read-only and always run; only the
		// import submission needs write admission.
		writes := !r.DryRun && !r.NoSearch
		reason := "dry-run or release searches disabled"
		if writes && !adm.NewWrites {
			writes, reason = false, adm.Reason
		}
		// Import uses the identity checked before the grab.
		if err := r.resume(ctx, now, st, writes, reason); err != nil {
			return err
		}
		if !checkOnly {
			return nil
		}
		activeJob = true
	}
	if !adm.NewWrites {
		st.Paused = "new archive work blocked: " + adm.Reason
		return nil
	}
	if !r.DryRun && !window && !adm.CheckNow {
		st.Paused = "outside nightly window"
		return nil
	}
	if !activeJob {
		r.newNight(st, night)
	}
	if !activeJob && !r.DryRun && !r.NoSearch {
		if started, err := r.nextApproval(ctx, now, st); started || err != nil {
			return err
		}
	}
	// The approval window must not carry over into automatic selection.
	if !activeJob {
		r.ignoreWindow = false
		window = inside
	}
	// Approvals alone opened the window; the automatic scan keeps its own.
	if !inside && !adm.CheckNow && !r.DryRun {
		if st.Paused == "" {
			st.Paused = "outside nightly window"
		}
		return nil
	}
	recent, guardErr := r.guards(ctx, now)
	if guardErr != nil {
		st.Paused = guardErr.Error()
		if !r.DryRun {
			return r.pause(st, guardErr)
		}
	}
	all := []Target{}
	queues := map[string][]QueueItem{}
	for _, kind := range []string{"sonarr", "radarr"} {
		a := r.Arr[kind]
		if e := a.Preflight(ctx); e != nil {
			return e
		}
		ts, e := a.Targets(ctx)
		if e != nil {
			return e
		}
		all = append(all, st.optedTargets(ts)...)
		qs, e := a.Queue(ctx)
		if e != nil {
			return e
		}
		queues[kind] = st.activeQueue(kind, qs)
	}
	r.resolvePlex(ctx, all, st, now)
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Season < b.Season
	})
	if window && st.SearchNight != night {
		if st.SearchNight != "" {
			st.DiscoveryStart++
		}
		st.SearchNight = night
		st.SearchesUsed = 0
	}
	if len(all) > 0 {
		start := st.DiscoveryStart % len(all)
		all = append(all[start:], all[:start]...)
	}
	st.Report = []Decision{}
	st.ProjectedSavings = 0
	if st.Lookups == nil {
		st.Lookups = map[string]Lookup{}
	}
	cooldown := time.Duration(r.Config.SearchCooldownHours) * time.Hour
	for k, l := range st.Lookups {
		if now.Sub(l.At) >= 7*24*time.Hour || l.At.After(now) {
			delete(st.Lookups, k)
		}
	}
	restored := map[string]time.Time{}
	for _, h := range st.History {
		if h.Status == HistoryRestored {
			restored[targetKey(h.Job.Target)] = h.StatusAt
		}
	}
	leased := map[string]bool{}
	var chosen *Decision
	for _, t := range all {
		d := Decision{Target: t}
		key := targetKey(t)
		why := t.Eligible(r.Config, now)
		if st.FailedTitle(t) {
			why = "title needs recovery"
		}
		archived := st.CompletedFiles[key] == filesKey(t)
		if archived {
			why = "already archived"
		}
		if guardErr != nil {
			why = guardErr.Error()
		}
		if at, ok := restored[key]; ok && why == "" {
			// The operator undid this title; without this it would be
			// archived again the next night.
			why = "restored by the operator on " + at.Format("2006-01-02") + "; remove the archive tag"
		}
		if why == "" && watched(recent, t.plexKey(r.Config), t) {
			why = "watched within idle window"
		}
		for _, q := range queues[t.Kind] {
			if q.Matches(t) {
				why = "title in application queue"
			}
		}
		cached, fresh := st.Lookups[key]
		fresh = fresh && cached.Files == filesKey(t) && now.Sub(cached.At) < cooldown
		// Pre-approval caches retained only the runner-up, without grab identity.
		// Refresh these once under the usual tracker rate limit.
		fresh = fresh && (cached.Decision.Reason != NoGoodRelease || len(cached.Releases) > 0)
		switch {
		case archived:
			d.Reason = "already archived"
		case why != "":
			d.Reason = why
			if prior, ok := st.Lookups[key]; ok && prior.Files == filesKey(t) {
				d.Candidates, d.SearchedAt, d.Rejected, d.Mode = prior.Decision.Candidates, prior.At, prior.Decision.Rejected, prior.Decision.Mode
			}
		case r.NoSearch:
			d.Reason = "release search disabled"
		case fresh:
			d = selectReleases(t, cached.Releases, r.Config, st.BadReleases[key])
			d.SearchedAt = cached.At
			d.Target = t
		case chosen != nil && !checkOnly:
			// Keep the remaining lookups available for the mandatory fresh
			// search before grab, including a dry-run followed by apply.
			d.Reason = "release lookup deferred: usable candidate already found"
		case !window && !adm.CheckNow:
			d.Reason = "release lookup waits for the nightly window"
		case r.lookupBudget != nil && *r.lookupBudget == 0:
			d.Reason = "check lookup limit reached; waits for next check or nightly cycle"
		case st.SearchesLastMinute(now) >= r.Config.SearchesPerMinute:
			d.Reason = "Search limit reached"
		default:
			release, e := r.acquire(t)
			if e != nil {
				d.Reason = "target lease conflict"
				break
			}
			defer release()
			leased[t.Key()] = true
			ok, e := r.lookup(st, night, now)
			if e != nil {
				return e
			}
			if !ok {
				d.Reason = "Search limit reached"
				break
			}
			rs, e := r.Arr[t.Kind].Search(ctx, t)
			if e != nil {
				return e
			}
			d = selectReleases(t, rs, r.Config, st.BadReleases[key])
			d.SearchedAt = now
			st.Lookups[key] = cachedLookup(t, d, rs, now)
		}
		if d.Release != nil {
			switch {
			case attempted(st, key):
				d.Skipped = "already attempted tonight"
				if why := st.Skipped[key]; why != "" {
					d.Skipped = why
				}
			case chosen == nil:
				copy := d
				chosen = &copy
			}
		}
		st.Report = append(st.Report, sanitized(d))
		st.ProjectedSavings += d.Savings
	}
	st.Reported = now
	if r.DryRun {
		if guardErr != nil && !errors.Is(guardErr, errPaused) {
			return guardErr
		}
		return nil
	}
	if !inside {
		st.Paused = "outside nightly window; candidates refreshed"
		return nil
	}
	if chosen == nil {
		st.Paused = "no eligible replacement"
		return nil
	}
	if !leased[chosen.Target.Key()] {
		release, e := r.acquire(chosen.Target)
		if e != nil {
			st.Paused = "target lease conflict"
			return nil
		}
		defer release()
	}
	return r.startAcquired(ctx, now, st, *chosen)
}

// newNight resets the per-night grab budget when the nightly window rolls.
func (r *Runner) newNight(st *State, night string) {
	if st.Night != night {
		st.Night = night
		st.Attempted = nil
		st.Skipped = nil
	}
}

// lookup reserves one tracker lookup in the persisted rolling minute. Save
// before sending the request so crashes and failed searches cannot refund it.
func (r *Runner) lookup(st *State, night string, now time.Time) (bool, error) {
	if r.OwnerPaused != nil && r.OwnerPaused() {
		return false, nil
	}
	if r.lookupBudget != nil && *r.lookupBudget == 0 {
		return false, nil
	}
	if r.Now != nil {
		now = r.Now()
	}
	if st.SearchNight != night {
		st.SearchNight = night
		st.SearchesUsed = 0
	}
	recent := make([]time.Time, 0, len(st.SearchTimes)+1)
	for _, at := range st.SearchTimes {
		if at.After(now.Add(-time.Minute)) {
			recent = append(recent, at)
		}
	}
	st.SearchTimes = recent
	if len(recent) >= r.Config.SearchesPerMinute {
		return false, nil
	}
	if r.lookupBudget != nil {
		*r.lookupBudget--
	}
	st.SearchTimes = append(st.SearchTimes, now)
	st.SearchesUsed++
	return true, r.Store.Save(*st)
}

func attempted(st *State, key string) bool {
	for _, k := range st.Attempted {
		if k == key {
			return true
		}
	}
	return false
}

// skip records why one candidate cannot be grabbed tonight, so the next
// cycle considers the next candidate instead of retrying this one.
func skip(st *State, t Target, why string) error {
	key := targetKey(t)
	if !attempted(st, key) {
		st.Attempted = append(st.Attempted, key)
	}
	if st.Skipped == nil {
		st.Skipped = map[string]string{}
	}
	st.Skipped[key] = why
	st.Paused = why
	return nil
}

func (r *Runner) acquire(t Target) (func(), error) {
	if r.Acquire == nil {
		return func() {}, nil
	}
	release, e := r.Acquire(t)
	if e == nil && release == nil {
		return nil, fmt.Errorf("lease release callback missing")
	}
	return release, e
}
func (r *Runner) start(ctx context.Context, now time.Time, st *State, d Decision) error {
	release, e := r.acquire(d.Target)
	if e != nil {
		st.Paused = "target lease conflict"
		return nil
	}
	defer release()
	return r.startAcquired(ctx, now, st, d)
}
func (r *Runner) startAcquired(ctx context.Context, now time.Time, st *State, d Decision) error {
	if r.DryRun || r.NoSearch || r.OwnerPaused != nil && r.OwnerPaused() {
		st.Paused = "write disabled"
		return nil
	}
	t := d.Target
	if st.FailedTitle(t) {
		return skip(st, t, "title needs recovery")
	}
	a := r.Arr[t.Kind]
	window, night := r.window(now)
	if !window {
		st.Paused = "outside nightly window"
		return nil
	}
	r.newNight(st, night)
	recent, e := r.guards(ctx, now)
	if e != nil {
		return r.pause(st, e)
	}
	if !d.ManuallyApproved && (t.plexKey(r.Config) == "" || watched(recent, t.plexKey(r.Config), t)) {
		st.Paused = "recent play before grab"
		return nil
	}
	qs, e := a.Queue(ctx)
	if e != nil {
		return e
	}
	for _, q := range qs {
		if q.Matches(t) && !st.abandoned(t.Kind, q.DownloadID) {
			st.Paused = "queue changed before grab"
			return nil
		}
	}
	current, e := a.Current(ctx, t)
	if e != nil {
		return e
	}
	if !SameFiles(t, current) {
		st.Attempted = append(st.Attempted, targetKey(t))
		st.Paused = "library changed before grab"
		return nil
	}
	opted, e := a.Opted(ctx, t)
	if e != nil {
		return e
	}
	if !opted {
		st.Attempted = append(st.Attempted, targetKey(t))
		st.Paused = "archive tag removed before grab"
		return nil
	}
	// Re-search immediately before grabbing; cached releases expire and flags
	// can change. This lookup counts against the rolling minute rate too.
	ok, e := r.lookup(st, night, now)
	if e != nil {
		return e
	}
	if !ok {
		st.Paused = "Search limit reached"
		return nil
	}
	rs, e := a.Search(ctx, t)
	if e != nil {
		return e
	}
	if r.Now != nil {
		now = r.Now()
	}
	if ok, _ := r.window(now); !ok {
		st.Paused = "window ended before grab"
		return nil
	}
	fresh := selectReleases(t, rs, r.Config, st.BadReleases[targetKey(t)])
	if d.ManuallyApproved {
		fresh.Release, fresh.Score, fresh.Candidates = nil, nil, nil
		for _, rel := range rs {
			if releaseID(rel) == releaseID(*d.Release) && manualReject(rel, current, r.Config) == "" {
				sc := score(rel, current, r.Config)
				fresh.Release, fresh.Score, fresh.Savings = &rel, &sc, t.Bytes()-rel.Size
				fresh.Reason, fresh.ManuallyApproved = "", true
				break
			}
		}
	}
	if fresh.Release == nil {
		st.Attempted = append(st.Attempted, targetKey(t))
		st.Paused = "replacement no longer available"
		return nil
	}
	fresh.SearchedAt = now
	for i, prior := range st.Report {
		if targetKey(prior.Target) == targetKey(t) {
			st.ProjectedSavings += fresh.Savings - prior.Savings
			st.Report[i] = sanitized(fresh)
		}
	}
	if st.Lookups != nil && !d.ManuallyApproved {
		st.Lookups[targetKey(t)] = cachedLookup(t, fresh, rs, now)
	}
	d = fresh
	recent, e = r.guards(ctx, now)
	if e != nil {
		return r.pause(st, e)
	}
	if !d.ManuallyApproved && (t.plexKey(r.Config) == "" || watched(recent, t.plexKey(r.Config), t)) {
		st.Paused = "recent play before grab"
		return nil
	}
	qs, e = a.Queue(ctx)
	if e != nil {
		return e
	}
	for _, q := range qs {
		if q.Matches(t) && !st.abandoned(t.Kind, q.DownloadID) {
			st.Paused = "queue changed during search"
			return nil
		}
	}
	// An approval may override a post-download rejection. Reuse that exact
	// completed download instead of submitting a duplicate torrent grab.
	retained := -1
	if d.ManuallyApproved {
		for i, saved := range st.AbandonedDownloads {
			if saved.Kind != t.Kind || saved.ReleaseID != releaseID(*d.Release) {
				continue
			}
			for _, q := range qs {
				if q.DownloadID == saved.DownloadID && q.Matches(t) && q.SizeLeft == 0 && (q.State == "importPending" || q.State == "importBlocked") {
					retained = i
					break
				}
			}
			if retained >= 0 {
				break
			}
		}
	}
	current, e = a.Current(ctx, t)
	if e != nil {
		return e
	}
	if !SameFiles(t, current) {
		st.Paused = "library changed during search"
		return nil
	}
	if r.Now != nil {
		now = r.Now()
	}
	if ok, _ := r.window(now); !ok {
		st.Paused = "window ended before grab"
		return nil
	}
	if e = r.admit(ctx); e != nil {
		return r.pause(st, e)
	}
	if r.Now != nil {
		now = r.Now()
	}
	if ok, _ := r.window(now); !ok {
		st.Paused = "nightly window ended before write"
		return nil
	}
	if min := int64(r.Config.DownloadMinFreeGB * float64(GB)); min > 0 {
		if r.DownloadFree == nil {
			return r.pause(st, fmt.Errorf("download disk free space unknown"))
		}
		free, e := r.DownloadFree()
		if e != nil {
			return r.pause(st, fmt.Errorf("download disk free space unknown"))
		}
		if free-d.Release.Size < min {
			st.Paused = fmt.Sprintf("download disk would drop below %.0f GB free", r.Config.DownloadMinFreeGB)
			return nil
		}
	}
	root, baseline, e := a.TargetPreflight(ctx, t, nil, d.Release.Size)
	if e != nil {
		return e
	}
	if opted, err := a.Opted(ctx, t); err != nil {
		return err
	} else if !opted {
		return skip(st, t, "archive tag removed before write")
	}
	copy := *d.Release
	copy.Guid = ""
	copy.Rejections = nil
	selection := r.Config.Selection
	j := &Job{ReleaseID: releaseID(*d.Release), ManuallyApproved: d.ManuallyApproved, Mode: releaseMode(*d.Release, t), PolicyVersion: PolicyVersion, Selection: &selection, Score: d.Score, RunnerUp: d.RunnerUp, Note: d.Note, Target: t, Release: copy, Phase: GrabIntent, Started: now, PhaseStarted: now, RecycleRoot: root, RecycleBaseline: baseline}
	// Arr may import an explicitly grabbed release automatically. Preserve
	// subtitles before handing it the download, not only before ManualImport.
	if err := StageSidecars(j, r.Config, r.Store.Dir); err != nil {
		j.SidecarMessage = "subtitles not carried over: " + err.Error()
	}
	j.SidecarsStaged = true
	// Staging can take time. Repeat the volatile grab guards afterwards,
	// before publishing any intent or handing the release to Arr.
	current, e = a.Current(ctx, t)
	if e != nil {
		return e
	}
	if !SameFiles(t, current) {
		st.Paused = "library changed during subtitle staging"
		return nil
	}
	if opted, err := a.Opted(ctx, t); err != nil {
		return err
	} else if !opted {
		return skip(st, t, "archive tag removed during subtitle staging")
	}
	qs, e = a.Queue(ctx)
	if e != nil {
		return e
	}
	for _, q := range qs {
		if q.Matches(t) && !st.abandoned(t.Kind, q.DownloadID) {
			st.Paused = "title queue changed during subtitle staging"
			return nil
		}
	}
	if e = r.finalAdmission(ctx); e != nil {
		return r.pause(st, e)
	}
	st.Job = j
	if i := st.approvalIndex(t); i >= 0 {
		st.Approvals = append(st.Approvals[:i], st.Approvals[i+1:]...)
	}
	st.Attempted = append(st.Attempted, targetKey(t))
	if retained >= 0 {
		saved := st.AbandonedDownloads[retained]
		j.Phase, j.DownloadID, j.DownloadCompletedAt = Downloading, saved.DownloadID, saved.CompletedAt
		st.AbandonedDownloads = append(st.AbandonedDownloads[:retained], st.AbandonedDownloads[retained+1:]...)
	}
	if e = r.Store.Save(*st); e != nil {
		return e
	}
	if retained >= 0 {
		return r.Store.Audit("abandoned_download_approved", j)
	}
	if e = r.Store.Audit("grab_intent", j); e != nil {
		return e
	}
	if t.Kind == "sonarr" {
		d.Release.SeriesID = t.ID
	} else {
		d.Release.MovieID = t.ID
	}
	if e = a.Grab(ctx, *d.Release); e != nil {
		var refusal *HTTPError
		if errors.As(e, &refusal) && refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != http.StatusRequestTimeout {
			app := "Sonarr"
			if t.Kind == "radarr" {
				app = "Radarr"
			}
			why := fmt.Sprintf("%s refused the grab (%d)", app, refusal.Status)
			if refusal.Reason != "" {
				why += ": " + refusal.Reason
			}
			return r.finishUngrabbed(ctx, st, now, why, "grab_refused")
		}
		j.Message = "Grab response uncertain; checking queue and history"
		if err := r.Store.Save(*st); err != nil {
			return err
		}
		return r.Store.AuditDetail("grab_uncertain", j, map[string]string{"result": e.Error()})
	}
	if e = Transition(j, Downloading); e != nil {
		return e
	}
	j.PhaseStarted = now
	if e = r.Store.Save(*st); e != nil {
		return e
	}
	return r.Store.Audit("grab_accepted", j)
}

// collectRecovery uses the same original-file proof on failures as on success.
// It does not retry a write or release the unresolved title lease.
func (r *Runner) collectRecovery(ctx context.Context, j *Job, message string) string {
	if !j.HasImportIntent() {
		return message
	}
	a := r.Arr[j.Target.Kind]
	if a != nil {
		recovery, err := a.Recovery(ctx, j)
		if err == nil {
			j.Recovery = recovery
			if _, err := RestorePlan(*j); err == nil {
				return message
			}
		}
	}
	return message + "; recovery evidence unavailable: " + RecoveryInspection
}

func (r *Runner) fail(ctx context.Context, st *State, message string) error {
	j := st.Job
	// Any terminal grab/download decision can follow an independent Arr
	// replacement. Capture recovery even when policy/identity checks prevent
	// adoption and the earlier library snapshot showed unchanged originals.
	if (j.Phase == Downloading || j.Phase == NeedsApproval || j.Phase == GrabIntent) && !j.HasImportIntent() {
		if a := r.Arr[j.Target.Kind]; a != nil {
			if current, err := a.Current(ctx, j.Target); err == nil {
				now := st.Checked
				if now.IsZero() {
					now = time.Now().UTC()
				}
				if err := r.observeLibraryChange(ctx, now, st, current); err != nil {
					return err
				}
			}
		}
	}
	message = r.collectRecovery(ctx, j, message)
	j.Message = message
	if j.Phase != Failed {
		if e := Transition(j, Failed); e != nil {
			return e
		}
		j.PhaseStarted = time.Now().UTC()
		if r.Now != nil {
			j.PhaseStarted = r.Now()
		}
	}
	if e := r.Store.Save(*st); e != nil {
		return e
	}
	if e := r.Store.Audit("failed", j); e != nil {
		return e
	}
	return fmt.Errorf("archive halted: %s", message)
}
func (r *Runner) resume(ctx context.Context, now time.Time, st *State, writes bool, blocked string) error {
	j := st.Job
	if j.Phase == NeedsApproval {
		if j.ForceImport {
			return r.forceImport(ctx, now, st, writes, blocked)
		}
		a := r.Arr[j.Target.Kind]
		if a == nil {
			return r.pause(st, pauseError("Library inspection unavailable"))
		}
		current, err := a.Current(ctx, j.Target)
		if err != nil {
			return r.pause(st, pauseError("Library inspection unavailable"))
		}
		if !SameFiles(j.Target, current) || j.HasImportIntent() {
			return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
		}
		st.Paused = "Needs owner approval"
		return nil
	}
	if !j.ForceImport && j.Selection != nil {
		if err := j.Selection.Validate(); err != nil {
			return r.requestImportApproval(ctx, now, st, "persisted selection settings invalid", writes, blocked)
		}
		selection := r.Config.Selection
		r.Config.Selection = *j.Selection
		defer func() { r.Config.Selection = selection }()
	}
	if !j.ForceImport && (j.PolicyVersion != PolicyVersion || j.Mode != j.Target.Mode() && !(j.Target.Mode() == Mode1080p && j.Mode == Mode2160p)) {
		return r.requestImportApproval(ctx, now, st, "persisted selection policy or mode changed", writes, blocked)
	}
	a := r.Arr[j.Target.Kind]
	if a == nil {
		return fmt.Errorf("unknown persisted application")
	}
	if j.Phase == Failed {
		return fmt.Errorf("archive halted: %s", j.Message)
	}
	if j.Phase != GrabIntent && j.Phase != ImportIntent && j.Phase != Downloading && j.Phase != Verifying {
		return fmt.Errorf("unknown persisted archive phase")
	}
	if j.Phase == GrabIntent {
		return r.reconcileGrab(ctx, now, st)
	}
	if j.Phase == ImportIntent {
		return r.fail(ctx, st, "ambiguous import intent; operator reconciliation required")
	}
	if j.Phase == Downloading && j.PlaybackWaitStarted.IsZero() && now.Sub(j.timeoutStart()) > time.Duration(r.Config.TimeoutHours)*time.Hour {
		return r.requestImportApproval(ctx, now, st, "replacement timeout", writes, blocked)
	}
	if e := a.Preflight(ctx); e != nil {
		return r.pause(st, pauseError("pre-import safety unavailable"))
	}
	if j.Phase == Downloading {
		r.downloadProgress(ctx, now, j, a)
	}
	if j.Phase == Verifying {
		if j.CommandID == 0 {
			return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
		}
		status, e := a.Command(ctx, j.CommandID)
		if e != nil {
			return e
		}
		j.Progress = &Progress{Phase: Verifying, CommandStatus: status, UpdatedAt: now}
		if status == "failed" || status == "aborted" || status == "cancelled" {
			return r.fail(ctx, st, "ManualImport command failed; old library file may be gone")
		}
		if status == "completed" {
			// Completion triggers the entire proof immediately, even for a partial import.
			current, e := a.Current(ctx, j.Target)
			if e != nil {
				for _, ep := range j.Target.Episodes {
					j.MissingEpisodeIDs = append(j.MissingEpisodeIDs, ep.ID)
				}
				for _, f := range j.Target.Files {
					j.MissingFileIDs = append(j.MissingFileIDs, f.ID)
				}
				return r.fail(ctx, st, "completed ManualImport replacement inspection unavailable")
			}
			return r.verifyReplacement(ctx, now, st, current, writes, blocked, "completed ManualImport")
		}
		if now.Sub(j.timeoutStart()) > time.Duration(r.Config.TimeoutHours)*time.Hour {
			return r.fail(ctx, st, "replacement not verified within timeout; no further swaps")
		}
		st.Paused = "waiting for ManualImport command completion"
		return nil
	}
	if j.PlaybackWaitStarted.IsZero() && now.Sub(j.timeoutStart()) > time.Duration(r.Config.TimeoutHours)*time.Hour {
		return r.requestImportApproval(ctx, now, st, "replacement timeout", writes, blocked)
	}
	qs, e := a.Queue(ctx)
	if e != nil {
		return e
	}
	if j.DownloadID == "" {
		id, err := r.selectedDownload(ctx, j)
		if err != nil {
			var conflict *EvidenceConflict
			if errors.As(err, &conflict) {
				return r.fail(ctx, st, "Grab evidence conflicts: "+err.Error())
			}
			return r.requestImportApproval(ctx, now, st, "cannot identify the selected download: "+err.Error(), writes, blocked)
		}
		if id == "" {
			for _, q := range qs {
				if q.Matches(j.Target) && !q.extraPackRow(j) && manualImportReady(q) {
					return r.requestImportApproval(ctx, now, st, "Selected download unidentified", writes, blocked)
				}
			}
			return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
		}
		j.DownloadID = id
	}
	var own *QueueItem
	ready := true
	for _, q := range qs {
		if q.extraPackRow(j) || !q.Matches(j.Target) || st.abandoned(j.Target.Kind, q.DownloadID) {
			continue
		}
		if q.DownloadID != j.DownloadID {
			return r.requestImportApproval(ctx, now, st, "another download exists for title", writes, blocked)
		}
		if q.SizeLeft > 0 {
			st.Paused = "waiting for download completion"
			return nil
		}
		ready = ready && manualImportReady(q)
		copy := q
		own = &copy
	}
	current, e := a.Current(ctx, j.Target)
	if e != nil {
		return e
	}
	if j.HasImportIntent() || !SameFiles(j.Target, current) || own == nil || !ready {
		return r.checkDownloadResult(ctx, now, st, &downloadSnapshot{current, qs}, writes, blocked)
	}
	if j.DownloadCompletedAt.IsZero() {
		j.DownloadCompletedAt = now
	}
	if e := r.importAdmission(ctx, now, st); e != nil {
		return r.pause(st, e)
	}
	if !writes {
		st.Paused = "import waits for write admission: " + blocked
		return nil
	}
	items, e := a.Manual(ctx, *own)
	if e != nil {
		return e
	}
	if len(items) == 0 {
		// Arr may finish between our queue read and its manual listing.
		return r.checkDownloadResult(ctx, now, st, nil, writes, blocked)
	}
	if len(ownerImportItems(items, j.Target)) != len(items) {
		return r.requestImportApproval(ctx, now, st, "Unrelated pack files found", writes, blocked)
	}
	if !j.ManuallyApproved {
		videos, err := r.downloadedVideos(*own, items, j.Target)
		if err != nil {
			return r.requestImportApproval(ctx, now, st, "download probe unavailable: "+err.Error(), writes, blocked)
		}
		for _, f := range videos {
			m, err := r.probe(ctx, f)
			if err != nil {
				return r.requestImportApproval(ctx, now, st, "download probe unavailable: "+err.Error(), writes, blocked)
			}
			if why := mediaReject(File{MediaInfo: m}, j.Mode); why != "" {
				proceed, err := r.manualImportAllowed(ctx, now, st, writes, blocked)
				if err != nil || !proceed {
					return err
				}
				return r.requestImportApproval(ctx, now, st, why, writes, blocked)
			}
			for i := range items {
				if items[i].Path == f.Path {
					items[i].MediaInfo = m
				}
			}
		}
	}
	items = targetImportItems(items, j.Target)
	files, e := ValidateImport(items, j.Target, r.Config, j.checkMode())
	if e != nil {
		return r.requestImportApproval(ctx, now, st, e.Error(), writes, blocked)
	}
	// Recheck volatile guards after file inspection, immediately before command submission.
	if e = r.importAdmission(ctx, now, st); e != nil {
		return r.pause(st, e)
	}
	opted, e := a.Opted(ctx, j.Target)
	if e != nil {
		return r.requestImportApproval(ctx, now, st, "Archive tags unavailable", writes, blocked)
	}
	if !opted {
		return r.requestImportApproval(ctx, now, st, "archive tag removed; downloaded replacement retained without import", writes, blocked)
	}
	if e = r.importAdmission(ctx, now, st); e != nil {
		return r.pause(st, e)
	}
	if r.Now != nil {
		if at := r.Now(); at.After(now) {
			now = at
		}
	}
	sum := int64(0)
	for _, it := range items {
		sum += it.Size
	}
	root, baseline, e := a.TargetPreflight(ctx, j.Target, items, sum)
	if e != nil {
		return r.pause(st, pauseError("pre-import safety failed: "+e.Error()))
	}
	if root != j.RecycleRoot || j.RecycleBaseline == nil {
		return r.pause(st, pauseError("recycle configuration or baseline changed before import"))
	}
	mergedBaseline, e := mergeBaseline(root, j.RecycleBaseline, baseline)
	if e != nil {
		return r.pause(st, pauseError("recycle baseline unavailable: "+e.Error()))
	}
	if !j.SidecarsStaged {
		if err := StageSidecars(j, r.Config, r.Store.Dir); err != nil {
			j.SidecarMessage = "subtitles not carried over: " + err.Error()
		}
		j.SidecarsStaged = true
		if err := r.Store.Save(*st); err != nil {
			return err
		}
	}
	if opted, err := a.Opted(ctx, j.Target); err != nil {
		return r.requestImportApproval(ctx, now, st, "Archive tags unavailable", writes, blocked)
	} else if !opted {
		return r.requestImportApproval(ctx, now, st, "archive tag removed before import write", writes, blocked)
	}
	// Filesystem preflight and legacy subtitle staging can also take time.
	// Recheck every queue row and the library immediately before admission.
	proceed, e := r.manualImportAllowed(ctx, now, st, writes, blocked)
	if e != nil || !proceed {
		return e
	}
	if e = r.importAdmission(ctx, now, st); e != nil {
		return r.pause(st, e)
	}
	return r.submitManualImport(ctx, now, st, files, mergedBaseline)
}

func (r *Runner) submitManualImport(ctx context.Context, now time.Time, st *State, files []map[string]any, mergedBaseline []Evidence) error {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	var e error
	// Until ManualImport is actually admitted, automatic-import recovery
	// must use the pre-grab baseline, not entries Arr just recycled.
	j.RecycleBaseline = mergedBaseline
	j.ImportedAt = now
	if e = Transition(j, ImportIntent); e != nil {
		return e
	}
	j.PhaseStarted = now
	if e = r.Store.Save(*st); e != nil {
		return e
	}
	if e = r.Store.Audit("library_replacement_intent", j); e != nil {
		return e
	}
	id, e := a.Import(ctx, files)
	if e != nil || id <= 0 {
		j.Message = r.collectRecovery(ctx, j, "ManualImport response uncertain; old file may be gone")
		return fmt.Errorf("ManualImport response uncertain; operator reconciliation required")
	}
	j.CommandID = id
	if e = Transition(j, Verifying); e != nil {
		return e
	}
	j.PhaseStarted = now
	if e = r.Store.Save(*st); e != nil {
		return e
	}
	return r.Store.Audit("manual_import_accepted", j)
}

func targetKey(t Target) string { return fmt.Sprintf("%s:%d", t.Key(), t.Season) }

var errPaused = errors.New("paused")

type pauseError string

func (e pauseError) Error() string        { return string(e) }
func (e pauseError) Is(target error) bool { return target == errPaused }
func (r *Runner) pause(st *State, e error) error {
	st.Paused = e.Error()
	if errors.Is(e, errPaused) {
		return nil
	}
	return e
}

func (r *Runner) admit(ctx context.Context) error {
	active, e := r.Plex.Active(ctx)
	if e != nil {
		return e
	}
	if active {
		return pauseError("Plex sessions active")
	}
	n, e := r.DState()
	if e != nil {
		return e
	}
	if n >= r.Config.MaxDState {
		return pauseError("host D-state threshold reached")
	}
	return nil
}

func (r *Runner) finalAdmission(ctx context.Context) error {
	if r.OwnerPaused != nil && r.OwnerPaused() {
		return pauseError("Paused by owner")
	}
	if e := r.admit(ctx); e != nil {
		return e
	}
	if r.Now != nil {
		if ok, _ := r.window(r.Now()); !ok {
			return pauseError("nightly window ended during safety preflight")
		}
	}
	return nil
}

// verifyReplacement applies the same proof and completion steps to manual
// commands and automatic imports of the selected download.
func (r *Runner) verifyReplacement(ctx context.Context, now time.Time, st *State, current Target, writes bool, blocked, label string) error {
	j := st.Job
	a := r.Arr[j.Target.Kind]
	if !j.ManuallyApproved {
		for i := range current.Files {
			m, err := r.probe(ctx, current.Files[i])
			if err != nil {
				return r.pause(st, pauseError("replacement probe unavailable: "+err.Error()))
			}
			current.Files[i].MediaInfo = m
		}
	}
	j.MissingEpisodeIDs, j.MissingFileIDs = verificationIssues(j.Target, current, r.Config, j.checkMode())
	if !Verified(j.Target, current, r.Config, j.checkMode()) {
		return r.fail(ctx, st, fmt.Sprintf("%s failed replacement verification; missing/invalid episode IDs %v file IDs %v", label, j.MissingEpisodeIDs, j.MissingFileIDs))
	}
	present, e := a.Present(ctx, current)
	if e != nil || !present {
		for _, ep := range current.Episodes {
			j.MissingEpisodeIDs = append(j.MissingEpisodeIDs, ep.ID)
		}
		for _, f := range current.Files {
			j.MissingFileIDs = append(j.MissingFileIDs, f.ID)
		}
		return r.fail(ctx, st, fmt.Sprintf("%s library file existence/size unproven; file IDs %v", label, j.MissingFileIDs))
	}
	recovery, e := a.Recovery(ctx, j)
	if e != nil {
		return r.fail(ctx, st, "old-file recovery unproven: "+e.Error())
	}
	j.Recovery = recovery
	// Refresh lifecycle evidence after the probes and recycle inspection.
	j.ignoredDownloads = map[string]bool{}
	for _, d := range st.AbandonedDownloads {
		if d.Kind == j.Target.Kind {
			j.ignoredDownloads[d.DownloadID] = true
		}
	}
	if e = a.Lifecycle(ctx, j, current); e != nil {
		var conflict *EvidenceConflict
		if j.CommandID == 0 && !errors.As(e, &conflict) {
			return r.pause(st, pauseError("import history unavailable"))
		}
		return r.fail(ctx, st, "download lifecycle unproven: "+e.Error())
	}
	if len(j.Sidecars) > 0 && !writes && j.SidecarMessage == "" {
		if !r.DryRun && !r.NoSearch {
			// The supervisor closed admission for now; carry them next cycle.
			return r.pause(st, pauseError("replacement verified; subtitle carry-over waits for write admission: "+blocked))
		}
		j.SidecarMessage = "subtitles not carried over: write admission closed"
	}
	if len(j.Sidecars) > 0 && j.SidecarMessage == "" {
		if opted, err := a.Opted(ctx, j.Target); err != nil || !opted {
			j.SidecarMessage = "subtitles not carried over: archive or 4k tag changed or unavailable"
		}
	}
	if j.SidecarMessage == "" {
		if err := CarrySidecars(j, current, r.Config); err != nil {
			j.SidecarMessage = "subtitles not carried over: " + err.Error()
		}
	}
	j.ActualBytes = current.Bytes()
	j.Completed = now
	j.Summary = JobSummary(*j)
	if e = Transition(j, Complete); e != nil {
		return e
	}
	j.PhaseStarted = now
	if st.TagRemovals == nil {
		st.TagRemovals = map[string]Target{}
	}
	st.TagRemovals[j.Target.Key()] = j.Target
	if st.CompletedFiles == nil {
		st.CompletedFiles = map[string]string{}
	}
	st.CompletedFiles[targetKey(current)] = filesKey(current)
	for i, d := range st.Report {
		if targetKey(d.Target) == targetKey(current) {
			st.ProjectedSavings -= d.Savings
			st.Report[i] = Decision{Target: current, Reason: "already archived"}
		}
	}
	if e = r.Store.Save(*st); e != nil {
		return e
	}
	if r.LogSummary != nil {
		r.LogSummary(j.Summary)
	}
	return r.Store.Audit("verified", j)
}
