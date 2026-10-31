package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/backup"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

func migrateBackupPlugins(dir string, config map[string]string, private bool) ([]string, error) {
	type entry struct {
		Enabled  bool           `json:"enabled"`
		Settings map[string]any `json:"settings"`
		Updated  time.Time      `json:"updated_at,omitempty"`
	}
	state := struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}{map[string]json.RawMessage{}}
	file := filepath.Join(dir, "state-plugins.json")
	if b, e := os.ReadFile(file); e == nil {
		if json.Unmarshal(b, &state) != nil || state.Plugins == nil {
			return nil, backup.ErrInvalid
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	issues := []string{}
	if !private {
		return issues, nil
	}
	save := func(id string, cfg map[string]any, enabled bool) error {
		b, e := json.Marshal(entry{enabled, cfg, time.Now().UTC()})
		if e == nil {
			state.Plugins[id] = b
		}
		return e
	}
	if private {
		mappings := map[string]map[string]string{
			"media":      {"sonarr_url": "SONARR_URL", "sonarr_key": "SONARR_API_KEY", "radarr_url": "RADARR_URL", "radarr_key": "RADARR_API_KEY", "overseerr_url": "OVERSEERR_URL", "overseerr_key": "OVERSEERR_API_KEY", "tautulli_url": "TAUTULLI_URL", "tautulli_key": "TAUTULLI_API_KEY"},
			"tdarr":      {"tdarr_url": "TDARR_URL", "policy_url": "MEDIAPOLICY_URL", "policy_key": "MEDIAPOLICY_TOKEN", "sidecar_url": "TDARR_SHRINKD_URL", "sidecar_key": "TDARR_SHRINKD_TOKEN", "sonarr_url": "SONARR_URL", "sonarr_key": "SONARR_API_KEY", "radarr_url": "RADARR_URL", "radarr_key": "RADARR_API_KEY", "tautulli_url": "TAUTULLI_URL", "tautulli_key": "TAUTULLI_API_KEY", "radarr_root": "TDARR_RADARR_ROOT", "sonarr_root": "TDARR_SONARR_ROOT", "tdarr_movie_root": "TDARR_MOVIE_ROOT", "tdarr_series_root": "TDARR_SERIES_ROOT", "protected_titles": "TDARR_PROTECTIONS"},
			"archive":    {"policy_url": "MEDIAPOLICY_URL", "policy_token": "MEDIAPOLICY_TOKEN"},
			"network":    {"prometheus_url": "PROMETHEUS_URL", "prometheus_token": "PROMETHEUS_TOKEN", "qbit_url": "QBIT_URL", "qbit_user": "QBIT_USER", "qbit_password": "QBIT_PASSWORD", "routes_url": "PANGOLIN_URL"},
			"uploads":    {"folder_path": "UPLOAD_DIR", "paperless_url": "PAPERLESS_URL", "paperless_user": "PAPERLESS_USER", "paperless_password": "PAPERLESS_PASSWORD", "calibre_url": "CALIBRE_WEB_URL", "calibre_user": "CALIBRE_WEB_USER", "calibre_password": "CALIBRE_WEB_PASSWORD"},
			"security":   {"tokens": "CANARY_TOKENS", "paths": "CANARY_PATHS"},
			"cloudflare": {"api_token": "CLOUDFLARE_TOKEN", "zone_id": "CLOUDFLARE_ZONE"},
		}
		for id, fields := range mappings {
			cfg := map[string]any{}
			for key, env := range fields {
				if v, ok := config[env]; ok {
					cfg[key] = v
				}
			}
			enabled := false
			switch id {
			case "media":
				for _, service := range []string{"sonarr", "radarr", "overseerr", "tautulli"} {
					url, _ := cfg[service+"_url"].(string)
					key, _ := cfg[service+"_key"].(string)
					if url != "" && key != "" {
						enabled = true
					} else {
						delete(cfg, service+"_url")
						delete(cfg, service+"_key")
					}
				}
			case "tdarr":
				enabled = config["TDARR_URL"] != "" && config["TDARR_RADARR_ROOT"] != "" && config["TDARR_PROTECTIONS"] != ""
			case "archive":
				enabled = config["MEDIAPOLICY_URL"] != ""
			case "network":
				enabled = true
				cfg["speedtest_url"] = "https://speed.cloudflare.com"
				cfg["speed_download_url"] = "http://test-debit.free.fr/image.iso"
				cfg["speed_fallback_url"] = "http://ping.online.net/10000Mo.dat"
				cfg["speed_download_size"] = 678428672
				cfg["speed_fallback_size"] = 10000000000
				if v, e := time.ParseDuration(config["QBIT_SEED_REQUIREMENT"]); e == nil {
					cfg["seed_hours"] = v.Hours()
				}
			case "uploads":
				if owner, ok := config["UPLOAD_OWNER"]; ok {
					value, e := strconv.ParseInt(owner, 10, 32)
					if e != nil || value < -1 {
						return nil, backup.ErrInvalid
					}
					cfg["folder_uid"] = value
					cfg["folder_gid"] = value
				}
				enabled = config["UPLOAD_DIR"] != "" || config["PAPERLESS_USER"] != "" && config["PAPERLESS_PASSWORD"] != "" || config["CALIBRE_WEB_USER"] != "" && config["CALIBRE_WEB_PASSWORD"] != ""
				if config["PAPERLESS_USER"] == "" || config["PAPERLESS_PASSWORD"] == "" {
					delete(cfg, "paperless_url")
					delete(cfg, "paperless_user")
					delete(cfg, "paperless_password")
				}
				if config["CALIBRE_WEB_USER"] == "" || config["CALIBRE_WEB_PASSWORD"] == "" {
					delete(cfg, "calibre_url")
					delete(cfg, "calibre_user")
					delete(cfg, "calibre_password")
				}
			case "security":
				enabled = config["CANARY_TOKENS"] != "" || config["CANARY_PATHS"] != ""
				if window, e := time.ParseDuration(config["CANARY_WINDOW"]); e == nil {
					cfg["window_hours"] = window.Hours()
				}
			case "cloudflare":
				enabled = config["CLOUDFLARE_TOKEN"] != "" && config["CLOUDFLARE_ZONE"] != ""
				cfg["api_url"] = "https://api.cloudflare.com/client/v4"
			}
			if len(cfg) == 0 {
				continue
			}
			if e := save(id, cfg, enabled); e != nil {
				return nil, e
			}
			if enabled {
				issues = append(issues, "Enable migrated "+id+" plugin with source connections. Polling resumes after successful restore; actions still require confirmation and operator permission.")
			}
		}
		for _, name := range []string{"canary-trips.jsonl", "canary-trips.jsonl.1", "speedtest-history.jsonl"} {
			src := filepath.Join(dir, name)
			if b, e := os.ReadFile(src); e == nil {
				target := "security"
				if name == "speedtest-history.jsonl" {
					target = "network"
				}
				dst := filepath.Join(dir, "plugins", target, name)
				if e = os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
					return nil, e
				}
				if e = fsx.WriteAtomic(dst, b, 0600); e != nil {
					return nil, e
				}
			} else if !os.IsNotExist(e) {
				return nil, e
			}
		}
	}
	b, e := json.Marshal(state)
	if e != nil {
		return nil, e
	}
	if e = fsx.WriteAtomic(file, b, 0600); e != nil {
		return nil, e
	}
	// Make explicit that destination plugin environment overrides remain authoritative.
	for _, key := range strings.Fields("PUBLIC_URL TRUSTED_PROXY ACTIONS_ENABLED ACTIONS_HOST ACTIONS_COMPOSE_FILES OPERATOR_TOKEN") {
		if _, ok := os.LookupEnv(key); ok {
			issues = append(issues, "Destination deployment setting "+key+" remains in force.")
		}
	}
	return issues, nil
}
