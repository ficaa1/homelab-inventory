package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentoffers"
	"github.com/ficaa1/homelab-inventory/internal/agentrelease"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func offerLogin(t *testing.T, s *server) {
	t.Helper()
	// Local operator seam; no production browser login is configured.
	s.offerOperator = func(r *http.Request) (string, bool) {
		c, err := r.Cookie("operator_test")
		return "operator@example.com", err == nil && c.Value == "test-operator"
	}

}
func offerPost(s *server, body, session, media string, bearer bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/agent/offers", strings.NewReader(body))
	if bearer {
		r.Header.Set("Authorization", "Bearer test-token")
	}
	if session != "" {
		r.AddCookie(&http.Cookie{Name: "operator_test", Value: session})
	}
	r.Header.Set("Content-Type", media)
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}
func seedOfferHost(t *testing.T, s *server, host string, seq uint64) {
	t.Helper()
	_, err := s.store.Save(&inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: time.Now(), Host: inventory.Host{Hostname: host}, AgentUpdate: &inventory.AgentUpdate{Sequence: seq}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestAgentOffersDisabledWithoutOperator(t *testing.T) {
	s, _ := releaseServer(t)
	for _, token := range []string{"test-token", ""} {
		s.token = token
		for _, cookie := range []string{"", "forged", "test-token"} {
			if w := offerPost(s, `{"action":"promote","sequence":1,"confirm":true}`, cookie, "application/json", true); w.Code != 403 {
				t.Fatal(w.Code)
			}
		}
	}
	if _, err := os.Stat(s.store.AgentOffersPath()); !os.IsNotExist(err) {
		t.Fatal("disabled operator wrote a decision")
	}
}

func TestAgentOfferPrecedence(t *testing.T) {
	s, m := releaseServer(t)
	offerLogin(t, s)
	seedOfferHost(t, s, "host-b", 0)
	seedOfferHost(t, s, "newer", m.Sequence+1)
	session := "test-operator"
	post := func(action, host string, seq uint64, want int) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"action": action, "host": host, "sequence": seq, "confirm": true})
		w := offerPost(s, string(b), session, "application/json", false)
		if w.Code != want {
			t.Fatal(action, w.Code, w.Body.String())
		}
	}
	offered := func(host string, want int) {
		t.Helper()
		if w := agentRequest(s, "GET", "/agent/manifest?host="+host); w.Code != want {
			t.Fatal(host, w.Code)
		}
	}
	post("update", "host-b", m.Sequence, 200)
	offered("host-b", 200)
	offered("host-c", 204)
	post("update", "missing", m.Sequence, 200)
	offered("missing", 200)
	post("update", "newer", m.Sequence, 200)
	offered("newer", 200)
	post("promote", "", m.Sequence+1, 409)
	post("promote", "", m.Sequence-1, 409)
	post("promote", "", m.Sequence, 200)
	offered("host-c", 200)
	offered("newer", 200)
	// /hosts displays inventory but uses the same offer policy as manifest.
	w := agentRequest(s, "GET", "/hosts")
	var hosts []hostSummary
	json.Unmarshal(w.Body.Bytes(), &hosts)
	for _, h := range hosts {
		if h.Name == "newer" && h.AgentOfferedSequence != m.Sequence {
			t.Fatal("reported sequence suppressed Hosts offer")
		}
	}
	post("hold", "", m.Sequence, 200)
	offered("host-a", 204)
	offered("host-b", 204)
	post("auto", "", m.Sequence, 200)
	offered("host-a", 200)
	offered("host-b", 204)
	post("update", "host-b", m.Sequence, 200)
	if err := agentrelease.WriteJSON(filepath.Join(s.agentReleaseDir, "control.json"), agentrelease.Control{Sequence: m.Sequence, Mode: "hold"}); err != nil {
		t.Fatal(err)
	}
	offered("host-b", 204)
	post("promote", "", m.Sequence, 409)
	// Publishing the next signed release resets all dashboard decisions.
	key := filepath.Join(t.TempDir(), "key")
	agentrelease.Keygen(key)
	// Restore publication key by generating an independent release tree for seq 2.
	os.Remove(filepath.Join(s.agentReleaseDir, "latest.json"))
	os.RemoveAll(filepath.Join(s.agentReleaseDir, "1"))
	input := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		os.WriteFile(filepath.Join(input, "discover-"+p), []byte("artifact"), 0755)
	}
	if err := agentrelease.Publish(s.agentReleaseDir, key, input, "next", m.Revision); err != nil {
		t.Fatal(err)
	}
	if err := agentrelease.Publish(s.agentReleaseDir, key, input, "next2", m.Revision); err != nil {
		t.Fatal(err)
	}
	offered("host-b", 204)
	offered("host-a", 200)
	post("update", "host-b", m.Sequence, 409)
	// Restart reads the journal; corrupt state fails closed.
	os.WriteFile(s.store.AgentOffersPath(), []byte("broken\n"), 0600)
	offered("host-a", 200)
	offered("host-b", 204)
	post("promote", "", 2, 503)
}

func TestConcurrentAgentOffersPersistAcrossRestart(t *testing.T) {
	s, m := releaseServer(t)
	offerLogin(t, s)
	session := "test-operator"
	done := make(chan *httptest.ResponseRecorder, 2)
	for _, host := range []string{"host-b", "host-c"} {
		seedOfferHost(t, s, host, 0)
		b, _ := json.Marshal(map[string]any{"action": "update", "host": host, "sequence": m.Sequence, "confirm": true})
		go func(body string) { done <- offerPost(s, body, session, "application/json", false) }(string(b))
	}
	for range 2 {
		if w := <-done; w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// The new API instance has no in-memory offer authority.
	restarted := &server{store: s.store, log: s.log, token: s.token, agentReleaseDir: s.agentReleaseDir}
	for _, host := range []string{"host-b", "host-c"} {
		if w := agentRequest(restarted, "GET", "/agent/manifest?host="+host); w.Code != 200 {
			t.Fatal(host, w.Code)
		}
	}
	b, _ := os.ReadFile(s.store.AgentOffersPath())
	if strings.Count(string(b), "\n") != 2 {
		t.Fatal("lost concurrent audit")
	}
}

func TestAgentOffersRefuseUnsignedReleaseAndUnreadableState(t *testing.T) {
	s, _ := releaseServer(t)
	offerLogin(t, s)
	seedOfferHost(t, s, "host-b", 0)
	session := "test-operator"
	body := `{"action":"promote","sequence":1,"confirm":true}`
	manifest := filepath.Join(s.agentReleaseDir, "latest.json")
	original, _ := os.ReadFile(manifest)
	os.WriteFile(manifest, []byte(`{"manifest":{"sequence":1},"signature":"bad"}`), 0644)
	if w := offerPost(s, body, session, "application/json", false); w.Code != 503 {
		t.Fatal("unsigned release accepted", w.Code)
	}
	if _, err := os.Stat(s.store.AgentOffersPath()); !os.IsNotExist(err) {
		t.Fatal("unsigned release saved decision")
	}
	os.WriteFile(manifest, original, 0644)
	// A directory at the journal path is a read/write failure, not fresh state.
	os.Mkdir(s.store.AgentOffersPath(), 0700)
	if w := offerPost(s, body, session, "application/json", false); w.Code != 503 {
		t.Fatal("unreadable state accepted", w.Code)
	}
	if w := agentRequest(s, "GET", "/agent/manifest?host=host-a"); w.Code != 200 {
		t.Fatal("unreadable state blocked SSH canary", w.Code)
	}
}

func TestCorruptAgentOffersUseOnlySSHControl(t *testing.T) {
	s, m := releaseServer(t)
	os.WriteFile(s.store.AgentOffersPath(), []byte("broken\n"), 0600)
	for _, mode := range []string{"auto", "hold", "promote"} {
		if err := agentrelease.WriteJSON(filepath.Join(s.agentReleaseDir, "control.json"), agentrelease.Control{Sequence: m.Sequence, Mode: mode}); err != nil {
			t.Fatal(err)
		}
		for _, host := range []string{"host-a", "host-b"} {
			want := 204
			if mode == "promote" || (mode == "auto" && host == "host-a") {
				want = 200
			}
			if w := agentRequest(s, "GET", "/agent/manifest?host="+host); w.Code != want {
				t.Fatalf("%s %s: got %d, want %d", mode, host, w.Code, want)
			}
		}
	}
}

func TestAgentOfferRechecksSSHAfterReleaseSnapshot(t *testing.T) {
	s, m := releaseServer(t)
	old := agentrelease.Control{Sequence: m.Sequence, Mode: "auto"}
	if err := agentoffers.Save(s.store.AgentOffersPath(), m, old, "promote", "", "example-user@example.com"); err != nil {
		t.Fatal(err)
	}
	// A handler can retain this snapshot while waiting for another journal write.
	if err := agentrelease.WriteJSON(filepath.Join(s.agentReleaseDir, "control.json"), agentrelease.Control{Sequence: m.Sequence, Mode: "hold"}); err != nil {
		t.Fatal(err)
	}
	if s.agentOffered("host-b", m, old) {
		t.Fatal("stale SSH snapshot overrode hold")
	}
	// Likewise, an old promoted snapshot must not offer the previous sequence
	// after latest.json has moved to a new release.
	key := filepath.Join(t.TempDir(), "key")
	if _, err := agentrelease.Keygen(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.agentReleaseDir, "latest.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(s.agentReleaseDir, "1")); err != nil {
		t.Fatal(err)
	}
	input := t.TempDir()
	for _, platform := range []string{"linux-amd64", "linux-arm64", "freebsd-amd64"} {
		os.WriteFile(filepath.Join(input, "discover-"+platform), []byte("artifact"), 0755)
	}
	for range 2 {
		if err := agentrelease.Publish(s.agentReleaseDir, key, input, "next", m.Revision); err != nil {
			t.Fatal(err)
		}
	}
	if s.agentOffered("host-b", m, old) {
		t.Fatal("old dashboard promotion survived new latest")
	}
}

func TestEveryOfferActionRejectsBearerCookie(t *testing.T) {
	s, _ := releaseServer(t)
	offerLogin(t, s)
	for _, action := range []string{"update", "promote", "hold", "auto"} {
		host := ""
		if action == "update" {
			host = "host-b"
		}
		b, _ := json.Marshal(map[string]any{"action": action, "host": host, "sequence": 1, "confirm": true})
		// Putting the shared token in the session cookie does not create a session.
		if w := offerPost(s, string(b), "test-token", "application/json", true); w.Code != 403 {
			t.Fatal(action, w.Code)
		}
	}
}
