package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
)

// verdictSignal maps the owner's explicit word on a title onto the taste
// scale used for Letterboxd ratings. "seen" only excludes; "ignore" removes a
// title from the taste entirely (watched for someone else).
var verdictSignal = map[string]float64{"loved": 1.0, "liked": 0.5, "seen": 0, "meh": -0.5, "disliked": -1.0, "pass": -0.3, "ignore": 0}

// Verdict is one explicit judgement from the dashboard's taste picker.
type Verdict struct {
	Verdict string    `json:"verdict"`
	Title   string    `json:"title"`
	Year    string    `json:"year,omitempty"`
	At      time.Time `json:"at"`
}

// Feedback stores verdicts in a private owner-bound file. They override
// whatever Letterboxd or Plex suggested about the same title.
type Feedback struct {
	Path  string
	Owner int
	mu    sync.Mutex
}
type savedFeedback struct {
	Owner  int                `json:"owner"`
	Titles map[string]Verdict `json:"titles"`
}

// Load returns the verdicts; a missing file is none, a damaged or foreign one
// is an error rather than silently forgetting the owner's answers.
func (f *Feedback) Load() (map[string]Verdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, err := f.load()
	return v.Titles, err
}

func (f *Feedback) load() (savedFeedback, error) {
	v := savedFeedback{Owner: f.Owner, Titles: map[string]Verdict{}}
	b, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if len(b) > 4<<20 || json.Unmarshal(b, &v) != nil || v.Owner != f.Owner || v.Titles == nil {
		return v, fmt.Errorf("invalid taste feedback")
	}
	return v, nil
}

// Set records a verdict; an empty verdict removes it.
func (f *Feedback) Set(kind string, id int, verdict, title, year string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := verdictSignal[verdict]; (!ok && verdict != "") || f.Owner <= 0 || (kind != "movie" && kind != "tv") || id <= 0 || id >= 1000000000 {
		return fmt.Errorf("invalid verdict")
	}
	title, year = strings.TrimSpace(title), strings.TrimSpace(year)
	if verdict != "" && (title == "" || len(title) > 200 || len(year) > 4) {
		return fmt.Errorf("invalid verdict")
	}
	v, err := f.load()
	if err != nil {
		return err
	}
	k := key(kind, id)
	if verdict == "" {
		delete(v.Titles, k)
	} else {
		if _, ok := v.Titles[k]; !ok && len(v.Titles) >= 20000 {
			return fmt.Errorf("feedback limit")
		}
		v.Titles[k] = Verdict{Verdict: verdict, Title: title, Year: year, At: now.UTC()}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(f.Path, b, 0600)
}
