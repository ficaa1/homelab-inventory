package plugin

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
var viewPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var refPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func validURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.Opaque == ""
}

func validateManifest(m Manifest) error {
	if !idPattern.MatchString(m.ID) || m.Name == "" {
		return errors.New("invalid plugin manifest identity")
	}
	actions := map[string]bool{}
	for _, a := range m.Actions {
		if !viewPattern.MatchString(a.ID) || actions[a.ID] || a.Label == "" || a.TimeoutSeconds < 0 || a.TimeoutSeconds > 360 {
			return errors.New("invalid action manifest")
		}
		actions[a.ID] = true
		if err := validateManifest(Manifest{ID: m.ID, Name: m.Name, Settings: a.Fields}); err != nil {
			return err
		}
	}
	keys := map[string]bool{}
	for _, f := range m.Settings {
		if !keyPattern.MatchString(f.Key) || keys[f.Key] {
			return errors.New("invalid or duplicate plugin field")
		}
		keys[f.Key] = true
		switch f.Type {
		case URL, Secret, Text, Number, Bool, Select:
		default:
			return errors.New("unknown plugin field type")
		}
		if f.Type == Secret && f.Default != nil {
			return errors.New("secret fields cannot have defaults")
		}
		if f.Type != Select && len(f.Options) != 0 {
			return errors.New("options require a select field")
		}
		if f.Type == Select {
			opts := map[string]bool{}
			if len(f.Options) == 0 {
				return errors.New("select field requires options")
			}
			for _, o := range f.Options {
				if opts[o.Value] {
					return errors.New("duplicate select option")
				}
				opts[o.Value] = true
			}
		}
		if f.Default != nil {
			if _, msg := fieldValue(f, f.Default); msg != "" {
				return errors.New("invalid plugin field default")
			}
		}
	}
	if m.Tab != nil {
		if len(m.Tab.Views) == 0 {
			return errors.New("plugin tab requires views")
		}
		names := map[string]bool{}
		for _, v := range m.Tab.Views {
			if !viewPattern.MatchString(v.Name) || names[v.Name] {
				return errors.New("invalid or duplicate view name")
			}
			names[v.Name] = true
		}
	}
	return nil
}

func cloneConfig(c Config) Config {
	out := Config{}
	for k, v := range c {
		out[k] = v
	}
	return out
}

// Merge preserves absent keys and records null as an explicit clear. Keeping
// the tombstone prevents an environment seed or default reviving a cleared key.
func Merge(stored Config, patch Config) Config {
	next := cloneConfig(stored)
	for k, v := range patch {
		next[k] = v
	}
	return next
}

func resolve(m Manifest, stored Config, required bool) (Config, error) {
	return resolveWithSeeds(m, stored, required, nil)
}
func resolveWithSeeds(m Manifest, stored Config, required bool, seeds Config) (Config, error) {
	out := Config{}
	fields := map[string]string{}
	known := map[string]bool{}
	for _, f := range m.Settings {
		known[f.Key] = true
		v, present := stored[f.Key]
		if !present {
			if seed, ok := seeds[f.Key]; ok {
				v = seed
			} else {
				env := "PLUGIN_" + strings.ToUpper(strings.ReplaceAll(m.ID, "-", "_")) + "_" + strings.ToUpper(f.Key)
				if s, ok := os.LookupEnv(env); ok {
					v = s
					if f.Type == Bool {
						b, err := strconv.ParseBool(s)
						if err != nil {
							fields[f.Key] = "must be a boolean"
							continue
						}
						v = b
					}
				} else {
					v = f.Default
				}
			}
		}
		if v == nil || (isString(v) && strings.TrimSpace(v.(string)) == "" && f.Type != Number && f.Type != Bool) {
			if required && f.Required {
				fields[f.Key] = "is required"
			}
			continue
		}
		value, msg := fieldValue(f, v)
		if msg != "" {
			fields[f.Key] = msg
		} else {
			out[f.Key] = value
		}
	}
	for k := range stored {
		if !known[k] {
			fields[k] = "unknown setting"
		}
	}
	if len(fields) > 0 {
		return out, &ValidationError{Message: "invalid plugin settings", Fields: fields}
	}
	return out, nil
}
func isString(v any) bool { _, ok := v.(string); return ok }
func fieldValue(f Field, v any) (any, string) {
	switch f.Type {
	case Bool:
		if b, ok := v.(bool); ok {
			return b, ""
		}
		return nil, "must be a boolean"
	case Number:
		var n float64
		switch value := v.(type) {
		case float64:
			n = value
		case int:
			n = float64(value)
		case int8:
			n = float64(value)
		case int16:
			n = float64(value)
		case int32:
			n = float64(value)
		case int64:
			n = float64(value)
		case uint:
			n = float64(value)
		case uint8:
			n = float64(value)
		case uint16:
			n = float64(value)
		case uint32:
			n = float64(value)
		case uint64:
			n = float64(value)
		case float32:
			n = float64(value)
		case json.Number:
			var err error
			n, err = value.Float64()
			if err != nil {
				return nil, "must be a number"
			}
		case string:
			var err error
			n, err = strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, "must be a number"
			}
		default:
			return nil, "must be a number"
		}
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, "must be a finite number"
		}
		return n, ""
	default:
		s, ok := v.(string)
		if !ok {
			return nil, "must be text"
		}
		if f.Type == URL && !validURL(s) {
			return nil, "must be an http(s) URL"
		}
		if f.Type == Select {
			for _, o := range f.Options {
				if o.Value == s {
					return s, ""
				}
			}
			return nil, "must be one of the listed options"
		}
		return s, ""
	}
}

func masked(m Manifest, c Config) Config {
	out := cloneConfig(c)
	for _, f := range m.Settings {
		if f.Type == Secret {
			s, _ := c[f.Key].(string)
			out[f.Key] = map[string]bool{"set": s != ""}
		}
	}
	return out
}
