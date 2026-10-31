package taste

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/twins"
)

// ValidateLocal opens durable state without creating profiles or starting work.
func (t *Integration) ValidateLocal() error {
	if t.artifact != "" {
		if _, e := twins.LoadJSON(filepath.Join(t.host.CacheDir(), t.artifact)); e != nil {
			return errors.New("invalid licensed recommendation artifact")
		}
	}
	for id, b := range t.bindings {
		dir := filepath.Join(t.host.CacheDir(), "profiles", id)
		p, e := letterboxd.Load(filepath.Join(dir, "letterboxd.json"))
		if e != nil || p != nil && (p.Owner != b.Owner || b.Username != "" && p.SyncUser != "" && p.SyncUser != b.Username) {
			return errors.New("invalid household Letterboxd state")
		}
		f := engine.Feedback{Path: filepath.Join(dir, "taste-feedback.json"), Owner: b.Owner}
		if _, e = f.Load(); e != nil {
			return errors.New("invalid household feedback")
		}
		pref := engine.Preferences{Path: filepath.Join(dir, "tv-discovery.json"), Owner: b.Owner}
		if _, e = pref.Apply(engine.Report{}); e != nil {
			return errors.New("invalid household preferences")
		}
		for _, name := range []string{"taste-model.json", "taste-cache.json"} {
			f, e := os.Open(filepath.Join(dir, name))
			if os.IsNotExist(e) {
				continue
			}
			if e != nil {
				return errors.New("household recommendation state unavailable")
			}
			raw, e := io.ReadAll(io.LimitReader(f, (64<<20)+1))
			f.Close()
			if e != nil || len(raw) > 64<<20 || !json.Valid(raw) {
				return errors.New("invalid household recommendation state")
			}
			if name == "taste-model.json" {
				var model engine.TasteModel
				if json.Unmarshal(raw, &model) != nil || model.Owner != b.Owner {
					return errors.New("household model owner mismatch")
				}
			}
		}
	}
	return nil
}
