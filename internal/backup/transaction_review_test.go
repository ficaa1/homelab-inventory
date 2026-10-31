package backup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRollbackRemovesRootsCreatedByValidator(t *testing.T) {
	root, stage := t.TempDir(), t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "original.json"), []byte("original"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(stage, "incoming.json"), []byte("incoming"), 0600); e != nil {
		t.Fatal(e)
	}
	validationFailure := errors.New("synthetic local validation failure")
	e := Install(root, stage, func() error {
		if e := os.WriteFile(filepath.Join(root, "state-session-key"), []byte("foreign"), 0600); e != nil {
			t.Fatal(e)
		}
		if e := os.MkdirAll(filepath.Join(root, "new-manager", "nested"), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(root, "new-manager", "nested", "state.json"), []byte("foreign"), 0600); e != nil {
			t.Fatal(e)
		}
		return validationFailure
	})
	if !errors.Is(e, validationFailure) {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(root, "original.json"))
	if e != nil || string(got) != "original" {
		t.Fatal("old generation lost", e)
	}
	for _, name := range []string{"incoming.json", "state-session-key", "new-manager"} {
		if _, e := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(e) {
			t.Fatalf("failed generation root %s survived: %v", name, e)
		}
	}
	if e = Recover(root); e != nil {
		t.Fatal("repeat recovery", e)
	}
}
