package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/backup"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

func (s *server) backupMigrationOptions(p *backup.Plan, r *http.Request) ([]string, error) {
	issues, err := migrateBackupPlugins(p.Directory, p.Manifest.Config, p.Manifest.Product == "homelab-dash")
	if err != nil {
		return nil, err
	}
	if p.Manifest.Product != "homelab-dash" {
		return issues, nil
	}
	if r.Header.Get("X-Backup-Legacy-Agent") == "true" {
		token := p.Manifest.Config["INGEST_TOKEN"]
		if token == "" || token == s.apiToken() || token == s.operatorToken || validateTokens("", token, "") != nil {
			return nil, backup.ErrInvalid
		}
		b, e := json.Marshal(map[string]string{"token": token})
		if e != nil {
			return nil, e
		}
		if e = fsx.WriteAtomic(filepath.Join(p.Directory, "state-migration", "legacy-agent.json"), b, 0600); e != nil {
			return nil, e
		}
		issues = append(issues, "Explicit compatibility enabled: the source dashboard key will authorize inventory posts only, without administrator, operator or release trust.")
	} else {
		issues = append(issues, "Legacy agent compatibility is off. Re-enroll or reconfigure old agents with the destination agent credential.")
	}
	owner := 0
	err = nil
	if raw := r.Header.Get("X-Backup-Owner"); raw != "" {
		owner, err = strconv.Atoi(raw)
		if err != nil || owner <= 0 {
			return nil, backup.ErrInvalid
		}
	}
	profile := r.Header.Get("X-Backup-Profile")
	if sourceOwner, e := strconv.Atoi(p.Manifest.Config["TV_PLEX_USER_ID"]); e == nil && sourceOwner > 0 {
		if owner == 0 {
			owner = sourceOwner
		}
		if profile == "" {
			profile = "home"
		}
		issues = append(issues, "Bind migrated Taste history to destination household profile "+profile+" using the source owner. Final restore accepts this household mapping.")
	}
	extra, err := migrateBackupDusk(p.Directory, p.Manifest.Config, profile, owner)
	issues = append(issues, extra...)
	return issues, err
}
