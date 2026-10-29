package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Policy ages are measured in UTC days (24 hours); zero DailyDays has no expiry.
// Compression is independent of thinning: KeepAllDays keeps every run.
type Policy struct {
	KeepAllDays       int `json:"keepAllDays"`
	HourlyDays        int `json:"hourlyDays"`
	DailyDays         int `json:"dailyDays"`
	CompressAfterDays int `json:"compressAfterDays"`
}

func DefaultPolicy() Policy { return Policy{7, 30, 365, 2} }
func (p Policy) Validate() error {
	if p.KeepAllDays < 0 || p.HourlyDays < 0 || p.DailyDays < 0 || p.CompressAfterDays < 0 {
		return errors.New("retention days must be non-negative")
	}
	if p.KeepAllDays > p.HourlyDays || (p.DailyDays != 0 && p.HourlyDays > p.DailyDays) {
		return errors.New("keepAllDays must be ≤ hourlyDays ≤ dailyDays (or dailyDays 0 for forever)")
	}
	return nil
}
func older(at, now time.Time, days int) bool { return at.Before(now.UTC().AddDate(0, 0, -days)) }

type HostUsage struct {
	Host       string    `json:"host"`
	Runs       int       `json:"runs"`
	Bytes      int64     `json:"bytes"`
	Oldest     time.Time `json:"oldest"`
	Newest     time.Time `json:"newest"`
	Compressed int       `json:"compressed"`
	Stale      bool      `json:"stale"`
}
type Usage struct {
	TotalBytes int64       `json:"totalBytes"`
	TotalRuns  int         `json:"totalRuns"`
	Hosts      []HostUsage `json:"hosts"`
}

func (s *Store) Usage(now time.Time) (Usage, error) {
	out := Usage{Hosts: []HostUsage{}}
	hosts, err := s.Hosts()
	if err != nil {
		return out, err
	}
	for _, h := range hosts {
		rs, err := s.History(h)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return out, err
		}
		u := HostUsage{Host: h, Runs: len(rs), Oldest: rs[len(rs)-1].CollectedAt, Newest: rs[0].CollectedAt, Stale: older(rs[0].CollectedAt, now, 7)}
		for _, r := range rs {
			u.Bytes += r.SizeBytes
			if r.Compressed {
				u.Compressed++
			}
		}
		out.TotalRuns += u.Runs
		out.TotalBytes += u.Bytes
		out.Hosts = append(out.Hosts, u)
	}
	return out, nil
}

type PruneHost struct {
	Host            string `json:"host"`
	RunsBefore      int    `json:"runsBefore"`
	RunsAfter       int    `json:"runsAfter"`
	BytesBefore     int64  `json:"bytesBefore"`
	BytesAfter      int64  `json:"bytesAfter"`
	FilesDeleted    int    `json:"filesDeleted"`
	FilesCompressed int    `json:"filesCompressed"`
}
type PruneAction struct {
	Host        string `json:"host"`
	ID          string `json:"id"`
	Action      string `json:"action"`
	BytesBefore int64  `json:"bytesBefore"`
	BytesAfter  int64  `json:"bytesAfter"`
}
type PruneResult struct {
	At              time.Time     `json:"at"`
	DryRun          bool          `json:"dryRun"`
	Hosts           []PruneHost   `json:"hosts"`
	Actions         []PruneAction `json:"actions"`
	FilesDeleted    int           `json:"filesDeleted"`
	FilesCompressed int           `json:"filesCompressed"`
	BytesBefore     int64         `json:"bytesBefore"`
	BytesAfter      int64         `json:"bytesAfter"`
	BytesSaved      int64         `json:"bytesSaved"`
}

// Prune is a sequential, cancellable maintenance pass. Plans are never reused
// for a later apply: each pass reads the current files before selecting deletes.
func (s *Store) Prune(p Policy, now time.Time, dryRun bool) (PruneResult, error) {
	return s.PruneContext(context.Background(), p, now, dryRun)
}
func (s *Store) PruneContext(ctx context.Context, p Policy, now time.Time, dryRun bool) (out PruneResult, err error) {
	out = PruneResult{At: now, DryRun: dryRun, Hosts: []PruneHost{}, Actions: []PruneAction{}}
	if err = p.Validate(); err != nil {
		return
	}
	s.maintenance.Lock()
	defer s.maintenance.Unlock()
	hosts, err := s.Hosts()
	if err != nil {
		return out, err
	}
	for _, h := range hosts {
		rs, e := s.History(h)
		if errors.Is(e, ErrNotFound) {
			continue
		}
		if e != nil {
			return out, e
		}
		out.Hosts = append(out.Hosts, PruneHost{Host: h, RunsBefore: len(rs), RunsAfter: len(rs)})
		ph := &out.Hosts[len(out.Hosts)-1]
		for _, r := range rs {
			ph.BytesBefore += r.SizeBytes
		}
		ph.BytesAfter = ph.BytesBefore
		out.BytesBefore += ph.BytesBefore
		out.BytesAfter += ph.BytesBefore
		// Oldest to newest preserves the first nonempty observation and every compose
		// transition, including reversions and disappearance of a file/project.
		keep := make(map[string]bool)
		buckets := make(map[string]bool)
		previous := "null"
		for i := len(rs) - 1; i >= 0; i-- {
			if e := pace(ctx); e != nil {
				return out, e
			}
			r := rs[i]
			inv, e := s.At(h, r.ID)
			if e != nil {
				return out, fmt.Errorf("prune %s/%s: %w", h, r.ID, e)
			}
			signature := composeSignature(inv)
			changed := signature != previous
			bucket := ""
			switch {
			case !older(r.CollectedAt, now, p.KeepAllDays):
				keep[r.ID] = true
			case !older(r.CollectedAt, now, p.HourlyDays):
				bucket = "h:" + r.CollectedAt.UTC().Format("2006-01-02T15")
			case p.DailyDays == 0 || !older(r.CollectedAt, now, p.DailyDays):
				bucket = "d:" + r.CollectedAt.UTC().Format("2006-01-02")
			}
			if changed || i == 0 || (bucket != "" && !buckets[bucket]) {
				keep[r.ID] = true
			}
			if keep[r.ID] {
				previous = signature
				if bucket != "" {
					buckets[bucket] = true
				}
			}
		}
		// No writes until every run in this host has been read successfully.
		for _, r := range rs {
			if e := pace(ctx); e != nil {
				return out, e
			}
			a := PruneAction{Host: h, ID: r.ID, BytesBefore: r.SizeBytes}
			if !keep[r.ID] {
				a.Action = "delete"
				if !dryRun {
					s.mu.Lock()
					e = s.deleteRun(h, r.ID)
					s.bumpRunEpoch(h)
					s.mu.Unlock()
					if e != nil {
						return out, e
					}
				}
				ph.RunsAfter--
				ph.FilesDeleted++
				out.FilesDeleted++
			} else if !r.Compressed && older(r.CollectedAt, now, p.CompressAfterDays) {
				a.Action = "compress"
				s.mu.Lock()
				a.BytesAfter, e = compressRun(filepath.Join(s.dir, h, r.ID+".json"), dryRun, readGzip)
				s.mu.Unlock()
				if e != nil {
					return out, e
				}
				ph.FilesCompressed++
				out.FilesCompressed++
			} else {
				continue
			}
			ph.BytesAfter += a.BytesAfter - a.BytesBefore
			out.BytesAfter += a.BytesAfter - a.BytesBefore
			out.BytesSaved = out.BytesBefore - out.BytesAfter
			out.Actions = append(out.Actions, a)
		}
	}
	return out, nil
}
func pace(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func composeSignature(inv *inventory.Inventory) string {
	// Collector ordering is not a change; include project/path/existence/hash.
	var files []string
	if inv.Docker != nil {
		for _, p := range inv.Docker.ComposeProjects {
			for _, f := range p.ConfigFiles {
				b, _ := json.Marshal([]any{p.Name, f.Path, f.Exists, f.SHA256})
				files = append(files, string(b))
			}
		}
	}
	sort.Strings(files)
	b, _ := json.Marshal(files)
	return string(b)
}
func readGzip(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	return io.ReadAll(z) // Read to EOF checks the gzip checksum.
}
func readRun(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return readGzip(path + ".gz")
	}
	return b, err
}

// compressRun verifies the final renamed file byte-for-byte before removing
// the original. A failed verification leaves the original readable. A crash
// between rename and unlink leaves both files; History deduplicates that pair.
func compressRun(path string, dryRun bool, verify func(string) ([]byte, error)) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	if _, err = z.Write(raw); err != nil {
		return 0, err
	}
	if err = z.Close(); err != nil {
		return 0, err
	}
	if dryRun {
		return int64(buf.Len()), nil
	}
	if err = fsx.WriteAtomic(path+".gz", buf.Bytes(), 0o600); err != nil {
		return 0, err
	}
	checked, err := verify(path + ".gz")
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(raw, checked) {
		return 0, errors.New("gzip verification mismatch; original retained")
	}
	if err = os.Remove(path); err != nil {
		return 0, err
	}
	if err = syncDir(filepath.Dir(path)); err != nil {
		return 0, err
	}
	return int64(buf.Len()), nil
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) deleteRun(host, id string) error {
	for _, ext := range []string{".json", ".json.gz"} {
		if err := os.Remove(filepath.Join(s.dir, host, id+ext)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

var ErrActiveHost = errors.New("host reported within the last 7 days")

func (s *Store) ForgetHost(host string, now time.Time) error {
	host, err := sanitizeHost(host)
	if err != nil {
		return err
	}
	s.maintenance.Lock()
	defer s.maintenance.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.history(host)
	if err != nil {
		return err
	}
	if !older(rs[0].CollectedAt, now, 7) {
		return ErrActiveHost
	}
	// Removing the host also forgets its softstore latest and history. Shared
	// package blobs remain: other hosts and retained histories may reference them.
	err = os.RemoveAll(filepath.Join(s.dir, host))
	s.bumpRunEpoch(host)
	return err
}
