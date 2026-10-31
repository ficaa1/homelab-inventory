package uploads

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (u *Uploads) FolderFiles(ctx context.Context) ([]plugin.FolderFile, error) {
	if u.folder == "" {
		return nil, plugin.ErrNotFound
	}
	entries, err := os.ReadDir(u.folder)
	if err != nil {
		return nil, errors.New("folder listing unavailable")
	}
	out := []plugin.FolderFile{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, plugin.FolderFile{Name: e.Name(), Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	if len(out) > 500 {
		out = out[:500]
	}
	return out, nil
}
func (u *Uploads) OpenFolderFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	if u.folder == "" || name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\\x00") {
		return nil, 0, plugin.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	root, err := os.OpenRoot(u.folder)
	if err != nil {
		return nil, 0, errors.New("folder unavailable")
	}
	f, err := root.Open(name)
	if err != nil {
		root.Close()
		return nil, 0, plugin.ErrNotFound
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		root.Close()
		return nil, 0, plugin.ErrNotFound
	}
	// Keep the root alive through the file lifetime on platforms where OpenRoot
	// owns the descriptor used for path resolution.
	return &rootFile{ReadCloser: f, root: root}, info.Size(), nil
}

type rootFile struct {
	io.ReadCloser
	root *os.Root
}

func (f *rootFile) Close() error { err := f.ReadCloser.Close(); f.root.Close(); return err }

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (u *Uploads) writeFolder(ctx context.Context, name string, size int64, body io.Reader) (plugin.UploadResult, error) {
	fail := func() (plugin.UploadResult, error) { return plugin.UploadResult{}, errors.New("Folder upload failed") }
	// Root confines every path operation to the opened destination, including if
	// another process changes a parent symlink while a file is streaming.
	root, err := os.OpenRoot(u.folder)
	if err != nil {
		return fail()
	}
	defer root.Close()
	var file *os.File
	var temporary string
	for attempt := 0; attempt < 10; attempt++ {
		temporary = ".hi-upload-" + rand.Text() + ".part"
		file, err = root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if !errors.Is(err, os.ErrExist) {
			break
		}
	}
	if err != nil {
		return fail()
	}
	defer root.Remove(temporary)
	defer file.Close()
	n, err := io.Copy(file, contextReader{ctx, io.LimitReader(body, size+1)})
	if err != nil || n != size || ctx.Err() != nil {
		return fail()
	}
	if (u.folderUID != -1 || u.folderGID != -1) && file.Chown(u.folderUID, u.folderGID) != nil {
		return fail()
	}
	if file.Chmod(0644) != nil || file.Sync() != nil || file.Close() != nil {
		return fail()
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for suffix := 0; suffix <= 99; suffix++ {
		candidate := name
		if suffix > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, suffix, ext)
		}
		if ctx.Err() != nil {
			return fail()
		}
		err = root.Link(temporary, candidate)
		if err == nil {
			if root.Remove(temporary) != nil {
				return fail()
			}
			directory, err := root.Open(".")
			if err != nil {
				return fail()
			}
			err = directory.Sync()
			directory.Close()
			if err != nil {
				return fail()
			}
			return plugin.UploadResult{Name: candidate, Message: "Saved to folder"}, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fail()
		}
	}
	return plugin.UploadResult{}, errors.New("Folder already contains all 100 variants of this name")
}
