// Package softstore keeps content-addressed package lists and change-only
// software history outside the frequently collected inventory runs.
package softstore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"github.com/ficaa1/homelab-inventory/internal/packages"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

// Store serializes blob creation and snapshot updates, including across hosts.
// Use one instance per data directory. Run retention never deletes its blobs.
type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) *Store { return &Store{dir: dir} }

// Set retains collection metadata but replaces the package list with its hash.
// The outer Packages field hides the embedded list from snapshot JSON.
type Set struct {
	inventory.PackageSet
	Packages    []inventory.Package `json:"packages,omitempty"`
	PackagesRef string              `json:"packages_ref"`
}
type Image struct {
	inventory.ImagePackages
	PackageSet Set `json:"package_set"`
}
type Software struct {
	inventory.Software
	Host   Set     `json:"host"`
	Images []Image `json:"images"`
}
type Snapshot struct {
	CollectedAt   time.Time `json:"collected_at"`
	AgentRevision int       `json:"agent_revision"`
	ConfirmedAt   time.Time `json:"confirmed_at"`
	Software      Software  `json:"software"`
}

func (s *Store) hostDir(host string) (string, error) {
	if err := store.ValidateHost(host); err != nil {
		return "", fmt.Errorf("softstore: invalid hostname: %w", err)
	}

	return filepath.Join(s.dir, host, "software"), nil
}

// Ingest saves software before the API strips it from the stored run. Identical
// content refreshes latest's timestamps without appending history or blobs.
func (s *Store) Ingest(inv *inventory.Inventory) error {
	if inv.Software == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.hostDir(inv.Host.Hostname)
	if err != nil {
		return err
	}
	// Clone before sorting or removing lists: callers still own their inventory.
	b, err := json.Marshal(inv.Software)
	if err != nil {
		return err
	}
	var sw inventory.Software
	if err = json.Unmarshal(b, &sw); err != nil {
		return err
	}
	sortJSON(sw.Runtimes)
	sort.Slice(sw.Images, func(i, j int) bool { return sw.Images[i].ImageID < sw.Images[j].ImageID })
	snap := Snapshot{CollectedAt: sw.CollectedAt, AgentRevision: inv.Tool.Revision, ConfirmedAt: time.Now().UTC(), Software: Software{Software: sw, Images: []Image{}}}
	if snap.CollectedAt.IsZero() {
		snap.CollectedAt = inv.CollectedAt
	}
	snap.Software.Host, err = s.putSet(sw.Host)
	if err != nil {
		return err
	}
	snap.Software.Software.Host = inventory.PackageSet{}
	snap.Software.Software.Images = nil
	for _, im := range sw.Images {
		sort.Strings(im.Containers)
		sort.Strings(im.RepoDigests)
		sortJSON(im.Runtimes)
		set, err := s.putSet(im.PackageSet)
		if err != nil {
			return err
		}
		im.PackageSet = inventory.PackageSet{}
		snap.Software.Images = append(snap.Software.Images, Image{ImagePackages: im, PackageSet: set})
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	// History is authoritative and committed first. Retrying after latest's
	// atomic replacement fails must not append the same transition again.
	tail, err := s.history(dir, 1)
	if err != nil {
		return err
	}
	if len(tail) == 0 || !same(tail[0], snap) {
		line, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if err = fsx.Append(filepath.Join(dir, "history.jsonl"), append(line, '\n'), fsx.SyncFileDir); err != nil {
			return err
		}
	}
	b, err = json.Marshal(snap)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(dir, "latest.json"), append(b, '\n'), 0600)
}

func sortJSON[T any](items []T) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := json.Marshal(items[i])
		b, _ := json.Marshal(items[j])
		return bytes.Compare(a, b) < 0
	})
}
func same(a, b Snapshot) bool {
	normalize := func(v Snapshot) []byte {
		v.CollectedAt = time.Time{}
		v.ConfirmedAt = time.Time{}
		v.AgentRevision = 0
		v.Software.CollectedAt = time.Time{}
		v.Software.DurationMS = 0
		v.Software.Host.CollectedAt = time.Time{}
		v.Software.Host.DurationMS = 0
		v.Software.Images = append([]Image{}, v.Software.Images...)
		for i := range v.Software.Images {
			v.Software.Images[i].PackageSet.CollectedAt = time.Time{}
			v.Software.Images[i].PackageSet.DurationMS = 0
		}
		out, _ := json.Marshal(v)
		return out
	}
	return bytes.Equal(normalize(a), normalize(b))
}
func (s *Store) putSet(set inventory.PackageSet) (Set, error) {
	ps := append([]inventory.Package{}, set.Packages...)
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		return a.SourceVersion < b.SourceVersion
	})
	b, err := json.Marshal(ps)
	if err != nil {
		return Set{}, err
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(b))
	dir := filepath.Join(s.dir, "packagesets")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Set{}, err
	}
	path := filepath.Join(dir, hash+".json.gz")
	if _, err = os.Stat(path); os.IsNotExist(err) {
		var buf bytes.Buffer
		z := gzip.NewWriter(&buf)
		if _, err = z.Write(b); err != nil {
			return Set{}, err
		}
		if err = z.Close(); err != nil {
			return Set{}, err
		}
		if err = fsx.WriteAtomic(path, buf.Bytes(), 0600); err != nil {
			return Set{}, err
		}
	} else if err != nil {
		return Set{}, err
	}
	set.Packages = nil
	set.Count = len(ps)
	return Set{PackageSet: set, PackagesRef: hash}, nil
}
func (s *Store) loadSet(set Set) (inventory.PackageSet, error) {
	raw, err := hex.DecodeString(set.PackagesRef)
	if err != nil || len(raw) != sha256.Size {
		return inventory.PackageSet{}, fmt.Errorf("softstore: invalid package reference")
	}
	f, err := os.Open(filepath.Join(s.dir, "packagesets", set.PackagesRef+".json.gz"))
	if err != nil {
		return inventory.PackageSet{}, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return inventory.PackageSet{}, err
	}
	defer z.Close()
	b, err := io.ReadAll(io.LimitReader(z, (64<<20)+1))
	if err != nil {
		return inventory.PackageSet{}, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(b)) != set.PackagesRef {
		return inventory.PackageSet{}, fmt.Errorf("softstore: package blob hash mismatch")
	}
	out := set.PackageSet
	if err = json.Unmarshal(b, &out.Packages); err != nil {
		return inventory.PackageSet{}, err
	}
	return out, nil
}
func (s *Store) latest(dir string) (*Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(dir, "latest.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	err = json.Unmarshal(b, &snap)
	return &snap, err
}

// history returns the last keep snapshots in arrival order, retaining only
// that many lines in memory. Package lists are never present in these lines.
func (s *Store) history(dir string, keep int) ([]Snapshot, error) {
	f, err := os.Open(filepath.Join(dir, "history.jsonl"))
	if os.IsNotExist(err) {
		return []Snapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	rows := make([]Snapshot, keep)
	n := 0
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 16<<20)
	for scan.Scan() {
		var v Snapshot
		if err = json.Unmarshal(scan.Bytes(), &v); err != nil {
			return nil, err
		}
		rows[n%keep] = v
		n++
	}
	if err = scan.Err(); err != nil {
		return nil, err
	}
	if n < keep {
		return rows[:n], nil
	}
	return append(rows[n%keep:], rows[:n%keep]...), nil
}

func (s *Store) inventory(host string) (*inventory.Inventory, time.Time, error) {
	dir, err := s.hostDir(host)
	if err != nil {
		return nil, time.Time{}, err
	}
	inv := &inventory.Inventory{Host: inventory.Host{Hostname: host}}
	snap, err := s.latest(dir)
	if err != nil || snap == nil {
		return inv, time.Time{}, err
	}
	sw := snap.Software.Software
	sw.Host, err = s.loadSet(snap.Software.Host)
	if err != nil {
		return nil, time.Time{}, err
	}
	sw.Images = []inventory.ImagePackages{}
	for _, image := range snap.Software.Images {
		im := image.ImagePackages
		im.PackageSet, err = s.loadSet(image.PackageSet)
		if err != nil {
			return nil, time.Time{}, err
		}
		sw.Images = append(sw.Images, im)
	}
	inv.Software = &sw
	inv.CollectedAt = snap.CollectedAt
	inv.Tool.Revision = snap.AgentRevision
	return inv, snap.ConfirmedAt, nil
}

// Host returns the existing paginated response from the latest software,
// independently of whether the latest 15-minute run collected packages.
func (s *Store) Host(host string, q packages.Query) (packages.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, confirmed, err := s.inventory(host)
	if err != nil {
		return packages.Response{}, err
	}
	out := packages.Host(inv, q)
	out.ConfirmedAt = confirmed
	return out, nil
}

// Search includes hosts with no software snapshot as explicit null coverage.
func (s *Store) Search(hosts []string, name string) (packages.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	invs := []*inventory.Inventory{}
	confirmed := map[string]time.Time{}
	for _, host := range hosts {
		inv, at, err := s.inventory(host)
		if err != nil {
			return packages.SearchResult{}, err
		}
		invs = append(invs, inv)
		confirmed[host] = at
	}
	out := packages.Search(invs, name)
	for i := range out.Matches {
		out.Matches[i].ConfirmedAt = confirmed[out.Matches[i].Host]
	}
	for i := range out.Coverage {
		out.Coverage[i].ConfirmedAt = confirmed[out.Coverage[i].Host]
	}
	return out, nil
}
