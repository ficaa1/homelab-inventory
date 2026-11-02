package archivetier

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// RemoveArchiveTag uses the editor to remove only archive, preserving all other tags.
func (c *Client) RemoveArchiveTag(ctx context.Context, t Target) error {
	id, err := c.archiveTag(ctx)
	if err != nil || id == 0 {
		return err
	}
	if err := c.Preflight(ctx); err != nil {
		return err
	}
	p, ids := "/movie/editor", "movieIds"
	if c.Kind == "sonarr" {
		p, ids = "/series/editor", "seriesIds"
	}
	return c.call(ctx, http.MethodPut, p, nil, map[string]any{ids: []int{t.ID}, "tags": []int{id}, "applyTags": "remove"}, nil)
}

// retryTagRemovals runs only with write admission. Failures stay queued, independently
// of the finished job and bounded undo history, so the next cycle can retry.
func (r *Runner) retryTagRemovals(ctx context.Context, now time.Time, st *State, adm Admission) {
	if r.DryRun || r.NoSearch || !adm.NewWrites || r.Store.WriteAllowed() != nil {
		return
	}
	st.TagRemovalError = ""
	for key, t := range st.TagRemovals {
		a := r.Arr[t.Kind]
		remover, ok := a.(interface {
			RemoveArchiveTag(context.Context, Target) error
		})
		if !ok {
			continue
		}
		pending := false
		for _, a := range st.Approvals {
			pending = pending || a.Kind == t.Kind && a.ID == t.ID
		}
		if pending {
			continue
		}
		if st.Job != nil && st.Job.Target.Key() == key && st.Job.Phase != Complete && st.Job.Phase != Abandoned {
			continue
		}
		if t.Kind == "sonarr" {
			targets, err := a.Targets(ctx)
			if err != nil {
				r.tagRemovalFailed(st, t)
				continue
			}
			pending := false
			for _, other := range targets {
				if other.Key() != key || !st.opted(other) {
					continue
				}
				done := st.CompletedFiles[targetKey(other)] == filesKey(other)
				// Playback and Plex availability must not make an otherwise eligible season disappear.
				other.PlexKey = "tag-cleanup"
				if !done && other.Eligible(r.Config, now) == "" {
					pending = true
					break
				}
			}
			if pending {
				continue
			}
		}
		if err := remover.RemoveArchiveTag(ctx, t); err != nil {
			r.tagRemovalFailed(st, t)
			continue
		}
		delete(st.TagRemovals, key)
		delete(st.Seasons, key)
	}
}
func (r *Runner) tagRemovalFailed(st *State, t Target) {
	st.TagRemovalError = "Tag removal needs attention"
	if r.LogSummary != nil {
		r.LogSummary(st.TagRemovalError)
	}
}

// TagCreateAllowed permits exactly one lowercase archive or 4k label field.
// Token reads reject duplicate keys, extra fields and trailing JSON.
func TagCreateAllowed(body []byte) bool {
	d := json.NewDecoder(bytes.NewReader(body))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return false
	}
	key, err := d.Token()
	if err != nil || key != "label" {
		return false
	}
	var label string
	if d.Decode(&label) != nil || (label != "archive" && label != FourKTag) {
		return false
	}
	end, err := d.Token()
	return err == nil && end == json.Delim('}') && d.Decode(new(any)) == io.EOF
}
