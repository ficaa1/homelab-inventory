package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/backup"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

func backupProduct() string { return "homelab-inventory" }

const backupEnvKeys = "INGEST_TOKEN API_TOKEN AGENT_TOKEN OIDC_CLIENT_ID OIDC_CLIENT_SECRET OIDC_REDIRECT_URL OIDC_ALLOWED_EMAILS OIDC_ENABLED OIDC_ISSUER OIDC_PUBLIC_URL OIDC_ALLOWED OIDC_SESSION_HOURS OIDC_PROVIDER_NAME NOTIFY_INTERVAL NOTIFY_LEVEL DIGEST_INTERVAL APPRISE_URL DIGEST_APPRISE_URL DASHBOARD_NAME RUN_KEEP_ALL_DAYS RUN_HOURLY_DAYS RUN_DAILY_DAYS RUN_COMPRESS_AFTER_DAYS ICONS_FETCH AGENT_RELEASE_DIR CANARY_TOKENS CANARY_PATHS CANARY_WINDOW TAUTULLI_URL TAUTULLI_API_KEY SONARR_URL SONARR_API_KEY RADARR_URL RADARR_API_KEY OVERSEERR_URL OVERSEERR_API_KEY TDARR_URL CLOUDFLARE_TOKEN CLOUDFLARE_ZONE PROMETHEUS_URL PANGOLIN_URL QBIT_URL QBIT_USER QBIT_PASSWORD QBIT_SEED_REQUIREMENT TDARR_SHRINKD_URL TDARR_SHRINKD_TOKEN SEASONGATE_URL SEASONGATE_TOKEN MEDIAPOLICY_URL MEDIAPOLICY_TOKEN UPLOAD_DIR UPLOAD_OWNER CALIBRE_WEB_URL CALIBRE_WEB_USER CALIBRE_WEB_PASSWORD PAPERLESS_URL PAPERLESS_USER PAPERLESS_PASSWORD TV_MEDIA_TOKEN TV_REQUEST_TOKEN TV_PLEX_USER_ID TV_LETTERBOXD_USERNAME"

func (s *server) backupConfig() map[string]string {
	out := map[string]string{}
	for _, k := range strings.Fields(backupEnvKeys) {
		if v, ok := os.LookupEnv(k); ok {
			out[k] = v
		}
	}
	defaults, _ := json.Marshal(s.defaults)
	out["APP_SETTINGS_DEFAULTS"] = string(defaults)
	eff := s.effective()
	raw := map[string]any{"notify_level": string(eff.NotifyLevel), "notify_interval": eff.NotifyInterval.String(), "digest_interval": eff.DigestInterval.String(), "apprise_url": eff.AppriseURL, "digest_apprise_url": eff.DigestAppriseURL, "notify_enabled": eff.NotifyEnabled, "run_retention": eff.RunRetention}
	b, _ := json.Marshal(raw)
	out["APP_SETTINGS_EFFECTIVE"] = string(b)
	if s.plugins != nil {
		b, _ := json.Marshal(s.plugins.BackupConfig())
		out["APP_PLUGINS_EFFECTIVE"] = string(b)
		for _, p := range s.plugins.List() {
			for _, field := range p.SettingsSchema {
				k := "PLUGIN_" + strings.ToUpper(strings.ReplaceAll(p.ID, "-", "_")) + "_" + strings.ToUpper(field.Key)
				if v, ok := os.LookupEnv(k); ok {
					out[k] = v
				}
			}
		}
	}
	return out
}
func (s *server) prepareBackup(p *backup.Plan) ([]string, error) {
	if p.Manifest.Product != "homelab-dash" && p.Manifest.Product != "homelab-inventory" {
		return nil, backup.ErrInvalid
	}
	issues := []string{"Replace removes the destination inventory and application state. Keep a destination backup first. New data arriving after this preview will also be replaced.", "Browser sessions and enrollment codes will be invalidated. Destination credentials and browser identity settings remain in force.", "External databases, media, upload files, service mounts, release signing trust and deployment configuration require separate backups."}
	// Keep original source config private for review/recovery, including unsupported
	// values. It is never returned by the preview endpoint or written to logs.
	archiveDir := filepath.Join(p.Directory, "state-migration")
	if e := os.MkdirAll(archiveDir, 0700); e != nil {
		return nil, e
	}
	b, e := json.Marshal(p.Manifest.Config)
	if e != nil {
		return nil, e
	}
	if e = fsx.WriteAtomic(filepath.Join(archiveDir, "source-config.json"), b, 0600); e != nil {
		return nil, e
	}
	// Classify legacy/unknown roots so the host enumerator never mistakes them
	// for inventory hosts. Unknown bytes remain available in a reserved namespace.
	ds, e := os.ReadDir(p.Directory)
	if e != nil {
		return nil, e
	}
	for _, d := range ds {
		n := d.Name()
		if n == "plugins" || n == "packagesets" || n == "state-migration" {
			continue
		}
		if d.IsDir() {
			host := false
			if store.ValidateHost(n) == nil {
				children, e := os.ReadDir(filepath.Join(p.Directory, n))
				if e != nil {
					return nil, e
				}
				for _, c := range children {
					if c.Name() == "software" || strings.HasSuffix(c.Name(), ".json") || strings.HasSuffix(c.Name(), ".json.gz") {
						host = true
					}
				}
			}
			if !host || n == "tv-requests" {
				if e = os.Rename(filepath.Join(p.Directory, n), filepath.Join(archiveDir, n)); e != nil {
					return nil, e
				}
				issues = append(issues, "Legacy or unknown directory preserved in migration records: "+n)
			}
		}
	}
	if p.Manifest.Product == "homelab-dash" {
		issues = append(issues, "Legacy agent compatibility follows the option selected for this preview. Request receipts are checked before restore can proceed.")
	}
	return issues, nil
}
