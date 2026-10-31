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
		defaults, err := mergeBackupDefaults(s.defaults, raw)
		if err != nil {
			return err
		}
		s.defaults = defaults
	}
	return nil
}

// mergeBackupDefaults layers the source environment over the destination
// environment without erasing fields introduced after the backup was made.
// Backups intentionally carry a snapshot of defaults, so decoding directly
// into a new struct makes a newer field silently become its zero value.
func mergeBackupDefaults(base settings.Defaults, raw string) (settings.Defaults, error) {
	var merged map[string]json.RawMessage
	b, err := json.Marshal(base)
	if err != nil {
		return settings.Defaults{}, err
	}
	if err := json.Unmarshal(b, &merged); err != nil {
		return settings.Defaults{}, err
	}
	var source map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &source); err != nil {
		return settings.Defaults{}, err
	}
	for k, v := range source {
		if k != "Auth" {
			merged[k] = v
		}
	}
	b, err = json.Marshal(merged)
	if err != nil {
		return settings.Defaults{}, err
	}
	if err := json.Unmarshal(b, &base); err != nil {
		return settings.Defaults{}, err
	}
	return base, nil
}
