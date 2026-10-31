package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste"
)

// migrateBackupDusk converts only staged private data, never contacting services.
func migrateBackupDusk(dir string, config map[string]string, profile string, owner int) ([]string, error) {
	issues := []string{}
	// Legacy filenames contain a target hash, not the target. Keep the original
	// global barrier; guessing a title from a hash would lose duplicate protection.
	for _, root := range []string{filepath.Join(dir, "state-migration", "tv-requests"), filepath.Join(dir, "tv-requests")} {
		entries, e := os.ReadDir(root)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		dest := filepath.Join(dir, "plugins", "dusk", "core", "legacy-requests")
		if e = os.MkdirAll(dest, 0700); e != nil {
			return nil, e
		}
		if len(entries) > 20000 {
			return nil, errors.New("legacy request receipt limit")
		}
		for _, entry := range entries {
			if entry.IsDir() || !regexp.MustCompile(`^[a-f0-9]{64}\.json$`).MatchString(entry.Name()) {
				return nil, errors.New("invalid legacy request receipt")
			}
			b, e := os.ReadFile(filepath.Join(root, entry.Name()))
			if e != nil {
				return nil, e
			}
			var receipt struct {
				Operation string `json:"operation"`
				State     string `json:"state"`
			}
			if len(b) > 4096 || json.Unmarshal(b, &receipt) != nil || receipt.State == "" {
				return nil, errors.New("invalid legacy request receipt")
			}
			if e = fsx.WriteAtomic(filepath.Join(dest, entry.Name()), b, 0600); e != nil {
				return nil, e
			}
		}
		issues = append(issues, "Legacy request receipts retained byte for byte and enforced as a global duplicate barrier; unknown outcomes cannot replay.")
	}
	if profile == "" || owner <= 0 {
		return append(issues, "Taste history retained inactive: supply an explicit household profile and matching legacy owner to activate full recommendation behavior."), nil
	}
	if source := config["TV_PLEX_USER_ID"]; source != "" {
		n, e := strconv.Atoi(source)
		if e != nil || n != owner {
			return nil, errors.New("legacy playback owner does not match explicit mapping")
		}
	}
	files := map[string][]byte{}
	for _, name := range []string{"letterboxd.json", "taste-feedback.json", "tv-discovery.json", "taste-model.json", "taste-cache.json", "tastetwins.gob"} {
		b, e := os.ReadFile(filepath.Join(dir, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		files[name] = b
	}
	converted, e := taste.ConvertLegacy(files, profile, owner)
	if e != nil {
		return nil, e
	}
	for name, b := range converted {
		dest := filepath.Join(dir, filepath.FromSlash(name))
		if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
			return nil, e
		}
		if e = fsx.WriteAtomic(dest, b, 0600); e != nil {
			return nil, e
		}
	}
	core := filepath.Join(dir, "plugins", "dusk", "core")
	if e = os.MkdirAll(core, 0700); e != nil {
		return nil, e
	}
	// A migration creates only the explicitly requested household identity. No
	// device, pairing code, bearer token or request policy crosses this boundary.
	devices := map[string]any{"profiles": []map[string]string{{"id": profile, "name": profile}}, "devices": []any{}, "codes": []any{}, "attempts": map[string]any{}}
	raw, e := json.Marshal(devices)
	if e != nil {
		return nil, e
	}
	if e = fsx.WriteAtomic(filepath.Join(core, "devices.json"), raw, 0600); e != nil {
		return nil, e
	}
	var state struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	b, e := os.ReadFile(filepath.Join(dir, "state-plugins.json"))
	if e == nil {
		if json.Unmarshal(b, &state) != nil {
			return nil, errors.New("invalid plugin migration settings")
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if state.Plugins == nil {
		state.Plugins = map[string]json.RawMessage{}
	}
	bindings, e := json.Marshal(map[string]taste.Binding{profile: {Owner: owner, Username: config["TV_LETTERBOXD_USERNAME"]}})
	if e != nil {
		return nil, e
	}
	settings := map[string]any{"bindings": string(bindings)}
	if config["TV_LETTERBOXD_USERNAME"] != "" {
		settings["letterboxd_url"] = "https://letterboxd.com"
		issues = append(issues, "Enabling Taste will resume public Letterboxd synchronization and Media metadata reads for the explicitly mapped owner.")
	}
	state.Plugins["taste"], e = json.Marshal(map[string]any{"enabled": true, "settings": settings})
	if e != nil {
		return nil, e
	}
	b, e = json.Marshal(state)
	if e != nil {
		return nil, e
	}
	if e = fsx.WriteAtomic(filepath.Join(dir, "state-plugins.json"), b, 0600); e != nil {
		return nil, e
	}
	issues = append(issues, "Full Taste history, feedback and recommendation model mapped to the explicit household profile. Pair devices and configure request policy separately.")
	if _, ok := files["tastetwins.gob"]; ok {
		issues = append(issues, "Restricted twins artifact preserved opaque. Supply a licensed sparse JSON conversion to resume twins lanes and familiar predictions.")
	}
	return issues, nil
}
