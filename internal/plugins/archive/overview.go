package archive

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/ficaa1/homelab-inventory/internal/overview"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type overviewJob struct {
	Phase  string `json:"phase"`
	Target struct {
		Title string `json:"title"`
	} `json:"target"`
	Progress *struct {
		BytesTotal int64 `json:"bytesTotal"`
		BytesLeft  int64 `json:"bytesLeft"`
	} `json:"progress"`
}

func (p *Integration) Overview(ctx context.Context) map[string]any {
	unavailable := func(reason string) map[string]any {
		return map[string]any{"archive": overview.Unavailable{Status: "unavailable", Error: reason}}
	}
	code, raw, err := p.call(ctx, "GET", "/status?refresh=progress", nil)
	if err != nil || code != 200 {
		return unavailable("archive status unavailable")
	}
	var in struct {
		Archive *struct {
			Error      string            `json:"error"`
			Paused     string            `json:"paused"`
			Approvals  []json.RawMessage `json:"approvals"`
			Job        *overviewJob      `json:"job"`
			FailedJobs []*overviewJob    `json:"failedJobs"`
		} `json:"archive"`
	}
	if json.Unmarshal(raw, &in) != nil || in.Archive == nil {
		return unavailable("invalid archive status")
	}
	st := in.Archive
	out := overview.Archive{Status: "ok", PendingApprovals: len(st.Approvals)}
	phase := "idle"
	j := st.Job
	if (j == nil || j.Phase == "complete" || j.Phase == "abandoned") && len(st.FailedJobs) > 0 {
		j = st.FailedJobs[0]
	}
	if j != nil {
		switch j.Phase {
		case "grab_intent", "downloading":
			phase = "downloading"
		case "import_intent":
			phase = "importing"
		case "verifying":
			phase = "verifying"
		case "failed":
			phase = "failed"
		case "complete", "abandoned":
		default:
			return unavailable("unknown archive phase")
		}
		title := j.Target.Title
		if p.token != "" {
			title = strings.ReplaceAll(title, p.token, "[redacted]")
		}
		out.Title = &title
		if j.Progress != nil && j.Progress.BytesTotal > 0 {
			pct := 100 * (1 - float64(j.Progress.BytesLeft)/float64(j.Progress.BytesTotal))
			if pct >= 0 && pct <= 100 {
				out.ProgressPct = &pct
			}
		}
	}
	if st.Error != "" {
		phase = "failed"
	}
	if phase == "idle" {
		out.Title = nil
		out.ProgressPct = nil
	}
	if st.Paused != "" && phase != "failed" {
		phase = "paused"
	}
	out.Phase = &phase
	// Bounded job history cannot establish lifetime savings; retain null.
	return map[string]any{"archive": out}
}

var _ plugin.OverviewSource = (*Integration)(nil)
