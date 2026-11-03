// Package agentreceipt durably accepts exact report bytes before returning a
// receipt. The raw report is the acceptance record; inventory indexing is a
// separate idempotent projection. Shared fleet tokens do not attest identity.
package agentreceipt

import (
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var mu sync.Mutex
var ErrConflict = errors.New("report transaction conflict")

type Accepted struct {
	Receipt agentproto.Receipt `json:"receipt"`
	Report  []byte             `json:"report"`
	Indexed bool               `json:"indexed"`
}

// Accept stores at most five recent receipts per host. Retries of an accepted
// transaction return its original server time, without duplicate ingest. The
// latest trial receipt is pinned while routine fallback reports continue.
func Accept(dir, host string, rt agentproto.Runtime, data []byte, index func(agentproto.Receipt) error) (agentproto.Receipt, error) {
	mu.Lock()
	defer mu.Unlock()
	root := filepath.Join(dir, "agent-receipts", agentrelease.Digest([]byte(host)))
	if e := os.MkdirAll(root, 0700); e != nil {
		return agentproto.Receipt{}, e
	}
	path := filepath.Join(root, rt.Nonce+".json")
	a := Accepted{Receipt: agentproto.Receipt{Protocol: 1, Host: host, Nonce: rt.Nonce, Identity: rt.Identity, Body: agentrelease.Digest(data), Received: time.Now().UTC()}, Report: data}
	old, e := os.ReadFile(path)
	if e == nil {
		var prior Accepted
		if json.Unmarshal(old, &prior) != nil {
			return a.Receipt, errors.New("receipt corrupt")
		}
		if prior.Receipt.Body != a.Receipt.Body || prior.Receipt.Identity != a.Receipt.Identity {
			return a.Receipt, ErrConflict
		}
		a = prior
	} else if !os.IsNotExist(e) {
		return a.Receipt, e
	} else {
		b, e := json.Marshal(a)
		if e != nil {
			return a.Receipt, e
		}
		if e = fsx.WriteAtomic(path, b, 0600); e != nil {
			return a.Receipt, e
		}
	}
	if !a.Indexed {
		if e = index(a.Receipt); e != nil {
			return a.Receipt, e
		}
		a.Indexed = true
		b, e := json.Marshal(a)
		if e != nil {
			return a.Receipt, e
		}
		if e = fsx.WriteAtomic(path, b, 0600); e != nil {
			return a.Receipt, e
		}
	}
	// Pin the latest pending candidate independently of ordinary fallback runs.
	var fields struct {
		Update agentproto.Update `json:"agent_update"`
	}
	_ = json.Unmarshal(data, &fields)
	pin := filepath.Join(root, "trial")
	if fields.Update.Candidate != nil && fields.Update.Candidate.Sequence == rt.Sequence && (fields.Update.Transaction == "pending" || fields.Update.Transaction == "report-pending") {
		if e = fsx.WriteAtomic(pin, []byte(rt.Nonce), 0600); e != nil {
			return a.Receipt, e
		}
	}
	pinned, _ := os.ReadFile(pin)
	entries, e := os.ReadDir(root)
	if e != nil {
		return a.Receipt, e
	}
	type saved struct {
		name string
		at   time.Time
	}
	var files []saved
	for _, entry := range entries {
		if len(entry.Name()) == 37 {
			st, e := entry.Info()
			if e != nil {
				return a.Receipt, e
			}
			files = append(files, saved{entry.Name(), st.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	for i := 3; i < len(files); i++ {
		name := files[i].name
		if name != rt.Nonce+".json" && name != string(pinned)+".json" {
			if e = os.Remove(filepath.Join(root, name)); e != nil {
				return a.Receipt, e
			}
		}
	}
	return a.Receipt, nil
}
