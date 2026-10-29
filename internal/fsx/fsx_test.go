package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAtomicReplacesContentAtPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new\n" {
		t.Fatalf("content = %q, %v; want new", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
	assertOnly(t, dir, "state.json")
}

func TestWriteAtomicFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, []byte("x"), 0o600); err == nil {
		t.Fatal("rename over a directory succeeded")
	}
	assertOnly(t, dir, "target")
}

func TestWriteAtomicAcceptsLongBasename(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 250)
	if err := WriteAtomic(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, dir, name)
}

func TestWriteAtomicSyncsTheDirectoryAfterRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	var synced []string
	restore := swapSyncDir(func(d string) error {
		synced = append(synced, d)
		return nil
	})
	defer restore()
	if err := WriteAtomic(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Fatalf("directory syncs = %v, want [%s]", synced, dir)
	}
}

func TestWriteAtomicReportsDirectorySyncFailure(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("sync refused")
	restore := swapSyncDir(func(string) error { return want })
	defer restore()
	if err := WriteAtomic(filepath.Join(dir, "state.json"), []byte("x"), 0o600); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func swapSyncDir(f func(string) error) (restore func()) {
	prev := syncDir
	syncDir = f
	return func() { syncDir = prev }
}

func assertOnly(t *testing.T, dir string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(names) {
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		t.Fatalf("directory has %v, want %v", got, names)
	}
	for i, e := range entries {
		if e.Name() != names[i] {
			t.Fatalf("directory has %s, want %s", e.Name(), names[i])
		}
	}
}

func TestAppendAddsLinesAndCreatesAtPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	for _, line := range []string{"one\n", "two\n"} {
		if err := Append(path, []byte(line), SyncFile); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "one\ntwo\n" {
		t.Fatalf("content = %q, %v; want both lines", got, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestAppendSyncLevelsControlDirectorySync(t *testing.T) {
	cases := []struct {
		level SyncLevel
		dirs  int
	}{
		{SyncNone, 0},
		{SyncFile, 0},
		{SyncFileDir, 1},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		var synced []string
		restore := swapSyncDir(func(d string) error {
			synced = append(synced, d)
			return nil
		})
		err := Append(filepath.Join(dir, "audit.jsonl"), []byte("x\n"), tc.level)
		restore()
		if err != nil {
			t.Fatal(err)
		}
		if len(synced) != tc.dirs {
			t.Fatalf("level %d: directory syncs = %v, want %d", tc.level, synced, tc.dirs)
		}
	}
}

func TestAppendReportsOpenFailure(t *testing.T) {
	dir := t.TempDir()
	if err := Append(filepath.Join(dir, "missing", "audit.jsonl"), []byte("x\n"), SyncFileDir); err == nil {
		t.Fatal("append into a missing directory succeeded")
	}
}

func TestCreateAtomicNeverReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-key")
	if err := CreateAtomic(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CreateAtomic(path, []byte("second"), 0600); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second create: %v", err)
	}
	b, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || string(b) != "first" || info.Mode().Perm() != 0600 {
		t.Fatal("atomic create replaced content or permissions")
	}
	assertOnly(t, dir, "session-key")
}
