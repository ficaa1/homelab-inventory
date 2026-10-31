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
	case "batch/execute":
		return 6 * time.Minute
	case "batch/preview", "batch/candidates", "candidates":
		return 25 * time.Second
	}
	return 0
}

func (p *Integration) API(ctx context.Context, r plugin.APIRequest) (plugin.APIResponse, error) {
	switch r.Method + " " + r.Path {
	case "GET status":
		return reply(200, p.reader.Status(ctx)), nil
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
