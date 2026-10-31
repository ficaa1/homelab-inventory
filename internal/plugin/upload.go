package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

const MaxUploadBytes int64 = 2 << 30
const UploadTimeout = 30 * time.Minute

// UploadError carries a public HTTP status without exposing submitted values.
type UploadError struct {
	Status  int
	Message string
}

func (e *UploadError) Error() string               { return e.Message }
func uploadError(status int, message string) error { return &UploadError{status, message} }

type TargetStatus struct {
	Plugin     string `json:"plugin"`
	PluginName string `json:"plugin_name"`
	UploadTarget
}

type FolderFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}
type FolderBrowser interface {
	FolderFiles(context.Context) ([]FolderFile, error)
	OpenFolderFile(context.Context, string) (io.ReadCloser, int64, error)
}

func validTarget(t UploadTarget) bool {
	if !viewPattern.MatchString(t.ID) || t.Label == "" || t.MaxBytes <= 0 {
		return false
	}
	for _, ext := range t.Accept {
		if len(ext) < 2 || ext[0] != '.' || ext != strings.ToLower(ext) || strings.ContainsAny(ext, "/\\\x00 \r\n") {
			return false
		}
	}
	return true
}
func (r *Registry) UploadTargets() []TargetStatus {
	out := []TargetStatus{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.op.Lock()
		if u, ok := e.plugin.(Uploader); ok && e.stored.Enabled && e.ready {
			for _, t := range u.UploadTargets() {
				if validTarget(t) {
					out = append(out, TargetStatus{e.manifest.ID, e.manifest.Name, t})
				}
			}
		}
		e.op.Unlock()
	}
	return out
}

func (r *Registry) FolderFiles(ctx context.Context) ([]FolderFile, error) {
	for _, id := range r.ids() {
		e := r.entries[id]
		e.op.Lock()
		if e.stored.Enabled && e.ready {
			if b, ok := e.plugin.(FolderBrowser); ok {
				out, err := b.FolderFiles(ctx)
				e.op.Unlock()
				return out, err
			}
		}
		e.op.Unlock()
	}
	return nil, ErrNotFound
}
func (r *Registry) OpenFolderFile(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	for _, id := range r.ids() {
		e := r.entries[id]
		e.op.Lock()
		if e.stored.Enabled && e.ready {
			if b, ok := e.plugin.(FolderBrowser); ok {
				f, size, err := b.OpenFolderFile(ctx, name)
				e.op.Unlock()
				return f, size, err
			}
		}
		e.op.Unlock()
	}
	return nil, 0, ErrNotFound
}

// UploadName decodes encodeURIComponent-compatible UTF-8. Paths are refused,
// rather than silently dropping components and surprising the sender.
func UploadName(encoded string) (string, error) {
	if encoded == "" {
		return "", uploadError(400, "X-Upload-Name is required")
	}
	name, err := url.PathUnescape(encoded)
	if err != nil || !utf8.ValidString(name) || name == "" || len(name) > 200 || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
		return "", uploadError(422, "use a plain file name without paths or a leading dot (maximum 200 bytes)")
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", uploadError(422, "file name contains control characters")
		}
	}
	return name, nil
}

type uploadAudit struct {
	Time   time.Time     `json:"time"`
	Event  string        `json:"event"`
	Actor  string        `json:"actor"`
	Plugin string        `json:"plugin"`
	Target string        `json:"target"`
	Name   string        `json:"name"`
	Bytes  int64         `json:"bytes"`
	SHA256 string        `json:"sha256"`
	Result *UploadResult `json:"result,omitempty"`
	Error  string        `json:"error,omitempty"`
}

func (r *Registry) audit(a uploadAudit) error {
	data, err := json.Marshal(a)
	if err != nil {
		return errors.New("encoding upload audit")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fsx.Append(filepath.Join(filepath.Dir(r.path), "upload-audit.jsonl"), append(data, '\n'), fsx.SyncFileDir) != nil {
		return errors.New("writing upload audit; destination may already have accepted the file")
	}
	return nil
}

type countedReader struct {
	reader io.Reader
	mu     sync.Mutex
	bytes  int64
	digest hash.Hash
}

func (r *countedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.mu.Lock()
	r.bytes += int64(n)
	_, _ = r.digest.Write(p[:n])
	r.mu.Unlock()
	return n, err
}
func (r *countedReader) snapshot() (int64, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bytes, hex.EncodeToString(r.digest.Sum(nil))
}

// Upload serializes the complete operation against reconfiguration and records
// only metadata. On failure the digest covers the bytes actually consumed.
func (r *Registry) Upload(ctx context.Context, id, target, name string, size int64, body io.Reader, actor string) (UploadResult, error) {
	e := r.entries[id]
	if e == nil {
		return UploadResult{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	u, ok := e.plugin.(Uploader)
	if !ok || !e.stored.Enabled || !e.ready {
		return UploadResult{}, ErrNotFound
	}
	var selected *UploadTarget
	for _, t := range u.UploadTargets() {
		if t.ID == target && validTarget(t) {
			selected = &t
			break
		}
	}
	if selected == nil {
		return UploadResult{}, ErrNotFound
	}
	if _, err := UploadName(url.PathEscape(name)); err != nil {
		return UploadResult{}, err
	}
	if size < 0 {
		return UploadResult{}, uploadError(411, "Content-Length is required")
	}
	if size > selected.MaxBytes || size > MaxUploadBytes {
		return UploadResult{}, uploadError(413, "file exceeds the destination size limit")
	}
	if len(selected.Accept) > 0 {
		accepted := false
		for _, ext := range selected.Accept {
			accepted = accepted || strings.EqualFold(filepath.Ext(name), ext)
		}
		if !accepted {
			return UploadResult{}, uploadError(415, "file extension is not accepted by this destination")
		}
	}
	a := uploadAudit{Time: e.host.Now().UTC(), Event: "intent", Actor: actor, Plugin: id, Target: target, Name: name, Bytes: size}
	if err := r.audit(a); err != nil {
		return UploadResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, UploadTimeout)
	defer cancel()
	counted := &countedReader{reader: io.LimitReader(body, size+1), digest: sha256.New()}
	result, err := u.Upload(ctx, target, name, size, counted)
	count, digest := counted.snapshot()
	if err == nil && (count != size || ctx.Err() != nil) {
		err = errors.New("upload body was incomplete or exceeded its declared length")
	}
	a.Event, a.Time, a.Bytes, a.SHA256 = "outcome", e.host.Now().UTC(), count, digest
	result.Name, result.Message = e.host.safe(result.Name), e.host.safe(result.Message)
	if err != nil {
		err = errors.New(e.host.safe(err.Error()))
		a.Error = err.Error()
	} else {
		a.Result = &result
	}
	if auditErr := r.audit(a); auditErr != nil {
		return UploadResult{}, auditErr
	}
	return result, err
}
