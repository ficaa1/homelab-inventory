// Package fsx holds the durable file primitives shared by the services and the
// separately pinned updater. It is stdlib-only, like httpx, so the updater can
// import it without pulling anything else in.
package fsx

import (
	"os"
	"path/filepath"
)

// WriteAtomic replaces path with data at perm. The data reaches a temp file in
// the same directory, is fsynced, renamed over path and the directory is
// fsynced, so a crash leaves either the old file or the new one, never a
// truncated one. The temp file is removed on every failure path.
func WriteAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, tempPattern(path))
	if err != nil {
		return err
	}
	name := f.Name()
	if err := writeClose(f, data, perm); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return syncDir(dir)
}

// tempPattern bounds the basename so a name near NAME_MAX still has room for
// the leading dot and the random suffix.
func tempPattern(path string) string {
	base := filepath.Base(path)
	if len(base) > 32 {
		base = base[:32]
	}
	return "." + base + "-*.tmp"
}

// SyncLevel says how far Append must go before it returns.
type SyncLevel int

const (
	// SyncNone leaves durability to the kernel.
	SyncNone SyncLevel = iota
	// SyncFile fsyncs the file after the write.
	SyncFile
	// SyncFileDir also fsyncs the directory, so a newly created file survives a crash.
	SyncFileDir
)

// Append writes data verbatim to the end of path, creating it at 0600 if
// needed. Callers supply the line terminator. Append never truncates or
// rewrites existing content.
func Append(path string, data []byte, level SyncLevel) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if level >= SyncFile {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if level == SyncFileDir {
		return syncDir(filepath.Dir(path))
	}
	return nil
}

func writeClose(f *os.File, data []byte, perm os.FileMode) error {
	err := f.Chmod(perm)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

var syncDir = syncDirPath

func syncDirPath(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}
