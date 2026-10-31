package main

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/ficaa1/homelab-inventory/internal/settings"
)

// Keep raw stored nullable overrides intact. Only their original environment
// seed moves into this private fallback layer; destination browser auth stays.
func applyBackupDefaults(s *server) error {
	b, e := os.ReadFile(filepath.Join(s.store.Dir(), "state-migration", "source-config.json"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var cfg map[string]string
	if e = json.Unmarshal(b, &cfg); e != nil {
		return e
	}
	if raw := cfg["APP_SETTINGS_DEFAULTS"]; raw != "" {
		var defaults settings.Defaults
		if e = json.Unmarshal([]byte(raw), &defaults); e != nil {
			return e
		}
		defaults.Auth = s.defaults.Auth
		s.defaults = defaults
	}
	return nil
}
