package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLockDirContentionReturnsErrLocked(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := LockDir(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock err = %v, want ErrLocked", err)
	}
}

func TestLockDirUnlockThenRelock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	unlock2, err := LockDir(dir)
	if err != nil {
		t.Fatalf("relock after unlock: %v", err)
	}
	unlock2()
}

func TestLockDirCreatesLockFileAtPerm(t *testing.T) {
	dir := t.TempDir()
	unlock, err := LockDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	info, err := os.Stat(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestLockDirOpenFailureIsNotErrLocked(t *testing.T) {
	_, err := LockDir(filepath.Join(t.TempDir(), "missing"))
	if err == nil || errors.Is(err, ErrLocked) {
		t.Fatalf("err = %v, want a non-ErrLocked open error", err)
	}
}
