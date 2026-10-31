// Package tdarrbatch joins the Archive Library to exact Arr files and existing
// Tdarr eligibility rules. It never queues files or changes archive state.
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrConflict = errors.New("preview expired, reused, or changed; preview again")
var ErrBlocked = errors.New("title excluded by configured protection policy")
var ErrUnavailable = errors.New("batch sources unavailable")

type Selection struct {
	Kind    string `json:"kind"`
	ID      int    `json:"id"`
	Seasons []int  `json:"seasons,omitempty"`
}
type File struct {
	Kind    string  `json:"kind"`
	TitleID int     `json:"title_id"`
	ID      int     `json:"file_id"`
	Season  int     `json:"season"`
	Path    string  `json:"path"`
	Size    int64   `json:"size_bytes"`
	Codec   string  `json:"codec,omitempty"`
	Class   string  `json:"class,omitempty"`
	Saving  float64 `json:"est_save_gb"`
	Skip    string  `json:"skip_reason,omitempty"`
}
type Plan struct {
	Selection     []Selection `json:"selection"`
	Files         []File      `json:"files"`
	Paths         []string    `json:"paths"`
	TotalBytes    int64       `json:"total_bytes"`
	SelectedBytes int64       `json:"selected_bytes"`
	WithPC        bool        `json:"withPc"`
}
type Preview struct {
	Status  string    `json:"status"`
	Plan    Plan      `json:"plan"`
	Hash    string    `json:"hash"`
	Token   string    `json:"confirmation_token,omitempty"`
	Expires time.Time `json:"expires_at,omitempty"`
	Error   string    `json:"error,omitempty"`
}
type Request struct {
	Selection []Selection `json:"selection"`
	WithPC    bool        `json:"withPc"`
}
type ExecuteRequest struct {
	Plan    Plan   `json:"plan"`
	Hash    string `json:"hash"`
	Token   string `json:"confirmation_token"`
	Confirm bool   `json:"confirm"`
}
type Inventory struct {
	Items  []LibraryItem
	Leases map[string]string
}
type confirmation struct {
	Hash, Actor string
	Expires     time.Time
}
type Engine struct {
	MapPath    func(string, string) (string, error)
	Blocked    func(LibraryTitle) bool
	Inventory  func(context.Context, bool) (Inventory, error)
	Resolve    func(context.Context, Selection) (LibraryTitle, []LibraryFile, error)
	Candidates func(context.Context) ([]ShrinkCandidate, error)
	Status     func(context.Context) TdarrStatus
	Idle       func(context.Context) error
	Now        func() time.Time
	mu         sync.Mutex
	tokens     map[string]confirmation
}

// LibraryCandidates retains the Archive inventory shape and ordering.
type LibraryCandidate struct {
	Entry         LibraryItem `json:"entry"`
	BlockedReason string      `json:"blocked_reason,omitempty"`
}

func (e *Engine) LibraryCandidates(ctx context.Context) ([]LibraryCandidate, error) {
	inv, err := e.Inventory(ctx, true)
	if err != nil {
		return nil, err
	}
	sort.Slice(inv.Items, func(i, j int) bool {
		if inv.Items[i].SizeBytes == inv.Items[j].SizeBytes {
			return inv.Items[i].Title < inv.Items[j].Title
		}
		return inv.Items[i].SizeBytes > inv.Items[j].SizeBytes
	})
	rows := []LibraryCandidate{}
	for _, item := range inv.Items {
		reason := ""
		if e.Blocked(LibraryTitle{Kind: item.Kind, ID: item.ID, Title: item.Title, Path: item.Path}) {
			reason = ErrBlocked.Error()
		}
		rows = append(rows, LibraryCandidate{item, reason})
	}
	return rows, nil
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func canonical(in []Selection) ([]Selection, error) {
	if len(in) == 0 || len(in) > 16 {
		return nil, errors.New("select 1 to 16 Library entries")
	}
	cp := append([]Selection(nil), in...)
	for i := range cp {
		s := &cp[i]
		if s.ID <= 0 || (s.Kind != "radarr" && s.Kind != "sonarr") || (s.Kind == "radarr" && len(s.Seasons) > 0) || len(s.Seasons) > 100 {
			return nil, errors.New("invalid selection")
		}
		s.Seasons = append([]int(nil), s.Seasons...)
		sort.Ints(s.Seasons)
		for j, n := range s.Seasons {
			if n < 0 || (j > 0 && n == s.Seasons[j-1]) {
				return nil, errors.New("invalid seasons")
			}
		}
	}
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].Kind == cp[j].Kind {
			return cp[i].ID < cp[j].ID
		}
		return cp[i].Kind < cp[j].Kind
	})
	for i := 1; i < len(cp); i++ {
		if cp[i].Kind == cp[i-1].Kind && cp[i].ID == cp[i-1].ID {
			return nil, errors.New("duplicate title")
		}
	}
	return cp, nil
}
func archiveBusy(s LibraryStatus) bool {
	return s.Status == "active" || s.Status == "failed" || s.Status == "pending_approval"
}
func Hash(p Plan) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Build reuses the same Library rows and shrink candidates; files not eligible
// remain visible with a reason but are never added to Paths.
func (e *Engine) Build(ctx context.Context, r Request) (Preview, error) {
	p := Preview{Status: "ok", Plan: Plan{Files: []File{}, Paths: []string{}, WithPC: r.WithPC}}
	sel, err := canonical(r.Selection)
	if err != nil {
		return p, err
	}
	p.Plan.Selection = sel
	inv, err := e.Inventory(ctx, true)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return p, err
		}
		return p, ErrUnavailable
	}
	candidates, err := e.Candidates(ctx)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return p, err
		}
		return p, fmt.Errorf("%w: Tdarr candidates", ErrUnavailable)
	}
	byPath := map[string]ShrinkCandidate{}
	for _, c := range candidates {
		byPath[c.Path] = c
	}
	seen := map[string]bool{}
	for _, s := range sel {
		var item *LibraryItem
		for i := range inv.Items {
			if inv.Items[i].Kind == s.Kind && inv.Items[i].ID == s.ID {
				item = &inv.Items[i]
				break
			}
		}
		if item == nil {
			return p, errors.New("selected entry missing from Library")
		}
		title, files, err := e.Resolve(ctx, s)
		if err != nil {
			if errors.Is(err, ErrNotConfigured) {
				return p, err
			}
			return p, fmt.Errorf("%w: Arr files", ErrUnavailable)
		}
		if title.ID != s.ID || title.Path != item.Path {
			return p, errors.New("Library and Arr identity differ")
		}
		if (e.Blocked(title) || e.Blocked(LibraryTitle{Kind: item.Kind, ID: item.ID, Title: item.Title, Path: item.Path})) {
			return p, ErrBlocked
		}
		matched := map[int]bool{}
		for _, f := range files {
			if s.Kind == "sonarr" && len(s.Seasons) > 0 {
				found := false
				for _, n := range s.Seasons {
					if n == f.Season {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			matched[f.Season] = true
			target, mapErr := e.MapPath(f.Path, s.Kind)
			out := File{Kind: s.Kind, TitleID: s.ID, ID: f.ID, Size: f.Size, Season: f.Season, Path: target, Codec: f.MediaInfo.VideoCodec}
			if mapErr != nil {
				out.Path = f.Path
				out.Skip = mapErr.Error()
			}
			if e.Blocked(LibraryTitle{Kind: s.Kind, Path: f.Path}) {
				return p, ErrBlocked
			}
			if f.ID <= 0 || f.Size <= 0 || !strings.HasPrefix(f.Path, strings.TrimRight(title.Path, "/")+"/") {
				out.Skip = "file outside title or missing identity/size"
			}
			if seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			c, eligible := byPath[target]
			if c.Codec != "" {
				out.Codec = c.Codec
			}
			out.Class = c.Kind
			out.Saving = c.EstSaveGB
			if eligible && (strings.EqualFold(out.Codec, "hevc") || strings.EqualFold(out.Codec, "h265")) && c.Kind != "bloat" && out.Skip == "" {
				out.Skip = "already HEVC and not bloat class"
			}
			if !eligible && (strings.EqualFold(out.Codec, "hevc") || strings.EqualFold(out.Codec, "h265")) && out.Skip == "" {
				out.Skip = "already HEVC and not bloat class (or not indexed by Tdarr)"
			}
			if !eligible && out.Skip == "" {
				out.Skip = "not a Tdarr candidate (already HEVC non-bloat without lossless audio, small, low bitrate, HDR, or unindexed)"
			}
			busy := archiveBusy(item.ArchiveStatus)
			for _, season := range item.Seasons {
				if season.Season == f.Season && archiveBusy(season.ArchiveStatus) {
					busy = true
				}
			}
			if inv.Leases[TitleKey(s.Kind, s.ID)] != "" || busy {
				out.Skip = "title currently archived, approved, or leased by mediapolicy"
			}
			if f.Size > 0 {
				p.Plan.SelectedBytes += f.Size
			}
			p.Plan.Files = append(p.Plan.Files, out)
			if out.Skip == "" {
				p.Plan.Paths = append(p.Plan.Paths, target)
				p.Plan.TotalBytes += f.Size
			}
		}
		if files == nil {
			return p, fmt.Errorf("%w: incomplete Arr files", ErrUnavailable)
		}
		if len(files) == 0 {
			return p, errors.New("selected title has no files")
		}
		for _, n := range s.Seasons {
			if !matched[n] {
				return p, errors.New("selected season has no files")
			}
		}
	}
	sort.Slice(p.Plan.Files, func(i, j int) bool { return p.Plan.Files[i].Path < p.Plan.Files[j].Path })
	sort.Strings(p.Plan.Paths)
	p.Hash = Hash(p.Plan)
	if raw, _ := json.Marshal(p.Plan); len(raw) > 60<<10 {
		p.Error = "preview too large; select fewer seasons"
	}
	if len(p.Plan.Paths) > 16 {
		p.Error = "sidecar limit is 16 eligible files; select a smaller batch"
	}
	return p, nil
}
func (e *Engine) Preview(ctx context.Context, actor string, r Request) (Preview, error) {
	p, err := e.Build(ctx, r)
	if err != nil {
		return p, err
	}
	if len(p.Plan.Paths) == 0 || len(p.Plan.Paths) > 16 || p.Error != "" {
		return p, nil
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return p, err
	}
	p.Token = hex.EncodeToString(random[:])
	p.Expires = e.now().Add(2 * time.Minute)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tokens == nil {
		e.tokens = map[string]confirmation{}
	}
	for k, v := range e.tokens {
		if !e.now().Before(v.Expires) {
			delete(e.tokens, k)
		}
	}
	if len(e.tokens) >= 256 {
		return p, errors.New("too many pending previews")
	}
	e.tokens[p.Token] = confirmation{p.Hash, actor, p.Expires}
	return p, nil
}

// Recheck consumes the preview once, even on failure. The caller serializes
// recheck through sidecar completion and durably audits before forwarding.
func (e *Engine) Recheck(ctx context.Context, actor string, r ExecuteRequest) (Plan, error) {
	e.mu.Lock()
	c, ok := e.tokens[r.Token]
	delete(e.tokens, r.Token)
	e.mu.Unlock()
	if !r.Confirm || !ok || !e.now().Before(c.Expires) || c.Actor != actor || r.Hash != c.Hash || Hash(r.Plan) != c.Hash {
		return Plan{}, ErrConflict
	}
	p, err := e.Build(ctx, Request{r.Plan.Selection, r.Plan.WithPC})
	if err != nil {
		return Plan{}, err
	}
	if p.Hash != c.Hash || len(p.Plan.Paths) == 0 || len(p.Plan.Paths) > 16 {
		return Plan{}, ErrConflict
	}
	if e.Idle != nil {
		if err := e.Idle(ctx); err != nil {
			return Plan{}, err
		}
	}
	status := e.Status(ctx)
	if status.Status != "ok" || status.Queue == nil || status.Queue.Queued == nil || status.Queue.Processing == nil {
		return Plan{}, fmt.Errorf("%w: Tdarr queue", ErrUnavailable)
	}
	if *status.Queue.Queued != 0 || *status.Queue.Processing != 0 {
		return Plan{}, errors.New("Tdarr already has queued or active work")
	}
	return p.Plan, nil
}
