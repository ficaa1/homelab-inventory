package agentoffers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
)

func TestPrecedence(t *testing.T) {
	t.Setenv("AGENT_CANARY_HOSTS", "host-a")
	m := agentrelease.Manifest{Sequence: 9}
	for _, c := range []agentrelease.Control{{Sequence: 9, Mode: "auto"}, {Sequence: 9, Mode: "hold"}, {Sequence: 9, Mode: "promote"}, {Sequence: 8, Mode: "auto"}, {Sequence: 9, Mode: "unknown"}} {
		for _, state := range []State{{}, {Sequence: 9, Mode: "auto", Hosts: map[string]bool{"host-b": true}}, {Sequence: 9, Mode: "promote"}, {Sequence: 9, Mode: "hold"}, {Sequence: 8, Mode: "promote"}} {
			mode := c.Mode
			if state.Sequence == 9 {
				mode = state.Mode
			}
			if c.Mode == "hold" || c.Sequence != 9 || c.Mode == "unknown" {
				mode = "hold"
			}
			if got := Effective(m, c, state); got != mode {
				t.Fatal(c, state, got)
			}
			for _, host := range []string{"host-a", "host-b", "host-c"} {
				want := mode != "hold" && (mode == "promote" || host == "host-a" || (state.Sequence == 9 && state.Hosts[host]))
				if Offered(host, m, c, state) != want {
					t.Fatal(c, state, host)
				}
			}
		}
	}
}
func TestAtomicJournalAndSequenceReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-offers.jsonl")
	m := agentrelease.Manifest{Sequence: 9}
	c := agentrelease.Control{Sequence: 9, Mode: "auto"}
	for _, host := range []string{"host-b", "host-c"} {
		if err := Save(path, m, c, "update", host, "example-user@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	state, data, err := Read(path)
	if err != nil || !state.Hosts["host-b"] || !state.Hosts["host-c"] {
		t.Fatal(state, err)
	}
	if strings.Count(string(data), "\n") != 2 {
		t.Fatal("expected audit per change")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
	m.Sequence++
	c.Sequence++
	if err := Save(path, m, c, "update", "host-d", "example-user@example.com"); err != nil {
		t.Fatal(err)
	}
	state, data, err = Read(path)
	if err != nil || state.Sequence != 10 || len(state.Hosts) != 1 || !state.Hosts["host-d"] || strings.Count(string(data), "\n") != 3 {
		t.Fatal(state, err)
	}
	if err := Save(path, m, c, "hold", "", "example-user@example.com"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := Save(path, m, c, "update", "host-b", "example-user@example.com"); err == nil {
		t.Fatal("hold accepted update")
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("refusal mutated journal")
	}
	if err := Save(path, m, c, "auto", "", "example-user@example.com"); err != nil {
		t.Fatal(err)
	}
	state, _, _ = Read(path)
	if state.Mode != "auto" || len(state.Hosts) != 0 {
		t.Fatal("auto retained hosts")
	}
	// A failure before replacement cannot alter the previous journal.
	if err := Save(path, m, c, "invalid", "", "example-user@example.com"); err == nil {
		t.Fatal("invalid action accepted")
	}
	os.WriteFile(path, []byte("corrupt\n"), 0600)
	if err := Save(path, m, c, "promote", "", "example-user@example.com"); err == nil {
		t.Fatal("corrupt journal overwritten")
	}
}

func TestEmptyCanarySetRequiresPromotion(t *testing.T) {
	t.Setenv("AGENT_CANARY_HOSTS", "")
	m := agentrelease.Manifest{Sequence: 1}
	c := agentrelease.Control{Sequence: 1, Mode: "auto"}
	if Offered("host-a", m, c, State{}) {
		t.Fatal("implicit canary")
	}
	t.Setenv("AGENT_CANARY_HOSTS", "host-c,host-d")
	if !Offered("host-c", m, c, State{}) || Offered("host-a", m, c, State{}) {
		t.Fatal("configured canary set ignored")
	}
}
