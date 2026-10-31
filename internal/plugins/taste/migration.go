package taste

import (
	"encoding/json"
	"errors"
	"path"
)

// ConvertLegacy maps owner-bound legacy files without re-encoding any history,
// verdict, sync checkpoint, unknown field or timestamp. It never decodes gob.
// The caller must persist the explicit household/owner binding in Taste settings.
// Request receipts belong to Dusk's durable coordinator and are not Taste data.
func ConvertLegacy(files map[string][]byte, profile string, owner int) (map[string][]byte, error) {
	if !profileName.MatchString(profile) || owner <= 0 {
		return nil, errors.New("explicit household owner binding required")
	}
	out := map[string][]byte{}
	for _, name := range []string{"letterboxd.json", "taste-feedback.json", "tv-discovery.json", "taste-model.json", "taste-cache.json", "tastetwins.gob"} {
		b, ok := files[name]
		if !ok {
			continue
		}
		if name != "tastetwins.gob" {
			if !json.Valid(b) {
				return nil, errors.New("invalid legacy taste JSON")
			}
			if name != "taste-cache.json" {
				var v struct {
					Owner int `json:"owner"`
				}
				if json.Unmarshal(b, &v) != nil || v.Owner != owner {
					return nil, errors.New("legacy taste owner mismatch")
				}
			}
		}
		out[path.Join("plugins/taste/profiles", profile, name)] = append([]byte(nil), b...)
	}
	return out, nil
}
