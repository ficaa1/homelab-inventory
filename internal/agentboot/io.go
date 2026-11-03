package agentboot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

type Config struct {
	Dir       string `json:"dir"`
	Public    string `json:"public"`
	URL       string `json:"api_url"`
	TokenFile string `json:"token_file"`
	AllowHTTP bool   `json:"allow_http,omitempty"`
}
type Runner struct {
	Config
	Host, Version string
	Timeout       time.Duration
	Client        *http.Client
}

func (r Runner) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: time.Minute, CheckRedirect: httpx.RefuseRedirects()}
}
func (r Runner) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	u, e := url.Parse(r.URL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Scheme != "https" && u.Scheme != "http" {
		return nil, failure("check", "network_unreachable")
	}
	if u.Scheme == "http" && !r.AllowHTTP && u.Hostname() != "localhost" && !loopback(u.Hostname()) {
		return nil, failure("check", "network_unreachable")
	}
	f, e := os.Open(r.TokenFile)
	if e != nil {
		return nil, failure("state", "permission_denied")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, failure("state", "permission_denied")
	}
	b, e := io.ReadAll(io.LimitReader(f, 4097))
	token := strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r")
	if e != nil || len(b) > 4096 || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n") {
		return nil, failure("state", "permission_denied")
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.URL, "/")+path, body)
	if e != nil {
		return nil, failure("check", "network_unreachable")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := r.client().Do(req)
	if e != nil {
		return nil, failure("check", "network_unreachable")
	}
	return resp, nil
}
func (r Runner) fetchOffer(ctx context.Context) ([]byte, error) {
	resp, e := r.request(ctx, "GET", "/agent/manifest?protocol=1&host="+url.QueryEscape(r.Host), nil)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, failure("check", "http_status")
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(b) > 65536 {
		return nil, failure("verify", "signature_invalid")
	}
	return b, nil
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// Each fault boundary is a no-op in production; the integration harness builds
// an explicitly tagged launcher to kill or fail real durability operations.
func (r Runner) write(path string, b []byte, mode os.FileMode, tag string) error {
	if e := faultPoint(tag + "-before"); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".write-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	e = f.Chmod(mode)
	if e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = faultPoint(tag + "-fsync"); e != nil {
		return e
	}
	if e = os.Rename(f.Name(), path); e != nil {
		return e
	}
	if e = faultPoint(tag + "-rename"); e != nil {
		return e
	}
	if e = syncDir(filepath.Dir(path)); e != nil {
		return e
	}
	return faultPoint(tag + "-dirsync")
}
func (r Runner) post(ctx context.Context, c Control, data []byte, id agentproto.Identity, nonce string) (*agentproto.Receipt, error) {
	if _, e := agentproto.ValidateReport(data, r.Host, nonce, id); e != nil {
		return nil, e
	}
	if e := faultPoint("receipt-before"); e != nil {
		return nil, e
	}
	resp, e := r.request(ctx, "POST", "/agent/report", bytes.NewReader(data))
	if e != nil {
		return nil, failure("post", "post_rejected")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, failure("post", "post_rejected")
	}
	var rec agentproto.Receipt
	dec := json.NewDecoder(io.LimitReader(resp.Body, 8193))
	dec.DisallowUnknownFields()
	if dec.Decode(&rec) != nil || dec.Decode(new(any)) != io.EOF || rec.Protocol != 1 || rec.Host != r.Host || rec.Nonce != nonce || rec.Identity != id || rec.Body != fmt.Sprintf("%x", sha256.Sum256(data)) || rec.Received.IsZero() {
		return nil, failure("post", "post_rejected")
	}
	if e = faultPoint("receipt-accepted"); e != nil {
		return nil, e
	}
	return &rec, nil
}
func (r Runner) status(ctx context.Context, c Control) error {
	u := c.Update
	u.Mode = "signed"
	u.Launcher = r.Version
	u.Sequence = c.Active.Sequence
	u.Transaction = c.State
	if c.Candidate != nil {
		id := c.Candidate.Identity
		u.Candidate = &id
	}
	if c.Active.Sequence > 0 {
		id := c.Active.Identity
		u.Active = &id
		u.Updater = id.Version
	}
	if c.Fallback.Sequence > 0 {
		id := c.Fallback.Identity
		u.Fallback = &id
	}
	b, _ := json.Marshal(struct {
		Host   string            `json:"host"`
		Update agentproto.Update `json:"update"`
	}{r.Host, u})
	resp, e := r.request(ctx, "POST", "/agent/status", bytes.NewReader(b))
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		return failure("post", "post_rejected")
	}
	return nil
}

type safeError struct{ Stage, Code string }

func (e *safeError) Error() string     { return e.Stage + ": " + e.Code }
func failure(stage, code string) error { return &safeError{stage, code} }
func classify(stage string, err error) *safeError {
	var s *safeError
	if errors.As(err, &s) {
		return s
	}
	code := "unknown"
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		code = "disk_full"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		code = "permission_denied"
	case errors.Is(err, syscall.EROFS):
		code = "read_only_fs"
	case errors.Is(err, agentproto.ErrOutput):
		code = "trial_invalid_output"
	case errors.Is(err, context.DeadlineExceeded):
		code = "trial_timeout"
	case errors.Is(err, fsx.ErrLocked):
		code = "lock_busy"
	}
	return &safeError{stage, code}
}
func mark(c *Control, stage string, err error) {
	s := classify(stage, err)
	f := &agentproto.Failure{At: time.Now().UTC(), Sequence: c.Update.Target, Stage: s.Stage, Code: s.Code}
	if c.Update.FirstFailure == nil {
		c.Update.FirstFailure = f
	}
	c.Update.LastFailure = f
	c.Update.Stage = s.Stage
	c.Update.Code = s.Code
	c.Update.Result = "failed"
	if c.Update.Failures < 1000000 {
		c.Update.Failures++
	}
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !st.Mode().IsRegular() {
		return nil, errCorrupt
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errCorrupt
	}
	return b, nil
}
