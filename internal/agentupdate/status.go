package agentupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentstatus"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

type outcome struct {
	stage, code string
	trial       bool
	cause       error
}

func (e *outcome) Error() string { return e.stage + ": " + e.code }
func (e *outcome) Unwrap() error { return e.cause }
func fault(stage, code string, err error) error {
	return &outcome{stage: stage, code: code, cause: err}
}
func trialFault(code string, err error) error {
	return &outcome{stage: "trial", code: code, trial: true, cause: err}
}
func classify(stage string, err error) *outcome {
	var o *outcome
	if errors.As(err, &o) {
		return o
	}
	code := "unknown"
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		code = "disk_full"
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM):
		code = "permission_denied"
	case errors.Is(err, syscall.EROFS):
		code = "read_only_fs"
	case errors.Is(err, syscall.ENOEXEC):
		code = "exec_format"
	}
	return &outcome{stage: stage, code: code, cause: err}
}
func failed(u *inventory.AgentUpdate, stage string, err error) {
	o := classify(stage, err)
	f := &inventory.UpdateFailure{At: time.Now().UTC(), Sequence: u.Target, Stage: o.stage, Code: o.code}
	if u.FirstFailure == nil {
		u.FirstFailure = f
	}
	u.LastFailure = f
	u.Stage = o.stage
	u.Code = o.code
	u.Result = "failed"
	if u.Failures < 1000000 {
		u.Failures++
	}
}
func resolved(u *inventory.AgentUpdate) {
	u.Result = "ok"
	u.Failures = 0
	u.FirstFailure = nil
	u.LastFailure = nil
	u.Stage = ""
	u.Code = ""
	u.Rejected = 0
}
func (r Runner) stamp(u *inventory.AgentUpdate, mode string) {
	u.Mode = mode
	u.Updater = r.Updater
	if u.Updater == "" {
		u.Updater = "dev"
	}
}
func (r Runner) postStatus(ctx context.Context, u inventory.AgentUpdate) error {
	b, err := json.Marshal(agentstatus.Document{Host: r.Host, Update: u})
	if err != nil {
		return err
	}
	_, _, err = r.request(ctx, "POST", "/agent/status", bytes.NewReader(b), agentstatus.MaxSize)
	return err
}
func executionError(stage string, err error) error {
	if errors.Is(err, errOutputTooLarge) {
		return trialFault("trial_invalid_output", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if stage == "selftest" {
			return fault(stage, "selftest_timeout", err)
		}
		return trialFault("trial_timeout", err)
	}
	var ex *exec.ExitError
	if errors.As(err, &ex) {
		if stage == "selftest" {
			return fault(stage, "selftest_failed", err)
		}
		return trialFault("trial_exit", err)
	}
	return classify(stage, err)
}

// Preserve every discover field as RawMessage and reject all folded duplicate
// top-level keys before validating the exact merged bytes.
func mergeUpdate(data []byte, u inventory.AgentUpdate) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, errors.New("invalid discover output")
	}
	raw := map[string]json.RawMessage{}
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return nil, errors.New("invalid discover output")
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("invalid discover output")
		}
		for old := range raw {
			if strings.EqualFold(old, key) {
				return nil, errors.New("invalid discover output")
			}
		}
		if key != "agent_update" && strings.EqualFold(key, "agent_update") {
			return nil, errors.New("invalid discover output")
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return nil, errors.New("invalid discover output")
		}
		raw[key] = value
	}
	if _, err = dec.Token(); err != nil || dec.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid discover output")
	}
	raw["agent_update"], err = json.Marshal(u)
	if err != nil {
		return nil, err
	}
	return json.Marshal(raw)
}
