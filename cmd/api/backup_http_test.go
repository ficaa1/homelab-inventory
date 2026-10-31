package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/ficaa1/homelab-inventory/internal/backup"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func backupTestHandler(t *testing.T) (*server, http.Handler) {
	s := testServer(t, strings.Repeat("a", 32))
	s.agentToken = strings.Repeat("b", 32)
	s.operatorToken = strings.Repeat("c", 32)
	s.backupMaintenance = func(ctx context.Context, f func() error) error { return f() }
	s.backupReload = func() error { return nil }
	mux := http.NewServeMux()
	s.backupRoutes(mux)
	return s, mux
}
func backupRequest(h http.Handler, method, path string, body io.Reader, token string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, body)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Requested-By", "homelab-inventory")
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestBackupAuthScopes(t *testing.T) {
	s, h := backupTestHandler(t)
	for _, token := range []string{"", s.agentToken, s.operatorToken, "expired-browser-session", "tv-token"} {
		w := backupRequest(h, "POST", "/admin/backup/export", strings.NewReader(`{"passphrase":"synthetic passphrase"}`), token, nil)
		if w.Code != 401 {
			t.Fatalf("scope accepted: %d", w.Code)
		}
	}
	w := backupRequest(h, "POST", "/admin/backup/export", nil, s.token, map[string]string{"X-Requested-By": ""})
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = backupRequest(h, "POST", "/admin/backup/export", nil, s.token, map[string]string{"Origin": "https://attacker.invalid"})
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestBackupPreviewConfirmAndCancel(t *testing.T) {
	s, h := backupTestHandler(t)
	if e := os.WriteFile(filepath.Join(s.store.Dir(), "state-notify.json"), []byte(`{"seeded":true,"private":"not-in-preview"}`), 0600); e != nil {
		t.Fatal(e)
	}
	w := backupRequest(h, "POST", "/admin/backup/export", strings.NewReader(`{"passphrase":"synthetic passphrase"}`), s.token, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	encrypted := append([]byte(nil), w.Body.Bytes()...)
	w = backupRequest(h, "POST", "/admin/backup/preview", bytes.NewReader(encrypted), s.token, map[string]string{"X-Backup-Passphrase": "synthetic passphrase"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "not-in-preview") || strings.Contains(w.Body.String(), s.token) {
		t.Fatal("preview leaked secrets")
	}
	var plan struct{ ID, Digest string }
	if e := json.Unmarshal(w.Body.Bytes(), &plan); e != nil {
		t.Fatal(e)
	}
	bad, _ := json.Marshal(map[string]string{"id": plan.ID, "digest": "wrong", "confirm": "REPLACE"})
	w = backupRequest(h, "POST", "/admin/backup/restore", bytes.NewReader(bad), s.token, nil)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	w = backupRequest(h, "DELETE", "/admin/backup/preview", nil, s.token, nil)
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	if s.backup.plan != nil {
		t.Fatal("plan survived cancel")
	}
	w = backupRequest(h, "POST", "/admin/backup/preview", bytes.NewReader(encrypted), s.token, map[string]string{"X-Backup-Passphrase": "synthetic passphrase"})
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &plan)
	body, _ := json.Marshal(map[string]string{"id": plan.ID, "digest": plan.Digest, "confirm": "REPLACE"})
	w = backupRequest(h, "POST", "/admin/backup/restore", bytes.NewReader(body), s.token, nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	b, e := os.ReadFile(filepath.Join(s.store.Dir(), "state-auth.json"))
	if e != nil || !bytes.Contains(b, []byte(s.token)) || !bytes.Contains(b, []byte(s.agentToken)) {
		t.Fatal("destination credentials changed", e)
	}
}
func TestPrepareBackupPreservesNullableOverrides(t *testing.T) {
	s, _ := backupTestHandler(t)
	dir := t.TempDir()
	raw := []byte(`{"notify_interval":null,"notify_enabled":false,"future_setting":{"x":null}}`)
	if e := os.WriteFile(filepath.Join(dir, "state-settings.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	plugins := []byte(`{"plugins":{"future":{"enabled":false,"settings":{"secret":null,"unknown":7}}}}`)
	if e := os.WriteFile(filepath.Join(dir, "state-plugins.json"), plugins, 0600); e != nil {
		t.Fatal(e)
	}
	p := backup.Plan{Directory: dir, Manifest: backup.Manifest{Product: "homelab-inventory", Config: map[string]string{"APP_SETTINGS_EFFECTIVE": `{"notify_interval":"5m0s","notify_enabled":true}`, "APP_PLUGINS_EFFECTIVE": `{"future":{"secret":"source-seed"}}`}}}
	if _, e := s.prepareBackup(&p); e != nil {
		t.Fatal(e)
	}
	if _, e := migrateBackupPlugins(dir, p.Manifest.Config, false); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(dir, "state-settings.json"))
	if e != nil || !bytes.Equal(got, raw) {
		t.Fatal("raw settings changed", e, string(got))
	}
	got, e = os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if e != nil || !bytes.Contains(got, []byte(`"secret":null`)) || !bytes.Contains(got, []byte(`"unknown":7`)) {
		t.Fatal("raw plugin settings changed", e, string(got))
	}
}
