package archivetier

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// MountMapping maps an app namespace to a locally mounted, read-only view.
// One mapping must contain both a library path and its configured recycle bin.
type MountMapping struct {
	AppRoot   string `json:"appRoot"`
	LocalRoot string `json:"localRoot"`
}
type Evidence struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Device    uint64 `json:"device"`
	Modified  int64  `json:"modified"`
	Directory bool   `json:"directory,omitempty"`
	// Inode identifies the recycle original for a later purge; evidence
	// recorded before it existed has none and is matched by size and mtime.
	Inode uint64 `json:"inode,omitempty"`
}
type Volume struct {
	Device   uint64
	Free     int64
	ReadOnly bool
}

// Filesystem exposes inspection only. It cannot restore, move, or delete files.
type Filesystem interface {
	Stat(string) (Evidence, error)
	Volume(string) (Volume, error)
	Walk(string) ([]Evidence, error)
}
type LocalFilesystem struct{}

func (LocalFilesystem) Stat(p string) (Evidence, error) {
	real, e := filepath.EvalSymlinks(p)
	if e != nil || real != p {
		return Evidence{}, fmt.Errorf("missing or symlinked inspection path")
	}
	st, e := os.Stat(p)
	if e != nil {
		return Evidence{}, fmt.Errorf("cannot inspect path")
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return Evidence{}, fmt.Errorf("nonregular inspection path")
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return Evidence{}, fmt.Errorf("device identity unavailable")
	}
	return Evidence{Path: p, Size: st.Size(), Device: uint64(raw.Dev), Modified: st.ModTime().UnixNano(), Directory: st.IsDir(), Inode: uint64(raw.Ino)}, nil
}
func (LocalFilesystem) Volume(p string) (Volume, error) {
	st, e := (LocalFilesystem{}).Stat(p)
	if e != nil {
		return Volume{}, e
	}
	var v syscall.Statfs_t
	if e = syscall.Statfs(p, &v); e != nil {
		return Volume{}, fmt.Errorf("cannot inspect filesystem")
	}
	return Volume{Device: st.Device, Free: int64(v.Bavail) * int64(v.Bsize), ReadOnly: v.Flags&1 != 0}, nil
}
func (l LocalFilesystem) Walk(root string) ([]Evidence, error) {
	out := []Evidence{}
	e := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("cannot inspect recycle tree")
		}
		if len(out) >= 100000 {
			return fmt.Errorf("recycle inspection exceeds safety limit")
		}
		st, e := l.Stat(p)
		if e != nil {
			return e
		}
		if !st.Directory {
			out = append(out, st)
		}
		return nil
	})
	return out, e
}
func (c *Client) filesystem() Filesystem {
	if c.FS != nil {
		return c.FS
	}
	return LocalFilesystem{}
}
func (c *Client) mapped(p string) (string, MountMapping, error) {
	var found *MountMapping
	for i := range c.Safety.Mounts {
		m := &c.Safety.Mounts[i]
		if m.AppRoot == "/" || path.Clean(m.AppRoot) != m.AppRoot || !strings.HasPrefix(m.AppRoot, "/") || filepath.Clean(m.LocalRoot) != m.LocalRoot || !filepath.IsAbs(m.LocalRoot) || m.LocalRoot == "/" {
			return "", MountMapping{}, fmt.Errorf("invalid filesystem mapping")
		}
		if under(p, m.AppRoot) {
			if found != nil {
				return "", MountMapping{}, fmt.Errorf("ambiguous filesystem mapping")
			}
			found = m
		}
	}
	if found == nil || path.Clean(p) != p {
		return "", MountMapping{}, fmt.Errorf("missing filesystem mapping")
	}
	local := filepath.Join(found.LocalRoot, strings.TrimPrefix(p, found.AppRoot))
	return local, *found, nil
}
func (c *Client) inspect(p string) (Evidence, Volume, error) {
	local, _, e := c.mapped(p)
	if e != nil {
		return Evidence{}, Volume{}, e
	}
	st, e := c.filesystem().Stat(local)
	if e != nil {
		return st, Volume{}, e
	}
	v, e := c.filesystem().Volume(local)
	if e != nil {
		return st, v, e
	}
	if !v.ReadOnly || st.Device != v.Device {
		return st, v, fmt.Errorf("inspection mount must be read-only with known device")
	}
	st.Path = p
	return st, v, nil
}
func (c *Client) recycle(ctx context.Context) (string, error) {
	var cfg struct {
		Recycle *string `json:"recycleBin"`
		Script  *bool   `json:"useScriptImport"`
	}
	if e := c.get(ctx, "/config/mediamanagement", nil, &cfg); e != nil {
		return "", e
	}
	if cfg.Script == nil || *cfg.Script {
		return "", fmt.Errorf("useScriptImport must be explicitly false")
	}
	if cfg.Recycle == nil || *cfg.Recycle == "" || path.Clean(*cfg.Recycle) != *cfg.Recycle {
		return "", fmt.Errorf("configured recycle bin required")
	}
	return *cfg.Recycle, nil
}

// TargetPreflight runs before each write, including imports resumed after restart.
// The returned recycle baseline must be persisted before submitting the write.
func (c *Client) TargetPreflight(ctx context.Context, t Target, items []ManualItem, bytes int64) (string, []Evidence, error) {
	if e := c.Preflight(ctx); e != nil {
		return "", nil, e
	}
	root, e := c.recycle(ctx)
	if e != nil {
		return "", nil, e
	}
	margin := c.Safety.FreeSpaceMarginGB * float64(GB)
	if math.IsNaN(margin) || math.IsInf(margin, 0) || margin <= 0 || margin >= float64(math.MaxInt64) || bytes <= 0 || bytes > math.MaxInt64-int64(margin) {
		return "", nil, fmt.Errorf("positive free-space margin and replacement size required")
	}
	rec, rv, e := c.inspect(root)
	if e != nil {
		return "", nil, e
	}
	if !rec.Directory {
		return "", nil, fmt.Errorf("recycle bin is not a directory")
	}
	_, rm, _ := c.mapped(root)
	libraryDevices := map[uint64]bool{}
	for _, f := range t.Files {
		if !safePath(f.Path, c.Safety.LibraryRoots) {
			return "", nil, fmt.Errorf("unapproved library path")
		}
		st, v, e := c.inspect(f.Path)
		if e != nil {
			return "", nil, e
		}
		_, m, _ := c.mapped(f.Path)
		if st.Directory || st.Size != f.Size || f.Size <= 0 || m != rm || st.Device != rec.Device || v.Free < bytes+int64(margin) {
			return "", nil, fmt.Errorf("library/recycle device, root, size or free-space safety failed")
		}
		libraryDevices[st.Device] = true
	}
	if len(libraryDevices) == 0 {
		return "", nil, fmt.Errorf("no library files")
	}
	if rv.Free < bytes+int64(margin) {
		return "", nil, fmt.Errorf("insufficient recycle filesystem headroom")
	}
	// Even a grab checks all source mounts: copy helpers must never hardlink into library storage.
	if len(c.Safety.DownloadRoots) == 0 {
		return "", nil, fmt.Errorf("source roots required")
	}
	for _, root := range c.Safety.DownloadRoots {
		st, _, e := c.inspect(root)
		if e != nil {
			return "", nil, e
		}
		if !st.Directory || libraryDevices[st.Device] {
			return "", nil, fmt.Errorf("source safety requires a separate filesystem")
		}
	}
	for _, it := range items {
		if !safePath(it.Path, c.Safety.DownloadRoots) {
			return "", nil, fmt.Errorf("unapproved source")
		}
		st, _, e := c.inspect(it.Path)
		if e != nil {
			return "", nil, e
		}
		if st.Directory || st.Size != it.Size || libraryDevices[st.Device] {
			return "", nil, fmt.Errorf("source file size/device safety failed")
		}
	}
	baseline, e := c.recycleEvidence(root)
	rec.Size = 0
	baseline = append(baseline, rec)
	return root, baseline, e
}
func (c *Client) recycleEvidence(root string) ([]Evidence, error) {
	local, _, e := c.mapped(root)
	if e != nil {
		return nil, e
	}
	_, v, e := c.inspect(root)
	if e != nil {
		return nil, e
	}
	entries, e := c.filesystem().Walk(local)
	if e != nil {
		return nil, e
	}
	for i := range entries {
		if entries[i].Directory || entries[i].Device != v.Device || !under(entries[i].Path, local) {
			return nil, fmt.Errorf("unsafe recycle evidence")
		}
		entries[i].Path = path.Join(root, strings.TrimPrefix(entries[i].Path, local))
	}
	return entries, nil
}
func mergeBaseline(root string, before, now []Evidence) ([]Evidence, error) {
	var oldDevice, newDevice uint64
	oldRoot, newRoot := false, false
	for _, e := range before {
		if e.Path == root && e.Directory {
			if oldRoot && oldDevice != e.Device {
				return nil, fmt.Errorf("inconsistent recycle baseline")
			}
			oldDevice = e.Device
			oldRoot = true
		}
	}
	for _, e := range now {
		if e.Path == root && e.Directory {
			newDevice = e.Device
			newRoot = true
		}
	}
	if !oldRoot || !newRoot || oldDevice != newDevice {
		return nil, fmt.Errorf("recycle filesystem changed before import or missing persisted baseline")
	}
	merged := append([]Evidence{}, before...)
	known := map[string]uint64{}
	for _, e := range before {
		known[e.Path] = e.Device
	}
	for _, e := range now {
		if device, ok := known[e.Path]; ok {
			if device != e.Device {
				return nil, fmt.Errorf("recycle entry device changed")
			}
			continue
		}
		known[e.Path] = e.Device
		merged = append(merged, e)
	}
	return merged, nil
}
func (c *Client) Recovery(ctx context.Context, j *Job) ([]Evidence, error) {
	if j.RecycleRoot == "" || j.RecycleBaseline == nil {
		return nil, fmt.Errorf("missing persisted recycle baseline")
	}
	root, e := c.recycle(ctx)
	if e != nil {
		return nil, e
	}
	if root != j.RecycleRoot {
		return nil, fmt.Errorf("recycle configuration changed")
	}
	st, _, e := c.inspect(root)
	if e != nil {
		return nil, e
	}
	knownDevice := false
	for _, old := range j.RecycleBaseline {
		if old.Directory && old.Path == root && old.Device == st.Device {
			knownDevice = true
		}
	}
	if !knownDevice {
		return nil, fmt.Errorf("recycle device changed or missing baseline")
	}
	entries, e := c.recycleEvidence(j.RecycleRoot)
	if e != nil {
		return nil, e
	}
	return recoveryEvidence(j.Target, j.RecycleBaseline, entries)
}
func recoveryEvidence(t Target, baseline, entries []Evidence) ([]Evidence, error) {
	recovered := []Evidence{}
	used := map[int]bool{}
	for _, f := range t.Files {
		match := -1
		for i, st := range entries {
			if used[i] || path.Base(st.Path) != path.Base(f.Path) || st.Size != f.Size || st.Directory {
				continue
			}
			existed := false
			for _, old := range baseline {
				if old.Path == st.Path {
					existed = true
				}
			}
			if !existed {
				if match >= 0 {
					return nil, fmt.Errorf("ambiguous recycle evidence for old file ID %d", f.ID)
				}
				match = i
			}
		}
		if match < 0 {
			return nil, fmt.Errorf("missing new recycle evidence for old file ID %d (%s, %d bytes)", f.ID, path.Base(f.Path), f.Size)
		}
		used[match] = true
		recovered = append(recovered, entries[match])
	}
	return recovered, nil
}

// RestorePlan is a pure, reviewable plan. No filesystem mutation API exists here.
type RestoreStep struct {
	FileID int    `json:"fileId"`
	From   string `json:"from"`
	To     string `json:"to"`
	Size   int64  `json:"size"`
}

// RecoveryInspection is printed when no complete automatic restore proof exists.
const RecoveryInspection = "stop the service and preserve state.json/audit.jsonl; inspect the job target.files and recycleRoot before seven-day cleanup; match each original basename and byte size to a new regular recycle file absent from recycleBaseline on the recorded device; inspect Arr command/history/queue and library paths, quarantine any replacements, then record reviewed one-to-one recovery evidence in the stopped state and rerun -restore without -apply"

func RestorePlan(j Job) ([]RestoreStep, error) {
	if len(j.Target.Files) == 0 || len(j.Recovery) != len(j.Target.Files) {
		return nil, fmt.Errorf("complete recovery evidence required: %s", RecoveryInspection)
	}
	out := []RestoreStep{}
	from, to := map[string]bool{}, map[string]bool{}
	for i, f := range j.Target.Files {
		st := j.Recovery[i]
		if st.Directory || f.ID <= 0 || path.Clean(f.Path) != f.Path || !strings.HasPrefix(f.Path, "/") || f.Path == "/" || from[st.Path] || to[f.Path] || st.Path == f.Path || f.Size <= 0 || path.Base(st.Path) != path.Base(f.Path) || st.Size != f.Size || !safePath(st.Path, []string{j.RecycleRoot}) {
			return nil, fmt.Errorf("invalid restore evidence")
		}
		from[st.Path], to[f.Path] = true, true
		out = append(out, RestoreStep{f.ID, st.Path, f.Path, f.Size})
	}
	return out, nil
}
