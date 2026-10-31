package taste

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

// The Taste page's own API. Every household route names a configured profile;
// verdicts land in that profile's owner-bound feedback file and nothing here
// writes upstream. Posters come from the configured image endpoint by file
// name only, so a caller never chooses a URL.

// posterFile is TMDB's poster naming; nothing else is ever fetched.
var posterFile = regexp.MustCompile(`^[A-Za-z0-9_-]{6,64}\.(jpg|png|webp)$`)

const posterCacheSize = 400

type posterCache struct {
	mu    sync.Mutex
	order []string
	items map[string]poster
}
type poster struct {
	ct   string
	body []byte
}

func (c *posterCache) get(f string) (poster, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.items[f]
	return p, ok
}
func (c *posterCache) put(f string, p poster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]poster{}
	}
	if _, ok := c.items[f]; ok {
		return
	}
	if len(c.order) >= posterCacheSize {
		delete(c.items, c.order[0])
		c.order = c.order[1:]
	}
	c.items[f] = p
	c.order = append(c.order, f)
}

// APITimeout gives the routes that may wait on a cold recommendation feed or a
// Letterboxd poll the private dashboard's budget.
func (t *Integration) APITimeout(method, path string) time.Duration {
	switch path {
	case "picks", "familiar", "watchlist":
		return 20 * time.Second
	case "sync":
		return 2 * time.Minute
	}
	return 0
}

type knownRow struct {
	engine.KnownTitle
	Verdict string `json:"verdict,omitempty"`
}

func (t *Integration) API(ctx context.Context, req plugin.APIRequest) (plugin.APIResponse, error) {
	route := func(method, path string) bool { return req.Method == method && req.Path == path }
	switch {
	case route("GET", "profiles"):
		ids := t.profileIDs()
		out := []map[string]any{}
		for _, id := range ids {
			out = append(out, map[string]any{"id": id, "sync": t.bindings[id].Username != ""})
		}
		return jsonOK(map[string]any{"profiles": out})
	case req.Method == "GET" && strings.HasPrefix(req.Path, "poster/"):
		return t.poster(ctx, strings.TrimPrefix(req.Path, "poster/"))
	case route("GET", "search"):
		if _, err := t.pageHousehold(req.Query.Get("profile")); err != nil {
			return plugin.APIResponse{}, err
		}
		q := strings.TrimSpace(req.Query.Get("q"))
		if q == "" || len(q) > 100 {
			return plugin.APIResponse{}, invalid("q", "query required")
		}
		rows, err := t.source.Search(ctx, q)
		if err != nil {
			return plugin.APIResponse{}, errors.New("search unavailable")
		}
		if rows == nil {
			rows = []tastedata.BrowseTitle{}
		}
		return jsonOK(map[string]any{"titles": rows})
	}
	var body struct {
		Profile string `json:"profile"`
		Kind    string `json:"kind"`
		ID      int    `json:"id"`
		Verdict string `json:"verdict"`
		Title   string `json:"title"`
		Year    string `json:"year"`
	}
	profile := req.Query.Get("profile")
	if req.Method != "GET" {
		d := json.NewDecoder(bytes.NewReader(req.Body))
		d.DisallowUnknownFields()
		if d.Decode(&body) != nil || d.Decode(new(any)) != io.EOF {
			return plugin.APIResponse{}, invalid("", "expected one JSON object")
		}
		profile = body.Profile
	}
	h, err := t.pageHousehold(profile)
	if err != nil {
		return plugin.APIResponse{}, err
	}
	kind := req.Query.Get("kind")
	if kind != "" && kind != "movie" && kind != "tv" {
		return plugin.APIResponse{}, invalid("kind", "kind must be movie or tv")
	}
	switch {
	case route("GET", "picks"), route("GET", "familiar"):
		out, err := t.Picks(ctx, profile, kind, req.Path == "familiar")
		if err != nil {
			return plugin.APIResponse{}, err
		}
		return jsonOK(out)
	case route("GET", "known"):
		verdicts, err := h.feedback.Load()
		if err != nil {
			return plugin.APIResponse{}, errors.New("taste feedback unavailable")
		}
		rows := []knownRow{}
		listed := map[string]bool{}
		m := h.model.Model()
		if m != nil {
			for _, k := range m.Known {
				id := fmt.Sprint(k.Kind, ":", k.ID)
				listed[id] = true
				rows = append(rows, knownRow{KnownTitle: k, Verdict: verdicts[id].Verdict})
			}
		}
		extra := []string{}
		for id := range verdicts {
			if !listed[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		for _, id := range extra {
			v := verdicts[id]
			k, num, _ := strings.Cut(id, ":")
			n, _ := strconv.Atoi(num)
			rows = append(rows, knownRow{KnownTitle: engine.KnownTitle{BrowseTitle: tastedata.BrowseTitle{ID: n, Kind: k, Title: v.Title, Year: v.Year}, Source: "you"}, Verdict: v.Verdict})
		}
		out := map[string]any{"titles": rows, "updating": h.model.Stale()}
		if m != nil {
			out["built_at"] = m.BuiltAt
		}
		return jsonOK(out)
	case route("GET", "watchlist"):
		r, err := t.Discovery(ctx, profile, false)
		if err != nil {
			return plugin.APIResponse{}, err
		}
		verdicts, err := h.feedback.Load()
		if err != nil {
			return plugin.APIResponse{}, errors.New("taste feedback unavailable")
		}
		titles := []map[string]any{}
		for _, p := range r.Watchlist {
			k := fmt.Sprint(p.Kind, ":", p.ID)
			titles = append(titles, map[string]any{"id": p.ID, "kind": p.Kind, "title": p.Title, "year": p.Year, "poster": p.Poster, "overview": p.Overview, "reason": p.Reason, "rating": p.Rating, "availability": p.Availability, "verdict": verdicts[k].Verdict})
		}
		return jsonOK(map[string]any{"titles": titles, "status": r.Status, "checked_at": r.CheckedAt})
	case route("GET", "import"):
		p, err := letterboxd.Load(h.model.ProfilePath)
		if err != nil || p != nil && p.Owner != h.binding.Owner {
			return plugin.APIResponse{}, errors.New("profile state unavailable")
		}
		out := map[string]any{"sync_configured": h.syncer != nil, "upload_target": "letterboxd-" + profile, "updating": h.model.Stale()}
		if p != nil {
			// Account names, film rows and RSS fingerprints stay private.
			out["letterboxd"] = map[string]any{"imported_at": p.ImportedAt, "ratings": p.Ratings, "watched": p.Watched, "watchlist": p.Watchlist, "likes": p.Likes, "unmatched": p.Unmatched, "films": len(p.Films), "sync": p.Sync}
		}
		if m := h.model.Model(); m != nil {
			out["model"] = map[string]any{"built_at": m.BuiltAt, "picks": len(m.Picks), "known": len(m.Known), "seeds": len(m.Seeds), "familiar": len(m.Familiar)}
		}
		if v, err := h.feedback.Load(); err == nil {
			out["verdicts"] = len(v)
		}
		return jsonOK(out)
	case route("POST", "verdict"):
		if err := t.Feedback(ctx, profile, body.Kind, body.ID, body.Verdict, body.Title, body.Year); err != nil {
			return plugin.APIResponse{}, invalid("verdict", "invalid verdict")
		}
		return jsonOK(map[string]any{"state": "ok"})
	case route("POST", "refresh"):
		h.model.Refresh()
		return jsonOK(map[string]any{"state": "ok"})
	case route("POST", "sync"):
		if h.syncer == nil {
			return plugin.APIResponse{}, invalid("", "Letterboxd sync is not configured for this profile")
		}
		if err := h.syncer.Step(ctx, t.host.Now()); err != nil {
			return plugin.APIResponse{}, err
		}
		h.model.Refresh()
		return jsonOK(map[string]any{"state": "ok"})
	}
	return plugin.APIResponse{}, plugin.ErrNotFound
}

func (t *Integration) profileIDs() []string {
	ids := []string{}
	for id := range t.bindings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// pageHousehold accepts only an administrator-configured profile name.
func (t *Integration) pageHousehold(id string) (*household, error) {
	if !profileName.MatchString(id) {
		return nil, invalid("profile", "choose a household profile")
	}
	if _, ok := t.bindings[id]; !ok {
		return nil, plugin.ErrNotFound
	}
	return t.household(id)
}

func (t *Integration) poster(ctx context.Context, file string) (plugin.APIResponse, error) {
	if !posterFile.MatchString(file) || t.posters == "" {
		return plugin.APIResponse{}, plugin.ErrNotFound
	}
	if p, ok := t.posterCache.get(file); ok {
		return plugin.APIResponse{ContentType: p.ct, Body: p.body}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.posters+"/t/p/w185/"+file, nil)
	if err != nil {
		return plugin.APIResponse{}, plugin.ErrNotFound
	}
	res, err := t.host.HTTP().Do(req)
	if err != nil {
		return plugin.APIResponse{}, errors.New("poster unavailable")
	}
	defer res.Body.Close()
	ct, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if res.StatusCode != 200 || (ct != "image/jpeg" && ct != "image/png" && ct != "image/webp") {
		return plugin.APIResponse{}, errors.New("poster unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil || len(b) > 2<<20 {
		return plugin.APIResponse{}, errors.New("poster unavailable")
	}
	t.posterCache.put(file, poster{ct: ct, body: b})
	return plugin.APIResponse{ContentType: ct, Body: b}, nil
}

func invalid(field, msg string) error {
	e := &plugin.ValidationError{Message: msg}
	if field != "" {
		e.Fields = map[string]string{field: msg}
	}
	return e
}
func jsonOK(v any) (plugin.APIResponse, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return plugin.APIResponse{}, err
	}
	return plugin.APIResponse{Body: b}, nil
}
