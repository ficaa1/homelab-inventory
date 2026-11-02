package mediapolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// Every component is opened no-follow and parents remain anchored during writes.
func seedDir(p string, create bool) (*os.File, error) {
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return nil, fmt.Errorf("unsafe seeding directory")
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
		if err == syscall.ENOENT && create {
			err = syscall.Mkdirat(fd, part, 0755)
			if err == nil {
				err = syscall.Fsync(fd)
			}
			if err == nil {
				next, err = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			}
		}
		syscall.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("seeding directory unsafe or unavailable")
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), p), nil
}
func seedLink(src, dst string, evidence archivetier.Evidence) error {
	s, err := seedDir(filepath.Dir(src), false)
	if err != nil {
		return err
	}
	defer s.Close()
	d, err := seedDir(filepath.Dir(dst), true)
	if err != nil {
		return err
	}
	defer d.Close()
	fd, err := syscall.Openat(int(s.Fd()), filepath.Base(src), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("link source unavailable")
	}
	f := os.NewFile(uintptr(fd), src)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Size() != evidence.Size || uint64(raw.Dev) != evidence.Device || uint64(raw.Ino) != evidence.Inode || st.ModTime().UnixNano() != evidence.Modified {
		return fmt.Errorf("link source changed")
	}
	sp, _ := syscall.BytePtrFromString(filepath.Base(src))
	dp, _ := syscall.BytePtrFromString(filepath.Base(dst))
	_, _, errno := syscall.Syscall6(syscall.SYS_LINKAT, s.Fd(), uintptr(unsafe.Pointer(sp)), d.Fd(), uintptr(unsafe.Pointer(dp)), 0, 0)
	runtime.KeepAlive(sp)
	runtime.KeepAlive(dp)
	if errno != 0 {
		return fmt.Errorf("hardlink refused (destination occupied or different mount)")
	}
	if err = d.Sync(); err != nil {
		return err
	}
	got, err := seedEvidence(dst, evidence.Size)
	if err != nil || got.Device != evidence.Device || got.Inode != evidence.Inode {
		return fmt.Errorf("hardlink identity mismatch")
	}
	return nil
}
func seedRemove(p string, evidence archivetier.Evidence, single bool) error {
	d, err := seedDir(filepath.Dir(p), false)
	if err != nil {
		return err
	}
	defer d.Close()
	fd, err := syscall.Openat(int(d.Fd()), filepath.Base(p), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("removal source unavailable")
	}
	f := os.NewFile(uintptr(fd), p)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	raw, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.Mode().IsRegular() || st.Size() != evidence.Size || uint64(raw.Dev) != evidence.Device || uint64(raw.Ino) != evidence.Inode || st.ModTime().UnixNano() != evidence.Modified || (single && raw.Nlink != 1) {
		return fmt.Errorf("removal file identity changed or still hardlinked")
	}
	if err = syscall.Unlinkat(int(d.Fd()), filepath.Base(p)); err != nil {
		return fmt.Errorf("exact file removal failed")
	}
	return d.Sync()
}
func seedLinks(p string) (uint64, error) {
	st, e := os.Lstat(p)
	if e != nil {
		return 0, e
	}
	r, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("link count unavailable")
	}
	return uint64(r.Nlink), nil
}

func seedPrepareDestination(p string) error {
	d, err := seedDir(filepath.Dir(p), true)
	if err != nil {
		return err
	}
	return d.Close()
}
