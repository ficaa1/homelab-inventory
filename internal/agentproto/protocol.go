// Package agentproto is the versioned launcher/worker contract. It deliberately
// validates only the stable envelope. Protocol 1 accepts schema 5 and passes
// unknown inventory fields through. Protocol changes require a compatible launcher.
package agentproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

const Version = 1
const MaxReport = 16 << 20
const MaxMessage = (MaxReport+2)/3*4 + 131072
const Product = "homelab-inventory"

var ErrOutput = errors.New("trial_invalid_output")

type Identity = inventory.AgentIdentity

type Runtime struct {
	Protocol int    `json:"protocol"`
	Nonce    string `json:"nonce"`
	Identity
}
type Input struct {
	Protocol  int      `json:"protocol"`
	Operation string   `json:"operation"`
	Host      string   `json:"host"`
	Nonce     string   `json:"nonce"`
	Discover  string   `json:"discover,omitempty"`
	Public    []byte   `json:"public,omitempty"`
	Floor     uint64   `json:"floor,omitempty"`
	Identity  Identity `json:"identity"`
	Update    Update   `json:"update"`
}

// Messages travel only over private stdin/stdout pipes. An offer is requested
// by the worker, fetched with credentials by the parent, then planned by the
// worker. The parent re-verifies the resulting envelope before staging.
type Message struct {
	Protocol  int    `json:"protocol"`
	Operation string `json:"operation"`
	Envelope  []byte `json:"envelope,omitempty"`
	Report    []byte `json:"report,omitempty"`
	Code      string `json:"code,omitempty"`
}
type Receipt struct {
	Protocol int       `json:"protocol"`
	Host     string    `json:"host"`
	Nonce    string    `json:"nonce"`
	Identity Identity  `json:"identity"`
	Body     string    `json:"body"`
	Received time.Time `json:"received"`
}

// Object rejects duplicate or case-folded keys, including Unicode folds used by
// encoding/json, before any identity is decoded. Unknown fields remain opaque.
func Object(data []byte) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	tok, e := d.Token()
	if e != nil || tok != json.Delim('{') {
		return nil, ErrOutput
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		t, e := d.Token()
		if e != nil {
			return nil, ErrOutput
		}
		k, ok := t.(string)
		if !ok {
			return nil, ErrOutput
		}
		for old := range out {
			if strings.EqualFold(k, old) {
				return nil, ErrOutput
			}
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, ErrOutput
		}
		out[k] = v
	}
	if _, e = d.Token(); e != nil || d.Decode(new(any)) != io.EOF {
		return nil, ErrOutput
	}
	return out, nil
}
func ExactObject(data []byte, names ...string) (map[string]json.RawMessage, error) {
	o, e := Object(data)
	if e != nil {
		return nil, e
	}
	for k := range o {
		for _, n := range names {
			if k != n && strings.EqualFold(k, n) {
				return nil, ErrOutput
			}
		}
	}
	return o, nil
}

// ValidateReport validates the stable envelope only, not the collector schema.
func ValidateReport(data []byte, host, nonce string, id Identity) (Runtime, error) {
	var rt Runtime
	if len(data) == 0 || len(data) > MaxReport {
		return rt, ErrOutput
	}
	o, e := ExactObject(data, "schema_version", "host", "tool", "agent_runtime", "agent_update", "diagnostics")
	if e != nil {
		return rt, e
	}
	var schema string
	if json.Unmarshal(o["schema_version"], &schema) != nil || schema != "5" {
		return rt, ErrOutput
	}
	h, e := ExactObject(o["host"], "hostname")
	if e != nil {
		return rt, e
	}
	var name string
	if json.Unmarshal(h["hostname"], &name) != nil || name != host || name == "" {
		return rt, ErrOutput
	}
	t, e := ExactObject(o["tool"], "version", "revision")
	if e != nil {
		return rt, e
	}
	var ver string
	var rev int
	if json.Unmarshal(t["version"], &ver) != nil || json.Unmarshal(t["revision"], &rev) != nil || ver != id.Version || rev != id.Revision {
		return rt, ErrOutput
	}
	if _, e = ExactObject(o["agent_runtime"], "protocol", "nonce", "sequence", "digest", "version", "revision", "discover", "agentupdate"); e != nil {
		return rt, e
	}
	d := json.NewDecoder(bytes.NewReader(o["agent_runtime"]))
	d.DisallowUnknownFields()
	if d.Decode(&rt) != nil || rt.Protocol != Version || rt.Nonce != nonce || rt.Identity != id || !ValidNonce(nonce) {
		return rt, ErrOutput
	}
	return rt, nil
}
func ValidNonce(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// ErrorIDs is the signed protocol-1 health contract: only new error-level
// collector/code pairs fail a trial. Messages and host details are never kept.
func ErrorIDs(data []byte) ([]string, error) {
	var v struct {
		Diagnostics []struct{ Severity, Collector, Code string } `json:"diagnostics"`
	}
	if json.Unmarshal(data, &v) != nil {
		return nil, ErrOutput
	}
	out := []string{}
	for _, d := range v.Diagnostics {
		if d.Severity == "error" {
			if len(out) >= 64 || len(d.Collector) > 128 || len(d.Code) > 128 {
				return nil, ErrOutput
			}
			out = append(out, d.Collector+"\x00"+d.Code)
		}
	}
	return out, nil
}
