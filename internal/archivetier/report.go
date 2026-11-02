package archivetier

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func targetLabel(t Target) string {
	if t.Kind == "sonarr" {
		return fmt.Sprintf("%s S%02d", t.Title, t.Season)
	}
	return t.Title
}
func JobSummary(j Job) string {
	tr := ParseTraits(j.Release.Title)
	codec := map[string]string{"hevc": "x265", "avc": "x264", "av1": "AV1"}[tr.Codec]
	bit := ""
	if tr.TenBit {
		bit = " 10-bit"
	}
	score := 0.0
	if j.Score != nil {
		score = j.Score.Total
	}
	group := releaseGroup(j.Release)
	if group == "" {
		group = "unknown group"
	}
	s := fmt.Sprintf("%s replaced: %.1f -> %.1f GB (%.1f GB saved). Picked %s %s %s%s %s, %d seeders, score %.1f", targetLabel(j.Target), float64(j.Target.Bytes())/float64(GB), float64(j.ActualBytes)/float64(GB), float64(j.Target.Bytes()-j.ActualBytes)/float64(GB), group, j.Mode, codec, bit, tr.Audio(), j.Release.Seeders, score)
	if j.RunnerUp != nil {
		g := j.RunnerUp.Group
		if g == "" {
			g = j.RunnerUp.Title
		}
		s += fmt.Sprintf("; runner-up %s %.1f", g, j.RunnerUp.Score.Total)
	}
	s += fmt.Sprintf(". Subtitles carried over: %d (timing unverified)", j.SidecarsCarried)
	if j.SidecarMessage != "" {
		s += "; " + j.SidecarMessage
	}
	if j.SidecarMessage != "" && j.SidecarStage != "" {
		s += "; staged at " + j.SidecarStage
	}
	if j.Note != "" {
		s += ". " + j.Note
	}
	undo := j.ImportedAt
	if undo.IsZero() {
		undo = j.Completed
	}
	if j.ManuallyApproved {
		s += ". Manually approved"
	}
	s += ". Undo until " + undo.AddDate(0, 0, 7).Format("2006-01-02") + "."
	return s
}

type Notification struct {
	Key  string `json:"key"`
	Body string `json:"body"`
}

// queueReports records transitions before sending. Skips are batched once per
// title per nightly report; resumed failures and completions are job-specific.
func (r *Runner) queueReports(st *State, now time.Time) {
	if r.DryRun || r.Notify == nil {
		return
	}
	if st.NotificationSeen == nil {
		st.NotificationSeen = map[string]string{}
	}
	loc, err := time.LoadLocation(r.Config.Timezone)
	if err != nil {
		loc = time.UTC
	}
	day := now.In(loc).Format("2006-01-02")
	add := func(key, body string) {
		if _, ok := st.NotificationSeen[key]; !ok {
			st.NotificationSeen[key] = day
			st.Notifications = append(st.Notifications, Notification{key, body})
		}
	}
	// The last job stays in state after it ends; its keys are never pruned
	// so it is announced once, not again every two weeks.
	jobKey := ""
	if j := st.Job; j != nil {
		id := targetKey(j.Target) + ":" + j.Started.Format(time.RFC3339Nano)
		jobKey = id + ":"
		// A job completed before summaries existed has nothing to say.
		if j.Phase == Abandoned {
			add(id+":abandoned", j.Summary)
		}
		if j.Phase == Complete && j.Summary != "" {
			add(id+":complete", j.Summary)
		}
		if j.Phase == Failed {
			add(id+":failed", targetLabel(j.Target)+" failed: "+j.Message)
		}
	}
	for _, j := range st.FailedJobs {
		add(targetKey(j.Target)+":"+j.Started.Format(time.RFC3339Nano)+":failed", targetLabel(j.Target)+" failed: "+j.Message)
	}

	window, _ := r.window(now)
	for _, d := range st.Report {
		why := d.Skipped
		if why == "" {
			why = d.Reason
		}
		// Only actionable nightly results, not idle daytime polling.
		if why != "" && (!d.SearchedAt.IsZero() || window && st.Reported.Equal(now)) {
			night := st.Night
			if night == "" {
				_, night = r.window(d.SearchedAt)
			}
			body := targetLabel(d.Target) + " skipped: " + why
			if len(d.Candidates) > 0 {
				c := d.Candidates[0]
				body = fmt.Sprintf("%s awaiting approval (%d releases): best %s, %.1f GB, score %.1f", targetLabel(d.Target), len(d.Candidates), c.Title, float64(c.Size)/float64(GB), c.Score.Total)
				if c.Rejection != "" {
					body += " (" + c.Rejection + ")"
				}
			}
			add("skip:"+night+":"+targetKey(d.Target), body)
		}
	}
	// Keep deduplication state bounded; pending message keys remain protected.
	cutoff := now.In(loc).AddDate(0, 0, -14).Format("2006-01-02")
	pending := map[string]bool{}
	for _, n := range st.Notifications {
		pending[n.Key] = true
	}
	for k, v := range st.NotificationSeen {
		if v < cutoff && !pending[k] && (jobKey == "" || !strings.HasPrefix(k, jobKey)) {
			delete(st.NotificationSeen, k)
		}
	}
}
func (r *Runner) sendReports(ctx context.Context, st *State, now time.Time) error {
	if r.Notify == nil || r.DryRun || len(st.Notifications) == 0 {
		return nil
	}
	loc, err := time.LoadLocation(r.Config.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	day := local.Format("2006-01-02")
	if local.Hour() < 9 || local.Hour() >= 21 || st.NotificationDay == day {
		return nil
	}
	// Drop obsolete awaiting-approval lines when approval or completion preceded
	// the morning summary; skip transitions for other reasons stay unchanged.
	pending := st.Notifications[:0]
	for _, n := range st.Notifications {
		if strings.Contains(n.Body, " awaiting approval") {
			waiting := false
			for _, d := range st.Report {
				if strings.HasSuffix(n.Key, ":"+targetKey(d.Target)) && len(d.Candidates) > 0 {
					approved := st.approvalIndex(d.Target) >= 0
					started := st.Job != nil && targetKey(st.Job.Target) == targetKey(d.Target) && SameFiles(st.Job.Target, d.Target)
					waiting = !approved && !started
				}
			}
			if !waiting {
				continue
			}
		}
		pending = append(pending, n)
	}
	st.Notifications = pending
	if len(pending) == 0 {
		return r.Store.Save(*st)
	}
	bodies := make([]string, 0, len(st.Notifications))
	for _, n := range st.Notifications {
		bodies = append(bodies, n.Body)
	}
	if err := r.Notify(ctx, strings.Join(bodies, "\n")); err != nil {
		st.NotificationError = "archive notification delivery failed"
		return r.Store.Save(*st)
	}
	st.NotificationDay = day
	st.Notifications = nil
	st.NotificationError = ""
	return r.Store.Save(*st)
}
