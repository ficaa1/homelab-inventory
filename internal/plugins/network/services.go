package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// The route provider and qBittorrent reads shared by the declarative views and
// the page API. Every upstream path is fixed here; none comes from a caller.

type route struct {
	Hostname string `json:"hostname"`
	Target   string `json:"target"`
	// SSO stays null: provider middleware alone does not prove a login policy.
	SSO    *bool  `json:"sso"`
	Source string `json:"source"`
}

var hostRule = regexp.MustCompile("^Host\\(`([a-zA-Z0-9.-]+)`\\)$")

func (n *Network) fetchRoutes(ctx context.Context) ([]route, error) {
	base := n.extraText("routes_url")
	if base == "" {
		return nil, errors.New("route provider is not configured")
	}
	var out struct {
		HTTP *struct {
			Routers  map[string]struct{ Rule, Service string }
			Services map[string]struct {
				LoadBalancer *struct{ Servers []struct{ URL string } }
			}
		} `json:"http"`
	}
	if e := n.extraJSON(ctx, n.host.HTTP(), base, "/api/v1/traefik-config", &out); e != nil {
		return nil, e
	}
	if out.HTTP == nil || out.HTTP.Routers == nil || out.HTTP.Services == nil {
		return nil, errors.New("route provider response is incomplete")
	}
	routes := []route{}
	seen := map[string]bool{}
	for _, r := range out.HTTP.Routers {
		m := hostRule.FindStringSubmatch(r.Rule)
		if m == nil {
			continue
		}
		service := out.HTTP.Services[r.Service]
		if service.LoadBalancer == nil {
			continue
		}
		for _, target := range service.LoadBalancer.Servers {
			u, e := url.Parse(target.URL)
			if e != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
				continue
			}
			hostname := strings.ToLower(m[1])
			key := hostname + "\x00" + target.URL
			if seen[key] {
				continue
			}
			seen[key] = true
			routes = append(routes, route{Hostname: n.text(hostname), Target: n.text(target.URL), Source: "http-provider"})
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Hostname == routes[j].Hostname {
			return routes[i].Target < routes[j].Target
		}
		return routes[i].Hostname < routes[j].Hostname
	})
	return routes, nil
}

// qbitLogin opens a session on a fresh cookie jar; the session lives only for
// the reads that follow it.
func (n *Network) qbitLogin(ctx context.Context) (*http.Client, error) {
	base := n.extraText("qbit_url")
	if base == "" {
		return nil, errors.New("qBittorrent is not configured")
	}
	jar, _ := cookiejar.New(nil)
	client := plugin.WithCookieJar(n.host.HTTP(), jar)
	values := url.Values{"username": {n.extraText("qbit_user")}, "password": {n.extraText("qbit_password")}}
	resp, e := n.fixed(ctx, client, base, "/api/v2/auth/login", "POST", strings.NewReader(values.Encode()))
	if e != nil {
		return nil, e
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if e != nil || (resp.StatusCode != 204 && strings.TrimSpace(string(b)) != "Ok.") {
		return nil, errors.New("qBittorrent login refused")
	}
	return client, nil
}

type qbitTransfer struct {
	Download   *int64  `json:"dl_info_speed"`
	Upload     *int64  `json:"up_info_speed"`
	Connection *string `json:"connection_status"`
	DHT        *int64  `json:"dht_nodes"`
}

func (n *Network) qbitTransfer(ctx context.Context, client *http.Client) (qbitTransfer, error) {
	var t qbitTransfer
	if e := n.extraJSON(ctx, client, n.extraText("qbit_url"), "/api/v2/transfer/info", &t); e != nil {
		return t, e
	}
	if t.Download == nil || t.Upload == nil || t.Connection == nil || t.DHT == nil || *t.Download < 0 || *t.Upload < 0 || *t.DHT < 0 {
		return t, errors.New("qBittorrent transfer response is incomplete")
	}
	return t, nil
}

func (n *Network) qbitVersion(ctx context.Context, client *http.Client) (string, error) {
	resp, e := n.fixed(ctx, client, n.extraText("qbit_url"), "/api/v2/app/version", "GET", nil)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if e != nil || len(b) == 0 || len(b) >= 1024 {
		return "", errors.New("qBittorrent version unavailable")
	}
	return n.text(string(b)), nil
}

// torrent is one sharing torrent. Save paths, trackers and magnet links are
// never decoded. Remaining is null when no seeding requirement is configured.
type torrent struct {
	Hash                 string     `json:"hash"`
	Name                 string     `json:"name"`
	SizeBytes            int64      `json:"sizeBytes"`
	SeedingSeconds       int64      `json:"seedingSeconds"`
	RemainingSeconds     *int64     `json:"remainingSeconds"`
	Done                 bool       `json:"done"`
	Ratio                float64    `json:"ratio"`
	UploadBytesPerSecond int64      `json:"uploadBytesPerSecond"`
	State                string     `json:"state"`
	CompletedAt          *time.Time `json:"completedAt"`
	Category             string     `json:"category"`
}

var seedingStates = map[string]bool{"uploading": true, "stalledUP": true, "forcedUP": true, "queuedUP": true}

func (n *Network) seedRequirement() int64 {
	hours, _ := n.extra["seed_hours"].(float64)
	return int64(hours * 3600)
}

// qbitSeeding reads the seeding filter only. A malformed entry fails the whole
// list rather than producing partial totals.
func (n *Network) qbitSeeding(ctx context.Context, client *http.Client) ([]torrent, error) {
	var raw []struct {
		Hash         *string `json:"hash"`
		Name         *string `json:"name"`
		Size         *int64  `json:"size"`
		SeedingTime  *int64  `json:"seeding_time"`
		Ratio        float64 `json:"ratio"`
		UpSpeed      int64   `json:"upspeed"`
		State        *string `json:"state"`
		CompletionOn int64   `json:"completion_on"`
		Category     string  `json:"category"`
	}
	if e := n.extraJSON(ctx, client, n.extraText("qbit_url"), "/api/v2/torrents/info?filter=seeding", &raw); e != nil {
		return nil, e
	}
	if len(raw) > 5000 {
		return nil, errors.New("qBittorrent returned too many torrents")
	}
	required := n.seedRequirement()
	out := []torrent{}
	for i, t := range raw {
		if t.Name == nil || t.Size == nil || t.SeedingTime == nil || t.State == nil || *t.Size < 0 || *t.SeedingTime < 0 {
			return nil, fmt.Errorf("qBittorrent entry %d is incomplete", i)
		}
		if !seedingStates[*t.State] {
			continue
		}
		item := torrent{Name: n.text(*t.Name), SizeBytes: *t.Size, SeedingSeconds: *t.SeedingTime, Ratio: t.Ratio, UploadBytesPerSecond: t.UpSpeed, State: *t.State, Category: n.text(t.Category)}
		if t.Hash != nil {
			item.Hash = n.text(*t.Hash)
		}
		if required > 0 {
			remaining := max(0, required-*t.SeedingTime)
			item.RemainingSeconds = &remaining
			item.Done = remaining == 0
		}
		if t.CompletionOn > 0 {
			at := time.Unix(t.CompletionOn, 0).UTC()
			item.CompletedAt = &at
		}
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Done != b.Done {
			return !a.Done
		}
		if a.RemainingSeconds != nil && b.RemainingSeconds != nil && *a.RemainingSeconds != *b.RemainingSeconds {
			return *a.RemainingSeconds < *b.RemainingSeconds
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Hash < b.Hash
	})
	return out, nil
}
