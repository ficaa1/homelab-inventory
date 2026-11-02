package archivetier

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// refused marks an approval the operator must redo; other errors are
// transient (an application not answering) and retried next cycle.
type refused struct{ error }

func refuse(format string, a ...any) error { return refused{fmt.Errorf(format, a...)} }

// IsSearchRefused identifies definitive title or policy refusals. Other search
// errors (unavailable reads, leases or rate admission) can retry next cycle.
func IsSearchRefused(err error) bool { return errors.As(err, new(refused)) }

// ApprovalRequest selects a worker-owned cached release, never a caller-supplied release.
type ApprovalRequest struct {
	Kind      string `json:"kind"`
	ID        int    `json:"id"`
	Season    int    `json:"season,omitempty"`
	ReleaseID string `json:"releaseId,omitempty"`
	IndexerID int    `json:"indexerId,omitempty"`
	Clear     bool   `json:"clear,omitempty"`
}

// Approval is one queued operator choice. Blocked approvals stay listed with
// their reason until the operator removes or re-approves them.
type Approval struct {
	ApprovalRequest
	RequestedAt time.Time `json:"requestedAt"`
	Message     string    `json:"message,omitempty"`
	Blocked     bool      `json:"blocked,omitempty"`
}

func (a ApprovalRequest) target() Target { return Target{Kind: a.Kind, ID: a.ID, Season: a.Season} }

// title finds the archive-tagged target the request names.
func (r *Runner) title(ctx context.Context, st State, in ApprovalRequest) (Target, error) {
	t := in.target()
	if st.FailedTitle(t) {
		return t, refuse("title needs recovery")
	}
	a := r.Arr[t.Kind]
	if a == nil || t.ID <= 0 || t.Kind != "sonarr" && t.Kind != "radarr" || t.Kind == "radarr" && t.Season != 0 || t.Kind == "sonarr" && t.Season <= 0 {
		return t, refuse("invalid title identity")
	}
	targets, err := a.Targets(ctx)
	if err != nil {
		return t, err
	}
	for _, target := range targets {
		if targetKey(target) == targetKey(t) {
			if !st.opted(target) {
				return t, refuse("season %d is not selected for archiving; tick it in Library", t.Season)
			}
			return target, nil
		}
	}
	return t, refuse("title is not archive-tagged")
}

// approvalDecision rechecks a queued approval against the cached lookup and
// the current library. Only manualReject and ManualEligible apply.
func (r *Runner) approvalDecision(ctx context.Context, now time.Time, st *State, in Approval) (Decision, error) {
	l, ok := st.Lookups[targetKey(in.target())]
	if !ok || l.At.After(now) {
		return Decision{}, refuse("no cached search for this title; search again")
	}
	t, err := r.title(ctx, *st, in.ApprovalRequest)
	if err != nil {
		return Decision{}, err
	}
	t.PlexKey, t.PlexReason = l.Decision.Target.PlexKey, l.Decision.Target.PlexReason
	if why := t.ManualEligible(r.Config, now); why != "" {
		return Decision{}, refuse("title cannot be replaced: %s", why)
	}
	if l.Files != filesKey(t) || !SameFiles(l.Decision.Target, t) {
		return Decision{}, refuse("library or mode changed since the search; search again")
	}
	for _, rel := range l.Releases {
		if releaseID(rel) != in.ReleaseID || rel.IndexerID != in.IndexerID {
			continue
		}
		if why := manualReject(rel, t, r.Config); why != "" {
			return Decision{}, refuse("release refused: %s", why)
		}
		sc := score(rel, t, r.Config)
		return Decision{Target: t, Release: &rel, Score: &sc, Mode: t.Mode(), Savings: t.Bytes() - rel.Size, ManuallyApproved: true, Note: l.Decision.Note}, nil
	}
	return Decision{}, refuse("Release no longer listed")
}

// Approve queues one approval, replacing an earlier one for the same title,
// or removes it with Clear. Jobs run one at a time in queue order.
func (r *Runner) Approve(ctx context.Context, now time.Time, in ApprovalRequest) (State, error) {
	st, err := r.Store.Load()
	if err != nil {
		return st, err
	}
	if err = r.Store.WriteAllowed(); err != nil {
		return st, err
	}
	i := st.approvalIndex(in.target())
	if in.Clear {
		if i < 0 {
			return st, fmt.Errorf("no pending approval for this title")
		}
		in.Clear = false
		if err = r.Store.AuditDetail("approval_cleared", nil, in); err != nil {
			return st, err
		}
		st.Approvals = append(st.Approvals[:i], st.Approvals[i+1:]...)
		return st, r.Store.Save(st)
	}
	if r.DryRun || r.NoSearch {
		return st, fmt.Errorf("Approvals are unavailable")
	}
	if st.Job != nil && st.Job.Phase != Complete && st.Job.Phase != Abandoned && st.Job.Phase != Failed && targetKey(st.Job.Target) == targetKey(in.target()) {
		return st, fmt.Errorf("this title is already being replaced")
	}
	ap := Approval{ApprovalRequest: in, RequestedAt: now}
	if _, err = r.approvalDecision(ctx, now, &st, ap); err != nil {
		return st, err
	}
	if err = r.Store.AuditDetail("approval_requested", nil, in); err != nil {
		return st, err
	}
	if i >= 0 {
		st.Approvals[i] = ap
	} else {
		st.Approvals = append(st.Approvals, ap)
	}
	return st, r.Store.Save(st)
}

func (st State) approvalIndex(t Target) int {
	for i, a := range st.Approvals {
		if targetKey(a.target()) == targetKey(t) {
			return i
		}
	}
	return -1
}

// nextApproval starts the first runnable approval. It reports false when
// none is runnable, so the nightly scan can proceed.
func (r *Runner) nextApproval(ctx context.Context, now time.Time, st *State) (bool, error) {
	for i := range st.Approvals {
		ap := &st.Approvals[i]
		if ap.Blocked {
			continue
		}
		d, err := r.approvalDecision(ctx, now, st, *ap)
		if errors.As(err, new(refused)) {
			ap.Blocked, ap.Message = true, err.Error()
			continue
		}
		if err != nil {
			ap.Message = err.Error()
			return true, err
		}
		// Approvals run outside the window, so streams and host load still hold them.
		if _, err = r.guards(ctx, now); err != nil {
			ap.Message, st.Paused = err.Error(), err.Error()
			return true, nil
		}
		if err = r.Arr[d.Target.Kind].Preflight(ctx); err != nil {
			return true, err
		}
		key := targetKey(d.Target)
		err = r.start(ctx, now, st, d)
		if j := st.approvalIndex(d.Target); j >= 0 && (st.Job == nil || targetKey(st.Job.Target) != key || (st.Job.Phase == Complete || st.Job.Phase == Abandoned)) {
			st.Approvals[j].Message = st.Paused
			if err != nil {
				st.Approvals[j].Message = err.Error()
			}
		}
		return true, err
	}
	return false, nil
}

var ErrSearchRateLimit = errors.New("Tracker rate limit reached")

// SearchTitle runs one release search for an archive-tagged title now,
// outside the window and the automatic eligibility rules, under the
// tracker rate limit. The result replaces the title's cached lookup and
// report entry so its releases can be approved.
func (r *Runner) SearchTitle(ctx context.Context, now time.Time, in ApprovalRequest) (State, error) {
	return r.searchTitle(ctx, now, in, false)
}

// SearchTitleCached is the queued lookup path. Fresh file-matching results
// are reused without spending another tracker request.
func (r *Runner) SearchTitleCached(ctx context.Context, now time.Time, in ApprovalRequest) (State, error) {
	return r.searchTitle(ctx, now, in, true)
}

func (r *Runner) searchTitle(ctx context.Context, now time.Time, in ApprovalRequest, cached bool) (State, error) {
	st, err := r.Store.Load()
	if err != nil {
		return st, err
	}
	if r.NoSearch {
		return st, refuse("release searches are disabled")
	}
	t, err := r.title(ctx, st, in)
	if err != nil {
		return st, err
	}
	if why := t.ManualEligible(r.Config, now); why != "" {
		return st, refuse("title cannot be replaced: %s", why)
	}
	release, err := r.acquire(t)
	if err != nil {
		return st, err
	}
	defer release()
	var rs []Release
	searchedAt := now
	l, fresh := st.Lookups[targetKey(t)]
	fresh = cached && fresh && l.Files == filesKey(t) && now.Sub(l.At) < time.Duration(r.Config.SearchCooldownHours)*time.Hour
	// Legacy decisions can describe releases without retaining their identities.
	// Only reuse those we can rescore, or a genuinely empty selection result.
	fresh = fresh && (len(l.Releases) > 0 || l.Decision.Reason == "no safe replacement" && len(l.Decision.Candidates) == 0)
	if fresh {
		t.PlexKey, t.PlexReason = l.Decision.Target.PlexKey, l.Decision.Target.PlexReason
		rs, searchedAt = l.Releases, l.At
	} else {
		resolved := []Target{t}
		r.resolvePlex(ctx, resolved, &st, now)
		t = resolved[0]
		if err := r.Store.AuditDetail("search_requested", nil, in); err != nil {
			return st, err
		}
		_, night := r.Config.Window(now)
		ok, err := r.lookup(&st, night, now)
		if err != nil {
			return st, err
		}
		if !ok {
			return st, ErrSearchRateLimit
		}
		rs, err = r.Arr[t.Kind].Search(ctx, t)
		if err != nil {
			return st, err
		}
	}
	d := selectReleases(t, rs, r.Config, st.BadReleases[targetKey(t)])
	d.SearchedAt = searchedAt
	if why := t.Eligible(r.Config, now); why != "" && d.Release != nil {
		d.Release, d.Score, d.Savings, d.Reason = nil, nil, 0, why
	}
	if st.Lookups == nil {
		st.Lookups = map[string]Lookup{}
	}
	st.Lookups[targetKey(t)] = cachedLookup(t, d, rs, searchedAt)
	found := false
	for i, prior := range st.Report {
		if targetKey(prior.Target) == targetKey(t) {
			st.Report[i], found = sanitized(d), true
		}
	}
	if !found {
		st.Report = append(st.Report, sanitized(d))
	}
	if i := st.approvalIndex(t); i >= 0 && st.Approvals[i].Blocked {
		st.Approvals[i].Message = "searched again; approve a release from the new results"
	}
	return st, r.Store.Save(st)
}

// SetSeasons records which seasons of a tagged series may be archived; nil
// clears the choice (every season, or untagged).
func (r *Runner) SetSeasons(key string, seasons []int) error {
	st, err := r.Store.Load()
	if err != nil {
		return err
	}
	if seasons == nil {
		delete(st.Seasons, key)
	} else {
		if st.Seasons == nil {
			st.Seasons = map[string][]int{}
		}
		st.Seasons[key] = seasons
	}
	return r.Store.Save(st)
}
