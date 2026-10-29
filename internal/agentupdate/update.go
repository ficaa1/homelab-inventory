// Package agentupdate is the explicitly privileged runner. Discover never imports it.
package agentupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type State struct {
	High     uint64                `json:"high"` // never decreased, including rollback
	Current  uint64                `json:"current"`
	Previous uint64                `json:"previous"`
	Pending  bool                  `json:"pending"`
	Update   inventory.AgentUpdate `json:"update"`
}
type Runner struct {
	AllowHTTP                     bool
	Dir, Public, URL, Token, Host string
	Client                        *http.Client
}

func (r Runner) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: time.Minute, CheckRedirect: httpx.RefuseRedirects()}
}
func (r Runner) request(ctx context.Context, method, path string, body io.Reader, limit int64) ([]byte, int, error) {
	if err := ValidateURL(r.URL, r.AllowHTTP); err != nil {
		return nil, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, method, r.URL+path, body)
	if err != nil {
		return nil, 0, errors.New("invalid API URL")
	}
	req.Header.Set("Authorization", "Bearer "+r.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return nil, 0, errors.New("API unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("API HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, resp.StatusCode, errors.New("response read failed or oversized")
	}
	return data, resp.StatusCode, nil
}
func (r Runner) load() (State, error) {
	var s State
	b, e := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if e == nil {
		e = json.Unmarshal(b, &s)
	}
	return s, e
}
func (r Runner) save(s State) error {
	return agentrelease.WriteJSON(filepath.Join(r.Dir, "state.json"), s)
}
func (r Runner) bundle(seq uint64) string {
	return filepath.Join(r.Dir, "releases", strconv.FormatUint(seq, 10))
}
func platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Stage verifies every component before executing even its read-only self-test.
func (r Runner) Stage(ctx context.Context, m agentrelease.Manifest, fetch func(agentrelease.Artifact) ([]byte, error)) error {
	if err := os.MkdirAll(filepath.Join(r.Dir, "releases"), 0700); err != nil {
		return err
	}
	temp, err := os.MkdirTemp(filepath.Join(r.Dir, "releases"), ".stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	for _, component := range []string{"discover"} {
		a, ok := m.Artifacts[platform()+"/"+component]
		if !ok {
			return errors.New("platform missing")
		}
		data, e := fetch(a)
		if e != nil {
			return e
		}
		if e = agentrelease.Check(data, a); e != nil {
			return e
		}
		if e = fsx.WriteAtomic(filepath.Join(temp, component), data, 0755); e != nil {
			return e
		}
	}
	if err = command(ctx, filepath.Join(temp, "discover"), []string{"-list"}, nil); err != nil {
		return errors.New("discover self-test failed")
	}
	if err = agentrelease.WriteJSON(filepath.Join(temp, "metadata.json"), m); err != nil {
		return err
	}
	if err = os.RemoveAll(r.bundle(m.Sequence)); err != nil {
		return err
	}
	if err = os.Rename(temp, r.bundle(m.Sequence)); err != nil {
		return err
	}
	return syncDir(filepath.Join(r.Dir, "releases"))
}
func command(ctx context.Context, path string, args []string, out io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.WaitDelay = 2 * time.Second
	// Credentials never go into discover's environment or command line.
	c.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	c.Stdout = out
	// Collector stderr may contain local details; only safe outcome errors are logged.
	c.Stderr = io.Discard
	return c.Run()
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (r Runner) point(seq uint64) error {
	tmp := filepath.Join(r.Dir, ".current")
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("releases", strconv.FormatUint(seq, 10)), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(r.Dir, "current")); err != nil {
		return err
	}
	return syncDir(r.Dir)
}

// Activate journals the high-water mark before swapping. A power loss during a
// trial restores the previous bundle at the next run; replay remains forbidden.
func (r Runner) Activate(s *State, m agentrelease.Manifest) error {
	if m.Sequence <= s.High {
		return errors.New("downgrade or replay refused")
	}
	next := *s
	next.High = m.Sequence
	next.Previous = s.Current
	next.Current = m.Sequence
	next.Pending = true
	next.Update.Sequence = m.Sequence
	if err := r.save(next); err != nil {
		return err
	}
	*s = next
	if err := r.point(m.Sequence); err != nil {
		if rollbackErr := r.Rollback(s); rollbackErr != nil {
			return fmt.Errorf("activation and rollback failed: %w", rollbackErr)
		}
		return err
	}
	// Convenience recovery path; current and previous bundles remain immutable.
	prev := filepath.Join(r.Dir, "discover.prev")
	_ = os.Remove(prev)
	_ = os.Symlink(filepath.Join(r.bundle(s.Previous), "discover"), prev)
	return nil
}
func (r Runner) Rollback(s *State) error {
	if s.Previous == 0 || s.Previous == s.Current {
		return errors.New("no previous release")
	}
	if err := r.point(s.Previous); err != nil {
		return err
	}
	s.Current = s.Previous
	s.Pending = false
	s.Update.Sequence = s.Current
	s.Update.Result = "rollback"
	s.Update.Failures++
	if err := r.save(*s); err != nil {
		log.Printf("agentupdate: rollback state persistence failed: %v", err)
	}
	return nil
}

// Initialize pins a trusted first release using the installed public key and
// the exact signed artifact hashes. It refuses to reset replay state.
func (r Runner) Initialize(data []byte, input string) error {
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		return err
	}
	unlock, err := fsx.LockDir(r.Dir)
	if err != nil {
		return err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(r.Dir, "state.json")); err == nil {
		return errors.New("already initialized")
	} else if !os.IsNotExist(err) {
		return err
	}
	pub, err := os.ReadFile(r.Public)
	if err != nil {
		return err
	}
	m, err := agentrelease.Verify(data, pub)
	if err != nil {
		return err
	}
	err = r.Stage(context.Background(), m, func(a agentrelease.Artifact) ([]byte, error) { return os.ReadFile(filepath.Join(input, a.Name)) })
	if err != nil {
		return err
	}
	lock, e := os.OpenFile(filepath.Join(r.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	if e = lock.Close(); e != nil {
		return e
	}
	s := State{High: m.Sequence, Current: m.Sequence, Update: inventory.AgentUpdate{Sequence: m.Sequence, Result: "installed"}}
	if err = r.save(s); err != nil {
		return err
	}
	return r.point(m.Sequence)
}
func (r Runner) check(ctx context.Context, s *State) error {
	data, status, err := r.request(ctx, "GET", "/agent/manifest?host="+url.QueryEscape(r.Host), nil, 65536)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	pub, err := os.ReadFile(r.Public)
	if err != nil {
		return err
	}
	m, err := agentrelease.Verify(data, pub)
	if err != nil {
		return err
	}
	// An identical offer is an ordinary no-op. Different bytes at the same
	// sequence, or a rolled-back offer, cannot be activated again.
	if m.Sequence == s.Current && m.Sequence == s.High {
		return nil
	}
	if m.Sequence <= s.High {
		return errors.New("downgrade or replay refused")
	}
	err = r.Stage(ctx, m, func(a agentrelease.Artifact) ([]byte, error) {
		b, _, e := r.request(ctx, "GET", "/agent/artifacts/"+strconv.FormatUint(m.Sequence, 10)+"/"+a.Name, nil, a.Size)
		return b, e
	})
	if err != nil {
		return err
	}
	return r.Activate(s, m)
}

// Run is the pinned updater/supervisor. Only discover is replaceable.
func (r Runner) Run(ctx context.Context) error {
	if r.Token == "" {
		return errors.New("bearer token required")
	}
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		return err
	}
	unlock, err := fsx.LockDir(r.Dir)
	if errors.Is(err, fsx.ErrLocked) {
		return errors.New("another run holds the lock")
	}
	if err != nil {
		return err
	}
	defer unlock()
	s, err := r.load()
	if err != nil {
		return err
	}
	if s.Pending {
		if err = r.Rollback(&s); err != nil {
			// ENOSPC can prevent even the recovery symlink. Report directly from
			// the immutable previous bundle while leaving the journal pending.
			log.Printf("agentupdate: recovery pointer failed: %v", err)
			s.Current = s.Previous
			s.Update.Sequence = s.Current
			s.Update.Result = "rollback"
			s.Update.Failures++
			return r.report(ctx, s)
		}
	}
	s.Update.AttemptedAt = time.Now().UTC()
	if err = r.check(ctx, &s); err != nil {
		log.Printf("agentupdate: %v", err)
		s.Update.Result = "failed"
		s.Update.Failures++
	} else {
		s.Update.Result = "ok"
		s.Update.Failures = 0
	}
	// Persisting status is best effort for an old bundle on a full disk. A trial
	// is never committed until both the report and state write have succeeded.
	if e := r.save(s); e != nil {
		log.Printf("agentupdate: status write failed: %v", e)
	}
	if s.Pending {
		s.Update.Result = "trial"
	}
	err = r.report(ctx, s)
	if err != nil && s.Previous != 0 && s.Previous != s.Current {
		log.Printf("agentupdate: report failed; restoring previous bundle")
		if e := r.Rollback(&s); e != nil {
			log.Printf("agentupdate: rollback pointer failed: %v", e)
			s.Current = s.Previous
			s.Update.Sequence = s.Current
			s.Update.Result = "rollback"
			s.Update.Failures++
		}
		return r.report(ctx, s)
	}
	if err != nil {
		return err
	}
	if s.Pending {
		s.Pending = false
		if err = r.save(s); err != nil {
			return err
		}
	}
	return nil
}

// Reporting stays in the pinned supervisor. A signed updater/wrapper returning
// success without posting must never be enough to commit a discover trial.
func (r Runner) report(ctx context.Context, s State) error {
	b, err := os.ReadFile(filepath.Join(r.bundle(s.Current), "metadata.json"))
	if err != nil {
		return err
	}
	var m agentrelease.Manifest
	if json.Unmarshal(b, &m) != nil || m.Sequence != s.Current {
		return errors.New("invalid installed metadata")
	}
	var buf cappedOutput
	if err := command(ctx, filepath.Join(r.bundle(s.Current), "discover"), []string{"-compact"}, &buf); err != nil {
		return errors.New("discover failed")
	}
	// Post discover's own JSON with agent_update merged in, so fields added to
	// the schema after this updater was installed are not dropped.
	var raw map[string]json.RawMessage
	if json.Unmarshal(buf.Bytes(), &raw) != nil {
		return errors.New("invalid discover output")
	}
	// encoding/json matches keys case-insensitively with Unicode folding, so
	// two keys that fold together could validate as one host and store as another.
	keys := make([]string, 0, len(raw))
	for k := range raw {
		// The updater owns agent_update: the exact key is overwritten below, any
		// case variant would survive beside it and merge on decode.
		if k != "agent_update" && strings.EqualFold(k, "agent_update") {
			return errors.New("invalid discover output")
		}
		for _, o := range keys {
			if strings.EqualFold(k, o) {
				return errors.New("invalid discover output")
			}
		}
		keys = append(keys, k)
	}
	if raw["agent_update"], err = json.Marshal(&s.Update); err != nil {
		return err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	// Validate the exact bytes that are posted.
	var inv inventory.Inventory
	if json.Unmarshal(data, &inv) != nil || inv.Host.Hostname != r.Host || inv.SchemaVersion != inventory.SchemaVersion || inv.Tool.Version != m.Version || inv.Tool.Revision != m.Revision {
		return errors.New("invalid discover output or build identity")
	}
	if _, _, err = r.request(ctx, "POST", "/inventory", bytes.NewReader(data), 65536); err != nil {
		return err
	}
	// Partial inventories from the established build remain useful. A new build
	// must pass strict diagnostics before its first trial can commit.
	if s.Pending {
		for _, d := range inv.Diagnostics {
			if d.Severity == inventory.SeverityError {
				return errors.New("strict diagnostic failure")
			}
		}
	}
	return nil
}

// Bound output while reading, before a broken collector can exhaust host RAM.
type cappedOutput struct{ buf bytes.Buffer }

func (b *cappedOutput) Bytes() []byte { return b.buf.Bytes() }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > (16<<20)-b.buf.Len() {
		return 0, errors.New("discover output too large")
	}
	return b.buf.Write(p)
}

// Recover is an operator-only local rollback for failures discovered after a trial.
func (r Runner) Recover() error {
	unlock, err := fsx.LockDir(r.Dir)
	if errors.Is(err, fsx.ErrLocked) {
		return errors.New("another run holds the lock")
	}
	if err != nil {
		return err
	}
	defer unlock()
	s, err := r.load()
	if err != nil {
		return err
	}
	return r.Rollback(&s)
}
