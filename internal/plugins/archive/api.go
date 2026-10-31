package archive

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

// API serves the Archive page (internal/webui/pages/archive.js). Every route
// maps to one fixed media-policy path; the page never names an upstream URL.
// Writes are validated to the exact shape the service accepts, carry the
// server-derived actor, and are relayed once: an unclear outcome is reported,
// never retried. Upstream refusals keep their status and JSON so the page can
// show the service's own reason (a changed plan, a rate limit).
func (p *Integration) API(ctx context.Context, req plugin.APIRequest) (plugin.APIResponse, error) {
	route := req.Method + " " + req.Path
	switch route {
	case "GET status":
		return p.status(ctx)
	case "GET library":
		path := "/library"
		if req.Query.Get("refresh") == "1" {
			path += "?refresh=1"
		}
		return p.relay(ctx, "GET", path, nil)
	case "GET seeding":
		return p.relay(ctx, "GET", "/seeding", nil)
	case "GET selection":
		return p.relay(ctx, "GET", "/archive/selection", nil)
	case "PUT selection", "POST selection/preview":
		body, err := selectionBody(req.Body, actor(req.Actor))
		if err != nil {
			return refuse(400, err.Error())
		}
		return p.relay(ctx, req.Method, "/archive/"+req.Path, body)
	case "POST tag":
		body, err := tagBody(req.Body, actor(req.Actor))
		if err != nil {
			return refuse(400, err.Error())
		}
		return p.relay(ctx, "POST", "/archive/tag", body)
	case "POST search", "POST approve", "POST run", "POST clear", "POST restore", "POST purge", "POST purge-all":
		body, err := actionBody(req.Path, req.Body, actor(req.Actor))
		if err != nil {
			return refuse(400, err.Error())
		}
		status, raw, err := p.call(ctx, "POST", "/archive/"+req.Path, body)
		if req.Path == "search" && status == http.StatusNotFound {
			return refuse(404, "Search scheduled")
		}
		if err != nil {
			return plugin.APIResponse{}, err
		}
		return plugin.APIResponse{Status: status, Body: raw}, nil
	case "POST pause", "POST job/approve", "POST job/drop", "POST search/reorder", "POST approvals/reorder", "POST seeding/stop", "POST seeding/queue/reorder":
		path := "/archive/" + req.Path
		if strings.HasPrefix(req.Path, "seeding/") {
			path = "/" + req.Path
		}
		body, err := controlBody(path, req.Body, actor(req.Actor))
		if err != nil {
			return refuse(400, err.Error())
		}
		return p.relay(ctx, "POST", path, body)
	case "POST seeding/preview", "POST seeding/move", "POST seeding/queue", "POST seeding/queue/cancel":
		body, err := seedingBody("/"+req.Path, req.Body)
		if err != nil {
			return refuse(400, err.Error())
		}
		return p.relay(ctx, "POST", "/"+req.Path, body)
	}
	return plugin.APIResponse{}, plugin.ErrNotFound
}

// APITimeout keeps the private dashboard's budgets: a cycle can hold an action
// for up to 20 seconds, and a seeding preview reads every file of a torrent.
func (*Integration) APITimeout(method, path string) time.Duration {
	switch {
	case strings.HasPrefix(path, "seeding"):
		return 60 * time.Second
	case path == "status":
		return 12 * time.Second
	case method != "GET" || path == "library" || path == "selection":
		return 25 * time.Second
	}
	return 0
}

func refuse(status int, message string) (plugin.APIResponse, error) {
	b, _ := json.Marshal(map[string]string{"error": message})
	return plugin.APIResponse{Status: status, Body: b}, nil
}

// actor names who asked, for the service's audit.
func actor(a string) string {
	if a == "" || a == "bearer" {
		a = "dashboard API bearer token"
	}
	if len(a) > 200 {
		a = a[:200]
	}
	return a
}

// relay sends one request and accepts only a single JSON value back.
func (p *Integration) relay(ctx context.Context, method, path string, body []byte) (plugin.APIResponse, error) {
	status, raw, err := p.call(ctx, method, path, body)
	if err != nil {
		return plugin.APIResponse{}, err
	}
	return plugin.APIResponse{Status: status, Body: raw}, nil
}

func (p *Integration) call(ctx context.Context, method, path string, body []byte) (int, json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, err := http.NewRequestWithContext(ctx, method, p.base+path, rd)
	if err != nil {
		return 0, nil, errors.New("policy request failed")
	}
	if p.token != "" {
		r.Header.Set("Authorization", "Bearer "+p.token)
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := plugin.WithoutRedirects(p.host.HTTP()).Do(r)
	if err != nil {
		if method == "GET" {
			return 0, nil, errors.New("media policy did not answer")
		}
		return 0, nil, errors.New("media policy did not answer; outcome unknown, inspect status before retrying")
	}
	defer resp.Body.Close()
	// The page treats 401 as its own session expiring; a refused worker
	// credential is a configuration fault, not a dashboard sign-out.
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return 0, nil, fmt.Errorf("media policy refused the configured token (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return 0, nil, fmt.Errorf("media policy answered a redirect (HTTP %d); not followed", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, plugin.MaxAPIResponseBytes+1))
	if err != nil || len(data) > plugin.MaxAPIResponseBytes {
		return 0, nil, errors.New("media policy response unreadable or too large")
	}
	var out json.RawMessage
	d := json.NewDecoder(bytes.NewReader(data))
	if d.Decode(&out) != nil || d.Decode(new(any)) != io.EOF {
		return resp.StatusCode, nil, fmt.Errorf("invalid media policy response (HTTP %d)", resp.StatusCode)
	}
	return resp.StatusCode, out, nil
}

// status is the page's one poll: the archive state, current job and its
// progress, the seeding mover and the window. An outage is a 200 with
// reachable:false, so the page says "it stopped" rather than failing.
func (p *Integration) status(ctx context.Context) (plugin.APIResponse, error) {
	now := p.host.Now().UTC()
	down := func(why string) (plugin.APIResponse, error) {
		b, _ := json.Marshal(map[string]any{"reachable": false, "error": "media policy did not answer: " + why, "fetched": now})
		return plugin.APIResponse{Body: b}, nil
	}
	code, raw, err := p.call(ctx, "GET", "/status?refresh=progress", nil)
	if err != nil {
		return down(err.Error())
	}
	if code != http.StatusOK {
		return down(fmt.Sprintf("status endpoint answered HTTP %d", code))
	}
	var in struct {
		ArchivePaused   bool                       `json:"archivePaused"`
		ArchiveSearches json.RawMessage            `json:"archiveSearches"`
		DryRun          bool                       `json:"dryRun"`
		Healthy         bool                       `json:"healthy"`
		Error           string                     `json:"error"`
		Checked         time.Time                  `json:"checked"`
		ArchiveEnabled  bool                       `json:"archiveEnabled"`
		Archive         map[string]json.RawMessage `json:"archive"`
		Seeding         json.RawMessage            `json:"seeding"`
		SeedingQueue    json.RawMessage            `json:"seedingQueue"`
		SeedingResults  json.RawMessage            `json:"seedingQueueResults"`
		Info            json.RawMessage            `json:"archiveInfo"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return down("invalid media policy status")
	}
	// The recycle baseline is the whole recycle inventory: the service needs
	// it, a browser does not.
	if job, ok := in.Archive["job"]; ok {
		var m map[string]json.RawMessage
		if json.Unmarshal(job, &m) == nil && m != nil {
			delete(m, "recycleBaseline")
			in.Archive["job"], _ = json.Marshal(m)
		}
	}
	if failed, ok := in.Archive["failedJobs"]; ok {
		var jobs []map[string]json.RawMessage
		if json.Unmarshal(failed, &jobs) == nil {
			for _, j := range jobs {
				delete(j, "recycleBaseline")
			}
			in.Archive["failedJobs"], _ = json.Marshal(jobs)
		}
	}
	b, err := json.Marshal(map[string]any{"reachable": true, "archivePaused": in.ArchivePaused, "dryRun": in.DryRun, "healthy": in.Healthy, "error": in.Error, "checked": in.Checked,
		"archiveSearches": in.ArchiveSearches, "seeding": in.Seeding, "seedingQueue": in.SeedingQueue, "seedingQueueResults": in.SeedingResults,
		"archiveEnabled": in.ArchiveEnabled, "archive": in.Archive, "info": in.Info, "fetched": now})
	if err != nil {
		return down("invalid media policy status")
	}
	return plugin.APIResponse{Body: b}, nil
}

// strict decodes one JSON object with no unknown fields. An empty body reads
// as {} where the private routes allowed that.
func strict(body []byte, max int, allowEmpty bool, v any) bool {
	if len(body) > max {
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return allowEmpty
	}
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	return d.Decode(v) == nil && d.Decode(new(any)) == io.EOF
}

// approvalRequest selects a worker-cached release, never a caller-supplied one.
type approvalRequest struct {
	Kind      string `json:"kind"`
	ID        int    `json:"id"`
	Season    int    `json:"season,omitempty"`
	ReleaseID string `json:"releaseId,omitempty"`
	IndexerID int    `json:"indexerId,omitempty"`
	Clear     bool   `json:"clear,omitempty"`
}

// actionBody mirrors the private API: restore and purge without apply return
// the service's plan; with apply they need its hash (and, for purge, the
// typed phrase), which the service checks.
func actionBody(action string, body []byte, by string) ([]byte, error) {
	var in struct {
		Approval *approvalRequest `json:"approval,omitempty"`
		Title    *approvalRequest `json:"title,omitempty"`
		ID       string           `json:"id,omitempty"`
		Apply    bool             `json:"apply,omitempty"`
		Hash     string           `json:"hash,omitempty"`
		Confirm  string           `json:"confirm,omitempty"`
	}
	if !strict(body, 8<<10, true, &in) {
		return nil, errors.New("invalid request")
	}
	switch {
	case action == "search" && in.Title == nil:
		return nil, errors.New("title is required")
	case action != "run" && action != "purge-all" && action != "approve" && action != "search" && in.ID == "":
		return nil, errors.New("id is required")
	case action == "approve" && in.Approval == nil:
		return nil, errors.New("approval is required")
	case action == "clear" && (!in.Apply || in.Hash != "" || in.Confirm != "" || in.Approval != nil || in.Title != nil):
		return nil, errors.New("id and apply are required")
	}
	return json.Marshal(struct {
		Approval    *approvalRequest `json:"approval,omitempty"`
		Title       *approvalRequest `json:"title,omitempty"`
		ID          string           `json:"id,omitempty"`
		Apply       bool             `json:"apply,omitempty"`
		Hash        string           `json:"hash,omitempty"`
		Confirm     string           `json:"confirm,omitempty"`
		RequestedBy string           `json:"requestedBy"`
	}{in.Approval, in.Title, in.ID, in.Apply, in.Hash, in.Confirm, by})
}

// controlRequest uses zero-based queue positions. Position is a pointer so
// omitting it cannot move an item to the front.
type controlRequest struct {
	ID          string `json:"id,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Key         string `json:"key,omitempty"`
	Position    *int   `json:"position,omitempty"`
	Paused      *bool  `json:"paused,omitempty"`
	Apply       bool   `json:"apply"`
	RequestedBy string `json:"requestedBy,omitempty"`
}

func validControl(path string, in controlRequest) bool {
	if !in.Apply || in.RequestedBy != "" {
		return false
	}
	switch path {
	case "/archive/pause":
		return in.Paused != nil && in.ID == "" && in.Hash == "" && in.Key == "" && in.Position == nil
	case "/archive/job/approve", "/archive/job/drop":
		return in.ID != "" && in.Hash == "" && in.Key == "" && in.Position == nil && in.Paused == nil
	case "/seeding/stop":
		return validSeedHash(in.Hash) && in.ID == "" && in.Key == "" && in.Position == nil && in.Paused == nil
	case "/seeding/queue/reorder":
		return validSeedHash(in.Hash) && in.ID == "" && in.Key == "" && in.Position != nil && *in.Position >= 0 && in.Paused == nil
	case "/archive/search/reorder", "/archive/approvals/reorder":
		return in.Key != "" && in.ID == "" && in.Hash == "" && in.Position != nil && *in.Position >= 0 && in.Paused == nil
	}
	return false
}

func controlBody(path string, body []byte, by string) ([]byte, error) {
	var in controlRequest
	if !strict(body, 8<<10, false, &in) || !validControl(path, in) {
		return nil, errors.New("Invalid control request")
	}
	in.RequestedBy = by
	return json.Marshal(in)
}

func validSeedHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

const seedQueueMax = 50

// seedingBody re-encodes one seeding POST so only a known shape reaches the
// service. A preview can never carry apply or a plan hash.
func seedingBody(path string, body []byte) ([]byte, error) {
	bad := errors.New("invalid seeding request")
	if strings.HasPrefix(path, "/seeding/queue") {
		var in struct {
			AllowMove bool `json:"allowMove,omitempty"`
			Items     []struct {
				Hash     string `json:"hash"`
				PlanHash string `json:"planHash"`
			} `json:"items,omitempty"`
			Apply bool `json:"apply"`
		}
		if !strict(body, 16<<10, false, &in) || len(in.Items) > seedQueueMax || (path == "/seeding/queue/cancel") != (len(in.Items) == 0) {
			return nil, bad
		}
		for _, it := range in.Items {
			if !validSeedHash(it.Hash) || it.PlanHash == "" {
				return nil, bad
			}
		}
		return json.Marshal(in)
	}
	var in struct {
		Hash     string `json:"hash"`
		PlanHash string `json:"planHash,omitempty"`
		Apply    bool   `json:"apply,omitempty"`
	}
	if !strict(body, 4096, false, &in) || !validSeedHash(in.Hash) || (path == "/seeding/preview" && (in.Apply || in.PlanHash != "")) {
		return nil, bad
	}
	return json.Marshal(in)
}

// tagBody is an explicit owner opt-in, with no profile or monitoring edits.
func tagBody(body []byte, by string) ([]byte, error) {
	var in struct {
		Kind        string `json:"kind"`
		ID          int    `json:"id"`
		Archive     *bool  `json:"archive"`
		FourK       *bool  `json:"fourK"`
		Seasons     []int  `json:"seasons,omitempty"`
		RequestedBy string `json:"requestedBy,omitempty"`
	}
	if !strict(body, 8<<10, false, &in) || in.Archive == nil || in.FourK == nil || in.RequestedBy != "" || in.ID <= 0 || (in.Kind != "radarr" && in.Kind != "sonarr") {
		return nil, errors.New("invalid tag request")
	}
	in.RequestedBy = by
	return json.Marshal(in)
}

// selectionOverrides layers fields over the service's file policy: nil
// inherits, a pointer to zero or an empty list is an explicit value.
type selectionOverrides struct {
	Sizes map[string]struct {
		Floor  *float64 `json:"floor,omitempty"`
		Target *float64 `json:"target,omitempty"`
		Cap    *float64 `json:"cap,omitempty"`
	} `json:"sizes,omitempty"`
	CodecFactors  map[string]*float64 `json:"codecFactors,omitempty"`
	TrustedGroups *[]string           `json:"trustedGroups,omitempty"`
	DeniedGroups  *[]string           `json:"deniedGroups,omitempty"`
	MinScore      *float64            `json:"minScore,omitempty"`
}

// selectionBody takes exactly one of overrides or reset; the service
// validates the values themselves.
func selectionBody(body []byte, by string) ([]byte, error) {
	var in struct {
		Overrides *selectionOverrides `json:"overrides,omitempty"`
		Reset     bool                `json:"reset,omitempty"`
	}
	if !strict(body, 16<<10, false, &in) || in.Reset && in.Overrides != nil || !in.Reset && in.Overrides == nil {
		return nil, errors.New("Invalid value")
	}
	return json.Marshal(struct {
		Overrides   *selectionOverrides `json:"overrides,omitempty"`
		Reset       bool                `json:"reset,omitempty"`
		RequestedBy string              `json:"requestedBy"`
	}{in.Overrides, in.Reset, by})
}
