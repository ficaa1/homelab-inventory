package archivetier

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

const maxSidecarBytes = 32 << 20

// openDir walks from / with O_NOFOLLOW at every component. Operations below
// keep the directory descriptor open so a renamed parent cannot redirect them.
func openDir(p string) (*os.File, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return nil, fmt.Errorf("unclean local path")
	}
	fd, e := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		if part == "" {
			continue
		}
		next, err := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("directory unsafe or unavailable")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), p), nil
}
func readRegular(dir *os.File, name string) ([]byte, error) {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("unsafe basename")
	}
	fd, e := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, fmt.Errorf("sidecar unsafe or unavailable")
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > maxSidecarBytes {
		return nil, fmt.Errorf("sidecar is not a bounded regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, maxSidecarBytes+1))
	if len(b) > maxSidecarBytes {
		return nil, fmt.Errorf("sidecar too large")
	}
	return b, e
}
func checksum(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func sidecarSuffix(stem, name string) (string, bool) {
	if !strings.HasPrefix(name, stem+".") {
		return "", false
	}
	suffix := strings.TrimPrefix(name, stem)
	switch strings.ToLower(filepath.Ext(suffix)) {
	case ".srt", ".ass", ".ssa", ".sub", ".idx", ".vtt":
	default:
		return "", false
	}
	// Preserve language and flags but refuse ambiguous empty components.
	if strings.Contains(suffix, "..") || strings.ContainsAny(suffix, "/\\") {
		return "", false
	}
	return suffix, true
}
func mappedSidecar(p string, mounts []MountMapping, roots []string) (string, error) {
	if !safePath(p, roots) {
		return "", fmt.Errorf("sidecar outside approved library roots")
	}
	best := -1
	for i, m := range mounts {
		if under(p, m.AppRoot) && (best < 0 || len(m.AppRoot) > len(mounts[best].AppRoot)) {
			best = i
		}
	}
	if best < 0 {
		return "", fmt.Errorf("no sidecar mount for library path")
	}
	m := mounts[best]
	if !filepath.IsAbs(m.LocalRoot) || filepath.Clean(m.LocalRoot) != m.LocalRoot {
		return "", fmt.Errorf("unclean sidecar mount")
	}
	return filepath.Join(m.LocalRoot, strings.TrimPrefix(p, m.AppRoot)), nil
}

// StageSidecars inventories the read-only library view before Arr recycles it.
// A private directory is allocated once per job; its manifest lives in state.
func StageSidecars(j *Job, c Config, stateDir string) error {
	if len(c.SidecarMounts) == 0 {
		j.SidecarMessage = "subtitles not carried over (sidecarMounts unset)"
		return nil
	}
	if stateDir == "" {
		return fmt.Errorf("sidecar state directory missing")
	}
	stateDir, e := filepath.Abs(stateDir)
	if e != nil {
		return e
	}
	base, e := openDir(stateDir)
	if e != nil {
		return e
	}
	defer base.Close()
	name := "subtitles-" + checksum([]byte(fmt.Sprintf("%s:%s", targetKey(j.Target), j.Started.Format("20060102T150405.000000000"))))[:20]
	if e = syscall.Mkdirat(int(base.Fd()), name, 0700); e != nil && e != syscall.EEXIST {
		return e
	}
	dir, e := openDir(filepath.Join(stateDir, name))
	if e != nil {
		return e
	}
	defer dir.Close()
	j.SidecarStage = filepath.Join(stateDir, name)
	for _, old := range j.Target.Files {
		local, e := mappedSidecar(old.Path, c.Mounts, c.LibraryRoots)
		if e != nil {
			return e
		}
		src, e := openDir(filepath.Dir(local))
		if e != nil {
			return e
		}
		names, e := src.Readdirnames(-1)
		if e != nil {
			src.Close()
			return e
		}
		sort.Strings(names)
		stem := strings.TrimSuffix(filepath.Base(old.Path), filepath.Ext(old.Path))
		for _, n := range names {
			suffix, ok := sidecarSuffix(stem, n)
			if !ok {
				continue
			}
			b, e := readRegular(src, n)
			if e != nil {
				src.Close()
				return e
			}
			staged := fmt.Sprintf("%d-%s%s", old.ID, checksum([]byte(suffix))[:16], filepath.Ext(suffix))
			if e = atomicSidecar(dir, staged, b); e != nil {
				src.Close()
				return e
			}
			sc := Sidecar{FileID: old.ID, Suffix: suffix, Staged: staged, SHA256: checksum(b)}
			for _, ep := range j.Target.Episodes {
				if ep.FileID == old.ID {
					sc.EpisodeIDs = append(sc.EpisodeIDs, ep.ID)
				}
			}
			j.Sidecars = append(j.Sidecars, sc)
		}
		src.Close()
	}
	return nil
}

func sidecarDestination(sc Sidecar, current Target) (string, error) {
	if current.Kind == "radarr" {
		if len(current.Files) == 1 {
			return current.Files[0].Path, nil
		}
		return "", fmt.Errorf("movie file mapping ambiguous")
	}
	ids := map[int]bool{}
	for _, id := range sc.EpisodeIDs {
		ids[id] = true
	}
	files := map[int]bool{}
	for _, ep := range current.Episodes {
		if ids[ep.ID] {
			files[ep.FileID] = true
			delete(ids, ep.ID)
		}
	}
	if len(ids) != 0 || len(files) != 1 {
		return "", fmt.Errorf("subtitle episode mapping ambiguous")
	}
	for _, f := range current.Files {
		if files[f.ID] {
			// A changed multi-episode grouping could give a subtitle the wrong timeline.
			count := 0
			for _, ep := range current.Episodes {
				if ep.FileID == f.ID {
					count++
				}
			}
			if count != len(sc.EpisodeIDs) {
				return "", fmt.Errorf("subtitle episode grouping changed")
			}
			return f.Path, nil
		}
	}
	return "", fmt.Errorf("subtitle destination missing")
}

func CarrySidecars(j *Job, current Target, c Config) error {
	if len(j.Sidecars) == 0 {
		return nil
	}
	src, e := openDir(j.SidecarStage)
	if e != nil {
		return e
	}
	defer src.Close()
	j.SidecarsCarried = 0
	for _, sc := range j.Sidecars {
		video, e := sidecarDestination(sc, current)
		if e != nil {
			return e
		}
		stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
		if suffix, ok := sidecarSuffix(stem, stem+sc.Suffix); !ok || suffix != sc.Suffix {
			return fmt.Errorf("invalid persisted subtitle suffix")
		}
		p, e := mappedSidecar(video, c.SidecarMounts, c.LibraryRoots)
		if e != nil {
			return e
		}
		dest, e := openDir(filepath.Dir(p))
		if e != nil {
			return e
		}
		b, e := readRegular(src, sc.Staged)
		if e != nil {
			dest.Close()
			return e
		}
		if checksum(b) != sc.SHA256 {
			dest.Close()
			return fmt.Errorf("staged subtitle checksum mismatch")
		}
		e = atomicSidecar(dest, stem+sc.Suffix, b)
		dest.Close()
		if e != nil {
			return e
		}
		j.SidecarsCarried++
	}
	return nil
}

// atomicSidecar publishes a fsynced temporary without replacing a destination. An
// existing regular file is accepted only when its checksum is identical.
func atomicSidecar(dir *os.File, name string, b []byte) error {
	return atomicSidecarWithRename(dir, name, b, renameSidecar)
}

func atomicSidecarWithRename(dir *os.File, name string, b []byte, rename func(*os.File, string, string) syscall.Errno) error {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("unsafe subtitle basename")
	}
	tmp := ".subtitle-" + checksum(b)[:20]
	flags := syscall.O_WRONLY | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	fd, e := syscall.Openat(int(dir.Fd()), tmp, flags, 0600)
	if e == syscall.EEXIST {
		// Left by a crash before the rename; the name is ours alone.
		if syscall.Unlinkat(int(dir.Fd()), tmp) == nil {
			fd, e = syscall.Openat(int(dir.Fd()), tmp, flags, 0600)
		}
	}
	if e != nil {
		return fmt.Errorf("subtitle temporary unavailable")
	}
	f := os.NewFile(uintptr(fd), tmp)
	defer syscall.Unlinkat(int(dir.Fd()), tmp)
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	errno := rename(dir, tmp, name)
	if errno == syscall.EINVAL || errno == syscall.ENOSYS || errno == syscall.EOPNOTSUPP {
		// NFSv3 does not support RENAME_NOREPLACE; linkat also refuses overwrites.
		from, _ := syscall.BytePtrFromString(tmp)
		to, _ := syscall.BytePtrFromString(name)
		_, _, errno = syscall.Syscall6(syscall.SYS_LINKAT, dir.Fd(), uintptr(unsafe.Pointer(from)), dir.Fd(), uintptr(unsafe.Pointer(to)), 0, 0)
	}
	if errno == syscall.EEXIST {
		old, err := readRegular(dir, name)
		if err != nil || checksum(old) != checksum(b) {
			return fmt.Errorf("subtitle destination differs or is unsafe")
		}
	} else if errno != 0 {
		return fmt.Errorf("atomic subtitle rename failed: %w", errno)
	}
	return dir.Sync()
}

func renameSidecar(dir *os.File, tmp, name string) syscall.Errno {
	// Linux syscall numbers are architecture-specific; the worker ships amd64.
	var number uintptr
	switch runtime.GOARCH {
	case "amd64":
		number = 316
	case "arm64":
		number = 276
	default:
		return syscall.ENOSYS
	}
	from, _ := syscall.BytePtrFromString(tmp)
	to, _ := syscall.BytePtrFromString(name)
	const renameNoReplace = 1
	_, _, errno := syscall.Syscall6(number, dir.Fd(), uintptr(unsafe.Pointer(from)), dir.Fd(), uintptr(unsafe.Pointer(to)), renameNoReplace, 0)
	return errno
}
