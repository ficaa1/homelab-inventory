package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Seal binds a preview to all prepared bytes, including conversion decisions.
// It does not include active destination files that continue to change normally.
func Seal(ctx context.Context, p *Plan) error {
	files := []Entry{}
	err := filepath.WalkDir(p.Directory, func(name string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if name == p.Directory {
			return nil
		}
		rel, e := filepath.Rel(p.Directory, name)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if !safe(rel) {
			return ErrInvalid
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return ErrInvalid
		}
		f, e := os.Open(name)
		if e != nil {
			return e
		}
		h := sha256.New()
		n, e := copyContext(ctx, h, f)
		f.Close()
		if e != nil {
			return e
		}
		files = append(files, Entry{rel, n, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	h := sha256.New()
	if err = json.NewEncoder(h).Encode(struct {
		Source Manifest
		Files  []Entry
	}{p.Manifest, files}); err != nil {
		return err
	}
	p.Digest = hex.EncodeToString(h.Sum(nil))
	return nil
}
func Verify(ctx context.Context, p Plan) error {
	want := p.Digest
	if e := Seal(ctx, &p); e != nil {
		return e
	}
	if p.Digest != want {
		return ErrInvalid
	}
	return nil
}
