// Package backup preserves application files without interpreting historical schemas.
// Callers must drain all application writers before Create or Install.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"time"
)

const Version = 1
const ManifestName = "manifest.json"

var ErrInvalid = errors.New("invalid or unsupported backup")

type Limits struct {
	Files                 int
	FileBytes, TotalBytes int64
}

func DefaultLimits() Limits { return Limits{1000000, 8 << 30, 256 << 30} }

type Entry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Schema     string            `json:"inventory_schema,omitempty"`
	Build      string            `json:"build,omitempty"`
	Version    int               `json:"version"`
	Product    string            `json:"product"`
	Created    time.Time         `json:"created"`
	Files      []Entry           `json:"files"`
	Config     map[string]string `json:"config,omitempty"`
	Exclusions []string          `json:"exclusions"`
}
type Plan struct {
	Manifest  Manifest `json:"manifest"`
	Digest    string   `json:"digest"`
	Bytes     int64    `json:"bytes"`
	Directory string   `json:"-"`
}

func safe(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\\x00") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.HasPrefix(p, ".backup-") && p != ManifestName
}
func digest(m Manifest) string {
	b, _ := json.Marshal(m)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func copyContext(ctx context.Context, w io.Writer, r io.Reader) (int64, error) {
	return io.Copy(w, contextReader{ctx, r})
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

// Create streams a secret-bearing archive. Config must be an explicit allowlist,
// never os.Environ. External configured paths are deliberately not followed.
func Create(ctx context.Context, root, product string, config map[string]string, out io.Writer, lim Limits) (Manifest, error) {
	m := Manifest{Schema: "5", Version: Version, Product: product, Created: time.Now().UTC(), Config: config, Files: []Entry{}, Exclusions: []string{"External databases, upload content, media libraries, deployment files, signer keys and agent installations require separate backups."}}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				m.Build = setting.Value
			}
		}
	}
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if p == root {
			return nil
		}
		rel, e := filepath.Rel(root, p)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".backup-") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !safe(rel) {
			return ErrInvalid
		}
		base := strings.ToLower(d.Name())
		if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".pem") || base == "id_ed25519" || base == "id_rsa" || base == "docker-compose.yml" || base == "compose.yml" || base == "compose.yaml" {
			m.Exclusions = append(m.Exclusions, "Excluded deployment or signing material: "+rel)
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
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
		total += info.Size()
		if info.Size() > lim.FileBytes || total > lim.TotalBytes || len(m.Files) >= lim.Files {
			return ErrInvalid
		}
		f, e := os.Open(p)
		if e != nil {
			return e
		}
		h := sha256.New()
		n, e := copyContext(ctx, h, f)
		f.Close()
		if e != nil {
			return e
		}
		m.Files = append(m.Files, Entry{rel, n, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return m, err
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	b, err := json.Marshal(m)
	if err != nil {
		return m, err
	}
	if len(b) > 64<<20 {
		return m, ErrInvalid
	}
	if err = tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0600, Size: int64(len(b)), Typeflag: tar.TypeReg}); err == nil {
		_, err = tw.Write(b)
	}
	if err != nil {
		return m, err
	}
	for _, e := range m.Files {
		if err = tw.WriteHeader(&tar.Header{Name: e.Path, Mode: 0600, Size: e.Size, Typeflag: tar.TypeReg}); err != nil {
			return m, err
		}
		f, er := os.Open(filepath.Join(root, filepath.FromSlash(e.Path)))
		if er != nil {
			return m, er
		}
		h := sha256.New()
		n, er := copyContext(ctx, io.MultiWriter(tw, h), f)
		f.Close()
		if er != nil {
			return m, er
		}
		if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
			return m, errors.New("application changed during backup")
		}
	}
	if err = tw.Close(); err != nil {
		return m, err
	}
	return m, gz.Close()
}

// Stage validates every entry before returning a plan; no active data is changed.
func Stage(ctx context.Context, in io.Reader, parent string, lim Limits) (p Plan, err error) {
	p.Directory, err = os.MkdirTemp(parent, ".backup-stage-")
	if err != nil {
		return p, err
	}
	defer func() {
		if err != nil {
			os.RemoveAll(p.Directory)
		}
	}()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return p, ErrInvalid
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil || h.Name != ManifestName || h.Typeflag != tar.TypeReg || h.Size < 0 || h.Size > 64<<20 {
		return p, ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(tr, 64<<20+1))
	if err != nil || json.Unmarshal(b, &p.Manifest) != nil || p.Manifest.Version != Version || len(p.Manifest.Files) > lim.Files {
		return p, ErrInvalid
	}
	expected := map[string]Entry{}
	for _, e := range p.Manifest.Files {
		if !safe(e.Path) || e.Size < 0 || e.Size > lim.FileBytes || len(e.SHA256) != 64 {
			return p, ErrInvalid
		}
		if _, ok := expected[e.Path]; ok {
			return p, ErrInvalid
		}
		expected[e.Path] = e
		p.Bytes += e.Size
		if p.Bytes > lim.TotalBytes {
			return p, ErrInvalid
		}
	}
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return p, ErrInvalid
		}
		want, ok := expected[h.Name]
		if !ok || seen[h.Name] || !safe(h.Name) || h.Typeflag != tar.TypeReg || h.Size != want.Size {
			return p, ErrInvalid
		}
		seen[h.Name] = true
		dst := filepath.Join(p.Directory, filepath.FromSlash(h.Name))
		if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
			return p, e
		}
		f, e := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return p, e
		}
		hash := sha256.New()
		n, e := copyContext(ctx, io.MultiWriter(f, hash), tr)
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			return p, e
		}
		if closeErr != nil {
			return p, closeErr
		}
		if n != want.Size || hex.EncodeToString(hash.Sum(nil)) != want.SHA256 {
			return p, ErrInvalid
		}
	}
	if len(seen) != len(expected) {
		return p, ErrInvalid
	}
	if n, e := io.Copy(io.Discard, io.LimitReader(contextReader{ctx, gz}, 1025)); e != nil || n > 1024 {
		return p, ErrInvalid
	}
	p.Digest = digest(p.Manifest)
	return p, nil
}
