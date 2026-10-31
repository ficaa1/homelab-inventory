// Package media provides read-only views of optional media services.
package media

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type service struct{ id, name, base, key string }

var services = []service{{id: "sonarr", name: "Sonarr"}, {id: "radarr", name: "Radarr"}, {id: "tautulli", name: "Tautulli"}, {id: "overseerr", name: "Overseerr"}}

// The host serializes lifecycle, views, images and alert calls on each instance.
type Media struct {
	cacheMu    sync.Mutex
	host       plugin.Host
	services   []service
	days       int
	cache      map[string]cached
	cacheBytes int
	images     map[string]imagePath
	imageRefs  map[imagePath]string
	imageOrder []string
	stuck      map[string]time.Time
}

var _ plugin.AlertSource = (*Media)(nil)
var _ plugin.ImageSource = (*Media)(nil)

func New(h plugin.Host) plugin.Plugin { return &Media{host: h} }
func (*Media) Manifest() plugin.Manifest {
	fields := []plugin.Field{}
	for _, s := range services {
		fields = append(fields, plugin.Field{Key: s.id + "_url", Label: "URL", Type: plugin.URL, Group: s.name}, plugin.Field{Key: s.id + "_key", Label: "API key", Type: plugin.Secret, Group: s.name})
	}
	fields = append(fields, plugin.Field{Key: "upcoming_days", Label: "Upcoming days", Type: plugin.Number, Default: 7, Help: "Days to show, from 1 to 30."})
	return plugin.Manifest{ID: "media", Name: "Media", Description: "Read-only playback, downloads, upcoming releases and library summaries.", Version: "1", Settings: fields, Tab: &plugin.Tab{Title: "Media", Icon: "film", Group: "media", Views: []plugin.ViewRef{{Name: "now", Title: "Now"}, {Name: "library", Title: "Services"}, {Name: "activity", Title: "Import activity"}}}}
}
func (m *Media) Configure(c plugin.Config) error {
	configured := []service{}
	for _, s := range services {
		base, _ := c[s.id+"_url"].(string)
		key, _ := c[s.id+"_key"].(string)
		base = strings.TrimSpace(base)
		if base == "" && key == "" {
			continue
		}
		if base == "" {
			return &plugin.FieldError{Key: s.id + "_url", Message: "URL is required with an API key"}
		}
		if strings.TrimSpace(key) == "" {
			return &plugin.FieldError{Key: s.id + "_key", Message: "API key is required with a URL"}
		}
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || u.Opaque != "" {
			return &plugin.FieldError{Key: s.id + "_url", Message: "Use an http(s) base URL without credentials, query or fragment"}
		}
		if strings.ContainsAny(key, "\r\n") {
			return &plugin.FieldError{Key: s.id + "_key", Message: "API key must not contain line breaks"}
		}
		s.base, s.key = strings.TrimRight(base, "/"), key
		configured = append(configured, s)
	}
	if len(configured) == 0 {
		return &plugin.FieldError{Key: "sonarr_url", Message: "Configure at least one service URL and API key"}
	}
	days := 7
	if v, ok := c["upcoming_days"]; ok && v != nil {
		n, err := strconv.ParseFloat(toNumber(v), 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 1 || n > 30 || n != math.Trunc(n) {
			return &plugin.FieldError{Key: "upcoming_days", Message: "Use a whole number from 1 to 30"}
		}
		days = int(n)
	}
	same := m.days == days && len(m.services) == len(configured)
	if same {
		for i := range configured {
			if m.services[i] != configured[i] {
				same = false
				break
			}
		}
	}
	if !same {
		m.reset()
	}
	if err := m.loadStuck(configured); err != nil {
		return err
	}
	m.services, m.days = configured, days
	return nil
}
func toNumber(v any) string {
	switch x := v.(type) {
	case int:
		return strconv.Itoa(x)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case json.Number:
		return string(x)
	case string:
		return x
	}
	return ""
}
func (m *Media) reset() {
	m.cache = map[string]cached{}
	m.cacheBytes = 0
	m.images = map[string]imagePath{}
	m.imageRefs = map[imagePath]string{}
	m.imageOrder = nil
	m.stuck = map[string]time.Time{}
}
func (m *Media) Close() error { m.reset(); m.services = nil; return nil }
func (m *Media) Test(ctx context.Context) plugin.TestResult {
	result := plugin.TestResult{OK: len(m.services) > 0, Checks: []plugin.Check{}}
	for _, s := range m.services {
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		var status struct {
			Version         string `json:"version"`
			TautulliVersion string `json:"tautulli_version"`
		}
		path := "/api/v3/system/status"
		if s.id == "overseerr" {
			path = "/api/v1/status"
		}
		q := url.Values{}
		if s.id == "tautulli" {
			path = "/api/v2"
			q.Set("cmd", "get_tautulli_info")
		}
		err := m.get(checkCtx, s, path, q, &status, false)
		cancel()
		version := status.Version
		if s.id == "tautulli" {
			version = status.TautulliVersion
		}
		c := plugin.Check{Name: s.name, OK: err == nil && version != "", Message: "Version was not returned"}
		if err != nil {
			c.Message = err.Error()
		} else if version != "" {
			c.Message = "Version " + m.text(version)
		}
		result.OK = result.OK && c.OK
		result.Checks = append(result.Checks, c)
	}
	if result.OK {
		result.Message = "All configured services responded"
	} else {
		result.Message = "One or more services could not be checked"
	}
	return result
}

// Output text is bounded, plain and cannot echo any configured credential.
// Diagnostic bodies, download messages, paths and user/device fields are never displayed.
func (m *Media) text(s string) string {
	for _, svc := range m.services {
		s = strings.ReplaceAll(s, svc.key, "[redacted]")
	}
	s = strings.Map(func(r rune) rune {
		if r == '<' || r == '>' || r < ' ' {
			return ' '
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) > 300 {
		s = string(r[:300])
	}
	return strings.TrimSpace(s)
}
