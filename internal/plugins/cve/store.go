package cve

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

// advisoryStore keeps trimmed OSV records in 256 gzip shards keyed by the
// first byte of the ID's SHA-256. A lab matches tens of thousands of
// advisories; one file each would dominate the data directory and its backups.
// Shards load on first use and dirty ones are rewritten atomically on flush.
type advisoryStore struct {
	dir    string
	mu     sync.Mutex
	shards map[string]map[string]advisory
	dirty  map[string]bool
}

const maxShardBytes = 64 << 20

func openAdvisoryStore(dir string) (*advisoryStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("advisory cache unavailable")
	}
	return &advisoryStore{dir: dir, shards: map[string]map[string]advisory{}, dirty: map[string]bool{}}, nil
}

func shardOf(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:1])
}

// loadLocked reads a shard once. A missing or unreadable shard starts empty:
// its advisories are fetched again rather than failing the scan.
func (s *advisoryStore) loadLocked(shard string) map[string]advisory {
	if m, ok := s.shards[shard]; ok {
		return m
	}
	m := map[string]advisory{}
	s.shards[shard] = m
	f, err := os.Open(filepath.Join(s.dir, shard+".json.gz"))
	if err != nil {
		return m
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return m
	}
	var stored map[string]advisory
	if json.NewDecoder(io.LimitReader(zr, maxShardBytes)).Decode(&stored) == nil {
		for id, a := range stored {
			if a.ID == id && shardOf(id) == shard {
				m[id] = a
			}
		}
	}
	return m
}

func (s *advisoryStore) get(id string) (advisory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.loadLocked(shardOf(id))[id]
	return a, ok
}

func (s *advisoryStore) put(a advisory) {
	s.mu.Lock()
	defer s.mu.Unlock()
	shard := shardOf(a.ID)
	s.loadLocked(shard)[a.ID] = a
	s.dirty[shard] = true
}

// prune drops records no current package matched, across every shard on disk.
func (s *advisoryStore) prune(keep map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if shard, ok := strings.CutSuffix(e.Name(), ".json.gz"); ok && len(shard) == 2 {
			s.loadLocked(shard)
		}
	}
	for shard, m := range s.shards {
		for id := range m {
			if _, ok := keep[id]; !ok {
				delete(m, id)
				s.dirty[shard] = true
			}
		}
	}
}

func (s *advisoryStore) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for shard := range s.dirty {
		file := filepath.Join(s.dir, shard+".json.gz")
		if len(s.shards[shard]) == 0 {
			if err := os.Remove(file); err != nil && !os.IsNotExist(err) {
				return errors.New("could not persist advisories")
			}
			delete(s.dirty, shard)
			continue
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := json.NewEncoder(zw).Encode(s.shards[shard]); err != nil {
			return errors.New("could not encode advisories")
		}
		if err := zw.Close(); err != nil {
			return errors.New("could not encode advisories")
		}
		if err := fsx.WriteAtomic(file, buf.Bytes(), 0600); err != nil {
			return errors.New("could not persist advisories")
		}
		delete(s.dirty, shard)
	}
	return nil
}
