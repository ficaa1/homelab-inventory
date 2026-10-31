package backup

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const journalName = ".backup-journal.json"

type journal struct {
	Rollback  string `json:"rollback"`
	Old, New  []string
	Committed bool
}

func syncDir(root string) error {
	f, e := os.Open(root)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func writeJournal(root string, j journal) error {
	b, e := json.Marshal(j)
	if e != nil {
		return e
	}
	p := filepath.Join(root, journalName)
	f, e := os.OpenFile(p+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return e
	}
	if e = os.Rename(p+".tmp", p); e != nil {
		return e
	}
	return syncDir(root)
}
func names(root string) ([]string, error) {
	ds, e := os.ReadDir(root)
	if e != nil {
		return nil, e
	}
	out := []string{}
	for _, d := range ds {
		if !strings.HasPrefix(d.Name(), ".backup-") {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

// Recover must run before any store, cache or background worker is opened.
// The stable volume root is never renamed. An unfinished transaction rolls back.
func Recover(root string) error {
	b, e := os.ReadFile(filepath.Join(root, journalName))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j journal
	if json.Unmarshal(b, &j) != nil || !strings.HasPrefix(j.Rollback, ".backup-rollback-") || filepath.Base(j.Rollback) != j.Rollback {
		return errors.New("invalid restore recovery journal")
	}
	for _, n := range append(append([]string{}, j.Old...), j.New...) {
		if filepath.Base(n) != n || !safe(n) {
			return ErrInvalid
		}
	}
	rb := filepath.Join(root, j.Rollback)
	if !j.Committed {
		// Only remove installed names once their old version is safely in rollback,
		// or when the name did not exist in the original generation.
		current, e := names(root)
		if e != nil {
			return e
		}
		original := map[string]bool{}
		for _, n := range j.Old {
			original[n] = true
		}
		for _, n := range current {
			if !original[n] {
				if e = os.RemoveAll(filepath.Join(root, n)); e != nil {
					return e
				}
			}
		}
		old := map[string]bool{}
		for _, n := range j.Old {
			old[n] = true
		}
		for _, n := range j.New {
			_, e := os.Lstat(filepath.Join(rb, n))
			if e != nil && !os.IsNotExist(e) {
				return e
			}
			if !old[n] || e == nil {
				if e = os.RemoveAll(filepath.Join(root, n)); e != nil {
					return e
				}
			}
		}
		for _, n := range j.Old {
			src := filepath.Join(rb, n)
			if _, e = os.Lstat(src); os.IsNotExist(e) {
				continue
			} else if e != nil {
				return e
			}
			if e = os.RemoveAll(filepath.Join(root, n)); e != nil {
				return e
			}
			if e = os.Rename(src, filepath.Join(root, n)); e != nil {
				return e
			}
			if e = syncDir(rb); e != nil {
				return e
			}
			if e = syncDir(root); e != nil {
				return e
			}
		}
	}
	if e = syncDir(root); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(root, journalName)); e != nil {
		return e
	}
	if e = syncDir(root); e != nil {
		return e
	}
	return os.RemoveAll(rb)
}

// Install replaces the flat application dataset under an exclusive lifecycle
// drain. validate must reopen/read all managers without starting side effects.
// A validation error restores the entire old generation before returning.
func Install(root, staged string, validate func() error) (err error) {
	if err = syncTree(staged); err != nil {
		return err
	}
	if err = Recover(root); err != nil {
		return err
	}
	old, err := names(root)
	if err != nil {
		return err
	}
	next, err := names(staged)
	if err != nil {
		return err
	}
	rb, err := os.MkdirTemp(root, ".backup-rollback-")
	if err != nil {
		return err
	}
	j := journal{Rollback: filepath.Base(rb), Old: old, New: next}
	if err = writeJournal(root, j); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if e := Recover(root); e != nil {
				err = errors.Join(err, e)
			}
		}
	}()
	for _, n := range old {
		if err = os.Rename(filepath.Join(root, n), filepath.Join(rb, n)); err != nil {
			return err
		}
		if err = syncDir(rb); err != nil {
			return err
		}
		if err = syncDir(root); err != nil {
			return err
		}
	}
	for _, n := range next {
		if err = os.Rename(filepath.Join(staged, n), filepath.Join(root, n)); err != nil {
			return err
		}
		if err = syncDir(root); err != nil {
			return err
		}
	}
	if validate != nil {
		if err = validate(); err != nil {
			return err
		}
	}
	j.Committed = true
	if err = writeJournal(root, j); err != nil {
		return err
	}
	return Recover(root)
}

func syncTree(root string) error {
	dirs := []string{}
	if e := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); e != nil {
		return e
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if e := syncDir(dirs[i]); e != nil {
			return e
		}
	}
	return nil
}
