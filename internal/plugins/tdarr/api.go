package tdarr

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/tdarr/engine"
)

// The page API serves internal/webui/pages/tdarr.js:
//
//	GET  status            cached Tdarr queue and workers
//	GET  candidates        files Tdarr would shrink, largest saving first
//	GET  batch/candidates  the Library, or with kind, id and seasons one title's exact files
//	POST batch/preview     the reviewed plan, its hash and a two-minute confirmation token
//	POST batch/execute     recheck everything, validate on the sidecar, submit once
//
// Writes take the normal dashboard login, like the private dashboard; the
// preview token and reviewed hash are what guard a batch.

func (p *Integration) APITimeout(method, path string) time.Duration {
	switch path {
	case "batch/execute", "shrink":
		return 6 * time.Minute
	case "batch/preview", "batch/candidates", "candidates", "shrink-candidates":
		return 25 * time.Second
	}
	return 0
}

func (p *Integration) API(ctx context.Context, r plugin.APIRequest) (plugin.APIResponse, error) {
	switch r.Method + " " + r.Path {
	case "GET status":
		return reply(200, p.reader.Status(ctx)), nil
	case "GET shrink-candidates":
		limit := 25
		if raw := r.Query.Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				return reply(400, map[string]string{"error": "limit must be a positive number"}), nil
			}
			limit = min(n, 500)
		}
		items, err := p.reader.ShrinkCandidates(ctx, limit)
		if err != nil {
			return reply(503, map[string]string{"error": p.safe(err.Error())}), nil
		}
		return reply(200, items), nil
	case "GET candidates":
		items, err := p.reader.ShrinkCandidates(ctx, 500)
		if err != nil {
			return p.failure(err, true), nil
		}
		return reply(200, map[string]any{"status": "ok", "items": items}), nil
	case "GET batch/candidates":
		return p.apiCandidates(ctx, r)
	case "POST batch/preview":
		var in engine.Request
		if !decode(r.Body, &in) {
			return reply(400, map[string]string{"error": "invalid request"}), nil
		}
		out, err := p.batch.Preview(ctx, r.Actor, in)
		if err != nil {
			return p.failure(err, false), nil
		}
		return reply(200, out), nil
	case "POST shrink":
		return p.shrink(ctx, r.Body), nil
	case "POST batch/execute":
		var in engine.ExecuteRequest
		if !decode(r.Body, &in) {
			return reply(400, map[string]string{"error": "invalid request"}), nil
		}
		return p.apiExecute(ctx, r.Actor, in), nil
	}
	return plugin.APIResponse{}, plugin.ErrNotFound
}

func (p *Integration) apiCandidates(ctx context.Context, r plugin.APIRequest) (plugin.APIResponse, error) {
	q := r.Query
	if raw := q.Get("id"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil {
			return reply(400, map[string]string{"error": "invalid id"}), nil
		}
		sel := engine.Selection{Kind: q.Get("kind"), ID: id}
		if raw := q.Get("seasons"); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				n, err := strconv.Atoi(part)
				if err != nil {
					return reply(400, map[string]string{"error": "invalid seasons"}), nil
				}
				sel.Seasons = append(sel.Seasons, n)
			}
		}
		out, err := p.batch.Build(ctx, engine.Request{Selection: []engine.Selection{sel}})
		if err != nil {
			return p.failure(err, errors.Is(err, engine.ErrUnavailable) || errors.Is(err, engine.ErrNotConfigured)), nil
		}
		return reply(200, out), nil
	}
	rows, err := p.batch.LibraryCandidates(ctx)
	if err != nil {
		return p.failure(err, true), nil
	}
	return reply(200, map[string]any{"status": "ok", "items": rows, "fetched": p.host.Now().UTC(), "secondary_label": p.str("secondary_label")}), nil
}

// apiExecute answers 200 queued, 500 failed (the sidecar ran and said no) or
// 502 unknown (inspect the sidecar before anything else). Refusals before
// submission keep the preview's codes: 409 means review again.
func (p *Integration) apiExecute(ctx context.Context, actor string, in engine.ExecuteRequest) plugin.APIResponse {
	plan, outcome, err := p.submit(ctx, actor, in)
	if err != nil {
		return p.failure(err, false)
	}
	switch outcome {
	case "queued":
		return reply(200, map[string]any{"status": "queued", "hash": in.Hash, "queued_files": len(plan.Paths)})
	case "failed":
		return reply(500, map[string]any{"status": "failed", "hash": in.Hash, "error": "sidecar reported failure; inspect it before another batch"})
	}
	return reply(502, map[string]any{"status": "unknown", "hash": in.Hash, "error": "outcome unknown; inspect the sidecar and Tdarr before another batch"})
}

// submit rechecks a reviewed plan, has the sidecar validate the exact files,
// writes a once-only receipt and forwards the batch. An error means nothing was
// sent; after that the outcome is queued, failed or unknown and never retried.
func (p *Integration) submit(ctx context.Context, actor string, in engine.ExecuteRequest) (engine.Plan, string, error) {
	check, cancel := context.WithTimeout(ctx, 20*time.Second)
	plan, err := p.batch.Recheck(check, actor, in)
	cancel()
	if err != nil {
		return plan, "", err
	}
	var validated struct {
		Paths []string `json:"paths"`
	}
	check, cancel = context.WithTimeout(ctx, 30*time.Second)
	err = p.read(check, "sidecar", "POST", "/validate", map[string]any{"paths": plan.Paths}, &validated)
	cancel()
	if err != nil {
		return plan, "", errors.New("sidecar validation unavailable; batch refused")
	}
	sort.Strings(validated.Paths)
	sort.Strings(plan.Paths)
	if len(validated.Paths) != len(plan.Paths) {
		return plan, "", engine.ErrConflict
	}
	for i, file := range validated.Paths {
		if file != plan.Paths[i] || p.blocked(engine.LibraryTitle{Path: file}) {
			return plan, "", engine.ErrBlocked
		}
	}
	receipt := []byte(p.host.Now().UTC().Format(time.RFC3339) + "\n" + in.Hash + "\n")
	if err = fsx.CreateAtomic(filepath.Join(p.host.CacheDir(), "submission-"+in.Hash+".receipt"), receipt, 0600); err != nil {
		return plan, "", errors.New("batch already submitted or receipt unavailable; inspect sidecar before another review")
	}
	var out struct {
		ExitCode *int   `json:"exit_code"`
		OK       *bool  `json:"ok"`
		Error    string `json:"error"`
	}
	if p.read(ctx, "sidecar", "POST", "/shrink", map[string]any{"paths": plan.Paths, "withPc": plan.WithPC}, &out) != nil {
		return plan, "unknown", nil
	}
	switch {
	case out.Error != "" || out.ExitCode != nil && *out.ExitCode != 0 || out.OK != nil && !*out.OK:
		return plan, "failed", nil
	case out.ExitCode != nil || out.OK != nil:
		return plan, "queued", nil
	}
	return plan, "unknown", nil
}

// failure keeps the private API's codes; reads answer 200 with a status so the
// page can tell "not configured" from "unavailable".
func (p *Integration) failure(err error, read bool) plugin.APIResponse {
	code, status, message := 400, "unavailable", p.safe(err.Error())
	switch {
	case errors.Is(err, engine.ErrNotConfigured):
		code, status, message = 503, "not_configured", "batch source not configured"
	case errors.Is(err, engine.ErrUnavailable):
		code = 503
	case errors.Is(err, engine.ErrConflict):
		code = 409
	case errors.Is(err, engine.ErrBlocked):
		code = 403
	}
	if read {
		code = 200
	}
	return reply(code, map[string]string{"status": status, "error": message})
}

func reply(code int, v any) plugin.APIResponse {
	b, _ := json.Marshal(v)
	return plugin.APIResponse{Status: code, Body: b}
}

func decode(body []byte, out any) bool {
	if len(body) > 64<<10 {
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	return d.Decode(out) == nil && d.Decode(new(any)) == io.EOF
}

// shrink accepts direct paths; registry serialization also excludes reviewed batches.
func (p *Integration) shrink(ctx context.Context, body []byte) plugin.APIResponse {
	if p.str("sidecar_url") == "" || p.str("sidecar_key") == "" {
		return reply(503, map[string]string{"error": "tdarr shrink sidecar is not configured"})
	}
	var in struct {
		Paths  []string `json:"paths"`
		WithPC bool     `json:"withPc"`
	}
	if !decode(body, &in) {
		return reply(400, map[string]string{"error": "invalid request"})
	}
	for _, file := range in.Paths {
		if p.blocked(engine.LibraryTitle{Path: file}) {
			return p.failure(engine.ErrBlocked, false)
		}
	}
	if len(in.Paths) == 0 {
		return reply(400, map[string]string{"error": "paths is required"})
	}
	if len(in.Paths) > 16 {
		return reply(400, map[string]string{"error": "maximum 16 paths"})
	}
	check, cancel := context.WithTimeout(ctx, 30*time.Second)
	status, raw, err := p.sidecar(check, "/validate", map[string]any{"paths": in.Paths})
	cancel()
	if err != nil || status != 200 {
		return p.failure(engine.ErrUnavailable, false)
	}
	var validated struct {
		Paths []string `json:"paths"`
	}
	if len(raw) > 64<<10 || json.Unmarshal(raw, &validated) != nil || len(validated.Paths) == 0 || len(validated.Paths) > 16 {
		return p.failure(engine.ErrUnavailable, false)
	}
	seen := map[string]bool{}
	for _, file := range validated.Paths {
		if p.blocked(engine.LibraryTitle{Path: file}) || p.blockedValidatedPath(file) {
			return p.failure(engine.ErrBlocked, false)
		}
		if seen[file] || !p.validatedPath(file) {
			return p.failure(engine.ErrUnavailable, false)
		}
		seen[file] = true
	}
	status, raw, err = p.sidecar(ctx, "/shrink", map[string]any{"paths": validated.Paths, "withPc": in.WithPC})
	if err != nil {
		return reply(502, map[string]string{"error": "sidecar unavailable; outcome may be unknown; inspect before retrying"})
	}
	return plugin.APIResponse{Status: status, Body: raw}
}

func (p *Integration) validatedPath(file string) bool {
	return validMediaPath(file, p.str("tdarr_movie_root")) || validMediaPath(file, p.str("tdarr_series_root"))
}

func (p *Integration) blockedValidatedPath(file string) bool {
	for _, roots := range [][2]string{{"radarr_root", "tdarr_movie_root"}, {"sonarr_root", "tdarr_series_root"}} {
		source, target := p.str(roots[0]), p.str(roots[1])
		if source != "" && target != "" && strings.HasPrefix(file, target+"/") && p.blocked(engine.LibraryTitle{Path: source + "/" + strings.TrimPrefix(file, target+"/")}) {
			return true
		}
	}
	return false
}
