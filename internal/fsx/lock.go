package fsx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked reports that another open file description holds dir's lock.
var ErrLocked = errors.New("directory is locked")

// LockDir takes a non-blocking exclusive lock on dir/lock. Contention returns
// an error matching ErrLocked; any other failure is the underlying open error.
// unlock releases the lock and closes the file.
func LockDir(dir string) (unlock func(), err error) {
	f, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w: %w", ErrLocked, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
