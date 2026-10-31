// Package cloudflare reads zone analytics and DNS using the guarded plugin client.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Integration struct {
	host              plugin.Host
	base, token, zone string
}

func New(h plugin.Host) plugin.Plugin { return &Integration{host: h} }
func (*Integration) Manifest() plugin.Manifest {
	return plugin.Manifest{ID: "cloudflare", Name: "Cloudflare", Description: "Read one configured zone's DNS and hourly traffic analytics.", Version: "1", Settings: []plugin.Field{{Key: "api_url", Label: "API base URL", Type: plugin.URL, Required: true, Default: "https://api.cloudflare.com/client/v4"}, {Key: "api_token", Label: "Zone read and analytics token", Type: plugin.Secret, Required: true}, {Key: "zone_id", Label: "Zone ID or name", Type: plugin.Text, Required: true}}, Tab: &plugin.Tab{Title: "Cloudflare", Icon: "cloud", Views: []plugin.ViewRef{{Name: "analytics", Title: "Day"}, {Name: "week", Title: "Week"}, {Name: "dns", Title: "DNS"}, {Name: "zone", Title: "Zone"}}}}
}
func (p *Integration) Configure(c plugin.Config) error {
	base, _ := c["api_url"].(string)
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return &plugin.FieldError{Key: "api_url", Message: "Use a base URL without credentials or query"}
	}
	zone, _ := c["zone_id"].(string)
	if !regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(zone) && !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]{0,251}\.[a-zA-Z]{2,63}$`).MatchString(zone) {
		return &plugin.FieldError{Key: "zone_id", Message: "Use a zone ID or DNS zone name"}
	}
	token, _ := c["api_token"].(string)
	if strings.ContainsAny(token, "\r\n") {
		return &plugin.FieldError{Key: "api_token", Message: "Token contains line breaks"}
	}
	p.base = strings.TrimRight(base, "/")
	p.token = token
	p.zone = zone
	return nil
}
func (p *Integration) Close() error { p.token = ""; p.base = ""; return nil }

func (p *Integration) DNSRecords(ctx context.Context) ([]plugin.DNSRecord, error) {
	if err := p.resolveZone(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Success bool `json:"success"`
		Result  []struct {
			Name, Type string
			Proxied    bool
		} `json:"result"`
	}
	if err := p.read(ctx, "/zones/"+p.zone+"/dns_records?per_page=100", nil, &out); err != nil || !out.Success {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("DNS read refused")
	}
	result := make([]plugin.DNSRecord, 0, len(out.Result))
	for _, r := range out.Result {
		result = append(result, plugin.DNSRecord{Name: safe(r.Name, p.token), Type: safe(r.Type, p.token), Proxied: r.Proxied})
	}
	return result, nil
}
func (p *Integration) read(ctx context.Context, path string, body any, out any) error {
	method := "GET"
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
		method = "POST"
	}
	r, e := http.NewRequestWithContext(ctx, method, p.base+path, bytes.NewReader(b))
	if e != nil {
		return errors.New("invalid Cloudflare request")
	}
	r.Header.Set("Authorization", "Bearer "+p.token)
	r.Header.Set("Content-Type", "application/json")
	resp, e := plugin.WithoutRedirects(p.host.HTTP()).Do(r)
	if e != nil {
		return errors.New("Cloudflare unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("Cloudflare refused read (HTTP %d)", resp.StatusCode)
	}
	if json.NewDecoder(resp.Body).Decode(out) != nil {
		return errors.New("invalid Cloudflare response")
	}
	return nil
}
func (p *Integration) Test(ctx context.Context) plugin.TestResult {
	if err := p.resolveZone(ctx); err != nil {
		return plugin.TestResult{OK: false, Message: "Zone identity lookup failed"}
	}
	var out struct {
		Success bool `json:"success"`
	}
	e := p.read(ctx, "/zones/"+p.zone, nil, &out)
	return plugin.TestResult{OK: e == nil && out.Success, Message: "Zone read permission check"}
}
func (p *Integration) View(ctx context.Context, name string, params url.Values) (plugin.View, error) {
	if err := p.resolveZone(ctx); err != nil {
		return plugin.View{}, err
	}
	v := plugin.View{Title: "Cloudflare", UpdatedAt: p.host.Now(), RefreshSeconds: 300, Blocks: []plugin.Block{}}
	switch name {
	case "zone":
		var out struct {
			Success bool `json:"success"`
			Result  struct {
				Name, Status string
				Plan         struct{ Name string }
			} `json:"result"`
		}
		if e := p.read(ctx, "/zones/"+p.zone, nil, &out); e != nil {
			return v, e
		}
		if !out.Success {
			return v, errors.New("zone read refused")
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "stats", Items: []plugin.Item{{Label: "Zone", Value: safe(out.Result.Name, p.token)}, {Label: "Status", Value: safe(out.Result.Status, p.token)}, {Label: "Plan", Value: safe(out.Result.Plan.Name, p.token)}}})
	case "dns":
		page, _ := strconv.Atoi(params.Get("page"))
		if page < 1 || page > 10000 {
			page = 1
		}
		var out struct {
			Success bool `json:"success"`
			Result  []struct {
				Name, Type, Content string
				Proxied             bool
			} `json:"result"`
			Info struct {
				Pages int `json:"total_pages"`
			} `json:"result_info"`
		}
		e := p.read(ctx, "/zones/"+p.zone+"/dns_records?per_page=100&page="+strconv.Itoa(page), nil, &out)
		if e != nil {
			return v, e
		}
		if !out.Success {
			return v, errors.New("DNS read refused")
		}
		rows := []map[string]any{}
		for _, r := range out.Result {
			rows = append(rows, map[string]any{"name": safe(r.Name, p.token), "type": safe(r.Type, p.token), "content": safe(r.Content, p.token), "proxied": r.Proxied})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "DNS", Columns: []plugin.Column{{Key: "name", Label: "Name"}, {Key: "type", Label: "Type"}, {Key: "content", Label: "Content"}, {Key: "proxied", Label: "Proxied"}}, Rows: rows})
		links := []plugin.Item{}
		if page > 1 {
			links = append(links, plugin.Item{Title: "Previous page", Link: &plugin.Link{View: "dns", Params: map[string]string{"page": strconv.Itoa(page - 1)}}})
		}
		if page < out.Info.Pages {
			links = append(links, plugin.Item{Title: "Next page", Link: &plugin.Link{View: "dns", Params: map[string]string{"page": strconv.Itoa(page + 1)}}})
		}
		if len(links) > 0 {
			v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Title: fmt.Sprintf("Page %d of %d", page, out.Info.Pages), Items: links})
		}

	case "analytics", "week":
		until := p.host.Now().UTC().Truncate(24 * time.Hour)
		since := until.Add(-24 * time.Hour)
		if name == "week" {
			since = until.Add(-7 * 24 * time.Hour)
		}
		query := `query($zone: String!, $since: String!, $until: String!) { viewer { zones(filter:{zoneTag:$zone}) { httpRequests1hGroups(limit:2000,filter:{date_geq:$since,date_lt:$until}) { dimensions { datetime } sum { requests bytes threats cachedRequests } uniq { uniques } } } } }`
		var out struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Viewer struct {
					Zones []struct {
						Groups []struct {
							Dimensions struct {
								Datetime string `json:"datetime"`
							} `json:"dimensions"`
							Sum  struct{ Requests, Bytes, Threats, CachedRequests int64 } `json:"sum"`
							Uniq struct{ Uniques int64 }                                  `json:"uniq"`
						} `json:"httpRequests1hGroups"`
					} `json:"zones"`
				} `json:"viewer"`
			} `json:"data"`
		}
		e := p.read(ctx, "/graphql", map[string]any{"query": query, "variables": map[string]string{"zone": p.zone, "since": since.Format("2006-01-02"), "until": until.Format("2006-01-02")}}, &out)
		if e != nil {
			return v, e
		}
		if len(out.Errors) > 0 || len(out.Data.Viewer.Zones) != 1 {
			return v, errors.New("analytics unavailable; check zone analytics permission")
		}
		var requests, bandwidth, threats, cached int64
		rows := []map[string]any{}
		for _, g := range out.Data.Viewer.Zones[0].Groups {
			if _, e := time.Parse(time.RFC3339, g.Dimensions.Datetime); e != nil {
				return v, errors.New("invalid analytics hour")
			}
			requests += g.Sum.Requests
			bandwidth += g.Sum.Bytes
			threats += g.Sum.Threats
			cached += g.Sum.CachedRequests
			rows = append(rows, map[string]any{"time": g.Dimensions.Datetime, "requests": g.Sum.Requests, "bytes": g.Sum.Bytes, "threats": g.Sum.Threats, "cached": g.Sum.CachedRequests})
		}
		v.Blocks = []plugin.Block{{Type: "notice", Tone: "info", Text: "Previous complete UTC day: " + since.Format("2006-01-02")}, {Type: "stats", Items: []plugin.Item{{Label: "Requests", Value: fmt.Sprint(requests)}, {Label: "Bytes", Value: fmt.Sprint(bandwidth)}, {Label: "Threats", Value: fmt.Sprint(threats)}, {Label: "Cached", Value: fmt.Sprint(cached)}}}, {Type: "table", Title: "Hourly traffic", Columns: []plugin.Column{{Key: "time", Label: "Hour", Format: "time"}, {Key: "requests", Label: "Requests", Format: "number"}, {Key: "bytes", Label: "Bandwidth", Format: "bytes"}, {Key: "threats", Label: "Threats", Format: "number"}, {Key: "cached", Label: "Cached", Format: "number"}}, Rows: rows}}
	default:
		return v, plugin.ErrNotFound
	}
	return v, nil
}
func safe(s, token string) string {
	if token != "" {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return strings.NewReplacer("<", " ", ">", " ").Replace(s)
}

func (p *Integration) resolveZone(ctx context.Context) error {
	if regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(p.zone) {
		return nil
	}
	var out struct {
		Success bool                        `json:"success"`
		Result  []struct{ ID, Name string } `json:"result"`
	}
	if e := p.read(ctx, "/zones?name="+url.QueryEscape(p.zone), nil, &out); e != nil {
		return e
	}
	if !out.Success {
		return errors.New("zone identity lookup refused")
	}
	for _, z := range out.Result {
		if strings.EqualFold(z.Name, p.zone) && regexp.MustCompile(`^[a-fA-F0-9]{32}$`).MatchString(z.ID) {
			p.zone = z.ID
			return nil
		}
	}
	return errors.New("configured zone not found")
}
