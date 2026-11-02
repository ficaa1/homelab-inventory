package archivetier

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Restorer is the operator's restore of an archive job: it moves each
// verified recycle original back to its recorded library path, optionally
// first moving replacement files aside to a quarantine directory, and then
// asks the application to rescan the title.
//
// It runs with the service stopped (the caller holds the state lock) and with
// a writable view of the application paths (Mounts), never the service's
// read-only inspection mounts. Every move is a hard link followed by removing
// the source, on one filesystem: an existing destination is never
// overwritten, and nothing is ever copied or deleted outright. Without Apply
// it only checks and reports; the rescan is an HTTP write and goes through
// Client, which refuses it unless the client was built with apply.
type Restorer struct {
	Mounts         []MountMapping
	LibraryRoots   []string
	Quarantine     []string
	QuarantineRoot string
	Apply          bool
	Rescan         bool
	Client         Rescanner
}

// Rescanner sends the one rescan command a restore ends with; *Client is one.
type Rescanner interface {
	Rescan(context.Context, Target) (int, error)
}

// Move is one checked or performed link-and-unlink.
type Move struct {
	FileID    int    `json:"fileId,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Size      int64  `json:"size"`
	Mode      string `json:"mode"`
	UID       uint32 `json:"uid"`
	GID       uint32 `json:"gid"`
	Inode     uint64 `json:"inode"`
	Done      bool   `json:"done"`
	fromLocal string
	toLocal   string
}

type RestoreResult struct {
	Applied    bool   `json:"applied"`
	Quarantine []Move `json:"quarantine"`
	Restore    []Move `json:"restore"`
	Rescan     string `json:"rescan"`
}

func (r *Restorer) local(p string) (string, error) { return writableLocal(r.Mounts, p) }

// writableLocal maps an app path through exactly one writable mapping.
func writableLocal(mounts []MountMapping, p string) (string, error) {
	var found *MountMapping
	for i := range mounts {
		m := &mounts[i]
		if m.AppRoot == "/" || path.Clean(m.AppRoot) != m.AppRoot || !strings.HasPrefix(m.AppRoot, "/") || filepath.Clean(m.LocalRoot) != m.LocalRoot || !filepath.IsAbs(m.LocalRoot) || m.LocalRoot == "/" {
			return "", fmt.Errorf("invalid restore mapping")
		}
		if under(p, m.AppRoot) {
			if found != nil {
				return "", fmt.Errorf("ambiguous restore mapping for %s", p)
			}
			found = m
		}
	}
	if found == nil || path.Clean(p) != p || !strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("no restore mapping for %s", p)
	}
	return filepath.Join(found.LocalRoot, strings.TrimPrefix(p, found.AppRoot)), nil
}

// statReal refuses symlinks anywhere in the path and returns the raw stat.
func statReal(p string) (os.FileInfo, *syscall.Stat_t, error) {
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return nil, nil, err
	}
	if real != p {
		return nil, nil, fmt.Errorf("%s is or passes through a symlink", p)
	}
	st, err := os.Lstat(p)
	if err != nil {
		return nil, nil, err
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, nil, fmt.Errorf("no device identity for %s", p)
	}
	return st, raw, nil
}

// check validates one move without changing anything: a real regular source
// of the expected size, an absent destination whose parent is a real
// directory on the source's filesystem, and a file the destination
// directory's owner can read.
func (r *Restorer) check(m *Move, wantSize int64, destDirMayBeMissing, destQuarantined bool) error {
	var err error
	if m.fromLocal, err = r.local(m.From); err != nil {
		return err
	}
	if m.toLocal, err = r.local(m.To); err != nil {
		return err
	}
	st, raw, err := statReal(m.fromLocal)
	if err != nil {
		return fmt.Errorf("source %s: %v", m.From, err)
	}
	if !st.Mode().IsRegular() || (wantSize > 0 && st.Size() != wantSize) {
		return fmt.Errorf("source %s is not a regular file of %d bytes", m.From, wantSize)
	}
	m.Size, m.Mode, m.UID, m.GID, m.Inode = st.Size(), st.Mode().Perm().String(), raw.Uid, raw.Gid, raw.Ino
	if _, err := os.Lstat(m.toLocal); !errors.Is(err, fs.ErrNotExist) && !destQuarantined {
		return fmt.Errorf("destination %s already exists; quarantine it first", m.To)
	}
	dir := filepath.Dir(m.toLocal)
	dst, draw, err := statReal(dir)
	if errors.Is(err, fs.ErrNotExist) && destDirMayBeMissing {
		// Created on apply next to its parent, which must then exist.
		dst, draw, err = statReal(filepath.Dir(dir))
	}
	if err != nil || !dst.IsDir() {
		return fmt.Errorf("destination directory for %s is missing or not a real directory", m.To)
	}
	if draw.Dev != raw.Dev {
		return fmt.Errorf("%s and %s are on different filesystems; a restore never copies", m.From, m.To)
	}
	perm := uint32(st.Mode().Perm())
	readable := perm&0o004 != 0 || (raw.Uid == draw.Uid && perm&0o400 != 0) || (raw.Gid == draw.Gid && perm&0o040 != 0)
	if !readable {
		return fmt.Errorf("%s (mode %s, uid %d) would not be readable by the library owner uid %d", m.From, m.Mode, raw.Uid, draw.Uid)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// move links the source at the destination (failing if it exists), proves
// both names are the same inode, then removes the source name.
func move(m *Move) error {
	if err := os.Link(m.fromLocal, m.toLocal); err != nil {
		return fmt.Errorf("link %s -> %s: %v", m.From, m.To, err)
	}
	a, errA := os.Lstat(m.fromLocal)
	b, errB := os.Lstat(m.toLocal)
	if errA != nil || errB != nil || !os.SameFile(a, b) {
		return fmt.Errorf("link %s -> %s did not produce the same file; source kept", m.From, m.To)
	}
	if err := os.Remove(m.fromLocal); err != nil {
		return fmt.Errorf("remove %s after linking: %v", m.From, err)
	}
	m.Done = true
	if err := syncDir(filepath.Dir(m.toLocal)); err != nil {
		return err
	}
	return syncDir(filepath.Dir(m.fromLocal))
}

func (r *Restorer) inLibrary(p string) bool { return safePath(p, r.LibraryRoots) }

// Restore checks every move first and changes nothing unless all pass. With
// Apply it then quarantines, restores and rescans, in that order, stopping at
// the first error; the result lists exactly which moves were done.
func (r *Restorer) Restore(ctx context.Context, j Job) (RestoreResult, error) {
	res := RestoreResult{Applied: r.Apply, Quarantine: []Move{}, Restore: []Move{}}
	if j.Phase != Failed && j.Phase != Complete {
		return res, fmt.Errorf("restore needs a failed or complete job, not %s", j.Phase)
	}
	plan, err := RestorePlan(j)
	if err != nil {
		return res, err
	}
	if len(r.Quarantine) > 0 && (r.QuarantineRoot == "" || path.Clean(r.QuarantineRoot) != r.QuarantineRoot) {
		return res, fmt.Errorf("quarantine directory required")
	}
	seen, names := map[string]bool{}, map[string]bool{}
	for _, p := range r.Quarantine {
		if !r.inLibrary(p) || seen[p] || names[path.Base(p)] {
			return res, fmt.Errorf("quarantine source %s is not a distinct library file", p)
		}
		seen[p], names[path.Base(p)] = true, true
		m := Move{From: p, To: path.Join(r.QuarantineRoot, path.Base(p))}
		if err := r.check(&m, 0, true, false); err != nil {
			return res, err
		}
		res.Quarantine = append(res.Quarantine, m)
	}
	for _, step := range plan {
		if !r.inLibrary(step.To) {
			return res, fmt.Errorf("restore destination %s is outside the library roots", step.To)
		}
		// A replacement at the original path is moved aside first.
		m := Move{FileID: step.FileID, From: step.From, To: step.To}
		if err := r.check(&m, step.Size, false, seen[step.To]); err != nil {
			return res, err
		}
		res.Restore = append(res.Restore, m)
	}
	if r.Rescan {
		name, field := "RescanMovie", "movieId"
		if j.Target.Kind == "sonarr" {
			name, field = "RescanSeries", "seriesId"
		}
		res.Rescan = fmt.Sprintf("POST /api/v3/command {\"name\":%q,%q:%d}", name, field, j.Target.ID)
	} else {
		res.Rescan = "skipped"
	}
	if !r.Apply {
		if r.Rescan {
			res.Rescan = "would " + res.Rescan
		}
		return res, nil
	}
	if len(res.Quarantine) > 0 {
		if err := os.MkdirAll(filepath.Dir(res.Quarantine[0].toLocal), 0o775); err != nil {
			return res, err
		}
	}
	for i := range res.Quarantine {
		if err := move(&res.Quarantine[i]); err != nil {
			return res, err
		}
	}
	for i := range res.Restore {
		m := &res.Restore[i]
		if err := move(m); err != nil {
			return res, err
		}
		st, raw, err := statReal(m.toLocal)
		if err != nil || !st.Mode().IsRegular() || st.Size() != m.Size || raw.Ino != m.Inode || raw.Uid != m.UID || raw.Gid != m.GID || st.Mode().Perm().String() != m.Mode {
			return res, fmt.Errorf("restored %s does not match its recycle original", m.To)
		}
	}
	if !r.Rescan {
		return res, nil
	}
	if r.Client == nil {
		return res, fmt.Errorf("files restored; rescan needs an application client")
	}
	id, err := r.Client.Rescan(ctx, j.Target)
	if err != nil {
		return res, fmt.Errorf("files restored; rescan not accepted: %v", err)
	}
	res.Rescan = fmt.Sprintf("accepted as command %d: %s", id, res.Rescan)
	return res, nil
}

// Rescan asks the application to rescan one title from disk. It is a POST
// /command, refused unless the client was built with apply.
func (c *Client) Rescan(ctx context.Context, t Target) (int, error) {
	body := map[string]any{"name": "RescanMovie", "movieId": t.ID}
	if c.Kind == "sonarr" {
		body = map[string]any{"name": "RescanSeries", "seriesId": t.ID}
	}
	if t.ID <= 0 || t.Kind != c.Kind {
		return 0, fmt.Errorf("invalid rescan title")
	}
	var v struct {
		ID int `json:"id"`
	}
	if e := c.call(ctx, http.MethodPost, "/command", nil, body, &v); e != nil {
		return 0, e
	}
	if v.ID <= 0 {
		return 0, fmt.Errorf("rescan command identity missing")
	}
	return v.ID, nil
}
