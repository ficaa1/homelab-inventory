// Package archive connects to an existing media-policy service. Workers and
// library files remain owned by that service; the dashboard never runs them.
package archive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type Integration struct {
	host        plugin.Host
	base, token string
}

func New(h plugin.Host) plugin.Plugin { return &Integration{host: h} }
func text(k, label string) plugin.Field {
	return plugin.Field{Key: k, Label: label, Type: plugin.Text, Required: true}
}
func action(id, label string, fields ...plugin.Field) plugin.ActionSpec {
	return plugin.ActionSpec{ID: id, Label: label, Fields: fields, Operator: true}
}
func actions() []plugin.ActionSpec {
	return append(extraActions(), []plugin.ActionSpec{
		action("run", "Run policy now"),
		action("pause", "Pause or resume", plugin.Field{Key: "paused", Label: "Paused", Type: plugin.Bool, Required: true}),
		action("job-approve", "Approve waiting job", text("id", "Job ID")), action("job-drop", "Drop waiting job", text("id", "Job ID")),
		action("search-reorder", "Reorder search", text("key", "Target key"), plugin.Field{Key: "position", Label: "Position (zero based)", Type: plugin.Number, Required: true}),
		action("approvals-reorder", "Reorder approvals", text("key", "Target key"), plugin.Field{Key: "position", Label: "Position (zero based)", Type: plugin.Number, Required: true}),
		action("seed-preview", "Preview seeding move", text("hash", "Torrent hash")),
		action("seed-move", "Apply reviewed seeding move", text("hash", "Torrent hash"), text("plan_hash", "Reviewed plan hash")),
		action("seed-stop", "Stop seeding job", text("hash", "Torrent hash")),
		action("seed-reorder", "Reorder seeding queue", text("hash", "Torrent hash"), plugin.Field{Key: "position", Label: "Position (zero based)", Type: plugin.Number, Required: true}),
		action("restore", "Restore archived original", text("id", "Job ID"), text("hash", "Reviewed plan hash"), text("confirm", "Service confirmation phrase")),
		action("purge", "Purge reviewed original", text("id", "Job ID"), text("hash", "Reviewed plan hash"), text("confirm", "Service confirmation phrase")),
	}...)
}
func (*Integration) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "archive", Name: "Archive", Description: "Control an existing media-policy service with explicit operator confirmation.", Version: "1", Actions: actions(), Settings: []plugin.Field{{Key: "policy_url", Label: "Media-policy URL", Type: plugin.URL, Required: true}, {Key: "policy_token", Label: "Bearer token", Type: plugin.Secret}}, Tab: &plugin.Tab{Title: "Archive", Icon: "archive", Views: []plugin.ViewRef{{Name: "status", Title: "Status"}, {Name: "library", Title: "Library"}, {Name: "seeding", Title: "Seeding"}, {Name: "selection", Title: "Scoring"}}}}
}
func (p *Integration) Configure(c plugin.Config) error {
	base, _ := c["policy_url"].(string)
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return &plugin.FieldError{Key: "policy_url", Message: "Use a base URL without credentials, query or fragment"}
	}
	token, _ := c["policy_token"].(string)
	if strings.ContainsAny(token, "\r\n") {
		return &plugin.FieldError{Key: "policy_token", Message: "Token contains line breaks"}
	}
	p.base = strings.TrimRight(base, "/")
	p.token = token
	return nil
}
func (p *Integration) Close() error { p.base = ""; p.token = ""; return nil }
func (p *Integration) request(ctx context.Context, method, path string, body any) (any, error) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r, e := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(b))
	if e != nil {
		return nil, errors.New("policy request failed")
	}
	r.Header.Set("Authorization", "Bearer "+p.token)
	r.Header.Set("Content-Type", "application/json")
	resp, e := plugin.WithoutRedirects(p.host.HTTP()).Do(r)
	if e != nil {
		return nil, errors.New("policy unavailable; outcome unknown for writes; inspect the service before retrying")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("policy refused request (HTTP %d); inspect the service", resp.StatusCode)
	}
	var out any
	d := json.NewDecoder(resp.Body)
	if d.Decode(&out) != nil || d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid policy response")
	}
	return out, nil
}
func (p *Integration) Test(ctx context.Context) plugin.TestResult {
	_, e := p.request(ctx, "GET", "/healthz", nil)
	return plugin.TestResult{OK: e == nil, Message: map[bool]string{true: "Media-policy responded", false: "Media-policy unavailable"}[e == nil]}
}
func (p *Integration) render(title string, data any) plugin.View {
	rows := []map[string]any{}
	var walk func(string, any)
	walk = func(key string, v any) {
		if len(rows) >= 500 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				lower := strings.ToLower(k)
				if strings.Contains(lower, "token") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "apikey") {
					continue
				}
				walk(strings.TrimPrefix(key+"."+k, "."), x[k])
			}
		case []any:
			for i, v := range x {
				walk(fmt.Sprintf("%s[%d]", key, i), v)
			}
		default:
			s := fmt.Sprint(v)
			if p.token != "" {
				s = strings.ReplaceAll(s, p.token, "[redacted]")
			}
			s = strings.NewReplacer("<", " ", ">", " ").Replace(s)
			if len(s) > 2000 {
				s = s[:2000]
			}
			rows = append(rows, map[string]any{"field": key, "value": s})
		}
	}
	walk("", data)
	return plugin.View{Title: title, UpdatedAt: p.host.Now(), Actions: actions(), Blocks: []plugin.Block{{Type: "notice", Tone: "info", Text: "External media-policy service owns policy execution and durable state. Writes are never automatically retried."}, {Type: "table", Title: title, Columns: []plugin.Column{{Key: "field", Label: "Field"}, {Key: "value", Label: "Value"}}, Rows: rows}}}
}
func (p *Integration) View(ctx context.Context, name string, q url.Values) (plugin.View, error) {
	paths := map[string]string{"status": "/status", "library": "/library", "seeding": "/seeding", "selection": "/archive/selection"}
	path, ok := paths[name]
	if !ok {
		return plugin.View{}, plugin.ErrNotFound
	}
	out, e := p.request(ctx, "GET", path, nil)
	if e != nil {
		return plugin.View{}, e
	}
	if name == "library" || name == "seeding" {
		return p.collection(name, out, q), nil
	}
	return p.render(name, out), nil
}
func (p *Integration) Action(ctx context.Context, id string, c plugin.Config) (plugin.View, error) {
	paths := map[string]string{"run": "/archive/run", "pause": "/archive/pause", "job-approve": "/archive/job/approve", "job-drop": "/archive/job/drop", "search-reorder": "/archive/search/reorder", "approvals-reorder": "/archive/approvals/reorder", "seed-preview": "/seeding/preview", "seed-move": "/seeding/move", "seed-stop": "/seeding/stop", "seed-reorder": "/seeding/queue/reorder", "restore": "/archive/restore", "purge": "/archive/purge"}
	path, ok := paths[id]
	if !ok {
		if path, method, body, e := extraPayload(id, c); e == nil {
			out, e := p.request(ctx, method, path, body)
			if e != nil {
				return plugin.View{}, e
			}
			return p.render("Action result", out), nil
		} else if e != plugin.ErrNotFound {
			return plugin.View{}, e
		}
		return plugin.View{}, plugin.ErrNotFound
	}
	body := plugin.Config{"apply": id != "seed-preview"}
	for k, v := range c {
		if s, ok := v.(string); ok && len(s) > 256 {
			return plugin.View{}, errors.New("action field too long")
		}
		if k == "position" {
			n, ok := v.(float64)
			if !ok || n < 0 || n > 10000 || n != float64(int(n)) {
				return plugin.View{}, errors.New("position must be an integer from 0 to 10000")
			}
		}
		if k == "plan_hash" {
			k = "planHash"
		}
		body[k] = v
	}
	out, e := p.request(ctx, "POST", path, body)
	if e != nil {
		return plugin.View{}, e
	}
	return p.render("Action result", out), nil
}
