package agenthealth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/agentstatus"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestRolloutHealth(t *testing.T) {
	t.Setenv("AGENT_CANARY_HOSTS", "host-a")
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	release, input := t.TempDir(), t.TempDir()
	key := filepath.Join(t.TempDir(), "key")
	if _, err := agentrelease.Keygen(key); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		if err := os.WriteFile(filepath.Join(input, "discover-"+p), []byte("artifact"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 6; i++ {
		if err := agentrelease.Publish(release, key, input, "test", inventory.AgentRevision); err != nil {
			t.Fatal(err)
		}
	}
	fail := inventory.AgentUpdate{Sequence: 5, Target: 6, Result: "failed", Failures: 466}
	ok := inventory.AgentUpdate{Mode: "signed", Sequence: 6, Target: 6, Result: "ok"}
	behind := inventory.AgentUpdate{Mode: "signed", Sequence: 5, Target: 6, Result: "ok"}
	for _, tc := range []struct {
		name string
		run  func(*testing.T, string, func(inventory.AgentUpdate, time.Time), func(string, time.Time) Snapshot)
	}{
		{"slot receipt is not a local commit", func(t *testing.T, _ string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			active := agentproto.Identity{Sequence: 6, Version: "pair-v2"}
			fallback := agentproto.Identity{Sequence: 5, Version: "pair-v1"}
			u := ok
			u.Updater = "pair-v2"
			u.Transaction = "report-pending"
			u.Active = &active
			u.Fallback = &fallback
			u.Candidate = &active
			save(u, at)
			snap := eval(release, at)
			p := snap.Hosts["host-a"]
			if p.State != "updating" || p.Transaction != "report-pending" || p.Active.Sequence != 6 || p.Fallback.Sequence != 5 || p.Candidate.Sequence != 6 {
				t.Fatal("slot health lost transaction", p)
			}
			u.Transaction = "committed"
			u.Candidate = nil
			save(u, at.Add(time.Minute))
			p = eval(release, at.Add(time.Minute)).Hosts["host-a"]
			if p.State != "current" || p.Updater != "pair-v2" || p.Fallback.Sequence != 5 {
				t.Fatal("committed pair not current", p)
			}
		}},
		{"unconfigured", func(t *testing.T, _ string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(fail, at)
			s := eval("", at)
			if s.Hosts["host-a"].State != "not_set_up" || len(s.Alerts()) != 0 {
				t.Fatal(s)
			}
		}},
		{"unsigned", func(t *testing.T, _ string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(inventory.AgentUpdate{Mode: "unsigned", Result: "ok"}, at)
			s := eval(release, at)
			if s.Hosts["host-a"].State != "unsigned" || len(s.Alerts()) != 0 {
				t.Fatal(s)
			}
		}},
		{"legacy and canary exclusion", func(t *testing.T, _ string, _ func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			s := eval(release, at)
			if s.Hosts["host-a"].State != "not_reported" || s.Hosts["host-b"].Offered != 0 || len(s.Alerts()) != 0 {
				t.Fatal(s)
			}
		}},
		{"fresh failed inventories and hold", func(t *testing.T, dir string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(fail, at)
			s := eval(release, at)
			if !s.Hosts["host-a"].Unhealthy || s.Hosts["host-a"].Reason != "reason not reported by this updater" {
				t.Fatal(s)
			}
			m, _, c, _ := agentrelease.Read(release)
			if err := agentoffers.Save(filepath.Join(dir, "agent-offers.jsonl"), m, c, "hold", "", "operator"); err != nil {
				t.Fatal(err)
			}
			s = eval(release, at.Add(time.Hour))
			if !s.Hosts["host-a"].Unhealthy || !s.Hosts["host-a"].Paused {
				t.Fatal(s)
			}
			s = eval(release, at.Add(24*time.Hour))
			if !s.Hosts["host-a"].Critical {
				t.Fatal(s)
			}
		}},
		{"lag reload and recovery", func(t *testing.T, _ string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(behind, at)
			s := eval(release, at)
			if s.Hosts["host-a"].Unhealthy {
				t.Fatal(s)
			}
			s = eval(release, at.Add(46*time.Minute))
			if !s.Hosts["host-a"].Unhealthy {
				t.Fatal("lag start lost on reload", s)
			}
			save(ok, at.Add(47*time.Minute))
			s = eval(release, at.Add(47*time.Minute))
			if s.Hosts["host-a"].Unhealthy || s.Hosts["host-a"].State != "current" {
				t.Fatal(s)
			}
		}},
		{"failure recovery requires receipt", func(t *testing.T, _ string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(fail, at)
			eval(release, at)
			save(ok, at)
			s := eval(release, at.Add(time.Minute))
			if !s.Hosts["host-a"].Unhealthy {
				t.Fatal("same receipt resolved", s)
			}
			save(ok, at.Add(2*time.Minute))
			s = eval(release, at.Add(2*time.Minute))
			if s.Hosts["host-a"].Unhealthy {
				t.Fatal(s)
			}
		}},
		{"malformed release and offer journal", func(t *testing.T, dir string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(fail, at)
			eval(release, at)
			for _, path := range []string{filepath.Join(release, "latest.json"), filepath.Join(dir, "agent-offers.jsonl")} {
				old, readErr := os.ReadFile(path)
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
				s := eval(release, at.Add(time.Hour))
				if !s.Unavailable || !s.Hosts["host-a"].Unhealthy || len(s.Alerts()) != 2 {
					t.Fatal(s)
				}
				if os.IsNotExist(readErr) {
					os.Remove(path)
				} else if err := os.WriteFile(path, old, 0600); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"withdrawn target", func(t *testing.T, dir string, save func(inventory.AgentUpdate, time.Time), eval func(string, time.Time) Snapshot) {
			save(fail, at)
			eval(release, at)
			t.Setenv("AGENT_CANARY_HOSTS", "")
			s := eval(release, at.Add(time.Minute))
			if s.Hosts["host-a"].Unhealthy {
				t.Fatal(s)
			}
			if s = eval(release, at.Add(2*time.Minute)); s.Hosts["host-a"].Unhealthy {
				t.Fatal("withdrawn failure reopened", s)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			save := func(u inventory.AgentUpdate, when time.Time) {
				t.Helper()
				if err := agentstatus.Save(dir, agentstatus.Document{Host: "host-a", Update: u}, "inventory", when); err != nil {
					t.Fatal(err)
				}
			}
			eval := func(release string, now time.Time) Snapshot {
				t.Helper()
				s, err := Evaluate(dir, release, []*inventory.Inventory{{Host: inventory.Host{Hostname: "host-a"}}, {Host: inventory.Host{Hostname: "host-b"}}}, now)
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			tc.run(t, dir, save, eval)
		})
	}
}
