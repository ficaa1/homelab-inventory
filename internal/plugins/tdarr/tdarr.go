// Package tdarr reviews exact Arr identities against an existing policy service
// and delegates execution exclusively to the existing guarded shrink sidecar.
package tdarr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/tdarr/engine"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type protection struct {
	Kind   string `json:"kind"`
	ID     int    `json:"id"`
	TMDBID int    `json:"tmdb_id"`
	IMDBID string `json:"imdb_id"`
	Title  string `json:"title"`
	Path   string `json:"path"`
}
type Integration struct {
	host      plugin.Host
	c         plugin.Config
	reader    *engine.Tdarr
	batch     *engine.Engine
	pending   map[string]engine.Preview
	protected []protection
}

func New(h plugin.Host) plugin.Plugin { return &Integration{host: h} }
func actions() []plugin.ActionSpec {
	return []plugin.ActionSpec{{ID: "preview-batch", Label: "Preview multiple library titles", Operator: true, Fields: []plugin.Field{{Key: "selections", Label: "One service:id:seasons per line (for example sonarr:12:1,2)", Type: plugin.Text, Required: true}, {Key: "with_pc", Label: "Include secondary worker", Type: plugin.Bool, Required: true}}}, {ID: "preview", Label: "Preview one library title", Operator: true, Fields: []plugin.Field{{Key: "kind", Label: "Service", Type: plugin.Select, Required: true, Options: []plugin.Option{{Value: "radarr", Label: "Radarr"}, {Value: "sonarr", Label: "Sonarr"}}}, {Key: "id", Label: "Service title ID", Type: plugin.Number, Required: true}, {Key: "seasons", Label: "Seasons (comma separated, blank means all)", Type: plugin.Text}, {Key: "with_pc", Label: "Include secondary worker", Type: plugin.Bool, Required: true}}}, {ID: "execute", Label: "Execute reviewed batch", Operator: true, TimeoutSeconds: 360, Fields: []plugin.Field{{Key: "token", Label: "Preview confirmation token", Type: plugin.Text, Required: true}, {Key: "hash", Label: "Reviewed plan hash", Type: plugin.Text, Required: true}}}}
}
func (*Integration) Manifest() plugin.Manifest {
	fields := []plugin.Field{}
	for _, s := range []struct{ id, label string }{{"tdarr", "Tdarr"}, {"policy", "Media-policy"}, {"sidecar", "Shrink sidecar"}, {"sonarr", "Sonarr"}, {"radarr", "Radarr"}, {"tautulli", "Tautulli"}} {
		fields = append(fields, plugin.Field{Key: s.id + "_url", Label: s.label + " URL", Type: plugin.URL, Required: s.id == "tdarr"}, plugin.Field{Key: s.id + "_key", Label: s.label + " key or token", Type: plugin.Secret})
	}
	for _, k := range []struct{ key, label string }{{"radarr_root", "Radarr media root"}, {"sonarr_root", "Sonarr media root"}, {"tdarr_movie_root", "Tdarr movie root"}, {"tdarr_series_root", "Tdarr series root"}} {
		fields = append(fields, plugin.Field{Key: k.key, Label: k.label, Type: plugin.Text, Help: "Absolute media root; no host filesystem is read by the dashboard."})
	}
	fields = append(fields, plugin.Field{Key: "protected_titles", Label: "Protected title identities (JSON array)", Type: plugin.Text, Help: "Explicit kind/id, tmdb_id, imdb_id, title and/or path exclusions. Empty means none."})
	return plugin.Manifest{ID: "tdarr", Name: "Tdarr", Version: "1", Description: "Review compression candidates and explicitly execute guarded batches through an existing sidecar.", Settings: fields, Actions: actions(), Tab: &plugin.Tab{Title: "Tdarr", Icon: "film", Group: "media", Views: []plugin.ViewRef{{Name: "library", Title: "Library"}, {Name: "status", Title: "Status"}, {Name: "candidates", Title: "Candidates"}}}}
}
func (p *Integration) str(k string) string {
	s, _ := p.c[k].(string)
	if strings.HasSuffix(k, "_url") || strings.HasSuffix(k, "_root") {
		return strings.TrimRight(s, "/")
	}
	return s
}
func (p *Integration) Configure(c plugin.Config) error {
	for k, v := range c {
		s, _ := v.(string)
		if strings.HasSuffix(k, "_url") && s != "" {
			u, e := url.Parse(s)
			if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return &plugin.FieldError{Key: k, Message: "Use a base URL without credentials or query"}
			}
		}
		if strings.HasSuffix(k, "_root") && s != "" && (!strings.HasPrefix(s, "/") || path.Clean(s) != s || strings.ContainsAny(s, "\\\x00")) {
			return &plugin.FieldError{Key: k, Message: "Use a clean absolute media root"}
		}
		if strings.HasSuffix(k, "_key") && strings.ContainsAny(s, "\r\n") {
			return &plugin.FieldError{Key: k, Message: "Credential contains line breaks"}
		}
	}
	raw, _ := c["protected_titles"].(string)
	protected := []protection{}
	if raw != "" {
		d := json.NewDecoder(strings.NewReader(raw))
		d.DisallowUnknownFields()
		if d.Decode(&protected) != nil || d.Decode(new(any)) != io.EOF || len(protected) > 1000 {
			return &plugin.FieldError{Key: "protected_titles", Message: "Use an array of at most 1000 protected identity records"}
		}
		for _, r := range protected {
			if r.ID == 0 && r.TMDBID == 0 && r.IMDBID == "" && r.Title == "" && r.Path == "" {
				return &plugin.FieldError{Key: "protected_titles", Message: "Each protection requires an identity"}
			}
		}
	}
	p.c = c
	p.protected = protected
	p.pending = map[string]engine.Preview{}
	prefixes := []string{}
	for _, key := range []string{"tdarr_movie_root", "tdarr_series_root"} {
		if s := p.str(key); s != "" {
			prefixes = append(prefixes, s+"/")
		}
	}
	p.reader = engine.NewTdarr(p.str("tdarr_url"), plugin.WithoutRedirects(plugin.MetadataHTTP(p.host.HTTP())), prefixes)
	p.batch = &engine.Engine{Inventory: p.inventory, Resolve: p.resolve, Candidates: func(ctx context.Context) ([]engine.ShrinkCandidate, error) { return p.reader.ShrinkCandidates(ctx, 0) }, Status: p.reader.FreshStatus, Idle: p.idle, MapPath: p.mapPath, Blocked: p.blocked, Now: p.host.Now}
	return nil
}
func (p *Integration) Close() error {
	p.c = nil
	p.pending = nil
	p.batch = nil
	p.reader = nil
	return nil
}
func (p *Integration) read(ctx context.Context, service, method, endpoint string, body any, out any) error {
	if p.str(service+"_url") == "" {
		return engine.ErrNotConfigured
	}
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	r, e := http.NewRequestWithContext(ctx, method, p.str(service+"_url")+endpoint, bytes.NewReader(b))
	if e != nil {
		return engine.ErrUnavailable
	}
	key := p.str(service + "_key")
	if service == "sonarr" || service == "radarr" {
		r.Header.Set("X-Api-Key", key)
	} else if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	r.Header.Set("Content-Type", "application/json")
	client := *plugin.WithoutRedirects(p.host.HTTP())
	if service == "sidecar" {
		client = *plugin.LongActionHTTP(&client)
		client.Timeout = 6 * time.Minute
	}
	resp, e := client.Do(r)
	if e != nil {
		if service == "sidecar" {
			return errors.New("sidecar unavailable; write outcome may be unknown; inspect sidecar before retrying")
		}
		return fmt.Errorf("%s unavailable", service)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s refused request (HTTP %d)", service, resp.StatusCode)
	}
	if out != nil {
		d := json.NewDecoder(resp.Body)
		if d.Decode(out) != nil || d.Decode(new(any)) != io.EOF {
			return engine.ErrUnavailable
		}
	}
	return nil
}
func (p *Integration) inventory(ctx context.Context, refresh bool) (engine.Inventory, error) {
	var library struct {
		Items []engine.LibraryItem `json:"items"`
	}
	endpoint := "/library"
	if refresh {
		endpoint += "?refresh=1"
	}
	if e := p.read(ctx, "policy", "GET", endpoint, nil, &library); e != nil {
		return engine.Inventory{}, e
	}
	var status struct {
		Service string    `json:"service"`
		Checked time.Time `json:"checked"`
		Leases  map[string]struct {
			Owner string `json:"owner"`
		} `json:"leases"`
		Archive *struct {
			Job *struct {
				Target struct {
					Kind string `json:"kind"`
					ID   int    `json:"id"`
				} `json:"target"`
				Phase string `json:"phase"`
			} `json:"job"`
			FailedJobs []struct {
				Target struct {
					Kind string `json:"kind"`
					ID   int    `json:"id"`
				} `json:"target"`
				Phase string `json:"phase"`
			} `json:"failedJobs"`
		} `json:"archive"`
	}
	if e := p.read(ctx, "policy", "GET", "/status", nil, &status); e != nil {
		return engine.Inventory{}, e
	}
	if library.Items == nil || status.Service != "mediapolicy" || status.Leases == nil || status.Archive == nil || status.Checked.IsZero() || p.host.Now().Sub(status.Checked) > 10*time.Minute || status.Checked.Sub(p.host.Now()) > 5*time.Second {
		return engine.Inventory{}, engine.ErrUnavailable
	}
	inv := engine.Inventory{Items: library.Items, Leases: map[string]string{}}
	for k, v := range status.Leases {
		if v.Owner == "" {
			return inv, engine.ErrUnavailable
		}
		inv.Leases[k] = v.Owner
	}
	if j := status.Archive.Job; j != nil && j.Phase != "complete" && j.Phase != "abandoned" {
		inv.Leases[engine.TitleKey(j.Target.Kind, j.Target.ID)] = "archive"
	}
	for _, j := range status.Archive.FailedJobs {
		if j.Phase != "complete" && j.Phase != "abandoned" {
			inv.Leases[engine.TitleKey(j.Target.Kind, j.Target.ID)] = "archive"
		}
	}
	return inv, nil
}
func (p *Integration) resolve(ctx context.Context, s engine.Selection) (engine.LibraryTitle, []engine.LibraryFile, error) {
	var title engine.LibraryTitle
	title.Kind = s.Kind
	endpoint := "/api/v3/movie/"
	if s.Kind == "sonarr" {
		endpoint = "/api/v3/series/"
	}
	e := p.read(ctx, s.Kind, "GET", endpoint+strconv.Itoa(s.ID), nil, &title)
	if e != nil {
		return title, nil, e
	}
	if title.ID != s.ID {
		return title, nil, engine.ErrUnavailable
	}
	if s.Kind == "radarr" {
		if title.MovieFile.ID <= 0 {
			return title, nil, engine.ErrUnavailable
		}
		return title, []engine.LibraryFile{title.MovieFile}, nil
	}
	var files []engine.LibraryFile
	e = p.read(ctx, "sonarr", "GET", "/api/v3/episodefile?seriesId="+strconv.Itoa(s.ID), nil, &files)
	for _, f := range files {
		if f.SeriesID != s.ID {
			return title, nil, engine.ErrUnavailable
		}
	}
	return title, files, e
}
func (p *Integration) idle(ctx context.Context) error {
	key := url.QueryEscape(p.str("tautulli_key"))
	var out struct {
		Response struct {
			Result string `json:"result"`
			Data   struct {
				Count    *int              `json:"stream_count"`
				Sessions []json.RawMessage `json:"sessions"`
			} `json:"data"`
		} `json:"response"`
	}
	if e := p.read(ctx, "tautulli", "GET", "/api/v2?cmd=get_activity&apikey="+key, nil, &out); e != nil {
		return e
	}
	if out.Response.Result != "success" || out.Response.Data.Count == nil || out.Response.Data.Sessions == nil || *out.Response.Data.Count != 0 || len(out.Response.Data.Sessions) != 0 {
		return errors.New("Plex playback is active or unknown")
	}
	return nil
}
func (p *Integration) blocked(t engine.LibraryTitle) bool {
	for _, r := range p.protected {
		if r.Kind != "" && t.Kind != "" && r.Kind != t.Kind {
			continue
		}
		if r.ID > 0 && r.ID == t.ID || r.TMDBID > 0 && r.TMDBID == t.TMDBID || r.IMDBID != "" && r.IMDBID == t.IMDBID || r.Title != "" && strings.EqualFold(r.Title, t.Title) {
			return true
		}
		if r.Path != "" {
			a, b := strings.ToLower(path.Clean(t.Path)), strings.ToLower(path.Clean(r.Path))
			if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
				return true
			}
		}
	}
	return false
}
func (p *Integration) mapPath(file, kind string) (string, error) {
	source, target := p.str("radarr_root"), p.str("tdarr_movie_root")
	if kind == "sonarr" {
		source, target = p.str("sonarr_root"), p.str("tdarr_series_root")
	} else if kind != "radarr" {
		return "", errors.New("invalid service kind")
	}
	ext := strings.ToLower(path.Ext(file))
	allowed := map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".m4v": true, ".ts": true, ".mov": true, ".m2ts": true, ".mpg": true, ".mpeg": true, ".flv": true, ".webm": true, ".wmv": true, ".vob": true, ".evo": true, ".iso": true}
	if source == "" || target == "" || path.Clean(file) != file || !strings.HasPrefix(file, source+"/") || !allowed[ext] || strings.ContainsAny(file, "\x00\\") || len(file) > 1024 {
		return "", errors.New("outside configured media roots")
	}
	return target + "/" + strings.TrimPrefix(file, source+"/"), nil
}
func (p *Integration) safe(s string) string {
	for k, v := range p.c {
		if strings.HasSuffix(k, "_key") {
			if secret, ok := v.(string); ok && secret != "" {
				s = strings.ReplaceAll(s, secret, "[redacted]")
			}
		}
	}
	return strings.NewReplacer("<", " ", ">", " ").Replace(s)
}
func (p *Integration) Test(ctx context.Context) plugin.TestResult {
	status := p.reader.FreshStatus(ctx)
	return plugin.TestResult{OK: status.Status == "ok", Message: "Tdarr read-only status check"}
}
func (p *Integration) View(ctx context.Context, name string, _ url.Values) (plugin.View, error) {
	v := plugin.View{Title: "Tdarr", UpdatedAt: p.host.Now(), RefreshSeconds: 30, Actions: actions(), Blocks: []plugin.Block{}}
	rows := []map[string]any{}
	switch name {
	case "status":
		s := p.reader.Status(ctx)
		if s.Status != "ok" {
			return v, engine.ErrUnavailable
		}
		for _, n := range s.Nodes {
			label := "Unknown"
			if n.Name != nil {
				label = p.safe(*n.Name)
			}
			rows = append(rows, map[string]any{"name": label, "online": n.Online, "active": len(n.Active)})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Workers", Columns: []plugin.Column{{Key: "name", Label: "Node"}, {Key: "online", Label: "Online"}, {Key: "active", Label: "Active jobs", Format: "number"}}, Rows: rows})
		if s.Queue != nil {
			scalar := func(n *int) string {
				if n == nil {
					return "Unknown"
				}
				return strconv.Itoa(*n)
			}
			v.Blocks = append(v.Blocks, plugin.Block{Type: "stats", Items: []plugin.Item{{Label: "Queued", Value: scalar(s.Queue.Queued)}, {Label: "Processing", Value: scalar(s.Queue.Processing)}, {Label: "Errors", Value: scalar(s.Queue.Error)}}})
		}
	case "candidates":
		items, e := p.reader.ShrinkCandidates(ctx, 500)
		if e != nil {
			return v, e
		}
		for _, i := range items {
			rows = append(rows, map[string]any{"path": p.safe(i.Path), "size": i.SizeGB, "saving": i.EstSaveGB, "kind": i.Kind, "reason": p.safe(i.Reason)})
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Compression candidates", Columns: []plugin.Column{{Key: "path", Label: "File"}, {Key: "size", Label: "Size GB", Format: "number"}, {Key: "saving", Label: "Estimated saving GB", Format: "number"}, {Key: "kind", Label: "Class"}, {Key: "reason", Label: "Reason"}}, Rows: rows})
	case "library":
		items, e := p.batch.LibraryCandidates(ctx)
		if e != nil {
			return v, e
		}
		for _, i := range items {
			rows = append(rows, map[string]any{"kind": i.Entry.Kind, "id": i.Entry.ID, "title": p.safe(i.Entry.Title), "skip": p.safe(i.BlockedReason)})
		}
		if len(rows) > 500 {
			rows = rows[:500]
		}
		v.Blocks = append(v.Blocks, plugin.Block{Type: "table", Title: "Library identities", Columns: []plugin.Column{{Key: "kind", Label: "Service"}, {Key: "id", Label: "ID", Format: "number"}, {Key: "title", Label: "Title"}, {Key: "skip", Label: "Protection"}}, Rows: rows})
	default:
		return v, plugin.ErrNotFound
	}
	return v, nil
}
func (p *Integration) planView(preview engine.Preview) plugin.View {
	rows := []map[string]any{}
	for _, f := range preview.Plan.Files {
		rows = append(rows, map[string]any{"path": p.safe(f.Path), "size": f.Size, "skip": p.safe(f.Skip), "class": f.Class})
	}
	return plugin.View{Title: "Review Tdarr batch", UpdatedAt: p.host.Now(), Actions: actions(), Blocks: []plugin.Block{{Type: "notice", Tone: "info", Text: "Confirmation token: " + preview.Token}, {Type: "notice", Tone: "info", Text: "Plan hash: " + preview.Hash}, {Type: "notice", Tone: "warn", Text: "Review every selected file. Confirmation expires after two minutes and is consumed once. " + p.safe(preview.Error)}, {Type: "table", Title: "Exact files", Columns: []plugin.Column{{Key: "path", Label: "File"}, {Key: "size", Label: "Bytes", Format: "bytes"}, {Key: "class", Label: "Class"}, {Key: "skip", Label: "Skipped reason"}}, Rows: rows}}}
}
func (p *Integration) Action(ctx context.Context, id string, c plugin.Config) (plugin.View, error) {
	switch id {
	case "preview", "preview-batch":
		selections := []engine.Selection{}
		if id == "preview-batch" {
			raw, _ := c["selections"].(string)
			lines := strings.Split(strings.TrimSpace(raw), "\n")
			if len(lines) < 1 || len(lines) > 16 {
				return plugin.View{}, errors.New("select 1 to 16 library identities")
			}
			for _, line := range lines {
				parts := strings.Split(strings.TrimSpace(line), ":")
				if len(parts) < 2 || len(parts) > 3 {
					return plugin.View{}, errors.New("use service:id:seasons")
				}
				titleID, e := strconv.Atoi(parts[1])
				if e != nil {
					return plugin.View{}, errors.New("invalid title ID")
				}
				selection := engine.Selection{Kind: parts[0], ID: titleID}
				if len(parts) == 3 {
					for _, part := range strings.Split(parts[2], ",") {
						n, e := strconv.Atoi(part)
						if e != nil {
							return plugin.View{}, errors.New("invalid season")
						}
						selection.Seasons = append(selection.Seasons, n)
					}
				}
				selections = append(selections, selection)
			}
		} else {
			kind, _ := c["kind"].(string)
			number, _ := c["id"].(float64)
			if number < 1 || number > 1e9 || number != float64(int(number)) {
				return plugin.View{}, errors.New("invalid title ID")
			}
			raw, _ := c["seasons"].(string)
			seasons := []int{}
			for _, s := range strings.Split(raw, ",") {
				if strings.TrimSpace(s) == "" {
					continue
				}
				n, e := strconv.Atoi(strings.TrimSpace(s))
				if e != nil || n < 0 || n > 10000 {
					return plugin.View{}, errors.New("invalid season")
				}
				seasons = append(seasons, n)
			}
			selections = append(selections, engine.Selection{Kind: kind, ID: int(number), Seasons: seasons})
		}
		pc, _ := c["with_pc"].(bool)
		preview, e := p.batch.Preview(ctx, "operator", engine.Request{Selection: selections, WithPC: pc})
		if e != nil {
			return plugin.View{}, e
		}
		if preview.Token != "" {
			if len(p.pending) > 256 {
				p.pending = map[string]engine.Preview{}
			}
			p.pending[preview.Token] = preview
		}
		return p.planView(preview), nil
	case "execute":
		token, _ := c["token"].(string)
		hash, _ := c["hash"].(string)
		preview, ok := p.pending[token]
		delete(p.pending, token)
		if !ok {
			return plugin.View{}, engine.ErrConflict
		}
		_, outcome, e := p.submit(ctx, "operator", engine.ExecuteRequest{Plan: preview.Plan, Token: token, Hash: hash, Confirm: true})
		if e != nil {
			return plugin.View{}, e
		}
		message, tone := "Sidecar accepted the batch; inspect Tdarr status for queue progress.", "info"
		if outcome == "failed" {
			message, tone = "Sidecar reported failure; inspect it before attempting another batch.", "warn"
		} else if outcome == "unknown" {
			message, tone = "Outcome unknown; inspect the sidecar and Tdarr before another batch.", "warn"
		}
		return plugin.View{Title: "Tdarr batch", UpdatedAt: p.host.Now(), Blocks: []plugin.Block{{Type: "notice", Tone: tone, Text: message}}}, nil
	}
	return plugin.View{}, plugin.ErrNotFound
}
