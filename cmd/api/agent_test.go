package main

import (
	"bytes"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentproto"
	"github.com/ficaa1/homelab-inventory/internal/agentreceipt"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/agentstatus"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
			if path == "/agent/status" && method == "POST" {
				continue
			}
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

func TestPostAgentStatusAuthenticatedBoundedAndLatestOnly(t *testing.T) {
	s := testServer(t, "admin-test")
	s.agentToken = strings.Repeat("a", 32)
	body := `{"host":"host-a","update":{"mode":"signed","updater":"v1","sequence":5,"target":6,"result":"failed","failures":466,"stage":"download","code":"disk_full"}}`
	send := func(token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest("POST", "/agent/status", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	if w := send("", body); w.Code != 401 {
		t.Fatal(w.Code)
	}
	for _, bad := range []string{strings.Repeat(" ", agentstatus.MaxSize) + body, strings.Replace(body, `"disk_full"`, `"raw local text"`, 1)} {
		if w := send(s.agentToken, bad); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w := send(s.agentToken, body); w.Code != 204 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	files, err := os.ReadDir(filepath.Join(s.store.Dir(), "agent-status"))
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	receipt, err := agentstatus.Latest(s.store.Dir(), "host-a")
	if err != nil || receipt.Received.IsZero() || receipt.Update.Target != 6 {
		t.Fatal(receipt, err)
	}
	// A later inventory receipt wins even when the host's collection time is old.
	u := inventory.AgentUpdate{Mode: "unsigned", Updater: "v2", Result: "ok"}
	if _, err = s.store.Save(&inventory.Inventory{Host: inventory.Host{Hostname: "host-a"}, CollectedAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), AgentUpdate: &u}); err != nil {
		t.Fatal(err)
	}
	receipt, err = agentstatus.Latest(s.store.Dir(), "host-a")
	if err != nil || receipt.Update.Mode != "unsigned" {
		t.Fatal(receipt, err)
	}
}

func TestRuntimeMigrationRetainedArtifactsAndReceipts(t *testing.T) {
	t.Setenv("AGENT_CANARY_HOSTS", "host-a")
	t.Setenv("AGENT_PLATFORMS", "linux/amd64")
	s := testServer(t, "test-token")
	s.agentReleaseDir = t.TempDir()
	key := filepath.Join(t.TempDir(), "key")
	if _, e := agentrelease.Keygen(key); e != nil {
		t.Fatal(e)
	}
	input := t.TempDir()
	for _, role := range []string{"discover", "agentupdate"} {
		if e := os.WriteFile(filepath.Join(input, role+"-linux-amd64"), []byte(role+"-fixture"), 0755); e != nil {
			t.Fatal(e)
		}
	}
	for _, v := range []string{"v1", "v2"} {
		if e := agentrelease.PublishRuntime(s.agentReleaseDir, key, input, v, inventory.AgentRevision); e != nil {
			t.Fatal(e)
		}
	}
	legacy := agentRequest(s, "GET", "/agent/manifest?host=host-a")
	runtimeOffer := agentRequest(s, "GET", "/agent/manifest?host=host-a&protocol=1")
	var old, new agentrelease.Signed
	var lm, rm agentrelease.Manifest
	json.Unmarshal(legacy.Body.Bytes(), &old)
	json.Unmarshal(runtimeOffer.Body.Bytes(), &new)
	json.Unmarshal(old.Manifest, &lm)
	json.Unmarshal(new.Manifest, &rm)
	if legacy.Code != 200 || runtimeOffer.Code != 200 || len(lm.Artifacts) != 1 || lm.LauncherProtocol != 0 || len(rm.Artifacts) != 2 || rm.LauncherProtocol != 1 {
		t.Fatal("legacy offer changed or pair unavailable")
	}
	if w := agentRequest(s, "GET", "/agent/manifest?host=host-b&protocol=1"); w.Code != 204 {
		t.Fatal("runtime bypassed canary")
	}
	for _, name := range []string{"discover-linux-amd64", "agentupdate-linux-amd64"} {
		if w := agentRequest(s, "GET", "/agent/artifacts/1/"+name); w.Code != 200 {
			t.Fatal("retained artifact unavailable")
		}
	}
	for _, name := range []string{"runtime.json", "manifest.json", "agentboot-linux-amd64", "other"} {
		if w := agentRequest(s, "GET", "/agent/artifacts/1/"+name); w.Code != 404 {
			t.Fatal("unlisted artifact served")
		}
	}
	m, b, e := agentrelease.ReadSequence(s.agentReleaseDir, 1, true)
	if e != nil {
		t.Fatal(e)
	}
	var signed agentrelease.Signed
	json.Unmarshal(b, &signed)
	id := agentproto.Identity{Sequence: 1, Digest: agentrelease.Digest(signed.Manifest), Version: m.Version, Revision: m.Revision, Discover: m.Artifacts["linux-amd64/discover"].SHA256, Updater: m.Artifacts["linux-amd64/agentupdate"].SHA256}
	rt := agentproto.Runtime{Protocol: 1, Nonce: strings.Repeat("a", 32), Identity: id}
	data := map[string]any{"schema_version": "5", "collected_at": "2001-01-01T00:00:00Z", "host": map[string]string{"hostname": "host-a"}, "tool": map[string]any{"version": id.Version, "revision": id.Revision, "platform": "linux/amd64"}, "agent_runtime": rt, "agent_update": inventory.AgentUpdate{Mode: "signed", Sequence: 1, Result: "trial", Transaction: "pending", Active: &id, Fallback: &id}, "future_field": map[string]int{"n": 7}, "docker": nil}
	raw, _ := json.Marshal(data)
	post := func(data []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/agent/report", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, req)
		return w
	}
	first := post(raw)
	// Simulate a server interruption after indexing but before the receipt's
	// indexed marker became durable. Retrying must finish that exact projection.
	acceptedPath := filepath.Join(s.store.Dir(), "agent-receipts", agentrelease.Digest([]byte("host-a")), rt.Nonce+".json")
	interruptedBytes, readErr := os.ReadFile(acceptedPath)
	var interrupted agentreceipt.Accepted
	if readErr != nil || json.Unmarshal(interruptedBytes, &interrupted) != nil {
		t.Fatal("missing acceptance journal")
	}
	interrupted.Indexed = false
	interruptedBytes, _ = json.Marshal(interrupted)
	if e = os.WriteFile(acceptedPath, interruptedBytes, 0600); e != nil {
		t.Fatal(e)
	}
	retry := post(raw)
	if first.Code != 201 || retry.Code != 201 || first.Body.String() != retry.Body.String() {
		t.Fatalf("durable retry failed: %d %d", first.Code, retry.Code)
	}
	history, e := s.store.History("host-a")
	if e != nil || len(history) != 1 || history[0].CollectedAt.Year() == 2001 {
		t.Fatal("retry duplicated run or used host clock")
	}
	actual, e := s.store.Latest("host-a")
	if e != nil || actual.CollectedAt.Year() != 2001 {
		t.Fatal("original collection time lost")
	}
	var rec agentproto.Receipt
	json.Unmarshal(first.Body.Bytes(), &rec)
	if rec.Body != agentrelease.Digest(raw) || rec.Identity != id || rec.Nonce != rt.Nonce {
		t.Fatal("receipt not bound to report")
	}
	path := filepath.Join(s.store.Dir(), "agent-receipts", agentrelease.Digest([]byte("host-a")), rt.Nonce+".json")
	stored, e := os.ReadFile(path)
	var accepted agentreceipt.Accepted
	if e != nil || json.Unmarshal(stored, &accepted) != nil || !bytes.Equal(accepted.Report, raw) {
		t.Fatal("exact report bytes not retained")
	}
	for _, tc := range []string{"body", "nonce", "digest", "role", "build", "schema", "folded"} {
		t.Run(tc, func(t *testing.T) {
			changed := map[string]any{}
			json.Unmarshal(raw, &changed)
			want := 400
			switch tc {
			case "body":
				changed["future_field"] = true
				want = 409
			case "nonce":
				runtime := changed["agent_runtime"].(map[string]any)
				runtime["nonce"] = "invalid"
			case "digest":
				runtime := changed["agent_runtime"].(map[string]any)
				runtime["digest"] = strings.Repeat("0", 64)
			case "role":
				runtime := changed["agent_runtime"].(map[string]any)
				runtime["agentupdate"] = strings.Repeat("0", 64)
			case "build":
				changed["tool"].(map[string]any)["version"] = "other"
			case "schema":
				changed["schema_version"] = "0"
			case "folded":
				changed["HOST"] = map[string]string{"hostname": "other"}
			}
			body, _ := json.Marshal(changed)
			if w := post(body); w.Code != want {
				t.Fatal(w.Code, want)
			}
		})
	}
	runtimePath := filepath.Join(s.agentReleaseDir, "1", "runtime.json")
	os.WriteFile(runtimePath, []byte("corrupt"), 0644)
	if w := agentRequest(s, "GET", "/agent/artifacts/1/agentupdate-linux-amd64"); w.Code != 404 {
		t.Fatal("unverified retained runtime served")
	}
	if w := agentRequest(s, "GET", "/agent/artifacts/1/discover-linux-amd64"); w.Code != 200 {
		t.Fatal("legacy retained artifact broken")
	}
	if w := post(raw); w.Code != 400 {
		t.Fatal("unverified runtime report accepted")
	}
}
