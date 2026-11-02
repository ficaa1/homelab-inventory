// Package mediapolicy supervises coordinated library policies in one process.
package mediapolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

type Lease struct {
	Owner string    `json:"owner"`
	Since time.Time `json:"since"`
}

type ArchiveSearch struct {
	Title    archivetier.ApprovalRequest `json:"title"`
	Name     string                      `json:"name"`
	State    string                      `json:"state"`
	Finished time.Time                   `json:"finished,omitempty"`
}

type State struct {
	ArchivePaused   bool            `json:"archivePaused"`
	ArchiveSearches []ArchiveSearch `json:"archiveSearches,omitempty"`
	// Preserve the original override value across unrelated state writes. Only
	// an explicit settings Save or Reset replaces it.
	SelectionOverridesRaw     json.RawMessage                `json:"-"`
	SelectionOverridesWarning string                         `json:"-"`
	SelectionOverrides        archivetier.SelectionOverrides `json:"selectionOverrides,omitempty"`
	Seeding                   *SeedJob                       `json:"seeding,omitempty"`
	SeedQueue                 []SeedQueueEntry               `json:"seedQueue,omitempty"`
	SeedQueueResults          []SeedQueueResult              `json:"seedQueueResults,omitempty"`
	Version                   int                            `json:"version"`
	Archive                   archivetier.State              `json:"archive"`
	Leases                    map[string]Lease               `json:"leases"`
	Checked                   time.Time                      `json:"checked"`
	Error                     string                         `json:"error,omitempty"`
}

// Decode overrides independently: a malformed optional settings value must
// never discard jobs, leases, or other durable state.
func (s *State) UnmarshalJSON(b []byte) error {
	type plain State
	var doc struct {
		*plain
		Overrides json.RawMessage `json:"selectionOverrides"`
	}
	*s = State{}
	doc.plain = (*plain)(s)
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	s.SelectionOverridesRaw = doc.Overrides
	if len(doc.Overrides) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(doc.Overrides))
	d.DisallowUnknownFields()
	if err := d.Decode(&s.SelectionOverrides); err != nil {
		s.SelectionOverrides = archivetier.SelectionOverrides{}
		s.SelectionOverridesWarning = invalidSelectionWarning
	}
	return nil
}

func (s State) MarshalJSON() ([]byte, error) {
	type plain State
	raw := s.SelectionOverridesRaw
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(s.SelectionOverrides)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(struct {
		plain
		Overrides json.RawMessage `json:"selectionOverrides"`
	}{plain(s), raw})
}

// Store owns the process lock, the single durable document and all title leases.
// A persistence error poisons write admission until the process is restarted.
type Store struct {
	mu     sync.Mutex
	dir    string
	lock   func()
	state  State
	poison error
	refs   map[string]int
}

func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	unlock, err := fsx.LockDir(dir)
	if errors.Is(err, fsx.ErrLocked) {
		return nil, fmt.Errorf("another media policy process holds the state lock")
	}
	if err != nil {
		return nil, err
	}
	s := &Store{dir: dir, lock: unlock, refs: map[string]int{}, state: State{Version: 1, Leases: map[string]Lease{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		var loaded State
		err = json.Unmarshal(b, &loaded)
		if err == nil {
			s.state = loaded
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.Close()
		return nil, fmt.Errorf("cannot load media policy state")
	}
	if s.state.Version != 1 || s.state.Leases == nil {
		s.Close()
		return nil, fmt.Errorf("invalid media policy state")
	}
	jobs := append([]*archivetier.Job{}, s.state.Archive.FailedJobs...)
	if s.state.Archive.Job != nil {
		jobs = append(jobs, s.state.Archive.Job)
	}
	for _, j := range jobs {
		if j.Target.ID <= 0 || (j.Target.Kind != "sonarr" && j.Target.Kind != "radarr") {
			s.Close()
			return nil, fmt.Errorf("invalid persisted archive title")
		}
		switch j.Phase {
		case archivetier.GrabIntent, archivetier.NeedsApproval, archivetier.Downloading, archivetier.ImportIntent, archivetier.Verifying, archivetier.Failed, archivetier.Complete, archivetier.Abandoned:
		default:
			s.Close()
			return nil, fmt.Errorf("invalid persisted archive phase")
		}
	}
	// Transient leases end with the process; write intents remain in each policy
	// and must reconcile before its next request. An archive job retains exclusion.
	s.state.Leases = map[string]Lease{}
	if s.state.Seeding.holds() {
		s.state.Seeding.Phase = "halted"
		s.state.Seeding.Error = "Stopped. Check status."
	}
	// A lookup interrupted after its persisted start is never replayed.
	for i := range s.state.ArchiveSearches {
		row := &s.state.ArchiveSearches[i]
		if row.State == "searching" {
			row.State = "failed"
			row.Finished = time.Now().UTC()
		}
	}
	s.retainPolicies()
	return s, nil
}

func (s *Store) Close() {
	if s.lock != nil {
		s.lock()
	}
}

func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; json.Unmarshal(b, &out); return out }

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := clone(s.state)
	return out
}
func (s *Store) WriteAllowed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return fmt.Errorf("state persistence failed; writes disabled until restart")
	}
	return nil
}

func (s *Store) retainPolicies() {
	if s.state.Seeding.holds() {
		for _, key := range s.state.Seeding.Plan.Titles {
			s.state.Leases[key] = Lease{Owner: "seeding", Since: s.state.Seeding.Started}
		}
	}
	jobs := append([]*archivetier.Job{}, s.state.Archive.FailedJobs...)
	if s.state.Archive.Job != nil {
		jobs = append(jobs, s.state.Archive.Job)
	}
	for _, j := range jobs {
		if j.HoldsLease() {
			s.state.Leases[j.Target.Key()] = Lease{Owner: "archive", Since: j.Started}
		}
	}
}

func (s *Store) saveLocked() error {
	if s.poison != nil {
		return s.poison
	}
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err == nil {
		err = atomicWrite(s.dir, "state.json", b)
	}
	if err != nil {
		s.poison = fmt.Errorf("media policy state persistence failed")
	}
	return err
}

func atomicWrite(dir, name string, b []byte) error {
	return fsx.WriteAtomic(filepath.Join(dir, name), append(b, '\n'), 0o600)
}

func (s *Store) LoadArchive() (archivetier.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return clone(s.state.Archive), s.poison
}
func (s *Store) SaveArchive(st archivetier.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := clone(s.state)
	// A runner may have been reading or probing while the owner reordered
	// waiting approvals. Preserve the current order for surviving entries.
	st.Approvals = keepQueueOrder(st.Approvals, s.state.Archive.Approvals, func(a archivetier.Approval) string { return titleQueueKey(a.ApprovalRequest) })
	s.state.Archive = clone(st)
	for key, l := range s.state.Leases {
		if l.Owner == "archive" && s.refs[key] == 0 {
			delete(s.state.Leases, key)
		}
	}
	s.retainPolicies()
	err := s.saveLocked()
	if err != nil {
		s.state = previous
	}
	return err
}
func (s *Store) Finish(now time.Time, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Checked = now
	s.state.Error = ""
	if err != nil {
		s.state.Error = err.Error()
	}
	return s.saveLocked()
}

func (s *Store) Blocked(key, owner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.state.Leases[key]
	return ok && l.Owner != owner
}

// leaseBusyLocked words a refused lease. Only work running now gets retry advice;
// a retained lease (halted move, failed import) needs inspection.
func (s *Store) leaseBusyLocked(key string, l Lease) error {
	if s.refs[key] > 0 || s.ownerRunningLocked(l.Owner) {
		return fmt.Errorf("Busy, try again shortly")
	}
	return fmt.Errorf("Needs attention")
}

func (s *Store) ownerRunningLocked(owner string) bool {
	switch owner {
	case "seeding":
		return s.state.Seeding != nil && seedPhaseActive(s.state.Seeding.Phase)
	case "archive":
		return s.state.Archive.Job != nil && s.state.Archive.Job.Phase == archivetier.Downloading
	}
	return false
}

func (s *Store) Acquire(key, owner string) (func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.state.Leases[key]; ok && l.Owner != owner {
		return nil, s.leaseBusyLocked(key, l)
	}
	if _, ok := s.state.Leases[key]; !ok {
		s.state.Leases[key] = Lease{Owner: owner, Since: time.Now().UTC()}
	}
	s.refs[key]++
	if err := s.saveLocked(); err != nil {
		s.refs[key]--
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.refs[key]--
			if s.refs[key] == 0 {
				delete(s.state.Leases, key)
				s.retainPolicies()
			}
			s.saveLocked()
		})
	}, nil
}

func (s *Store) Audit(action string, j *archivetier.Job) error { return s.AuditDetail(action, j, nil) }

// AuditDetail appends an action with a detail document (an operator's plan
// or its outcome) and fsyncs it before returning.
func (s *Store) AuditDetail(action string, j *archivetier.Job, detail any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poison != nil {
		return s.poison
	}
	b, err := json.Marshal(struct {
		At     time.Time        `json:"at"`
		Action string           `json:"action"`
		Job    *archivetier.Job `json:"job"`
		Detail any              `json:"detail,omitempty"`
	}{time.Now().UTC(), action, j, detail})
	if err == nil {
		err = fsx.Append(filepath.Join(s.dir, "audit.jsonl"), append(b, '\n'), fsx.SyncFileDir)
	}
	if err != nil {
		s.poison = fmt.Errorf("media policy audit persistence failed")
	}
	return err
}

// ArchiveClearAllowed excludes live leases. The
// failed archive job's own durable lease is released by the clear itself.
func (s *Store) ArchiveClearAllowed(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs[key] > 0 {
		return fmt.Errorf("title has an active lease")
	}
	if l, ok := s.state.Leases[key]; ok && l.Owner != "archive" {
		return s.leaseBusyLocked(key, l)
	}
	return nil
}
