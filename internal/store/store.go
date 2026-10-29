// Package store persists inventories to disk, one file per host per
// collection run, so that a run's history is just "the files that
// accumulated" rather than a schema to maintain.
//
// This exists so cmd/api does not have to invent a storage format: the API
// only knows how to decode HTTP requests into an inventory.Inventory and hand
// it to a Store. A future increment could swap the backend (a database, once
// history grows large enough that directory listings are the bottleneck)
// without touching the HTTP layer, because internal/portmap and everything
// else downstream only ever sees []*inventory.Inventory.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// ErrNotFound is returned when a host or a specific run has no data.
var ErrNotFound = errors.New("store: not found")

// ErrInvalidID is returned when a run ID does not have the shape Save
// produces — a caller error (e.g. a malformed query parameter), not a lookup
// miss, so handlers can tell the two apart.
var ErrInvalidID = errors.New("store: invalid run id")

// timeFormat is used both to name files and to parse them back. It sorts
// lexically in the same order as chronologically, which is what lets Latest
// and History avoid keeping a separate index.
const timeFormat = "20060102T150405.000000000Z"

// Record describes one stored run without loading its body.
type Record struct {
	Host string `json:"host"`
	// ID is opaque and safe to echo back in a lookup (see At): callers should
	// not parse it. It exists because CollectedAt carries nanosecond
	// precision that round-trips awkwardly through query parameters and JSON
	// clients; passing the ID back avoids re-deriving that exact instant.
	ID          string    `json:"id"`
	CollectedAt time.Time `json:"collected_at"`
	SizeBytes   int64     `json:"size_bytes"`
	Compressed  bool      `json:"compressed"`
}

// Store is a directory of per-host subdirectories, each holding one JSON file
// per collection run.
type Store struct {
	dir         string
	mu          sync.RWMutex
	maintenance sync.Mutex
	runEpoch    uint64
}

// Dir is the directory the store is rooted at.
func (s *Store) Dir() string { return s.dir }

// Open returns a Store rooted at dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", dir, err)
	}
	return &Store{dir: dir}, nil
}

// Save writes inv under its host directory, named for its collection time.
// The write is atomic (temp file, then rename) so a reader never observes a
// partial document, matching cmd/discover's own output convention.
// Stored runs always omit Software; API ingest saves it separately in softstore
// before calling Save, so run retention never duplicates package lists.
func (s *Store) Save(inv *inventory.Inventory) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	host, err := sanitizeHost(inv.Host.Hostname)
	if err != nil {
		return Record{}, err
	}
	at := inv.CollectedAt
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()

	hostDir := filepath.Join(s.dir, host)
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return Record{}, fmt.Errorf("store: creating %s: %w", hostDir, err)
	}

	stored := *inv
	stored.Software = nil
	data, err := json.MarshalIndent(&stored, "", "  ")
	if err != nil {
		return Record{}, fmt.Errorf("store: encoding inventory: %w", err)
	}

	id := at.Format(timeFormat)
	path := filepath.Join(hostDir, id+".json")

	// A run that lands in the same nanosecond as a previous one (unlikely,
	// but a misconfigured poster could retry fast) must not clobber it: fail
	// loudly rather than silently discarding a run.
	if _, err := os.Stat(path + ".gz"); err == nil {
		return Record{}, fmt.Errorf("store: %s already has a compressed run at %s", host, at.Format(time.RFC3339Nano))
	}
	if _, err := os.Stat(path); err == nil {
		return Record{}, fmt.Errorf("store: %s already has a run at %s", host, at.Format(time.RFC3339Nano))
	}
	if err := fsx.WriteAtomic(path, append(data, '\n'), 0o600); err != nil {
		return Record{}, fmt.Errorf("store: writing %s: %w", path, err)
	}

	return Record{Host: host, ID: id, CollectedAt: at, SizeBytes: int64(len(data) + 1)}, nil
}

// Hosts lists every host that has at least one stored run, sorted.
func (s *Store) Hosts() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("store: listing %s: %w", s.dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && !reservedHost(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// History lists every stored run for host, newest first.
func (s *Store) History(host string) ([]Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history(host)
}

func (s *Store) history(host string) ([]Record, error) {
	host, err := sanitizeHost(host)
	if err != nil {
		return nil, err
	}
	hostDir := filepath.Join(s.dir, host)
	entries, err := os.ReadDir(hostDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: listing %s: %w", hostDir, err)
	}

	var out []Record
	records := map[string]Record{}
	for _, e := range entries {
		compressed := strings.HasSuffix(e.Name(), ".json.gz")
		if e.IsDir() || (!compressed && !strings.HasSuffix(e.Name(), ".json")) {
			continue
		}
		id := strings.TrimSuffix(strings.TrimSuffix(e.Name(), ".gz"), ".json")
		at, err := time.Parse(timeFormat, id)
		if err != nil {
			continue // not one of ours (e.g. a leftover temp file)
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		r, exists := records[id]
		if !exists {
			r = Record{Host: host, ID: id, CollectedAt: at, Compressed: compressed}
		} else {
			r.Compressed = r.Compressed && compressed
		}
		r.SizeBytes += info.Size()
		records[id] = r
	}
	for _, r := range records {
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CollectedAt.After(out[b].CollectedAt) })
	if out == nil {
		return nil, ErrNotFound
	}
	return out, nil
}

// Latest returns the most recently collected inventory for host.
func (s *Store) Latest(host string) (*inventory.Inventory, error) {
	records, err := s.History(host)
	if err != nil {
		return nil, err
	}
	return s.At(host, records[0].ID)
}

// At returns the inventory stored for host under id (a Record.ID from
// History or Save). id is validated against the filename shape Save
// produces before touching the filesystem, both to reject typos clearly and
// because it otherwise arrives as a raw, untrusted path component.
func (s *Store) At(host, id string) (*inventory.Inventory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.at(host, id)
}

func (s *Store) at(host, id string) (*inventory.Inventory, error) {
	host, err := sanitizeHost(host)
	if err != nil {
		return nil, err
	}
	if _, err := time.Parse(timeFormat, id); err != nil {
		return nil, ErrInvalidID
	}
	path := filepath.Join(s.dir, host, id+".json")
	data, err := readRun(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: reading %s: %w", path, err)
	}
	var inv inventory.Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("store: parsing %s: %w", path, err)
	}
	return &inv, nil
}

// LatestAll returns the most recent inventory for every host with at least
// one stored run. It is the input portmap.Build wants: unlike LoadDir it
// reads from the store's per-host history rather than a flat directory of
// one file per host.
func (s *Store) LatestAll() ([]*inventory.Inventory, error) {
	hosts, err := s.Hosts()
	if err != nil {
		return nil, err
	}
	out := make([]*inventory.Inventory, 0, len(hosts))
	for _, h := range hosts {
		inv, err := s.Latest(h)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		out = append(out, inv)
	}
	return out, nil
}

// ReadState decodes auxiliary JSON state saved by WriteState into v. A name
// that has never been written is not an error: v is left untouched and the
// caller sees its zero value, which is what "nothing has happened yet" should
// look like.
//
// This exists so that things which need to remember something between runs —
// notification bookkeeping, so far — do not each invent their own file layout
// next to the inventories. Store stays the only thing that knows what is on
// disk here.
func (s *Store) ReadState(name string, v any) error {
	path, err := s.statePath(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("store: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("store: parsing %s: %w", path, err)
	}
	return nil
}

// WriteState saves v as JSON under name, atomically and at mode 0600, the same
// way inventories are written.
func (s *Store) WriteState(name string, v any) error {
	path, err := s.statePath(name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("store: encoding %s state: %w", name, err)
	}

	// Unlike a run, state is meant to be replaced, so this write overwrites.
	if err := fsx.WriteAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("store: writing %s: %w", path, err)
	}
	return nil
}

// statePath places state as a file directly in the store root rather than in a
// subdirectory. That is deliberate: Hosts() lists directories, so a state file
// can never be mistaken for a host, and no caller has to filter it out.
func (s *Store) statePath(name string) (string, error) {
	if name == "" {
		return "", errors.New("store: empty state name")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
		default:
			return "", fmt.Errorf("store: state name %q contains character %q that is not allowed", name, r)
		}
	}
	return filepath.Join(s.dir, "state-"+name+".json"), nil
}

// sanitizeHost restricts hostnames to a safe filesystem-path charset. The
// value arrives over HTTP from whatever posted the inventory, so it must
// never be trusted as a raw path component — "..", "/", and friends would
// otherwise let a request escape the store's directory.
func sanitizeHost(host string) (string, error) {
	if host == "" {
		return "", errors.New("store: empty hostname")
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return "", fmt.Errorf("store: hostname %q contains character %q that is not allowed", host, r)
		}
	}
	if host == "." || host == ".." || reservedHost(host) {
		return "", fmt.Errorf("store: hostname %q is not allowed", host)
	}
	return host, nil
}

// RunEpoch changes when inventory history loses runs. Save refuses overwrites;
// compression preserves content. Removal allows an old ID to be reused later.
func (s *Store) RunEpoch(_ string) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.runEpoch
}

func (s *Store) bumpRunEpoch(_ string) {
	s.runEpoch++
}

// Reserve every auxiliary root name, including state's generated namespace and
// temporary files. Apply before either run or software ingest creates a directory.
func reservedHost(host string) bool {
	switch host {
	case "agent-offers.jsonl", "actions-audit.jsonl", "settings", "packagesets", "plugins":
		return true
	}
	return strings.HasPrefix(host, "state-") || strings.HasPrefix(host, ".")
}

// ValidateHost shares the root namespace guard with software storage.
func ValidateHost(host string) error { _, err := sanitizeHost(host); return err }
