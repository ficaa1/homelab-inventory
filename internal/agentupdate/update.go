// Package agentupdate is the explicitly privileged runner. Discover never imports it.
package agentupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type State struct {
	checkedOffer    bool
	Format          int                   `json:"format,omitempty"`
	Digest          string                `json:"digest,omitempty"`
	TrialStarted    bool                  `json:"trialStarted,omitempty"`
	TrialHealthy    bool                  `json:"trialHealthy,omitempty"`
	Errors          []string              `json:"errors,omitempty"`
	CandidateErrors []string              `json:"candidateErrors,omitempty"`
	High            uint64                `json:"high"` // never decreased, including rollback
	Current         uint64                `json:"current"`
	Previous        uint64                `json:"previous"`
	Pending         bool                  `json:"pending"`
	Update          inventory.AgentUpdate `json:"update"`
}
type Runner struct {
	AllowHTTP                     bool
	Dir, Public, URL, Token, Host string
	Updater                       string
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
		return nil, 0, fault("check", "network_unreachable", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, resp.StatusCode, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fault("check", "http_status", nil)
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
	if e == nil && (s.High == 0 || s.Current == 0 || s.Current > s.High || s.Format > 1 || s.Pending && (s.Previous == 0 || s.Previous == s.Current)) {
		e = errors.New("invalid updater state")
	}
	if s.Update.Failures > 1000000 {
		s.Update.Failures = 1000000
	}
	if s.Update.Failures < 0 {
		s.Update.Failures = 0
	}
	if e == nil && s.Format == 0 {
		s.Format = 1
		if s.Pending {
			s.TrialStarted = true
		}
		if s.High > s.Current && !s.Pending {
			s.Update.Rejected = s.High
			s.Update.Target = s.High
			failed(&s.Update, "state", fault("state", "unknown", nil))
			s.Update.Result = "blocked"
		}
		e = r.save(s)
	}
	return s, e
}
func (r Runner) loadReadOnly() (State, error) {
	var s State
	b, err := os.ReadFile(filepath.Join(r.Dir, "state.json"))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
func (r Runner) save(s State) error {
	s.Format = 1
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(filepath.Join(r.Dir, "state.json"), b, 0600)
}
func (r Runner) bundle(seq uint64) string {
	return filepath.Join(r.Dir, "releases", strconv.FormatUint(seq, 10))
}
func platform() string { return runtime.GOOS + "-" + runtime.GOARCH }

// Stage verifies every component before executing even its read-only self-test.
func (r Runner) Stage(ctx context.Context, m agentrelease.Manifest, fetch func(agentrelease.Artifact) ([]byte, error)) error {
	if err := os.MkdirAll(filepath.Join(r.Dir, "releases"), 0700); err != nil {
		return classify("stage_write", err)
	}
	temp, err := os.MkdirTemp(filepath.Join(r.Dir, "releases"), ".stage-")
	if err != nil {
		return classify("stage_write", err)
	}
	defer os.RemoveAll(temp)
	for _, component := range []string{"discover"} {
		a, ok := m.Artifacts[platform()+"/"+component]
		if !ok {
			return fault("verify", "platform_missing", nil)
		}
		data, e := fetch(a)
		if e != nil {
			return classify("download", e)
		}
		if e = agentrelease.Check(data, a); e != nil {
			return fault("verify", "hash_mismatch", e)
		}
		if e = fsx.WriteAtomic(filepath.Join(temp, component), data, 0755); e != nil {
			return classify("stage_write", e)
		}
	}
	if err = command(ctx, filepath.Join(temp, "discover"), []string{"-list"}, nil); err != nil {
		return executionError("selftest", err)
	}
	if err = agentrelease.WriteJSON(filepath.Join(temp, "metadata.json"), m); err != nil {
		return classify("stage_write", err)
	}
	if err = os.RemoveAll(r.bundle(m.Sequence)); err != nil {
		return classify("stage_write", err)
	}
	if err = os.Rename(temp, r.bundle(m.Sequence)); err != nil {
		return classify("stage_write", err)
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
	err := c.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
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
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return r.activate(s, m, agentrelease.Digest(b))
}
func (r Runner) activate(s *State, m agentrelease.Manifest, digest string) error {
	if m.Sequence <= s.High {
		return errors.New("downgrade or replay refused")
	}
	next := *s
	next.High = m.Sequence
	next.Digest = digest
	next.TrialHealthy = false
	next.TrialStarted = false
	next.Previous = s.Current
	next.Current = m.Sequence
	next.Pending = true
	next.Update.Sequence = m.Sequence
	if err := r.save(next); err != nil {
		return err
	}
	*s = next
	if err := r.point(m.Sequence); err != nil {
		return classify("activate", err)
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
	next := *s
	next.Current = s.Previous
	next.Pending = false
	next.TrialStarted = false
	next.TrialHealthy = false
	next.Update.Rejected = s.High
	next.Update.Sequence = next.Current
	next.Update.Result = "rollback"
	if next.Update.FirstFailure == nil {
		failed(&next.Update, "trial", trialFault("unknown", nil))
		next.Update.Result = "rollback"
	}
	if err := r.save(next); err != nil {
		// Preserve a coherent in-memory fallback for emergency reporting. A later
		// best-effort save must never write pending=true with current=previous.
		*s = next
		return classify("state", err)
	}
	*s = next
	return r.point(s.Current)
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
	var envelope agentrelease.Signed
	_ = json.Unmarshal(data, &envelope)
	s := State{Format: 1, Digest: agentrelease.Digest(envelope.Manifest), High: m.Sequence, Current: m.Sequence, Update: inventory.AgentUpdate{Sequence: m.Sequence, Result: "installed"}}
	if err = r.save(s); err != nil {
		return err
	}
	return r.point(m.Sequence)
}
func (r Runner) check(ctx context.Context, s *State) error {
	s.checkedOffer = false
	data, status, err := r.request(ctx, "GET", "/agent/manifest?host="+url.QueryEscape(r.Host), nil, 65536)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		return nil
	}
	pub, err := os.ReadFile(r.Public)
	if err != nil {
		return classify("verify", err)
	}
	m, err := agentrelease.Verify(data, pub)
	if err != nil {
		return fault("verify", "signature_invalid", err)
	}
	s.checkedOffer = true
	s.Update.Target = m.Sequence
	// Hash exact signed manifest bytes, independent of envelope/key rotation.
	var envelope agentrelease.Signed
	if json.Unmarshal(data, &envelope) != nil {
		return fault("verify", "signature_invalid", nil)
	}
	digest := agentrelease.Digest(envelope.Manifest)
	if m.Sequence == s.High && s.Digest != "" && s.Digest != digest {
		return fault("verify", "manifest_conflict", nil)
	}
	if m.Sequence == s.High && s.Digest == "" {
		if m.Sequence == s.Current {
			data, err := os.ReadFile(filepath.Join(r.bundle(s.Current), "metadata.json"))
			if err != nil {
				return classify("state", err)
			}
			var installed agentrelease.Manifest
			if json.Unmarshal(data, &installed) != nil {
				return fault("state", "state_corrupt", nil)
			}
			old, _ := json.Marshal(installed)
			offered, _ := json.Marshal(m)
			if !bytes.Equal(old, offered) {
				return fault("verify", "manifest_conflict", nil)
			}
		}
		s.Digest = digest
	}
	if m.Sequence == s.Update.Rejected {
		s.Update.Result = "blocked"
		return nil
	}
	if m.Sequence == s.Current && m.Sequence == s.High {
		if s.Digest == "" {
			s.Digest = digest
		}
		return nil
	}
	if m.Sequence <= s.High {
		return fault("check", "replay_refused", nil)
	}
	if err = r.Stage(ctx, m, func(a agentrelease.Artifact) ([]byte, error) {
		b, _, e := r.request(ctx, "GET", "/agent/artifacts/"+strconv.FormatUint(m.Sequence, 10)+"/"+a.Name, nil, a.Size)
		if e != nil {
			o := classify("download", e)
			return nil, fault("download", o.code, e)
		}
		return b, e
	}); err != nil {
		return err
	}
	// Capture a fresh baseline before switching; no message text is retained.
	{
		_, inv, e := r.collect(ctx, *s)
		if e != nil {
			return e
		}
		s.Errors = diagnosticIDs(inv)
	}
	if err = r.activate(s, m, digest); err != nil {
		return err
	}
	return nil
}

// Run is the pinned updater/supervisor. Only discover is replaceable.
func (r Runner) Run(ctx context.Context) (runErr error) {
	if r.Token == "" {
		return errors.New("bearer token required")
	}
	s := State{}
	r.stamp(&s.Update, "signed")
	// This channel works even when the lock, state, collector or inventory fails.
	defer func() {
		r.stamp(&s.Update, "signed")
		if e := r.postStatus(ctx, s.Update); runErr == nil && e != nil {
			runErr = e
		}
	}()
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		failed(&s.Update, "state", err)
		return classify("state", err)
	}
	unlock, err := fsx.LockDir(r.Dir)
	if err != nil {
		if prior, e := r.loadReadOnly(); e == nil {
			s = prior
		}
		if errors.Is(err, fsx.ErrLocked) {
			err = fault("lock", "lock_busy", nil)
		}
		failed(&s.Update, "lock", err)
		return classify("lock", err)
	}
	defer unlock()
	s, err = r.load()
	if err != nil {
		s = State{}
		failed(&s.Update, "state", fault("state", "state_corrupt", nil))
		return fault("state", "state_corrupt", nil)
	}
	r.stamp(&s.Update, "signed")
	s.Update.AttemptedAt = time.Now().UTC()
	if s.Pending && s.TrialStarted && !s.TrialHealthy {
		// Execution interrupted before durable validation: quarantine conservatively.
		failed(&s.Update, "trial", trialFault("unknown", nil))
		if e := r.Rollback(&s); e != nil {
			s.Current = s.Previous
			s.Update.Sequence = s.Current
			s.Update.Result = "rollback"
			_ = r.save(s)
			return r.report(ctx, s)
		}
	}
	checkFailed := false
	if !s.Pending {
		if err = r.check(ctx, &s); err != nil {
			failed(&s.Update, "check", err)
			checkFailed = true
		}
	}
	if s.Pending {
		s.Update.Result = "trial"
		if e := r.point(s.Current); e != nil {
			failed(&s.Update, "activate", e)
			_ = r.save(s)
			fallback := s
			fallback.Current = s.Previous
			fallback.Update.Sequence = s.Previous
			return r.report(ctx, fallback)
		}
		s.TrialStarted = true
	}
	if err = r.save(s); err != nil {
		failed(&s.Update, "state", err)
		if s.Pending {
			return classify("state", err)
		}
	}
	data, inv, err := r.collect(ctx, s)
	if err == nil && s.Pending {
		ids := diagnosticIDs(inv)
		for _, id := range ids {
			if !contains(s.Errors, id) {
				err = trialFault("trial_new_errors", nil)
				break
			}
		}
		if err == nil {
			s.TrialHealthy = true
			s.CandidateErrors = ids
			if e := r.save(s); e != nil {
				err = classify("state", e)
			}
		}
	}
	if err == nil {
		if _, _, e := r.request(ctx, "POST", "/inventory", bytes.NewReader(data), 65536); e != nil {
			err = fault("post", "post_rejected", e)
		}
	}
	if err != nil {
		o := classify("trial", err)
		failed(&s.Update, o.stage, o)
		// Only invalid/crashed trial code is quarantined. Delivery stays pending.
		if o.trial && s.Previous != 0 && s.Previous != s.Current {
			if e := r.Rollback(&s); e != nil {
				s.Current = s.Previous
				s.Update.Sequence = s.Current
				s.Update.Result = "rollback"
			}
			_ = r.save(s)
			return r.report(ctx, s)
		}
		if !o.trial && s.Pending && !s.TrialHealthy {
			s.TrialStarted = false
		}
		_ = r.save(s)
		return o
	}
	if s.Pending {
		next := s
		next.Pending = false
		next.TrialHealthy = false
		next.TrialStarted = false
		next.Errors = s.CandidateErrors
		next.CandidateErrors = nil
		resolved(&next.Update)
		if err = r.save(next); err != nil {
			failed(&s.Update, "state", err)
			return classify("state", err)
		}
		s = next
	} else {
		// An old bundle posting successfully cannot resolve a failed newer offer.
		s.Errors = diagnosticIDs(inv)
		if !checkFailed && s.checkedOffer && s.Update.Rejected == 0 && s.Update.Target == s.Current {
			resolved(&s.Update)
		}
		if err = r.save(s); err != nil {
			failed(&s.Update, "state", err)
			return classify("state", err)
		}
	}
	return nil
}

// Reporting stays in the pinned supervisor. A signed updater/wrapper returning
// success without posting must never be enough to commit a discover trial.
func (r Runner) collect(ctx context.Context, s State) ([]byte, *inventory.Inventory, error) {
	b, err := os.ReadFile(filepath.Join(r.bundle(s.Current), "metadata.json"))
	if err != nil {
		return nil, nil, classify("state", err)
	}
	var m agentrelease.Manifest
	if json.Unmarshal(b, &m) != nil || m.Sequence != s.Current {
		return nil, nil, fault("state", "state_corrupt", nil)
	}
	var buf cappedOutput
	if err := command(ctx, filepath.Join(r.bundle(s.Current), "discover"), []string{"-compact"}, &buf); err != nil {
		return nil, nil, executionError("trial", err)
	}
	// Post discover's own JSON with agent_update merged in, so fields added to
	// the schema after this updater was installed are not dropped.
	data, err := mergeUpdate(buf.Bytes(), s.Update)
	if err != nil {
		return nil, nil, trialFault("trial_invalid_output", nil)
	}
	// Validate the exact bytes that are posted.
	var inv inventory.Inventory
	if json.Unmarshal(data, &inv) != nil || inv.Host.Hostname != r.Host || inv.SchemaVersion != inventory.SchemaVersion || inv.Tool.Version != m.Version || inv.Tool.Revision != m.Revision {
		return nil, nil, trialFault("trial_invalid_output", nil)
	}
	return data, &inv, nil
}
func (r Runner) report(ctx context.Context, s State) error {
	data, _, err := r.collect(ctx, s)
	if err != nil {
		return err
	}
	_, _, err = r.request(ctx, "POST", "/inventory", bytes.NewReader(data), 65536)
	return err
}
func diagnosticIDs(inv *inventory.Inventory) []string {
	ids := []string{}
	for _, d := range inv.Diagnostics {
		if d.Severity == inventory.SeverityError {
			ids = append(ids, d.Collector+"\x00"+d.Code)
		}
	}
	sort.Strings(ids)
	return ids
}
func contains(ids []string, id string) bool {
	for _, s := range ids {
		if s == id {
			return true
		}
	}
	return false
}

// Bound output while reading, before a broken collector can exhaust host RAM.
var errOutputTooLarge = errors.New("discover output too large")

type cappedOutput struct{ buf bytes.Buffer }

func (b *cappedOutput) Bytes() []byte { return b.buf.Bytes() }

func (b *cappedOutput) Write(p []byte) (int, error) {
	if len(p) > (16<<20)-b.buf.Len() {
		return 0, errOutputTooLarge
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
