package archivetier

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// recycleOriginal re-verifies one recorded recycle original through the
// writable view, right before it is listed or deleted: under the recycle
// root, every directory reached without following a symlink, a regular file
// with one name, and the size, inode and mtime that were recorded. The
// device is not compared: the writable view is another mount of the same
// share, and NFS reports a different device per mount; inode and mtime are
// the server's.
func recycleOriginal(j Job, e Evidence, mounts []MountMapping) (*os.File, string, error) {
	if e.Directory || !safePath(e.Path, []string{j.RecycleRoot}) {
		return nil, "", fmt.Errorf("%s is not a file under the recycle bin %s", e.Path, j.RecycleRoot)
	}
	local, err := writableLocal(mounts, e.Path)
	if err != nil {
		return nil, "", err
	}
	dir, err := openDir(filepath.Dir(local))
	if err != nil {
		return nil, "", fmt.Errorf("%s: its directory is missing or passes through a symlink", e.Path)
	}
	base := filepath.Base(local)
	var st syscall.Stat_t
	fd, err := syscall.Openat(int(dir.Fd()), base, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err == nil {
		err = syscall.Fstat(fd, &st)
		syscall.Close(fd)
	}
	if err != nil {
		dir.Close()
		switch err {
		case syscall.ENOENT:
			return nil, "", fmt.Errorf("%s is already gone from the recycle bin", e.Path)
		case syscall.ELOOP:
			return nil, "", fmt.Errorf("%s is a symlink; refusing", e.Path)
		}
		return nil, "", fmt.Errorf("%s cannot be inspected", e.Path)
	}
	why := ""
	switch {
	case st.Mode&syscall.S_IFMT != syscall.S_IFREG:
		why = "is not a regular file"
	case st.Size != e.Size:
		why = fmt.Sprintf("is %d bytes, not the recorded %d", st.Size, e.Size)
	case e.Inode != 0 && uint64(st.Ino) != e.Inode:
		why = "is a different file (inode changed)"
	case e.Modified != 0 && st.Mtim.Nano() != e.Modified:
		why = "was modified after it was recorded"
	case uint64(st.Nlink) != 1:
		why = "has another name (a hard link), so it may be a restored copy"
	}
	if why != "" {
		dir.Close()
		return nil, "", fmt.Errorf("%s %s; refusing", e.Path, why)
	}
	return dir, base, nil
}

// PurgeOriginals deletes a completed job's recycle originals: exactly the
// paths in its recovery evidence, each re-verified first. Every file is
// checked before any is deleted; a failure stops at once and the result
// says which were deleted.
func PurgeOriginals(j Job, mounts []MountMapping, apply bool) ([]PurgeFile, error) {
	out := []PurgeFile{}
	if j.Phase != Complete {
		return out, fmt.Errorf("only a completed job's originals can be deleted, not a %s one", j.Phase)
	}
	if len(mounts) == 0 {
		return out, fmt.Errorf("archive.restoreMounts is not configured; no writable view of the recycle bin")
	}
	plan, err := RestorePlan(j)
	if err != nil {
		return out, err
	}
	for i, step := range plan {
		dir, _, err := recycleOriginal(j, j.Recovery[i], mounts)
		if err != nil {
			return out, err
		}
		dir.Close()
		out = append(out, PurgeFile{FileID: step.FileID, Path: step.From, Size: step.Size, Inode: j.Recovery[i].Inode})
	}
	if !apply {
		return out, nil
	}
	for i := range out {
		dir, base, err := recycleOriginal(j, j.Recovery[i], mounts)
		if err != nil {
			return out, err
		}
		err = syscall.Unlinkat(int(dir.Fd()), base)
		if err == nil {
			out[i].Done = true
			err = dir.Sync()
		}
		dir.Close()
		if err != nil {
			return out, fmt.Errorf("delete %s: %v", out[i].Path, err)
		}
	}
	return out, nil
}

// RestoreSidecars puts the staged copies of the original subtitles back next
// to the restored originals, through the writable view. A missing or
// damaged staged copy, or a different file already in place, is noted and
// skipped: subtitles never block restoring the video.
func RestoreSidecars(j Job, mounts []MountMapping, roots []string, apply bool) []SidecarRestore {
	out := []SidecarRestore{}
	if len(j.Sidecars) == 0 {
		return out
	}
	var stage *os.File
	if j.SidecarStage != "" {
		stage, _ = openDir(j.SidecarStage)
	}
	if stage != nil {
		defer stage.Close()
	}
	for _, sc := range j.Sidecars {
		var orig *File
		for i := range j.Target.Files {
			if j.Target.Files[i].ID == sc.FileID {
				orig = &j.Target.Files[i]
			}
		}
		if orig == nil {
			out = append(out, SidecarRestore{Staged: sc.Staged, Note: "original video for this subtitle is unknown"})
			continue
		}
		stem := strings.TrimSuffix(path.Base(orig.Path), path.Ext(orig.Path))
		r := SidecarRestore{Path: path.Join(path.Dir(orig.Path), stem+sc.Suffix), Staged: sc.Staged}
		if suffix, ok := sidecarSuffix(stem, stem+sc.Suffix); !ok || suffix != sc.Suffix {
			r.Note = "invalid recorded subtitle suffix"
			out = append(out, r)
			continue
		}
		if stage == nil {
			r.Note = "staged copy missing"
			out = append(out, r)
			continue
		}
		b, err := readRegular(stage, sc.Staged)
		if err != nil || checksum(b) != sc.SHA256 {
			r.Note = "staged copy missing or changed"
			out = append(out, r)
			continue
		}
		local, err := mappedSidecar(r.Path, mounts, roots)
		if err != nil {
			r.Note = err.Error()
			out = append(out, r)
			continue
		}
		dest, err := openDir(filepath.Dir(local))
		if err != nil {
			r.Note = "library directory unavailable"
			out = append(out, r)
			continue
		}
		if old, err := readRegular(dest, filepath.Base(local)); err == nil {
			if checksum(old) == sc.SHA256 {
				r.Note = "already in place"
			} else {
				r.Note = "a different subtitle is already there; left alone"
			}
			dest.Close()
			out = append(out, r)
			continue
		}
		if apply {
			if err := atomicSidecar(dest, filepath.Base(local), b); err != nil {
				r.Note = "not restored: " + err.Error()
			} else {
				r.Done = true
			}
		}
		dest.Close()
		out = append(out, r)
	}
	return out
}

// carriedSidecars are the subtitles a completed job carried over next to its
// replacement, as app paths; a restore moves them aside with it.
func carriedSidecars(j Job, current Target) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, sc := range j.Sidecars {
		video, err := sidecarDestination(sc, current)
		if err != nil {
			continue
		}
		stem := strings.TrimSuffix(path.Base(video), path.Ext(video))
		p := path.Join(path.Dir(video), stem+sc.Suffix)
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
