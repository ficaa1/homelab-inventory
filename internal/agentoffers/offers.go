// Package agentoffers layers sequence-scoped browser decisions over SSH controls.
package agentoffers

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

type State struct {
	Sequence uint64          `json:"sequence"`
	Mode     string          `json:"mode"`
	Hosts    map[string]bool `json:"hosts,omitempty"`
}
type Change struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Host   string    `json:"host,omitempty"`
	State  State     `json:"state"`
}

// Read fails closed on any corrupt journal entry. The last line is the state;
// one atomic journal replacement commits state and its audit together.
func Read(path string) (State, []byte, error) {
	var state State
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil, nil
	}
	if err != nil {
		return state, nil, err
	}
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var change Change
		if err := json.Unmarshal(line, &change); err != nil {
			return State{}, nil, err
		}
		if change.Actor == "" || change.State.Sequence == 0 || !validMode(change.State.Mode) {
			return State{}, nil, errors.New("invalid offer journal")
		}
		state = change.State
	}
	return state, data, nil
}
func validMode(mode string) bool { return mode == "auto" || mode == "hold" || mode == "promote" }
func Effective(m agentrelease.Manifest, c agentrelease.Control, state State) string {
	if c.Mode == "hold" || c.Sequence != m.Sequence || !validMode(c.Mode) {
		return "hold"
	}
	if state.Sequence == m.Sequence && validMode(state.Mode) {
		return state.Mode
	}
	return c.Mode
}
func Offered(host string, m agentrelease.Manifest, c agentrelease.Control, state State) bool {
	mode := Effective(m, c, state)
	if mode == "hold" {
		return false
	}
	canary := false
	for _, name := range CanaryHosts() {
		if host == name {
			canary = true
		}
	}
	return mode == "promote" || canary || (state.Sequence == m.Sequence && state.Hosts[host])
}

// Save must be serialized with other reads/writes by the caller.
func Save(path string, m agentrelease.Manifest, c agentrelease.Control, action, host, actor string) error {
	state, journal, err := Read(path)
	if err != nil {
		return err
	}
	if state.Sequence != m.Sequence {
		state = State{Sequence: m.Sequence, Mode: c.Mode}
	}
	switch action {
	case "update":
		if state.Hosts == nil {
			state.Hosts = map[string]bool{}
		}
		state.Hosts[host] = true
		if state.Mode == "hold" {
			return errors.New("dashboard hold")
		}
	case "auto", "hold", "promote":
		state.Mode = action
		state.Hosts = nil
	default:
		return errors.New("invalid offer action")
	}
	line, err := json.Marshal(Change{At: time.Now().UTC(), Actor: actor, Action: action, Host: host, State: state})
	if err != nil {
		return err
	}
	if len(journal) > 0 && journal[len(journal)-1] != '\n' {
		journal = append(journal, '\n')
	}
	return fsx.WriteAtomic(path, append(append(journal, line...), '\n'), 0600)
}

// CanaryHosts returns the explicit automatic-offer set; empty means no canaries.
func CanaryHosts() []string {
	names := []string{}
	seen := map[string]bool{}
	for _, item := range strings.Split(os.Getenv("AGENT_CANARY_HOSTS"), ",") {
		name := strings.TrimSpace(item)
		if name != "" && !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}
	sort.Strings(names)
	return names
}
