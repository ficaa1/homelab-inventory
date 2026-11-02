package archivetier

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SelectionOverrides layers individual fields over the file policy. Nil means
// inherit, while pointers to zero or an empty group list are explicit values.
type SelectionOverrides struct {
	Sizes         map[string]SizeOverrides `json:"sizes,omitempty"`
	CodecFactors  map[string]*float64      `json:"codecFactors,omitempty"`
	TrustedGroups *[]string                `json:"trustedGroups,omitempty"`
	DeniedGroups  *[]string                `json:"deniedGroups,omitempty"`
	MinScore      *float64                 `json:"minScore,omitempty"`
}
type SizeOverrides struct {
	Floor  *float64 `json:"floor,omitempty"`
	Target *float64 `json:"target,omitempty"`
	Cap    *float64 `json:"cap,omitempty"`
}

func (o SelectionOverrides) Apply(base Selection) (Selection, error) {
	out := base
	out.Sizes = make(map[string]SizeBand, len(base.Sizes))
	for k, v := range base.Sizes {
		out.Sizes[k] = v
	}
	out.CodecFactors = make(map[string]float64, len(base.CodecFactors))
	for k, v := range base.CodecFactors {
		out.CodecFactors[k] = v
	}
	out.TrustedGroups = append([]string{}, base.TrustedGroups...)
	out.DeniedGroups = append([]string{}, base.DeniedGroups...)
	for k, v := range o.Sizes {
		if k != Mode1080p && k != Mode2160p {
			return Selection{}, fmt.Errorf("invalid size mode")
		}
		b := out.Sizes[k]
		if v.Floor != nil {
			b.Floor = *v.Floor
		}
		if v.Target != nil {
			b.Target = *v.Target
		}
		if v.Cap != nil {
			b.Cap = *v.Cap
		}
		out.Sizes[k] = b
	}
	for k, v := range o.CodecFactors {
		if k != "hevc" && k != "avc" && k != "av1" {
			return Selection{}, fmt.Errorf("invalid codec")
		}
		if v != nil {
			out.CodecFactors[k] = *v
		}
	}
	if o.TrustedGroups != nil {
		out.TrustedGroups = append([]string{}, (*o.TrustedGroups)...)
	}
	if o.DeniedGroups != nil {
		out.DeniedGroups = append([]string{}, (*o.DeniedGroups)...)
	}
	if o.MinScore != nil {
		out.MinScore = *o.MinScore
	}
	var err error
	out.TrustedGroups, out.DeniedGroups, err = normalizeGroups(out.TrustedGroups, out.DeniedGroups)
	if err != nil {
		return Selection{}, err
	}
	return out, out.Validate()
}

// Group identity follows the same case-insensitive matching as releases.
// Limit the supplied lists before deduplication so repeated input cannot bypass
// the entry limit; reject controls before trimming, including edge newlines.
func normalizeGroups(trusted, denied []string) ([]string, []string, error) {
	normalize := func(groups []string) ([]string, error) {
		if len(groups) > 50 {
			return nil, fmt.Errorf("at most 50 groups per list")
		}
		out := []string{}
		for _, group := range groups {
			if !utf8.ValidString(group) || strings.ContainsFunc(group, func(r rune) bool {
				return unicode.IsControl(r) || r == '\u2028' || r == '\u2029'
			}) {
				return nil, fmt.Errorf("group names cannot contain controls or newlines")
			}
			group = strings.TrimSpace(group)
			if group == "" || utf8.RuneCountInString(group) > 64 {
				return nil, fmt.Errorf("group names need 1..64 characters")
			}
			if !groupIn(group, out) {
				out = append(out, group)
			}
		}
		return out, nil
	}
	t, err := normalize(trusted)
	if err != nil {
		return nil, nil, err
	}
	d, err := normalize(denied)
	if err != nil {
		return nil, nil, err
	}
	for _, group := range t {
		if groupIn(group, d) {
			return nil, nil, fmt.Errorf("group cannot be both trusted and denied")
		}
	}
	return t, d, nil
}

type SelectionPreview struct {
	Titles           []SelectionChange `json:"titles"`
	AutoPicks        int               `json:"autoPicks"`
	CurrentAutoPicks int               `json:"currentAutoPicks"`
}
type SelectionChange struct {
	Target   Target     `json:"target"`
	Current  *Candidate `json:"current"`
	Proposed *Candidate `json:"proposed"`
}

// RescoreCached is pure: it never reads an Arr service, searches, or starts a
// job. It preserves cooldowns, rate counters, approvals and saved job decisions.
// AutoPicks counts cached selection winners, not jobs admitted by runtime guards.
func RescoreCached(st *State, current, proposed Config) SelectionPreview {
	preview := SelectionPreview{Titles: []SelectionChange{}}
	keys := make([]string, 0, len(st.Lookups))
	for key := range st.Lookups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		l := st.Lookups[key]
		t := l.Decision.Target
		if l.Files != filesKey(t) || len(l.Releases) == 0 {
			continue
		}
		before := selectReleases(t, l.Releases, current, st.BadReleases[key])
		after := selectReleases(t, l.Releases, proposed, st.BadReleases[key])
		pick := func(d Decision) *Candidate {
			if d.Release == nil || d.Score == nil {
				return nil
			}
			return candidate(*d.Release, *d.Score)
		}
		preview.Titles = append(preview.Titles, SelectionChange{Target: t, Current: pick(before), Proposed: pick(after)})
		if before.Release != nil {
			preview.CurrentAutoPicks++
		}
		if after.Release != nil {
			preview.AutoPicks++
		}
		after.SearchedAt = l.At
		l.Decision = sanitized(after)
		st.Lookups[key] = l
		for i, d := range st.Report {
			if targetKey(d.Target) != key || filesKey(d.Target) != l.Files {
				continue
			}
			// Keep a title's eligibility/lease/playback refusal. Only selection reasons
			// can be cleared by changing scoring settings.
			if d.Reason != "" && d.Reason != NoGoodRelease && d.Reason != "no safe replacement" {
				st.Report[i].Candidates = after.Candidates
				st.Report[i].Rejected = after.Rejected
				continue
			}
			after.Skipped = d.Skipped
			st.Report[i] = sanitized(after)
		}
	}
	st.ProjectedSavings = 0
	for _, d := range st.Report {
		st.ProjectedSavings += d.Savings
	}
	return preview
}
