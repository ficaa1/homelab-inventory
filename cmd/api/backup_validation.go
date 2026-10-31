package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/ficaa1/homelab-inventory/internal/backup"
)

// Software histories reference shared blobs that ordinary run retention never
// owns. Validate those links without decoding or rewriting inventory runs.
func validateBackupPackages(dir string) error {
	hosts, e := os.ReadDir(dir)
	if e != nil {
		return e
	}
	for _, host := range hosts {
		if !host.IsDir() {
			continue
		}
		for _, name := range []string{"latest.json", "history.jsonl"} {
			f, e := os.Open(filepath.Join(dir, host.Name(), "software", name))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return e
			}
			scan := bufio.NewScanner(f)
			scan.Buffer(make([]byte, 4096), 64<<20)
			for scan.Scan() {
				var value any
				if json.Unmarshal(scan.Bytes(), &value) != nil {
					f.Close()
					return backup.ErrInvalid
				}
				if e = checkBackupReferences(dir, value); e != nil {
					f.Close()
					return e
				}
			}
			e = scan.Err()
			f.Close()
			if e != nil {
				return backup.ErrInvalid
			}
		}
	}
	return nil
}
func checkBackupReferences(dir string, value any) error {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if e := checkBackupReferences(dir, item); e != nil {
				return e
			}
		}
	case map[string]any:
		for key, item := range v {
			if key == "packages_ref" {
				ref, ok := item.(string)
				if !ok {
					return backup.ErrInvalid
				}
				if ref == "" {
					continue
				}
				if len(ref) != 64 {
					return backup.ErrInvalid
				}
				if _, e := hex.DecodeString(ref); e != nil {
					return backup.ErrInvalid
				}
				info, e := os.Stat(filepath.Join(dir, "packagesets", ref+".json.gz"))
				if e != nil || !info.Mode().IsRegular() {
					return backup.ErrInvalid
				}
			}
			if e := checkBackupReferences(dir, item); e != nil {
				return e
			}
		}
	}
	return nil
}

// validateBackupPlugins checks only local configuration and persisted state.
// Configure is forbidden to probe providers. Error text identifies the plugin,
// never secret field values or provider responses.
func (s *server) validateBackupPlugins(dir string) ([]string, error) {
	r, e := openPlugins(dir, log.New(io.Discard, "", 0))
	if e != nil {
		return nil, backup.ErrInvalid
	}
	defer r.Close()
	blockers := []string{}
	for _, p := range r.List() {
		if p.Enabled && p.State != "ok" {
			blockers = append(blockers, "Plugin "+p.ID+" needs valid destination configuration or external mounts before restore.")
		}
	}
	if e = r.ValidateTasteState(); e != nil {
		blockers = append(blockers, "Taste history or model could not be validated for the selected household.")
	}
	return blockers, nil
}
