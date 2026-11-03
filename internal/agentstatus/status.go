// Package agentstatus stores latest-only update telemetry, ordered by server receipt.
// Shared agent credentials provide telemetry, never release authorization.
package agentstatus

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

const MaxSize = 8192

var mu sync.Mutex

type Document struct {
	Host   string                `json:"host"`
	Update inventory.AgentUpdate `json:"update"`
}
type Receipt struct {
	Document
	Received time.Time `json:"received"`
}

func member(s, values string) bool {
	for _, value := range strings.Split(values, "|") {
		if s == value {
			return true
		}
	}
	return false
}
func ValidStage(s string) bool {
	return member(s, "check|download|verify|stage_write|selftest|activate|trial|post|lock|state")
}
func ValidCode(s string) bool {
	return member(s, "network_unreachable|http_status|signature_invalid|hash_mismatch|platform_missing|disk_full|permission_denied|read_only_fs|noexec|exec_format|selftest_timeout|selftest_failed|trial_exit|trial_timeout|trial_invalid_output|trial_new_errors|post_rejected|lock_busy|state_corrupt|replay_refused|manifest_conflict|earlier_updater|unknown")
}
func Valid(d Document) bool {
	u := d.Update
	if d.Host == "" || len(d.Host) > 253 || strings.ContainsAny(d.Host, "/\\\x00\r\n") || (u.Mode != "signed" && u.Mode != "unsigned") || len(u.Updater) > 128 {
		return false
	}
	for _, c := range u.Updater {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune(".-_+", c)) {
			return false
		}
	}
	if !member(u.Result, "ok|installed|trial|failed|rollback|blocked") || u.Failures < 0 || u.Failures > 1000000 || u.Stage != "" && !ValidStage(u.Stage) || u.Code != "" && !ValidCode(u.Code) {
		return false
	}
	for _, f := range []*inventory.UpdateFailure{u.FirstFailure, u.LastFailure} {
		if f != nil && (!ValidStage(f.Stage) || !ValidCode(f.Code)) {
			return false
		}
	}
	return true
}
func path(dir, host, source string) string {
	return filepath.Join(dir, "agent-status", fmt.Sprintf("%x-%s.json", sha256.Sum256([]byte(host)), source))
}
func Save(dir string, d Document, source string, now time.Time) error {
	mu.Lock()
	defer mu.Unlock()
	if source != "status" && source != "inventory" {
		return errors.New("invalid status source")
	}
	if err := os.MkdirAll(filepath.Join(dir, "agent-status"), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(Receipt{Document: d, Received: now.UTC()})
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(path(dir, d.Host, source), b, 0600)
}

// Latest keeps inventory receipt separate: host collection clocks cannot win.
func Latest(dir, host string) (Receipt, error) {
	mu.Lock()
	defer mu.Unlock()
	var best Receipt
	for _, source := range []string{"inventory", "status"} {
		b, err := os.ReadFile(path(dir, host, source))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return best, err
		}
		var r Receipt
		if err = json.Unmarshal(b, &r); err != nil {
			return best, err
		}
		if r.Host != host {
			return best, errors.New("invalid status host")
		}
		if r.Received.After(best.Received) {
			best = r
		}
	}
	return best, nil
}
func Hosts(dir string) ([]string, error) {
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(dir, "agent-status"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var hosts []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "agent-status", e.Name()))
		if err != nil {
			return nil, err
		}
		var r Receipt
		if err = json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		if !seen[r.Host] {
			seen[r.Host] = true
			hosts = append(hosts, r.Host)
		}
	}
	return hosts, nil
}
