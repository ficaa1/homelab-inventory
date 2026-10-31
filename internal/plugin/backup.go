package plugin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// BackupConfig is private configuration for encrypted operator backups only.
// It deliberately includes secrets and must never be served as dashboard JSON.
func (r *Registry) BackupConfig() map[string]Config {
	out := map[string]Config{}
	for _, id := range r.ids() {
		e := r.entries[id]
		e.op.Lock()
		out[id] = cloneConfig(e.config)
		e.op.Unlock()
	}
	return out
}

func loadBackupSeeds(dir string) (map[string]Config, error) {
	out := map[string]Config{}
	b, e := os.ReadFile(filepath.Join(dir, "state-migration", "source-config.json"))
	if os.IsNotExist(e) {
		return out, nil
	}
	if e != nil {
		return nil, errors.New("reading migrated plugin defaults")
	}
	var config map[string]string
	if json.Unmarshal(b, &config) != nil {
		return nil, errors.New("invalid migrated configuration")
	}
	if raw := config["APP_PLUGINS_EFFECTIVE"]; raw != "" {
		if json.Unmarshal([]byte(raw), &out) != nil {
			return nil, errors.New("invalid migrated plugin defaults")
		}
	}
	return out, nil
}
