// Package security records optional bearer and path canaries without recording
// presented credentials. It never permits a request or sends notifications.
package security

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Trip struct {
	Name   string    `json:"name"`
	Kind   string    `json:"kind"`
	Time   time.Time `json:"time"`
	Remote string    `json:"remote"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
}
type Integration struct {
	excluded      []string
	host          plugin.Host
	tokens, paths map[string]string
	last          map[string]time.Time
	window        time.Duration
}

func New(h plugin.Host) plugin.Plugin { return &Integration{host: h} }
func (*Integration) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "security", Name: "Security canaries", Version: "1", Description: "Record fake credential and bait-path use as durable security alerts.", Settings: []plugin.Field{{Key: "tokens", Label: "Fake bearer tokens", Type: plugin.Secret, Help: "Comma-separated name=value. Use unique fake tokens, never real credentials."}, {Key: "paths", Label: "Bait paths", Type: plugin.Text, Help: "Comma-separated name=/path. Never select a legitimate route."}, {Key: "window_hours", Label: "Alert window (hours)", Type: plugin.Number, Default: 24}}, Tab: &plugin.Tab{Title: "Security", Icon: "shield", Views: []plugin.ViewRef{{Name: "trips", Title: "Canary trips"}}}}
}
func parse(raw string, token bool) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,40}$`).MatchString(name) {
			return nil, errors.New("use named entries separated by commas")
		}
		if token {
			if len(value) < 32 || len(value) > 256 || strings.ContainsAny(value, " \r\n\t") {
				return nil, errors.New("fake tokens require 32 to 256 characters")
			}
		} else {
			u, e := url.Parse(value)
			if e != nil || !strings.HasPrefix(value, "/") || len(value) < 2 || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(value, "{} \r\n\t") || len(value) > 200 {
				return nil, errors.New("use exact absolute bait paths without query")
			}
		}
		out[name] = value
	}
	if len(out) > 100 {
		return nil, errors.New("at most 100 canaries")
	}
	return out, nil
}
func (p *Integration) Configure(c plugin.Config) error {
	tokens, _ := c["tokens"].(string)
	paths, _ := c["paths"].(string)
	t, e := parse(tokens, true)
	if e != nil {
		return &plugin.FieldError{Key: "tokens", Message: e.Error()}
	}
	b, e := parse(paths, false)
	if e != nil {
		return &plugin.FieldError{Key: "paths", Message: e.Error()}
	}
	hours, ok := c["window_hours"].(float64)
	if !ok {
		hours = 24
	}
	if hours < 1 || hours > 8760 {
		return &plugin.FieldError{Key: "window_hours", Message: "Choose 1 to 8760 hours"}
	}
	p.tokens = t
	p.paths = b
	p.window = time.Duration(hours * float64(time.Hour))
	p.last = map[string]time.Time{}
	return nil
}
func (p *Integration) Close() error { p.tokens = nil; p.paths = nil; return nil }
func (p *Integration) Test(context.Context) plugin.TestResult {
	return plugin.TestResult{OK: len(p.tokens)+len(p.paths) > 0, Message: "Local canary configuration check; no bait requests are sent"}
}
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == '<' || r == '>' {
			return ' '
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
func (p *Integration) Observe(r *http.Request) bool {
	for _, real := range p.excluded {
		if real != "" && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")), []byte(real)) == 1 {
			return false
		}
	}
	hit := false
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	for name, token := range p.tokens {
		if subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) == 1 {
			p.record(r, name, "token")
			hit = true
		}
	}
	for name, path := range p.paths {
		if r.URL.Path == path {
			p.record(r, name, "path")
			hit = true
		}
	}
	return hit
}
func (p *Integration) record(r *http.Request, name, kind string) {
	now := p.host.Now().UTC()
	remote, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		remote = r.RemoteAddr
	}
	key := name + ":" + remote
	if now.Sub(p.last[key]) < 5*time.Minute {
		return
	}
	path := filepath.Join(p.host.CacheDir(), "canary-trips.jsonl")
	if st, e := os.Stat(path); e == nil && st.Size() > 1<<20 {
		if e = os.Rename(path, path+".1"); e != nil {
			p.host.Logf("canary history rotation failed")
			return
		}
	}
	f, e := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		p.host.Logf("canary history write failed")
		return
	}
	trip := Trip{Name: name, Kind: kind, Time: now, Remote: clean(remote), Method: clean(r.Method), Path: "[request path withheld]"}
	e = json.NewEncoder(f).Encode(trip)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e == nil {
		if len(p.last) >= 4096 {
			oldestKey := ""
			oldest := now
			for candidate, at := range p.last {
				if now.Sub(at) >= 5*time.Minute {
					delete(p.last, candidate)
				} else if oldestKey == "" || at.Before(oldest) {
					oldestKey = candidate
					oldest = at
				}
			}
			if len(p.last) >= 4096 {
				delete(p.last, oldestKey)
			}
		}
		p.last[key] = now
	}
}
func (p *Integration) history() ([]Trip, error) {
	out := []Trip{}
	for _, suffix := range []string{".1", ""} {
		f, e := os.Open(filepath.Join(p.host.CacheDir(), "canary-trips.jsonl"+suffix))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, errors.New("canary history unavailable")
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 16384)
		for scan.Scan() {
			var t Trip
			if json.Unmarshal(scan.Bytes(), &t) != nil {
				f.Close()
				return nil, errors.New("invalid canary history")
			}
			out = append(out, t)
		}
		e = scan.Err()
		f.Close()
		if e != nil {
			return nil, errors.New("invalid canary history")
		}
	}
	return out, nil
}
func (p *Integration) View(_ context.Context, name string, _ url.Values) (plugin.View, error) {
	if name != "trips" {
		return plugin.View{}, plugin.ErrNotFound
	}
	trips, e := p.history()
	if e != nil {
		return plugin.View{}, e
	}
	rows := []map[string]any{}
	for i := len(trips) - 1; i >= 0 && len(rows) < 500; i-- {
		t := trips[i]
		rows = append(rows, map[string]any{"name": clean(t.Name), "kind": clean(t.Kind), "time": t.Time.Format(time.RFC3339), "remote": clean(t.Remote), "path": clean(t.Path)})
	}
	return plugin.View{Title: "Canary trips", UpdatedAt: p.host.Now(), RefreshSeconds: 60, Blocks: []plugin.Block{{Type: "table", Empty: "No canaries tripped", Columns: []plugin.Column{{Key: "time", Label: "Time", Format: "time"}, {Key: "name", Label: "Canary"}, {Key: "kind", Label: "Kind"}, {Key: "remote", Label: "Source"}, {Key: "path", Label: "Path"}}, Rows: rows}}}, nil
}
func (p *Integration) Alerts(context.Context) ([]plugin.Alert, error) {
	trips, e := p.history()
	if e != nil {
		return nil, e
	}
	out := []plugin.Alert{}
	seen := map[string]bool{}
	for i := len(trips) - 1; i >= 0; i-- {
		t := trips[i]
		if p.host.Now().Sub(t.Time) > p.window || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		out = append(out, plugin.Alert{Rule: "canary.tripped", Subject: clean(t.Name), Severity: "critical", Summary: "Security canary used", Detail: "A configured fake credential or bait path was accessed."})
	}
	return out, nil
}

// ExcludeCredentials is supplied by core, never by browser action parameters.
func (p *Integration) ExcludeCredentials(tokens ...string) {
	p.excluded = append([]string(nil), tokens...)
}
