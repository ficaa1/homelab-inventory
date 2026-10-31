package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/backup"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

type backupAdmin struct {
	generation atomic.Uint64
	blockers   []string
	mu         sync.Mutex
	plan       *backup.Plan
	id         string
	expires    time.Time
}

func (s *server) backupRoutes(mux *http.ServeMux) {
	if s.backup == nil {
		s.backup = &backupAdmin{}
	}
	for route, handler := range map[string]http.HandlerFunc{"POST /admin/backup/export": s.handleBackupExport, "POST /admin/backup/preview": s.handleBackupPreview, "POST /admin/backup/restore": s.handleBackupRestore, "DELETE /admin/backup/preview": s.handleBackupCancel} {
		authorized := s.auth(s.backupAdminOnly(handler))
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			generation := s.backup.generation.Load()
			s.backup.mu.Lock()
			defer s.backup.mu.Unlock()
			if generation != s.backup.generation.Load() {
				writeError(w, 409, "application changed; reload and sign in again")
				return
			}
			authorized(w, r)
		})
	}
}
func (s *server) backupAdminOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		origin, err := url.Parse(r.Header.Get("Origin"))
		if r.Header.Get("X-Requested-By") != "homelab-inventory" || (r.Header.Get("Origin") != "" && (err != nil || origin.Host != r.Host)) {
			writeError(w, 403, "same-origin request required")
			return
		}
		if s.apiToken() == "" {
			writeError(w, 403, "claim this instance before using backups")
			return
		}
		if s.backupMaintenance == nil || s.backupReload == nil {
			writeError(w, 503, "backup lifecycle is unavailable")
			return
		}
		next(w, r)
	}
}
func (s *server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Passphrase) < 12 {
		writeError(w, 400, "use a passphrase of at least 12 characters")
		return
	}
	root := s.store.Dir()
	f, err := os.CreateTemp(root, ".backup-export-")
	if err != nil {
		writeError(w, 500, "cannot create backup staging file")
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	err = s.backupMaintenance(r.Context(), func() error {
		return backup.Encrypt(r.Context(), f, req.Passphrase, func(out io.Writer) error {
			_, e := backup.Create(r.Context(), root, backupProduct(), s.backupConfig(), out, configuredBackupLimits())
			return e
		})
	})
	if err != nil {
		writeError(w, 500, "backup could not be completed")
		return
	}
	if _, err = f.Seek(0, 0); err != nil {
		writeError(w, 500, "cannot read backup")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="inventory-backup.hlb"`)
	http.ServeContent(w, r, "inventory-backup.hlb", time.Now(), f)
}
func (s *server) clearBackupPlan() {
	if s.backup.plan != nil {
		os.RemoveAll(s.backup.plan.Directory)
	}
	s.backup.plan = nil
	s.backup.id = ""
}
func (s *server) handleBackupCancel(w http.ResponseWriter, r *http.Request) {
	s.clearBackupPlan()
	w.WriteHeader(204)
}
func (s *server) handleBackupPreview(w http.ResponseWriter, r *http.Request) {
	s.clearBackupPlan()
	r.Body = http.MaxBytesReader(w, r.Body, configuredBackupLimits().TotalBytes+(1<<30))
	pass, decodeErr := url.PathUnescape(r.Header.Get("X-Backup-Passphrase"))
	if decodeErr != nil {
		writeError(w, 400, "invalid passphrase encoding")
		return
	}
	reader, clean, err := backup.Decrypt(r.Context(), r.Body, s.store.Dir(), pass, configuredBackupLimits().TotalBytes+(1<<30))
	if err != nil {
		writeError(w, 400, "cannot authenticate backup: check passphrase and file")
		return
	}
	defer clean()
	plan, err := backup.Stage(r.Context(), reader, s.store.Dir(), configuredBackupLimits())
	if err != nil {
		writeError(w, 400, "backup validation failed")
		return
	}
	err = validateBackupPackages(plan.Directory)
	var issues []string
	if err == nil {
		issues, err = s.prepareBackup(&plan)
	}
	if err == nil {
		var extra []string
		extra, err = s.backupMigrationOptions(&plan, r)
		issues = append(issues, extra...)
	}
	if err != nil {
		os.RemoveAll(plan.Directory)
		writeError(w, 400, "backup contents are incompatible")
		return
	}
	s.backup.blockers, err = s.validateBackupPlugins(plan.Directory)
	if err != nil {
		os.RemoveAll(plan.Directory)
		writeError(w, 400, "backup plugin validation failed")
		return
	}
	if err = backup.Seal(r.Context(), &plan); err != nil {
		os.RemoveAll(plan.Directory)
		writeError(w, 400, "cannot verify prepared restore plan")
		return
	}
	id := make([]byte, 32)
	if _, err = rand.Read(id); err != nil {
		os.RemoveAll(plan.Directory)
		writeError(w, 500, "cannot stage backup")
		return
	}
	s.backup.id = hex.EncodeToString(id)
	s.backup.plan = &plan
	s.backup.expires = time.Now().Add(30 * time.Minute)
	hosts := map[string]bool{}
	runs, software, packages := 0, 0, 0
	for _, e := range plan.Manifest.Files {
		parts := strings.Split(e.Path, "/")
		if len(parts) > 1 && parts[0] == "packagesets" {
			packages++
		}
		if len(parts) > 1 && parts[1] == "software" {
			software++
			hosts[parts[0]] = true
		}
		if len(parts) == 2 && (strings.HasSuffix(parts[1], ".json") || strings.HasSuffix(parts[1], ".json.gz")) && parts[0] != "packagesets" && parts[0] != "tv-requests" && parts[0] != "plugins" {
			runs++
			hosts[parts[0]] = true
		}
	}
	writeJSON(w, 200, map[string]any{"id": s.backup.id, "digest": plan.Digest, "source": plan.Manifest.Product, "created": plan.Manifest.Created, "files": len(plan.Manifest.Files), "bytes": plan.Bytes, "hosts": len(hosts), "runs": runs, "software_files": software, "package_sets": packages, "issues": issues, "exclusions": plan.Manifest.Exclusions, "expires_at": s.backup.expires, "mode": "replace", "blockers": s.backup.blockers, "credentials": "Destination administrator, agent and operator credentials are kept. Browser sessions and enrollment codes are invalidated."})
}
func (s *server) handleBackupRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string `json:"id"`
		Digest  string `json:"digest"`
		Confirm string `json:"confirm"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Confirm != "REPLACE" || s.backup.plan == nil || req.ID != s.backup.id || req.Digest != s.backup.plan.Digest || time.Now().After(s.backup.expires) {
		writeError(w, 409, "restore plan expired or does not match; preview the file again")
		return
	}
	if len(s.backup.blockers) > 0 {
		writeError(w, 409, "restore prerequisites are unresolved; fix destination configuration and preview again")
		return
	}
	plan := s.backup.plan
	err := s.backupMaintenance(r.Context(), func() error {
		// Destination credentials can change while a preview is open. Apply the
		// current credentials only under the lifecycle drain, never the source keys.
		if e := backup.Verify(r.Context(), *plan); e != nil {
			return e
		}
		if e := s.preserveBackupCredentials(plan.Directory); e != nil {
			return e
		}
		e := backup.Install(s.store.Dir(), plan.Directory, s.backupReload)
		if e != nil {
			return errors.Join(e, s.backupReload())
		}
		return nil
	})
	s.clearBackupPlan()
	s.backup.generation.Add(1)
	if err != nil {
		writeError(w, 500, "Restore encountered an error. Check server status before retrying; automatic recovery protects the last committed dataset.")
		return
	}
	writeJSON(w, 200, map[string]any{"restored": true, "sign_in_required": true})
}
func (s *server) preserveBackupCredentials(dir string) error {
	auth := map[string]any{"api_token": s.apiToken(), "agent_token": s.agentCredential()}
	b, e := json.Marshal(auth)
	if e != nil {
		return e
	}
	if e = fsx.WriteAtomic(filepath.Join(dir, "state-auth.json"), b, 0600); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(dir, "state-session-key")); e != nil && !os.IsNotExist(e) {
		return e
	}
	// Browser identity origin/client remains the destination's deliberate setup.
	var source map[string]json.RawMessage
	b, e = os.ReadFile(filepath.Join(dir, "state-settings.json"))
	if e == nil {
		if json.Unmarshal(b, &source) != nil {
			return backup.ErrInvalid
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if source == nil {
		source = map[string]json.RawMessage{}
	}
	current, e := s.readSettings()
	if e != nil {
		return e
	}
	source["auth"], e = json.Marshal(current.Auth)
	if e != nil {
		return e
	}
	source["auth_generation"], _ = json.Marshal(current.AuthGeneration + 1)
	b, e = json.Marshal(source)
	if e != nil {
		return e
	}
	return fsx.WriteAtomic(filepath.Join(dir, "state-settings.json"), b, 0600)
}
