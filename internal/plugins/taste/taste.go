// Package taste provides optional household-bound, read-only recommendations.
package taste

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/engine"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/letterboxd"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste/twins"
	"github.com/ficaa1/homelab-inventory/internal/tastedata"
)

var profileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type Binding struct {
	Owner    int    `json:"owner"`
	Username string `json:"letterboxd_username,omitempty"`
}
type household struct {
	binding     Binding
	model       *engine.Taste
	feed        *engine.Cache
	feedback    *engine.Feedback
	preferences *engine.Preferences
	syncer      *letterboxd.Syncer
}
type Integration struct {
	host     plugin.Host
	source   tastedata.Source
	bindings map[string]Binding
	profiles map[string]*household
	endpoint string
	artifact string
	cancel   context.CancelFunc
	workers  sync.WaitGroup
}

func New(h plugin.Host) plugin.Plugin                             { return &Integration{host: h} }
func (t *Integration) SetRecommendationSource(s tastedata.Source) { t.source = s }
func text(k, label string, required bool) plugin.Field {
	return plugin.Field{Key: k, Label: label, Type: plugin.Text, Required: required}
}
func (t *Integration) Manifest() plugin.Manifest {
	profile := text("profile", "Household profile", true)
	return plugin.Manifest{ID: "taste", Name: "Taste", Description: "Household recommendations, explicit verdicts and Letterboxd preferences", Version: "1", Settings: []plugin.Field{text("bindings", "Profile bindings JSON", true), {Key: "letterboxd_url", Label: "Letterboxd public endpoint", Type: plugin.URL}, text("twins_artifact", "Optional licensed JSON artifact filename", false)}, Tab: &plugin.Tab{Title: "Taste", Icon: "film", Views: []plugin.ViewRef{{Name: "picks", Title: "Picks"}, {Name: "known", Title: "Known"}, {Name: "familiar", Title: "Familiar"}, {Name: "watchlist", Title: "Watchlist"}, {Name: "import", Title: "Import and sync"}}}, Actions: []plugin.ActionSpec{
		{ID: "feedback", Label: "Record verdict", Fields: []plugin.Field{profile, {Key: "kind", Label: "Kind", Type: plugin.Select, Required: true, Options: []plugin.Option{{Value: "movie", Label: "Film"}, {Value: "tv", Label: "Series"}}}, {Key: "id", Label: "TMDB ID", Type: plugin.Number, Required: true}, text("title", "Title", true), text("year", "Year", false), {Key: "verdict", Label: "Verdict (empty clears)", Type: plugin.Select, Options: []plugin.Option{{Value: "loved", Label: "Loved"}, {Value: "liked", Label: "Liked"}, {Value: "seen", Label: "Seen"}, {Value: "meh", Label: "Meh"}, {Value: "disliked", Label: "Disliked"}, {Value: "pass", Label: "Pass"}, {Value: "ignore", Label: "Ignore"}}}}},
		{ID: "hide", Label: "Hide or restore recommendation", Fields: []plugin.Field{profile, text("kind", "movie or tv", true), {Key: "id", Label: "TMDB ID", Type: plugin.Number, Required: true}, {Key: "hidden", Label: "Hidden", Type: plugin.Bool}}},
		{ID: "refresh", Label: "Refresh model", Fields: []plugin.Field{profile}},
		{ID: "sync", Label: "Sync Letterboxd now", Fields: []plugin.Field{profile}},
		{ID: "import", Label: "Import Letterboxd export ZIP (base64)", Fields: []plugin.Field{profile, text("export", "Base64 ZIP", true)}},
	}}
}
func (t *Integration) Configure(c plugin.Config) error {
	var bindings map[string]Binding
	raw, _ := c["bindings"].(string)
	if json.Unmarshal([]byte(raw), &bindings) != nil || len(bindings) == 0 || len(bindings) > 100 {
		return &plugin.FieldError{Key: "bindings", Message: "provide 1–100 profile bindings"}
	}
	owners := map[int]bool{}
	for id, b := range bindings {
		if !profileName.MatchString(id) || b.Owner <= 0 || owners[b.Owner] || !validUsername(b.Username) {
			return &plugin.FieldError{Key: "bindings", Message: "invalid or duplicate owner binding"}
		}
		owners[b.Owner] = true
	}
	endpoint, _ := c["letterboxd_url"].(string)
	artifact, _ := c["twins_artifact"].(string)
	if artifact != "" && (!profileName.MatchString(strings.TrimSuffix(artifact, ".json")) || !strings.HasSuffix(artifact, ".json")) {
		return &plugin.FieldError{Key: "twins_artifact", Message: "use a JSON filename in the plugin cache directory"}
	}
	for _, b := range bindings {
		if b.Username != "" && endpoint == "" {
			return &plugin.FieldError{Key: "letterboxd_url", Message: "public endpoint required for sync"}
		}
	}
	t.Close()
	t.bindings = bindings
	t.endpoint = strings.TrimRight(endpoint, "/")
	t.artifact = artifact
	t.profiles = map[string]*household{}
	return nil
}
func validUsername(s string) bool {
	return s == "" || regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]{0,39}$`).MatchString(s)
}
func (t *Integration) start() error {
	if t.cancel != nil {
		return nil
	}
	if t.source == nil {
		return errors.New("Media recommendation bridge unavailable")
	}
	var tw *twins.Model
	var err error
	if t.artifact != "" {
		tw, err = twins.LoadJSON(filepath.Join(t.host.CacheDir(), t.artifact))
		if err != nil {
			return errors.New("licensed recommendation artifact unavailable or invalid")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	for id, b := range t.bindings {
		dir := filepath.Join(t.host.CacheDir(), "profiles", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Close()
			return err
		}
		path := filepath.Join(dir, "letterboxd.json")
		p, err := letterboxd.Load(path)
		if err != nil {
			t.Close()
			return errors.New("profile state unavailable")
		}
		if p != nil && p.Owner != b.Owner {
			t.Close()
			return errors.New("profile owner binding mismatch")
		}
		if p == nil {
			if err := letterboxd.Save(path, letterboxd.Profile{Owner: b.Owner, Version: 1, ImportedAt: t.host.Now(), Films: []letterboxd.Film{}}); err != nil {
				t.Close()
				return err
			}
		}
		feedback := &engine.Feedback{Path: filepath.Join(dir, "taste-feedback.json"), Owner: b.Owner}
		model := &engine.Taste{Source: t.source, History: t.source, Owner: b.Owner, ProfilePath: path, Dir: dir, Feedback: feedback, Twins: tw, Log: t.host.Logf}
		h := &household{binding: b, model: model, feedback: feedback, preferences: &engine.Preferences{Path: filepath.Join(dir, "tv-discovery.json"), Owner: b.Owner}}
		h.feed = &engine.Cache{Lifetime: ctx, History: t.source, Catalog: t.source, Owner: b.Owner, ProfilePath: path, Taste: model}
		t.profiles[id] = h
		t.workers.Add(1)
		go func() { defer t.workers.Done(); model.Run(ctx) }()
		if b.Username != "" {
			h.syncer = &letterboxd.Syncer{Path: path, Username: b.Username, Owner: b.Owner, Source: letterboxd.NewRemote(b.Username, t.endpoint, t.host.HTTP()), Catalog: guardedCatalog{Source: t.source, host: t.host, endpoint: t.endpoint}, Log: t.host.Logf}
			t.workers.Add(1)
			go func() { defer t.workers.Done(); h.syncer.Run(ctx) }()
		}
	}
	return nil
}
func (t *Integration) Close() error {
	if t.cancel != nil {
		t.cancel()
		t.workers.Wait()
		for _, p := range t.profiles {
			p.feed.Wait()
		}
		t.cancel = nil
	}
	t.profiles = nil
	return nil
}
func (t *Integration) household(id string) (*household, error) {
	if _, ok := t.bindings[id]; !ok {
		return nil, errors.New("household profile is not bound")
	}
	if err := t.start(); err != nil {
		return nil, err
	}
	return t.profiles[id], nil
}
func (t *Integration) Test(ctx context.Context) plugin.TestResult {
	ok := t.source != nil && len(t.bindings) > 0
	return plugin.TestResult{OK: ok, Message: "Recommendation profile bindings checked locally"}
}
func (t *Integration) Discovery(ctx context.Context, profile string, refresh bool) (engine.Report, error) {
	h, e := t.household(profile)
	if e != nil {
		return engine.Report{}, e
	}
	var r engine.Report
	if refresh {
		r = h.feed.Refresh(ctx)
	} else {
		r = h.feed.Get(ctx)
	}
	return h.preferences.Apply(r)
}
func (t *Integration) Feedback(ctx context.Context, profile, kind string, id int, verdict, title, year string) error {
	h, e := t.household(profile)
	if e != nil {
		return e
	}
	if e = h.feedback.Set(kind, id, verdict, title, year, t.host.Now()); e == nil {
		h.model.Refresh()
	}
	return e
}
func (t *Integration) Hidden(ctx context.Context, profile, kind string, id int, hidden bool) error {
	h, e := t.household(profile)
	if e != nil {
		return e
	}
	return h.preferences.Set(kind, id, hidden)
}
func (t *Integration) Action(ctx context.Context, action string, v plugin.Config) (plugin.View, error) {
	str := func(k string) string { s, _ := v[k].(string); return s }
	id := str("profile")
	h, e := t.household(id)
	if e != nil {
		return plugin.View{}, e
	}
	switch action {
	case "feedback":
		n, _ := v["id"].(float64)
		if float64(int(n)) != n {
			return plugin.View{}, errors.New("invalid title ID")
		}
		e = t.Feedback(ctx, id, str("kind"), int(n), str("verdict"), str("title"), str("year"))
	case "hide":
		n, _ := v["id"].(float64)
		if float64(int(n)) != n {
			return plugin.View{}, errors.New("invalid title ID")
		}
		e = t.Hidden(ctx, id, str("kind"), int(n), v["hidden"] == true)
	case "refresh":
		h.model.Refresh()
	case "sync":
		if h.syncer == nil {
			e = errors.New("Letterboxd sync not configured")
		} else {
			e = h.syncer.Step(ctx, t.host.Now())
		}
	case "import":
		var b []byte
		b, e = base64.StdEncoding.DecodeString(str("export"))
		if e == nil {
			var p letterboxd.Profile
			p, e = letterboxd.Parse(b, t.host.Now())
			if e == nil {
				p.Owner = h.binding.Owner
				p, e = letterboxd.Resolve(ctx, p, guardedCatalog{Source: t.source, host: t.host, endpoint: t.endpoint})
				if e == nil {
					e = letterboxd.Save(h.model.ProfilePath, p)
					h.model.Refresh()
				}
			}
		}
	default:
		e = errors.New("unknown taste action")
	}
	if e != nil {
		return plugin.View{}, e
	}
	return plugin.View{Title: "Taste", UpdatedAt: t.host.Now(), Blocks: []plugin.Block{{Type: "notice", Tone: "ok", Text: "Taste preferences saved; model refreshes in the background."}}}, nil
}
func (t *Integration) View(ctx context.Context, name string, q url.Values) (plugin.View, error) {
	v := plugin.View{Title: "Taste", UpdatedAt: t.host.Now(), Actions: t.Manifest().Actions, Blocks: []plugin.Block{}}
	id := q.Get("profile")
	if id == "" {
		ids := []string{}
		for k := range t.bindings {
			ids = append(ids, k)
		}
		sort.Strings(ids)
		items := []plugin.Item{}
		for _, k := range ids {
			items = append(items, plugin.Item{Title: k, Link: &plugin.Link{View: name, Params: map[string]string{"profile": k}}})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Title: "Household profiles", Items: items, Empty: "Configure profile bindings"})
		return v, nil
	}
	h, e := t.household(id)
	if e != nil {
		return v, e
	}
	verdicts, e := h.feedback.Load()
	if e != nil {
		return v, e
	}
	items := []plugin.Item{}
	if name == "known" {
		listed := map[string]bool{}
		if m := h.model.Model(); m != nil {
			for _, k := range m.Known {
				key := fmt.Sprint(k.Kind, ":", k.ID)
				listed[key] = true
				items = append(items, plugin.Item{Title: k.Title, Subtitle: k.Label, Meta: key + " · " + k.Effect + " · " + verdicts[key].Verdict})
			}
		}
		for key, x := range verdicts {
			if !listed[key] {
				items = append(items, plugin.Item{Title: x.Title, Subtitle: x.Verdict, Meta: key})
			}
		}
	} else if name == "familiar" {
		if m := h.model.Model(); m != nil {
			by := map[string]engine.TastePick{}
			for _, p := range m.Picks {
				by[fmt.Sprint(p.Kind, ":", p.ID)] = p
			}
			for _, key := range m.Familiar {
				p, ok := by[key]
				if _, judged := verdicts[key]; ok && !judged {
					items = append(items, plugin.Item{Title: p.Title, Subtitle: p.Reason, Meta: key})
				}
			}
		}
	} else if name == "import" {
		p, e := letterboxd.Load(h.model.ProfilePath)
		if e != nil {
			return v, e
		}
		if p != nil {
			v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: fmt.Sprintf("%d ratings · %d watched · %d watchlist · %d unmatched", p.Ratings, p.Watched, p.Watchlist, p.Unmatched)})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Import an export ZIP as base64, or configure public Letterboxd sync. Imports and verdicts never write to upstream services."})
		return v, nil
	} else {
		r, e := t.Discovery(ctx, id, false)
		if e != nil {
			return v, e
		}
		rows := r.Titles
		if name == "watchlist" {
			rows = r.Watchlist
		}
		hidden := map[string]bool{}
		for _, k := range r.Hidden {
			hidden[k] = true
		}
		kind := q.Get("kind")
		if kind != "" && kind != "tv" && kind != "movie" {
			return v, errors.New("invalid kind")
		}
		for _, p := range rows {
			key := fmt.Sprint(p.Kind, ":", p.ID)
			if _, judged := verdicts[key]; (judged && name != "watchlist") || hidden[key] || (kind != "" && p.Kind != kind) {
				continue
			}
			items = append(items, plugin.Item{Title: p.Title, Subtitle: p.Reason, Meta: key + " · " + p.Availability})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "notice", Tone: "info", Text: "Model status: " + r.Status})
	}
	if len(items) > 500 {
		items = items[:500]
	}
	v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Title: id + " · " + name, Items: items, Empty: "No titles yet"})
	return v, nil
}

// guardedCatalog routes canonical public film lookups through configured transport.
type guardedCatalog struct {
	tastedata.Source
	host     plugin.Host
	endpoint string
}

func (g guardedCatalog) PublicIdentity(ctx context.Context, uri string) (string, int, error) {
	original, e := url.Parse(uri)
	base, be := url.Parse(g.endpoint)
	if e != nil || be != nil || base.Host == "" || original.Scheme != "https" || original.User != nil || original.RawQuery != "" || original.Fragment != "" || !strings.HasPrefix(original.Path, "/film/") || !regexp.MustCompile(`^/film/[A-Za-z0-9_-]+/$`).MatchString(original.Path) {
		return "", 0, errors.New("invalid public film URL")
	}
	if original.Host != "letterboxd.com" && original.Host != "www.letterboxd.com" && original.Host != base.Host {
		return "", 0, errors.New("invalid public film host")
	}
	target := g.endpoint + original.Path
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if e != nil {
		return "", 0, e
	}
	res, e := g.host.HTTP().Do(req)
	if e != nil {
		return "", 0, errors.New("public identity unavailable")
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if e != nil || res.StatusCode != 200 || len(b) > 2<<20 {
		return "", 0, errors.New("invalid public film response")
	}
	return letterboxd.PageIdentity(b)
}

// Bound reports administrator configuration without starting workers or networking.
func (t *Integration) Bound(profile string) bool { _, ok := t.bindings[profile]; return ok }

// Picks preserves whole-model kind browsing and the twins familiar ordering.
func (t *Integration) Picks(ctx context.Context, profile, kind string, familiar bool) (any, error) {
	h, e := t.household(profile)
	if e != nil {
		return nil, e
	}
	if kind != "" && kind != "movie" && kind != "tv" {
		return nil, errors.New("invalid kind")
	}
	verdicts, e := h.feedback.Load()
	if e != nil {
		return nil, e
	}
	r, e := t.Discovery(ctx, profile, false)
	if e != nil {
		return nil, e
	}
	hidden := map[string]bool{}
	for _, k := range r.Hidden {
		hidden[k] = true
	}
	picks := []engine.Pick{}
	inFeed := map[string]engine.Pick{}
	for _, p := range r.Titles {
		inFeed[fmt.Sprint(p.Kind, ":", p.ID)] = p
	}
	m := h.model.Model()
	if m != nil && (kind != "" || familiar) {
		pool := m.Picks
		if familiar {
			by := map[string]engine.TastePick{}
			for _, p := range pool {
				by[fmt.Sprint(p.Kind, ":", p.ID)] = p
			}
			pool = nil
			for _, k := range m.Familiar {
				if p, ok := by[k]; ok {
					pool = append(pool, p)
				}
			}
		}
		for _, p := range pool {
			k := fmt.Sprint(p.Kind, ":", p.ID)
			if _, judged := verdicts[k]; judged || hidden[k] || (kind != "" && p.Kind != kind) {
				continue
			}
			availability := "unknown"
			if f, ok := inFeed[k]; ok {
				availability = f.Availability
			}
			picks = append(picks, engine.Pick{BrowseTitle: p.BrowseTitle, Reason: p.Reason, Because: p.Because, Seeds: p.Seeds, Rating: p.Rating, Availability: availability})
			if len(picks) == 100 {
				break
			}
		}
	} else if !familiar {
		for _, p := range r.Titles {
			k := fmt.Sprint(p.Kind, ":", p.ID)
			if _, judged := verdicts[k]; !judged && !hidden[k] {
				picks = append(picks, p)
			}
		}
	}
	out := map[string]any{"status": r.Status, "judged": len(verdicts), "updating": h.model.Stale()}
	if familiar {
		out["titles"] = picks
	} else {
		out["picks"] = picks
	}
	if m != nil {
		out["built_at"] = m.BuiltAt
	}
	return out, nil
}
