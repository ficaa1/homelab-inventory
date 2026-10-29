package plugin

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"
)

const MaxViewBytes = 1 << 20

type View struct {
	Title          string    `json:"title"`
	UpdatedAt      time.Time `json:"updated_at"`
	RefreshSeconds int       `json:"refresh_seconds"`
	Blocks         []Block   `json:"blocks"`
}

// Block is the closed union of the six declarative block types. Fields not
// belonging to a selected type are rejected rather than silently rendered.
type Block struct {
	Type    string           `json:"type"`
	Title   string           `json:"title,omitempty"`
	Empty   string           `json:"empty,omitempty"`
	Items   []Item           `json:"items,omitempty"`
	Columns []Column         `json:"columns,omitempty"`
	Rows    []map[string]any `json:"rows,omitempty"`
	Tone    string           `json:"tone,omitempty"`
	Text    string           `json:"text,omitempty"`
	Blocks  []Block          `json:"blocks,omitempty"`
}

// Item is shared by stats, list and posters; each block admits its own fields.
type Item struct {
	Label    string    `json:"label,omitempty"`
	Value    string    `json:"value,omitempty"`
	Hint     string    `json:"hint,omitempty"`
	Title    string    `json:"title,omitempty"`
	Subtitle string    `json:"subtitle,omitempty"`
	Meta     string    `json:"meta,omitempty"`
	Tone     string    `json:"tone,omitempty"`
	Image    string    `json:"image,omitempty"`
	Progress *Progress `json:"progress,omitempty"`
	Badges   []Badge   `json:"badges,omitempty"`
	Badge    *Badge    `json:"badge,omitempty"`
	Link     *Link     `json:"link,omitempty"`
}
type Progress struct {
	Value float64 `json:"value"`
	Label string  `json:"label,omitempty"`
}
type Badge struct {
	Text string `json:"text"`
	Tone string `json:"tone,omitempty"`
}
type Link struct {
	View   string            `json:"view,omitempty"`
	Params map[string]string `json:"params,omitempty"`
	Href   string            `json:"href,omitempty"`
}
type Column struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Format string `json:"format,omitempty"`
	Align  string `json:"align,omitempty"`
}

func toneOK(t string) bool {
	return t == "" || t == "ok" || t == "info" || t == "warn" || t == "crit" || t == "muted"
}

// ValidateView returns the exact compact JSON to send, bounded before serving.
func ValidateView(v View, manifest Manifest) ([]byte, error) {
	count := 0
	var walk func([]Block, int) error
	walk = func(blocks []Block, depth int) error {
		if depth > 2 && len(blocks) > 0 {
			return errors.New("view nesting exceeds depth 2")
		}
		count += len(blocks)
		if count > 50 {
			return errors.New("view exceeds 50 blocks")
		}
		for _, b := range blocks {
			if !toneOK(b.Tone) {
				return errors.New("invalid view tone")
			}
			if len(b.Items) > 500 || len(b.Rows) > 500 {
				return errors.New("view block exceeds 500 items or rows")
			}
			if b.Type != "section" && len(b.Blocks) > 0 {
				return errors.New("only sections can nest blocks")
			}
			if b.Type != "table" && (len(b.Rows) > 0 || len(b.Columns) > 0) {
				return errors.New("rows and columns require a table")
			}
			if b.Type != "stats" && b.Type != "list" && b.Type != "posters" && len(b.Items) > 0 {
				return errors.New("items require stats, list or posters")
			}
			if b.Type != "notice" && (b.Text != "" || b.Tone != "") {
				return errors.New("block text and tone require a notice")
			}
			switch b.Type {
			case "stats", "list", "posters":
				for _, i := range b.Items {
					if !toneOK(i.Tone) {
						return errors.New("invalid item tone")
					}
					if i.Image != "" && !refPattern.MatchString(i.Image) {
						return errors.New("invalid image ref")
					}
					if i.Progress != nil && (math.IsNaN(i.Progress.Value) || math.IsInf(i.Progress.Value, 0) || i.Progress.Value < 0 || i.Progress.Value > 1) {
						return errors.New("progress must be between 0 and 1")
					}
					badges := i.Badges
					if i.Badge != nil {
						badges = append(append([]Badge{}, badges...), *i.Badge)
					}
					for _, badge := range badges {
						if !toneOK(badge.Tone) {
							return errors.New("invalid badge tone")
						}
					}
					if err := validateLink(i.Link, manifest); err != nil {
						return err
					}
					if b.Type == "stats" && (i.Title != "" || i.Subtitle != "" || i.Meta != "" || i.Image != "" || i.Progress != nil || len(i.Badges) > 0 || i.Badge != nil || i.Link != nil) {
						return errors.New("invalid stats item fields")
					}
					if b.Type != "stats" && (i.Label != "" || i.Value != "" || i.Hint != "") {
						return errors.New("stats fields in a non-stats item")
					}
					if b.Type == "list" && i.Badge != nil || b.Type == "posters" && (len(i.Badges) > 0 || i.Meta != "" || i.Tone != "") {
						return errors.New("invalid item fields")
					}
				}
			case "table":
				keys := map[string]bool{}
				for _, c := range b.Columns {
					if c.Key == "" || keys[c.Key] {
						return errors.New("invalid table column key")
					}
					keys[c.Key] = true
					switch c.Format {
					case "", "text", "number", "bytes", "duration_s", "time", "percent":
					default:
						return errors.New("invalid column format")
					}
					if c.Align != "" && c.Align != "left" && c.Align != "right" {
						return errors.New("invalid column alignment")
					}
				}
				for _, row := range b.Rows {
					for key, value := range row {
						if !keys[key] {
							return errors.New("unknown table column")
						}
						switch value.(type) {
						case nil, string, float64, float32, int, int64, int32, uint, uint64, uint32, json.Number, bool:
						default:
							return errors.New("table cells must be scalar values")
						}
					}
				}
			case "notice":
			case "section":
				if err := walk(b.Blocks, depth+1); err != nil {
					return err
				}
			default:
				return errors.New("unknown view block type")
			}
		}
		return nil
	}
	if err := walk(v.Blocks, 1); err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, errors.New("invalid view JSON")
	}
	if len(data) > MaxViewBytes {
		return nil, errors.New("view exceeds 1 MiB")
	}
	// Inspect decoded strings rather than encoded JSON (encoding/json escapes <).
	var decoded any
	if json.Unmarshal(data, &decoded) != nil || !textOnly(decoded) {
		return nil, errors.New("views contain text only; HTML is not allowed")
	}
	return data, nil
}
func textOnly(v any) bool {
	switch x := v.(type) {
	case string:
		return !strings.ContainsAny(x, "<>")
	case []any:
		for _, v := range x {
			if !textOnly(v) {
				return false
			}
		}
	case map[string]any:
		for k, v := range x {
			if strings.ContainsAny(k, "<>") || !textOnly(v) {
				return false
			}
		}
	}
	return true
}
func validateLink(l *Link, m Manifest) error {
	if l == nil {
		return nil
	}
	if l.Href != "" {
		if l.View != "" || len(l.Params) > 0 || !validURL(l.Href) {
			return errors.New("link must be an http(s) URL or an in-plugin view")
		}
		return nil
	}
	if m.Tab != nil {
		for _, v := range m.Tab.Views {
			if v.Name == l.View {
				return nil
			}
		}
	}
	return errors.New("unknown link view")
}
