package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type overviewPlugin struct {
	fakePlugin
	id     string
	values map[string]any
	wait   bool
	calls  int
}

func (p *overviewPlugin) Manifest() Manifest {
	m := p.fakePlugin.Manifest()
	m.ID = p.id
	return m
}
func (p *overviewPlugin) Overview(ctx context.Context) map[string]any {
	p.calls++
	if p.wait {
		<-ctx.Done()
		return nil
	}
	return p.values
}

func TestOverviewMergeShapeCacheAndDeadline(t *testing.T) {
	media := &overviewPlugin{id: "media", values: map[string]any{
		"streams": map[string]any{"status": "ok", "count": 1, "direct": 0, "transcode": 1, "bandwidth_kbps": 1200, "sessions": []any{map[string]any{"user": "initial-value", "title": "Example", "progress_pct": nil}}},
		"arr":     map[string]any{"sonarr": map[string]any{"status": "ok", "total": 0, "problems": 0, "attention": 0}},
	}}
	network := &overviewPlugin{id: "network", values: map[string]any{"plex_load": map[string]any{"status": "partial", "label": "Example", "cpu_pct": 24, "gpu": map[string]any{"status": "unavailable", "devices": nil}, "transcode_sessions": nil}}}
	slow := &overviewPlugin{id: "slow", wait: true}
	disabled := &overviewPlugin{id: "disabled", values: map[string]any{"disabled": true}}
	builtins := []Builtin{}
	for _, p := range []*overviewPlugin{media, network, slow, disabled} {
		builtins = append(builtins, Builtin{ID: p.id, New: func(h Host) Plugin { p.host = h; return p }})
	}
	r, err := Open(t.TempDir(), builtins, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	for _, id := range []string{"media", "network", "slow"} {
		if _, err := r.Update(id, enabledUpdate()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := r.Overview(ctx)
	if time.Since(start) > time.Second {
		t.Fatal("slow contribution delayed overview")
	}
	if len(out) != 3 || out["disabled"] != nil {
		t.Fatalf("wrong merged sections: %v", out)
	}
	streams := out["streams"].(map[string]any)
	load := out["plex_load"].(map[string]any)
	if streams["count"] != float64(1) || load["transcode_sessions"] != float64(1) || load["label"] != "Example" {
		t.Fatalf("wrong overview shape: %v", out)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "initial-value") || !strings.Contains(string(b), "[redacted]") {
		t.Fatal("overview bypassed credential redaction")
	}
	off := false
	if _, err := r.Update("slow", Update{Enabled: &off}); err != nil {
		t.Fatal(err)
	}
	r.Overview(context.Background())
	if media.calls != 1 {
		t.Fatal("overview did not cache contribution")
	}
	if _, err := r.Update("media", Update{Settings: Config{"key": "replacement-value"}}); err != nil {
		t.Fatal(err)
	}
	r.Overview(context.Background())
	if media.calls != 2 {
		t.Fatal("settings update did not invalidate overview cache")
	}
}
