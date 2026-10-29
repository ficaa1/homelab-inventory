package main

import (
	"bytes"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func releaseServer(t *testing.T) (*server, agentrelease.Manifest) {
	t.Setenv("AGENT_CANARY_HOSTS", "host-a")
	t.Helper()
	s := testServer(t, "test-token")
	s.agentReleaseDir = t.TempDir()
	key := filepath.Join(t.TempDir(), "key")
	if _, e := agentrelease.Keygen(key); e != nil {
		t.Fatal(e)
	}
	input := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		for _, c := range []string{"discover"} {
			if e := os.WriteFile(filepath.Join(input, c+"-"+p), []byte("artifact"), 0755); e != nil {
				t.Fatal(e)
			}
		}
	}
	if e := agentrelease.Publish(s.agentReleaseDir, key, input, "signed-version", inventory.AgentRevision); e != nil {
		t.Fatal(e)
	}
	m, _, _, e := agentrelease.Read(s.agentReleaseDir)
	if e != nil {
		t.Fatal(e)
	}
	return s, m
}
func agentRequest(s *server, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}
func TestAgentRoutesReadOnlyAndAuthenticated(t *testing.T) {
	s, _ := releaseServer(t)
	for _, path := range []string{"/agent/manifest?host=host-a", "/agent/status", "/agent/artifacts/1/discover-linux-amd64"} {
		if w := doRequest(t, s.routes(), "GET", path, nil); w.Code != 401 {
			t.Fatal("unauthenticated route", path, w.Code)
		}
		if w := agentRequest(s, "GET", path); w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
			if w := agentRequest(s, method, path); w.Code != 405 && w.Code != 404 {
				t.Fatal("write route exists", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/agent/publish", "/agent/upload", "/agent/promote", "/agent/hold"} {
		if w := agentRequest(s, "POST", path); w.Code != 405 && w.Code != 404 {
			t.Fatal("publish/control write route exists", path, w.Code)
		}
	}
	for _, path := range []string{"/agent/artifacts/1/public.key", "/agent/artifacts/1/manifest.json", "/agent/artifacts/2/discover-linux-amd64"} {
		if w := agentRequest(s, "GET", path); w.Code != 404 {
			t.Fatal("unexpected file", path, w.Code)
		}
	}
}
func TestForgedCanaryCannotPromoteAndSSHControlWins(t *testing.T) {
	s, m := releaseServer(t)
	// Even stale state from an older API must not authorize deployment.
	if e := s.store.WriteState("agent-promoted", m.Sequence); e != nil {
		t.Fatal(e)
	}
	for _, when := range []time.Time{m.Created.Add(-time.Minute), time.Now(), time.Now().Add(time.Hour)} {
		inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: when, Host: inventory.Host{Hostname: "host-a"}, Tool: inventory.Tool{Version: m.Version, Revision: m.Revision}, AgentUpdate: &inventory.AgentUpdate{Sequence: m.Sequence, Result: "ok"}}
		b, _ := json.Marshal(inv)
		req := httptest.NewRequest("POST", "/inventory", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		if w := agentRequest(s, "GET", "/agent/manifest?host=host-b"); w.Code != 204 {
			t.Fatal("forged inventory or stored state promoted fleet", w.Code)
		}
	}
	for _, mode := range []string{"promote", "hold", "auto"} {
		if e := agentrelease.WriteJSON(filepath.Join(s.agentReleaseDir, "control.json"), agentrelease.Control{Sequence: m.Sequence, Mode: mode}); e != nil {
			t.Fatal(e)
		}
		for _, host := range []string{"host-a", "host-b"} {
			want := 204
			if mode == "promote" || (mode == "auto" && host == "host-a") {
				want = 200
			}
			if w := agentRequest(s, "GET", "/agent/manifest?host="+host); w.Code != want {
				t.Fatal(mode, host, w.Code)
			}
		}
	}
}

func TestForgedInventoriesCannotChangeAgentOffers(t *testing.T) {
	for _, mode := range []string{"auto", "promote", "hold", "host-grant", "dashboard-hold"} {
		t.Run(mode, func(t *testing.T) {
			s, m := releaseServer(t)
			c := agentrelease.Control{Sequence: m.Sequence, Mode: "auto"}
			if mode == "promote" || mode == "hold" {
				c.Mode = mode
			}
			if err := agentrelease.WriteJSON(filepath.Join(s.agentReleaseDir, "control.json"), c); err != nil {
				t.Fatal(err)
			}
			if mode == "host-grant" || mode == "dashboard-hold" {
				action, host := "update", "host-b"
				if mode == "dashboard-hold" {
					action, host = "hold", ""
				}
				if err := agentoffers.Save(s.store.AgentOffersPath(), m, c, action, host, "example-user@example.com"); err != nil {
					t.Fatal(err)
				}
			}
			hosts := []string{"host-a", "host-b", "host-c", "invented-host"}
			baseline := map[string]*httptest.ResponseRecorder{}
			for _, host := range hosts {
				baseline[host] = agentRequest(s, "GET", "/agent/manifest?host="+host)
			}
			// The shared token can claim any host, installed sequence or build.
			// Each POST must leave every host's exact manifest response unchanged.
			for _, forgedHost := range hosts {
				for i, update := range []*inventory.AgentUpdate{
					{Sequence: ^uint64(0), Result: "ok"},
					{Sequence: m.Sequence + 1, Result: "installed"},
					{Sequence: m.Sequence, Result: "trial"},
					{Sequence: 0, Result: "rollback", Failures: 999},
					{Sequence: 0, Result: "failed"},
					nil,
				} {
					version, revision := "forged-newer", m.Revision+100
					if i%2 == 1 {
						version, revision = m.Version, m.Revision
					}
					inv := inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: time.Now().Add(time.Duration(i) * time.Hour), Host: inventory.Host{Hostname: forgedHost}, Tool: inventory.Tool{Version: version, Revision: revision}, AgentUpdate: update}
					b, err := json.Marshal(inv)
					if err != nil {
						t.Fatal(err)
					}
					req := httptest.NewRequest("POST", "/inventory", bytes.NewReader(b))
					req.Header.Set("Authorization", "Bearer test-token")
					w := httptest.NewRecorder()
					s.routes().ServeHTTP(w, req)
					if w.Code != 201 {
						t.Fatal(w.Code, w.Body.String())
					}
					for _, host := range hosts {
						got := agentRequest(s, "GET", "/agent/manifest?host="+host)
						want := baseline[host]
						if got.Code != want.Code || got.Body.String() != want.Body.String() {
							t.Fatalf("inventory for %s (%+v) changed manifest for %s", forgedHost, update, host)
						}
					}
				}
			}
		})
	}
}
