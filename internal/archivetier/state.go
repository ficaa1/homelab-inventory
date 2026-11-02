package archivetier

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

type Phase string

const (
	GrabIntent    Phase = "grab_intent"
	Downloading   Phase = "downloading"
	NeedsApproval Phase = "needs_approval"
	ImportIntent  Phase = "import_intent"
	Verifying     Phase = "verifying"
	Complete      Phase = "complete"
	Failed        Phase = "failed"
	Abandoned     Phase = "abandoned"
)

type Job struct {
	NonHalting           bool      `json:"nonHalting,omitempty"`
	GrabQueueSeen        bool      `json:"grabQueueSeen,omitempty"`
	GrabChecks           int       `json:"grabChecks,omitempty"`
	GrabCheckedAt        time.Time `json:"grabCheckedAt,omitempty"`
	ignoredDownloads     map[string]bool
	ReleaseID            string        `json:"releaseId,omitempty"`
	PlaybackWaitStarted  time.Time     `json:"playbackWaitStarted,omitempty"`
	PlaybackWaitDuration time.Duration `json:"playbackWaitDuration,omitempty"`
	ForceImport          bool          `json:"forceImport,omitempty"`
	ApprovalReason       string        `json:"approvalReason,omitempty"`
	DownloadCompletedAt  time.Time     `json:"downloadCompletedAt,omitempty"`

	ManuallyApproved  bool       `json:"manuallyApproved,omitempty"`
	Sidecars          []Sidecar  `json:"sidecars,omitempty"`
	SidecarStage      string     `json:"sidecarStage,omitempty"`
	SidecarMessage    string     `json:"sidecarMessage,omitempty"`
	SidecarsCarried   int        `json:"sidecarsCarried"`
	SidecarsStaged    bool       `json:"sidecarsStaged"`
	Mode              string     `json:"mode"`
	PolicyVersion     string     `json:"policyVersion"`
	Selection         *Selection `json:"selection,omitempty"`
	Score             *Score     `json:"score,omitempty"`
	RunnerUp          *Candidate `json:"runnerUp,omitempty"`
	Note              string     `json:"note,omitempty"`
	Summary           string     `json:"summary,omitempty"`
	ImportedAt        time.Time  `json:"importedAt,omitempty"`
	Completed         time.Time  `json:"completed,omitempty"`
	Target            Target     `json:"target"`
	Release           Release    `json:"release"`
	Phase             Phase      `json:"phase"`
	Started           time.Time  `json:"started"`
	PhaseStarted      time.Time  `json:"phaseStarted,omitempty"`
	DownloadID        string     `json:"downloadId,omitempty"`
	CommandID         int        `json:"commandId,omitempty"`
	Message           string     `json:"message,omitempty"`
	ActualBytes       int64      `json:"actualBytes,omitempty"`
	RecycleBaseline   []Evidence `json:"recycleBaseline"`
	RecycleRoot       string     `json:"recycleRoot"`
	Recovery          []Evidence `json:"recovery,omitempty"`
	MissingEpisodeIDs []int      `json:"missingEpisodeIds,omitempty"`
	MissingFileIDs    []int      `json:"missingFileIds,omitempty"`
	// Progress is the job's live position, from GET reads only.
	Progress *Progress `json:"progress,omitempty"`
}

// UnmarshalJSON accepts old snapshots. Fold an observed library change into
// the existing recovery evidence timestamp; it never establishes success.
func (j *Job) UnmarshalJSON(data []byte) error {
	type plain Job
	var saved struct {
		*plain
		LibraryChangedAt time.Time `json:"libraryChangedAt"`
	}
	saved.plain = (*plain)(j)
	if err := json.Unmarshal(data, &saved); err != nil {
		return err
	}
	if j.ImportedAt.IsZero() {
		j.ImportedAt = saved.LibraryChangedAt
	}
	return nil
}

func Transition(j *Job, next Phase) error {
	allowed := map[Phase][]Phase{GrabIntent: {Downloading, Failed, Abandoned}, Downloading: {NeedsApproval, ImportIntent, Verifying, Failed, Abandoned}, NeedsApproval: {ImportIntent, Verifying, Failed, Abandoned}, ImportIntent: {Verifying, Failed}, Verifying: {Complete, Failed}}
	for _, p := range allowed[j.Phase] {
		if next == p {
			j.Phase = next
			return nil
		}
	}
	return fmt.Errorf("invalid transition %s -> %s", j.Phase, next)
}

type State struct {
	LastStreamSeen     time.Time                    `json:"lastStreamSeen,omitempty"`
	StreamQuietSince   time.Time                    `json:"streamQuietSince,omitempty"`
	BadReleases        map[string]map[string]string `json:"badReleases,omitempty"`
	AbandonedDownloads []AbandonedDownload          `json:"abandonedDownloads,omitempty"`
	CleanupError       string                       `json:"cleanupError,omitempty"`

	CompletedFiles map[string]string `json:"completedFiles,omitempty"`
	Approvals      []Approval        `json:"approvals,omitempty"`
	// Seasons limits a tagged series to the seasons chosen in the Library
	// ("sonarr:92" -> [3]); a series without an entry opts in every season.
	Seasons           map[string][]int        `json:"seasons,omitempty"`
	TagRemovals       map[string]Target       `json:"tagRemovals,omitempty"`
	TagRemovalError   string                  `json:"tagRemovalError,omitempty"`
	PlexLibraries     map[string]PlexLibrary  `json:"plexLibraries,omitempty"`
	PlexIdentities    map[string]PlexIdentity `json:"plexIdentities,omitempty"`
	Notifications     []Notification          `json:"notifications,omitempty"`
	NotificationSeen  map[string]string       `json:"notificationSeen,omitempty"`
	NotificationDay   string                  `json:"notificationDay,omitempty"`
	NotificationError string                  `json:"notificationError,omitempty"`
	Checked           time.Time               `json:"checked"`
	Error             string                  `json:"error,omitempty"`
	DryRun            bool                    `json:"dryRun"`
	Night             string                  `json:"night"`
	Attempted         []string                `json:"attempted"`
	FailedJobs        []*Job                  `json:"failedJobs,omitempty"`
	Job               *Job                    `json:"job,omitempty"`
	Reported          time.Time               `json:"reported"`
	Report            []Decision              `json:"report"`
	ProjectedSavings  int64                   `json:"projectedSavings"`
	Paused            string                  `json:"paused,omitempty"`
	// Lookups caches each title's last release lookup (keyed like
	// Attempted). SearchNight/SearchesUsed counts tonight's lookups.
	// SearchTimes persists rolling-minute rate admission before each request.
	// DiscoveryStart rotates the starting title each night. Skipped records
	// tonight's per-title skip reasons.
	Lookups        map[string]Lookup `json:"lookups,omitempty"`
	SearchNight    string            `json:"searchNight,omitempty"`
	SearchTimes    []time.Time       `json:"searchTimes,omitempty"`
	SearchesUsed   int               `json:"searchesUsed"`
	DiscoveryStart int               `json:"discoveryStart,omitempty"`
	Skipped        map[string]string `json:"skipped,omitempty"`
	// History keeps the last HistoryLimit finished jobs with their recovery
	// evidence, newest first, so restore and purge outlive the next job.
	History []HistoryEntry `json:"history,omitempty"`
}

// MigrateFailed frees the active slot while retaining each unresolved title.
func (st *State) MigrateFailed() {
	if j := st.Job; j != nil && j.Phase == Failed && !j.CleanFailure() {
		for _, old := range st.FailedJobs {
			if JobID(*old) == JobID(*j) {
				st.Job = nil
				return
			}
		}
		st.FailedJobs = append(st.FailedJobs, j)
		st.Job = nil
	}
}
func (st *State) UnmarshalJSON(data []byte) error {
	type plain State
	if err := json.Unmarshal(data, (*plain)(st)); err != nil {
		return err
	}
	st.MigrateFailed()
	return nil
}
func (st State) FindJob(id string) *Job {
	if st.Job != nil && JobID(*st.Job) == id {
		return st.Job
	}
	for _, j := range st.FailedJobs {
		if JobID(*j) == id {
			return j
		}
	}
	return nil
}
func (st State) FailedTitle(t Target) bool {
	for _, j := range st.FailedJobs {
		if j.Target.Key() == t.Key() {
			return true
		}
	}
	return false
}

// Lookup is one persisted release lookup. Files fingerprints the library
// files it was made for; a changed library invalidates it.
type Lookup struct {
	Releases []Release `json:"releases,omitempty"`
	At       time.Time `json:"at"`
	Files    string    `json:"files"`
	Decision Decision  `json:"decision"`
}

func filesKey(t Target) string {
	ids := make([]string, 0, len(t.Files))
	for _, f := range t.Files {
		ids = append(ids, fmt.Sprintf("%d:%d", f.ID, f.Size))
	}
	sort.Strings(ids)
	return fmt.Sprintf("%s:%d:%s:%s", t.Mode(), t.Runtime, PolicyVersion, strings.Join(ids, ","))
}

// Store delegates persistence to supervisor hooks when configured. OpenStore
// supplies a standalone process lock, atomic snapshots and fsynced audit intent.
// Ambiguous writes are never retried automatically.
type Store struct {
	Dir         string
	lock        func()
	LoadState   func() (State, error)
	SaveState   func(State) error
	AuditRecord func(string, *Job) error
	// AuditDetailRecord audits an operator action with what it planned or
	// did; nil falls back to the standalone audit log.
	AuditDetailRecord func(string, *Job, any) error
	// Writable reports whether the supervisor still admits writes after a
	// persistence failure; nil means it does.
	Writable func() error
}

// WriteAllowed is the supervisor's persistence latch, if it has one.
func (s *Store) WriteAllowed() error {
	if s.Writable != nil {
		return s.Writable()
	}
	return nil
}

func OpenStore(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	unlock, e := fsx.LockDir(dir)
	if errors.Is(e, fsx.ErrLocked) {
		return nil, fmt.Errorf("another archive worker holds state lock")
	}
	if e != nil {
		return nil, e
	}
	return &Store{Dir: dir, lock: unlock}, nil
}
func (s *Store) Close() {
	if s.lock != nil {
		s.lock()
	}
}
func (s *Store) Load() (State, error) {
	if s.LoadState != nil {
		return s.LoadState()
	}
	return ReadState(s.Dir)
}
func ReadState(dir string) (State, error) {
	st := State{Report: []Decision{}}
	b, e := os.ReadFile(filepath.Join(dir, "state.json"))
	if os.IsNotExist(e) {
		return st, nil
	}
	if e != nil {
		return st, e
	}
	e = json.Unmarshal(b, &st)
	return st, e
}
func (s *Store) Save(st State) error {
	if s.SaveState != nil {
		return s.SaveState(st)
	}
	b, e := json.MarshalIndent(st, "", "  ")
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(filepath.Join(s.Dir, "state.json"), b, 0o600)
}
func (s *Store) Audit(action string, j *Job) error {
	if s.AuditRecord != nil {
		return s.AuditRecord(action, j)
	}
	return s.AuditDetail(action, j, nil)
}

// AuditDetail records an action with a detail document (a plan, a result).
func (s *Store) AuditDetail(action string, j *Job, detail any) error {
	if s.AuditDetailRecord != nil {
		return s.AuditDetailRecord(action, j, detail)
	}
	if s.AuditRecord != nil && detail == nil {
		return s.AuditRecord(action, j)
	}
	record := struct {
		Time   time.Time `json:"time"`
		Action string    `json:"action"`
		Job    *Job      `json:"job"`
		Detail any       `json:"detail,omitempty"`
	}{time.Now().UTC(), action, j, detail}
	b, e := json.Marshal(record)
	if e != nil {
		return e
	}
	return fsx.Append(filepath.Join(s.Dir, "audit.jsonl"), append(b, '\n'), fsx.SyncFile)
}
func sanitized(d Decision) Decision {
	if d.Release != nil {
		r := *d.Release
		r.Guid = ""
		r.Rejections = nil
		d.Release = &r
	}
	return d
}

// Verified requires changed file IDs, complete episode coverage, the job's
// mode (resolution, and audio/HDR from the media probe), caps and savings.
func Verified(old, current Target, c Config, mode string) bool {
	if old.Kind != current.Kind || old.ID != current.ID || old.Season != current.Season || len(current.Files) == 0 || old.Kind == "radarr" && len(current.Files) != 1 {
		return false
	}
	ids := map[int]bool{}
	for _, f := range old.Files {
		ids[f.ID] = true
	}
	newIDs := map[int]bool{}
	for _, f := range current.Files {
		if f.ID <= 0 || ids[f.ID] || f.Size <= 0 || !safePath(f.Path, c.LibraryRoots) || mode != ModeAny && (resolution(f.Quality) != modeResolution(mode) || mediaReject(f, mode) != "" || !sourceOK(f.Quality, ParseTraits(f.Path))) {
			return false
		}
		if newIDs[f.ID] {
			return false
		}
		newIDs[f.ID] = true
		if mode != ModeAny && (f.Size > fileCap(old, current.Episodes, f, c, mode) || unsafeEdition(f.Path+" "+f.Edition)) {
			return false
		}
	}
	if old.Kind == "sonarr" {
		eps := map[int]bool{}
		for _, ep := range current.Episodes {
			if newIDs[ep.FileID] {
				if ep.ID <= 0 || eps[ep.ID] {
					return false
				}
				eps[ep.ID] = true
			}
		}
		for _, ep := range old.Episodes {
			if !eps[ep.ID] {
				return false
			}
		}
	}
	if old.Kind == "sonarr" && len(current.Episodes) != len(old.Episodes) {
		return false
	}
	factor := 0.0
	for _, f := range current.Files {
		factor = math.Max(factor, fileFactor(c, f))
	}
	if mode == ModeAny {
		return current.Bytes() < old.Bytes()
	}
	return current.Bytes() <= capBytes(old, c, mode, factor) && float64(current.Bytes()) < float64(old.Bytes())*c.MaxFraction
}
func SameFiles(a, b Target) bool {
	if len(a.Files) != len(b.Files) {
		return false
	}
	m := map[int]File{}
	for _, f := range a.Files {
		m[f.ID] = f
	}
	for _, f := range b.Files {
		v, ok := m[f.ID]
		if !ok || v.Size != f.Size || v.Path != f.Path {
			return false
		}
	}
	if a.Kind == "sonarr" {
		m := map[int]int{}
		for _, ep := range a.Episodes {
			m[ep.ID] = ep.FileID
		}
		if len(m) != len(b.Episodes) {
			return false
		}
		for _, ep := range b.Episodes {
			if m[ep.ID] != ep.FileID {
				return false
			}
		}
	}
	return true
}

func verificationIssues(old, current Target, c Config, mode string) ([]int, []int) {
	oldIDs := map[int]bool{}
	for _, f := range old.Files {
		oldIDs[f.ID] = true
	}
	valid := map[int]bool{}
	bad := []int{}
	missing := []int{}
	for _, f := range current.Files {
		if f.ID <= 0 || oldIDs[f.ID] || f.Size <= 0 || !safePath(f.Path, c.LibraryRoots) || mode != ModeAny && (resolution(f.Quality) != modeResolution(mode) || mediaReject(f, mode) != "" || !sourceOK(f.Quality, ParseTraits(f.Path)) || f.Size > fileCap(old, current.Episodes, f, c, mode)) {
			bad = append(bad, f.ID)
		} else {
			valid[f.ID] = true
		}
	}
	for _, ep := range old.Episodes {
		found := false
		for _, now := range current.Episodes {
			if now.ID == ep.ID && valid[now.FileID] {
				found = true
			}
		}
		if !found {
			missing = append(missing, ep.ID)
			bad = append(bad, ep.FileID)
		}
	}
	if len(current.Files) == 0 {
		for _, f := range old.Files {
			bad = append(bad, f.ID)
		}
	}
	return missing, bad
}

// SearchesLastMinute includes future timestamps conservatively if the clock
// moves backwards; a restart or clock adjustment cannot refund recent lookups.
func (st State) SearchesLastMinute(now time.Time) int {
	n := 0
	for _, at := range st.SearchTimes {
		if at.After(now.Add(-time.Minute)) {
			n++
		}
	}
	return n
}

// checkMode is the mode import and verification enforce. A manually approved
// job is checked only for structure and savings (ModeAny).
func (j *Job) checkMode() string {
	if j.ManuallyApproved {
		return ModeAny
	}
	return j.Mode
}

// ModeAny skips resolution, source, audio, HDR and size-band checks.
const ModeAny = ""

// opted reports whether the operator's season choice includes the target.
func (st State) opted(t Target) bool {
	list, ok := st.Seasons[t.Key()]
	if t.Kind != "sonarr" || !ok {
		return true
	}
	for _, n := range list {
		if n == t.Season {
			return true
		}
	}
	return false
}

// optedTargets drops seasons the operator did not choose.
func (st State) optedTargets(ts []Target) []Target {
	out := ts[:0:0]
	for _, t := range ts {
		if st.opted(t) {
			out = append(out, t)
		}
	}
	return out
}

// HasRunnableApproval reports a queued approval the runner may start.
func (st State) HasRunnableApproval() bool { return st.runnableApproval() }

func (st State) runnableApproval() bool {
	for _, a := range st.Approvals {
		if !a.Blocked {
			return true
		}
	}
	return false
}

// PublicState omits cached tracker URLs, which can contain credentials.
func PublicState(st State) State {
	if st.Job != nil && !st.Job.HasImportIntent() {
		copy := *st.Job
		copy.Message = preImportMessage(copy.Message)
		st.Job = &copy
		st.Error = preImportMessage(st.Error)
	}

	st.FailedJobs = append([]*Job(nil), st.FailedJobs...)
	for i, j := range st.FailedJobs {
		copy := *j
		if !copy.HasImportIntent() {
			copy.Message = preImportMessage(copy.Message)
		}
		st.FailedJobs[i] = &copy
	}
	st.Error = preImportMessage(st.Error)

	report := st.Report
	if len(report) > 0 {
		report = append([]Decision(nil), report...)
	}
	st.Report = report
	lookups := make(map[string]Lookup, len(st.Lookups))
	for k, l := range st.Lookups {
		l.Releases = nil
		lookups[k] = l
	}
	st.Lookups = lookups
	return st
}

func cachedLookup(t Target, d Decision, rs []Release, now time.Time) Lookup {
	l := Lookup{At: now, Files: filesKey(t), Decision: sanitized(d)}
	// Selection already applied every hard reject; retain only offered identities.
	for _, c := range d.Candidates {
		for _, r := range rs {
			if c.ID == releaseID(r) {
				l.Releases = append(l.Releases, r)
				break
			}
		}
	}
	return l
}

// HasImportIntent also recognizes older jobs written before this recovery flow.
// ImportedAt is persisted before the import audit and before the HTTP write,
// or records a library change pending proof. It does not establish success.
func (j Job) HasImportIntent() bool {
	return !j.ImportedAt.IsZero() || j.CommandID > 0 || j.Phase == ImportIntent || j.Phase == Verifying || j.Phase == Complete
}
func (j Job) HoldsLease() bool {
	return j.Phase != Complete && j.Phase != Abandoned && !j.CleanFailure()
}

func (j Job) CleanFailure() bool { return j.Phase == Failed && j.NonHalting && !j.HasImportIntent() }

// Approval wait time never consumes the new import command's deadline.
func (j Job) timeoutStart() time.Time {
	if j.ForceImport && !j.ImportedAt.IsZero() {
		return j.ImportedAt
	}
	return j.Started.Add(j.PlaybackWaitDuration)
}
