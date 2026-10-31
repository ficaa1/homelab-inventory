package plugin

import (
	"context"
	"encoding/json"
	"time"
)

// Overview merges redacted plugin contributions under one three-second budget.
// Busy plugins are omitted instead of queueing behind a long page/action call.
// The short cache preserves fast Home refreshes; settings updates invalidate it.
func (r *Registry) Overview(ctx context.Context) map[string]any {
	out := map[string]any{}
	if r == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	type result struct {
		id     string
		values map[string]any
	}
	ids := r.ids()
	results := make(chan result, len(ids))
	pending := 0
	for _, id := range ids {
		e := r.entries[id]
		src, ok := e.plugin.(OverviewSource)
		if !ok || !e.op.TryLock() {
			continue
		}
		if !e.stored.Enabled || !e.ready {
			e.op.Unlock()
			continue
		}
		pending++
		go func(id string, e *entry, src OverviewSource) {
			defer e.op.Unlock()
			res := result{id: id}
			defer func() { results <- res }()
			if ctx.Err() != nil {
				return
			}
			now := e.host.Now()
			body := e.overviewBody
			if body == nil || !now.Before(e.overviewExpires) {
				var err error
				body, err = json.Marshal(src.Overview(ctx))
				if err != nil || len(body) > 1<<20 {
					return
				}
				body = e.host.redact(body)
				if ctx.Err() == nil {
					e.overviewBody, e.overviewExpires = body, now.Add(15*time.Second)
				}
			}
			_ = json.Unmarshal(body, &res.values)
		}(id, e, src)
	}
	contributions := map[string]map[string]any{}
	for pending > 0 {
		select {
		case res := <-results:
			contributions[res.id] = res.values
			pending--
		case <-ctx.Done():
			pending = 0
		}
	}
	// Stable registration order resolves duplicate keys without timing races.
	for _, id := range ids {
		for key, value := range contributions[id] {
			if _, exists := out[key]; !exists {
				out[key] = value
			}
		}
	}
	if load, ok := out["plex_load"].(map[string]any); ok {
		if streams, ok := out["streams"].(map[string]any); ok && streams["status"] == "ok" {
			load["transcode_sessions"] = streams["transcode"]
		}
	}
	return out
}
