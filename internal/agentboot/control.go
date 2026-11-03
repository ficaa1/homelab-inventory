// Package agentboot owns the pinned trust root, runtime selection, watchdog and
// receipt authority. It imports only the small versioned wire contract.
package agentboot

import (
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"path/filepath"
)

type Slot struct {
	Bank string `json:"bank"`
	agentproto.Identity
}
type Control struct {
	Format          int                 `json:"format"`
	Generation      uint64              `json:"generation"`
	Floor           uint64              `json:"floor"`
	FloorDigest     string              `json:"floor_digest"`
	State           string              `json:"state"`
	Active          Slot                `json:"active"`
	Fallback        Slot                `json:"fallback"`
	Candidate       *Slot               `json:"candidate,omitempty"`
	Nonce           string              `json:"nonce,omitempty"`
	ReportHash      string              `json:"report_hash,omitempty"`
	Receipt         *agentproto.Receipt `json:"receipt,omitempty"`
	Continuations   uint64              `json:"continuations"`
	Errors          []string            `json:"errors,omitempty"`
	CandidateErrors []string            `json:"candidate_errors,omitempty"`
	Update          agentproto.Update   `json:"update"`
}
type record struct {
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

var errCorrupt = failure("state", "state_corrupt")

func validSlot(s Slot, floor uint64) bool {
	return (s.Bank == "a" || s.Bank == "b") && s.Sequence > 0 && s.Sequence <= floor && validDigest(s.Digest) && validDigest(s.Discover) && validDigest(s.Updater) && s.Version != "" && s.Revision > 0
}
func valid(c Control) bool {
	if c.Format != 1 || c.Generation == 0 || c.Floor == 0 || !validDigest(c.FloorDigest) || !validSlot(c.Active, c.Floor) || !validSlot(c.Fallback, c.Floor) {
		return false
	}
	switch c.State {
	case "prepared", "pending", "report-pending":
		if c.Candidate == nil || !validSlot(*c.Candidate, c.Floor) || c.Candidate.Sequence != c.Floor || c.Candidate.Digest != c.FloorDigest || !agentproto.ValidNonce(c.Nonce) || c.Candidate.Bank == c.Fallback.Bank {
			return false
		}
	case "committed", "quarantined":
		if c.Candidate != nil {
			return false
		}
	default:
		return false
	}
	if c.State == "report-pending" && len(c.ReportHash) != 64 {
		return false
	}
	return true
}

// Load never takes the update lock. Every new record carries both floor and
// fallback; the highest valid generation wins after an interrupted mirror write.
func (r Runner) Load() (Control, error) {
	var best Control
	n := 0
	for _, name := range []string{"control.0", "control.1"} {
		b, e := readBounded(filepath.Join(r.Dir, name), 65536)
		if e != nil || len(b) > 65536 {
			continue
		}
		var rec record
		var c Control
		if json.Unmarshal(b, &rec) != nil || agentrelease.Digest(rec.Payload) != rec.Checksum || json.Unmarshal(rec.Payload, &c) != nil || !valid(c) {
			continue
		}
		n++
		if best.Generation == c.Generation && best.Generation != 0 {
			b1, _ := json.Marshal(best)
			b2, _ := json.Marshal(c)
			if string(b1) != string(b2) {
				return Control{}, errCorrupt
			}
		}
		if c.Generation > best.Generation {
			if best.Generation != 0 && c.Floor < best.Floor {
				return Control{}, errCorrupt
			}
			best = c
		} else if c.Floor > best.Floor {
			return Control{}, errCorrupt
		}
	}
	if n == 0 {
		return Control{}, errCorrupt
	}
	return best, nil
}
func (r Runner) save(c *Control) error {
	c.Format = 1
	c.Generation++
	if !valid(*c) {
		return errCorrupt
	}
	b, e := json.Marshal(c)
	if e != nil {
		return e
	}
	b, e = json.Marshal(record{Payload: b, Checksum: agentrelease.Digest(b)})
	if e != nil {
		return e
	}
	if len(b) > 65536 {
		return errCorrupt
	}
	for _, name := range []string{"control.0", "control.1"} {
		if e = r.write(filepath.Join(r.Dir, name), b, 0600, c.State+"-"+name); e != nil {
			// The first mirror may already contain a reserved higher floor. The
			// caller must continue from that record even if the second write failed.
			if durable, loadErr := r.Load(); loadErr == nil {
				// Preserve an unsaved safe failure for the independent status channel;
				// floor/selection always come from the surviving durable record.
				update := c.Update
				if update.FirstFailure != nil {
					if durable.Update.FirstFailure != nil {
						update.FirstFailure = durable.Update.FirstFailure
					}
					durable.Update = update
				}
				*c = durable
			}
			return e
		}
	}
	return nil
}
func (r Runner) slot(s Slot) string { return filepath.Join(r.Dir, "slots", s.Bank, s.Digest) }

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
