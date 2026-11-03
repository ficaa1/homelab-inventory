package agentboot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentproc"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

func (r Runner) deadline() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 6 * time.Minute
}

// The total worker deadline covers the entire process group and pipe traffic.
func (r Runner) worker(ctx context.Context, s Slot, in agentproto.Input) (agentproto.Message, error) {
	var reply agentproto.Message
	if _, e := r.checkSlot(s); e != nil {
		return reply, e
	}
	ctx, cancel := context.WithTimeout(ctx, r.deadline())
	defer cancel()
	ir, iw, e := os.Pipe()
	if e != nil {
		return reply, e
	}
	defer ir.Close()
	defer iw.Close()
	or, ow, e := os.Pipe()
	if e != nil {
		return reply, e
	}
	defer or.Close()
	done := make(chan error, 1)
	go func() {
		e := agentproc.Run(ctx, filepath.Join(r.slot(s), "agentupdate"), []string{"-worker"}, ir, ow)
		ow.Close()
		done <- e
	}()
	enc := json.NewEncoder(iw)
	dec := json.NewDecoder(io.LimitReader(or, agentproto.MaxMessage+1))
	in.Protocol = agentproto.Version
	var offered []byte
	e = enc.Encode(in)
	if e == nil && in.Operation == "plan" {
		var request agentproto.Message
		if dec.Decode(&request) != nil || request.Protocol != 1 || request.Operation != "offer" {
			e = agentproto.ErrOutput
		} else {
			var data []byte
			data, e = r.fetchOffer(ctx)
			offered = data
			if e == nil {
				e = enc.Encode(agentproto.Message{Protocol: 1, Operation: "offer", Envelope: data})
			}
		}
	}
	iw.Close()
	if e == nil {
		e = dec.Decode(&reply)
	}
	if e == nil && (reply.Protocol != agentproto.Version || dec.Decode(new(any)) != io.EOF) {
		e = agentproto.ErrOutput
	}
	if e != nil {
		cancel()
		or.Close()
	}
	runErr := <-done
	if errors.Is(runErr, context.DeadlineExceeded) {
		return reply, failure("trial", "trial_timeout")
	}
	if runErr != nil {
		return reply, failure("trial", "trial_exit")
	}
	if e != nil {
		return reply, agentproto.ErrOutput
	}

	if in.Operation == "plan" && reply.Operation == "plan" && !bytes.Equal(offered, reply.Envelope) {
		return reply, agentproto.ErrOutput
	}
	if reply.Operation == "error" {
		switch reply.Code {
		case "trial_exit", "trial_timeout", "trial_invalid_output":
			return reply, failure("trial", reply.Code)
		}
		return reply, agentproto.ErrOutput
	}
	return reply, nil
}
func (r Runner) collect(ctx context.Context, c Control, s Slot, n string) ([]byte, []string, error) {
	u := c.Update
	u.Sequence = s.Sequence
	u.Mode = "signed"
	u.Updater = s.Version
	u.Launcher = r.Version
	u.Transaction = c.State
	if c.Candidate != nil {
		id := c.Candidate.Identity
		u.Candidate = &id
	}
	active := s.Identity
	fallback := c.Fallback.Identity
	u.Active = &active
	u.Fallback = &fallback
	reply, e := r.worker(ctx, s, agentproto.Input{Operation: "collect", Host: r.Host, Nonce: n, Discover: filepath.Join(r.slot(s), "discover"), Identity: s.Identity, Update: u})
	if e != nil {
		return nil, nil, e
	}
	if reply.Operation != "report" {
		return nil, nil, agentproto.ErrOutput
	}
	if _, e = agentproto.ValidateReport(reply.Report, r.Host, n, s.Identity); e != nil {
		return nil, nil, e
	}
	ids, e := agentproto.ErrorIDs(reply.Report)
	return reply.Report, ids, e
}
func (r Runner) offer(ctx context.Context, c Control) ([]byte, error) {
	pub, e := readBounded(r.Public, 4096)
	if e != nil {
		return nil, e
	}
	reply, e := r.worker(ctx, c.Active, agentproto.Input{Operation: "plan", Host: r.Host, Public: pub, Floor: c.Floor})
	if e == nil && reply.Operation == "plan" {
		return reply.Envelope, nil
	}
	// Emergency path does not execute the failing worker or depend on it to
	// understand its successor. It uses the same policy-controlled endpoint.
	return r.fetchOffer(ctx)
}
func (r Runner) prepare(ctx context.Context, c *Control, data []byte) error {
	m, e := r.verify(data)
	if e != nil {
		return e
	}
	id := identity(m, data)
	c.Update.Target = m.Sequence
	if m.Sequence == c.Floor && id.Digest != c.FloorDigest {
		return failure("verify", "manifest_conflict")
	}
	if m.Sequence <= c.Floor {
		return nil
	}
	// Establish the actual last good pair before staging its successor. If a
	// previously committed updater has died, retain the older recorded fallback.
	n, e := nonce()
	if e != nil {
		return e
	}
	if _, ids, err := r.collect(ctx, *c, c.Active, n); err == nil {
		c.Errors = ids
	} else {
		if c.Active != c.Fallback {
			if e = r.quarantine(c, err); e != nil {
				return e
			}
		} else {
			mark(c, "trial", err)
		}
	}
	if e = r.prune(*c); e != nil {
		return e
	}
	bank := "a"
	if c.Active.Bank == "a" {
		bank = "b"
	}
	candidate, e := r.stage(ctx, data, bank, "")
	if e != nil {
		return e
	}
	next := *c
	next.Floor = m.Sequence
	next.FloorDigest = id.Digest
	next.Fallback = c.Active
	next.Candidate = &candidate
	next.Nonce, e = nonce()
	if e != nil {
		return e
	}
	next.State = "prepared"
	next.Receipt = nil
	next.ReportHash = ""
	next.Continuations = 0
	next.Update.Result = "trial"
	// The reservation and known-good pair are mirrored before execution. No
	// selector symlink is required, so ENOSPC cannot block direct fallback.
	*c = next
	return r.save(c)
}
func (r Runner) quarantine(c *Control, err error) error {
	mark(c, "trial", err)
	if c.Active == c.Fallback && c.Candidate == nil {
		_ = r.save(c)
		return err
	}
	rejected := c.Active.Sequence
	if c.Candidate != nil {
		rejected = c.Candidate.Sequence
	}
	c.Active = c.Fallback
	c.Candidate = nil
	c.State = "quarantined"
	c.ReportHash = ""
	c.Receipt = nil
	c.CandidateErrors = nil
	c.Update.Rejected = rejected
	c.Update.Result = "rollback"
	c.Update.Sequence = c.Active.Sequence
	return r.save(c)
}
func (r Runner) finish(ctx context.Context, c *Control) error {
	if _, e := r.checkSlot(*c.Candidate); e != nil {
		return r.quarantine(c, classify("verify", e))
	}
	path := filepath.Join(r.Dir, "report.json")
	data, e := readBounded(path, agentproto.MaxReport)
	if e != nil || agentrelease.Digest(data) != c.ReportHash {
		return r.quarantine(c, agentproto.ErrOutput)
	}
	// A bounded continuation is journalled before each retry. Reboots and wall
	// clock changes cannot reset the budget or authorize another execution.
	// Retry the exact report once per scheduled tick, never in a hot loop.
	if c.Continuations < 1000000 {
		c.Continuations++
	}
	if e = r.save(c); e != nil {
		return e
	}
	receipt, e := r.post(ctx, *c, data, c.Candidate.Identity, c.Nonce)
	if e != nil {
		mark(c, "post", e)
		_ = r.save(c)
		return e
	}
	c.Receipt = receipt
	// Receipt observed, then journalled, then committed. A lost response retries
	// the same exact bytes against the server's durable, idempotent receipt.
	if e = r.save(c); e != nil {
		return e
	}
	next := *c
	next.Active = *c.Candidate
	next.Candidate = nil
	next.State = "committed"
	next.Errors = c.CandidateErrors
	next.CandidateErrors = nil
	next.ReportHash = ""
	next.Update = agentproto.Update{Mode: "signed", Sequence: next.Active.Sequence, Target: next.Update.Target, Result: "ok"}
	*c = next
	if e = r.save(c); e != nil {
		return e
	}
	_ = os.Remove(path)
	return r.prune(*c)
}
func newErrors(baseline, ids []string) bool {
	for _, id := range ids {
		found := false
		for _, old := range baseline {
			if old == id {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return false
}

// Run holds one stable lock inode. Status can still be read or sent when that
// lock is busy or the control records are corrupt. Only Run writes the floor.
func (r Runner) Run(ctx context.Context) (runErr error) {
	aggregate, cancel := context.WithTimeout(ctx, 12*time.Minute)
	defer cancel()
	statusParent := ctx
	ctx = aggregate
	c, e := r.Load()
	defer func() {
		statusCtx, statusCancel := context.WithTimeout(statusParent, 30*time.Second)
		defer statusCancel()
		if e := r.status(statusCtx, c); runErr == nil && e != nil {
			runErr = e
		}
	}()
	if e != nil {
		mark(&c, "state", errCorrupt)
		return errCorrupt
	}
	unlock, e := fsx.LockDir(r.Dir)
	if e != nil {
		mark(&c, "lock", classify("lock", e))
		return classify("lock", e)
	}
	defer unlock()
	c, e = r.Load()
	if e != nil {
		mark(&c, "state", errCorrupt)
		return errCorrupt
	}
	// Reconcile the selected highest generation to both copies before mutating
	// slots; a partially mirrored floor must never be forgotten by later writes.
	if e = r.save(&c); e != nil {
		mark(&c, "state", e)
		return r.fallbackReport(ctx, &c)
	}
	if e = r.prune(c); e != nil {
		mark(&c, "stage_write", e)
	}
	if c.State == "pending" {
		if e = r.quarantine(&c, failure("trial", "unknown")); e != nil {
			return r.fallbackReport(ctx, &c)
		}
	}
	if c.State == "report-pending" {
		e = r.finish(ctx, &c)
		if e != nil {
			_ = r.fallbackReport(ctx, &c)
			// A higher offered pair can also repair a report-pending updater. Do not
			// quarantine healthy code merely because the API could not receive it.
			data, err := r.fetchOffer(ctx)
			if err == nil && len(data) > 0 {
				m, ve := r.verify(data)
				if ve == nil && m.Sequence > c.Floor {
					if err = r.prepare(ctx, &c, data); err == nil {
						e = nil
					}
				}
			}
			if c.State != "prepared" {
				return e
			}
		}
	}
	checkedOffer := false
	if c.State != "prepared" {
		data, err := r.offer(ctx, c)
		if err != nil {
			mark(&c, "check", err)
			_ = r.save(&c)
		} else if len(data) > 0 {
			if err = r.prepare(ctx, &c, data); err != nil {
				mark(&c, "check", err)
				_ = r.save(&c)
				return r.fallbackReport(ctx, &c)
			}
			checkedOffer = c.Update.Target == c.Active.Sequence
		}
	}
	if c.State == "prepared" {
		c.State = "pending"
		if e = r.save(&c); e != nil {
			mark(&c, "state", e)
			return r.fallbackReport(ctx, &c)
		}
		data, ids, err := r.collect(ctx, c, *c.Candidate, c.Nonce)
		if err == nil && newErrors(c.Errors, ids) {
			err = failure("trial", "trial_new_errors")
		}
		if err != nil {
			e = r.quarantine(&c, err)
			if e != nil {
				mark(&c, "state", e)
			}
			return r.fallbackReport(ctx, &c)
		}
		if e = r.write(filepath.Join(r.Dir, "report.json"), data, 0600, "report"); e != nil {
			_ = r.quarantine(&c, classify("stage_write", e))
			return r.fallbackReport(ctx, &c)
		}
		c.ReportHash = agentrelease.Digest(data)
		c.CandidateErrors = ids
		c.State = "report-pending"
		if e = r.save(&c); e != nil {
			return r.fallbackReport(ctx, &c)
		}
		e = r.finish(ctx, &c)
		if e != nil {
			_ = r.fallbackReport(ctx, &c)
		}
		return e
	}
	// A committed runtime is supervised on every tick, including regressions
	// after a successful trial. A fallback is never used as a candidate replay.
	n, e := nonce()
	if e != nil {
		return e
	}
	data, ids, err := r.collect(ctx, c, c.Active, n)
	if err != nil || newErrors(c.Errors, ids) && c.Active.Identity != c.Fallback.Identity {
		if err == nil {
			err = failure("trial", "trial_new_errors")
		}
		_ = r.quarantine(&c, err)
		return r.fallbackReport(ctx, &c)
	}
	if _, e = r.post(ctx, c, data, c.Active.Identity, n); e != nil {
		mark(&c, "post", e)
		_ = r.save(&c)
		return e
	}
	c.Errors = ids
	if checkedOffer && c.State == "committed" && c.Update.Rejected == 0 && c.Active.Sequence == c.Floor {
		c.Update = agentproto.Update{Mode: "signed", Sequence: c.Active.Sequence, Target: c.Update.Target, Result: "ok"}
	}
	// An old runtime succeeding does not erase the newer rejected release.
	if c.Update.Rejected != 0 {
		c.Update.Result = "blocked"
	}
	return r.save(&c)
}
func (r Runner) fallbackReport(ctx context.Context, c *Control) error {
	n, e := nonce()
	if e != nil {
		return e
	}
	s := c.Fallback
	data, _, e := r.collect(ctx, *c, s, n)
	if e != nil {
		return classify("trial", e)
	}
	_, e = r.post(ctx, *c, data, s.Identity, n)
	c.Active = s
	c.Update.Sequence = s.Sequence
	return e
}
