package media

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Queue first-observed timestamps survive backup's runtime reopen. Only fixed
// provider:queue-ID keys and timestamps are stored, never titles or credentials.
type stuckState struct {
	Sources  map[string]string    `json:"sources"`
	Observed map[string]time.Time `json:"observed"`
}

func sourceKeys(services []service) map[string]string {
	out := map[string]string{}
	for _, s := range services {
		if s.id == "sonarr" || s.id == "radarr" {
			out[s.id] = fmt.Sprintf("%x", sha256.Sum256([]byte(s.base)))
		}
	}
	return out
}
func (m *Media) loadStuck(services []service) error {
	if m.host.CacheDir() == "" {
		return nil
	}
	b, e := os.ReadFile(filepath.Join(m.host.CacheDir(), "stuck.json"))
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return errors.New("queue observation history unavailable")
	}
	if len(b) > 1<<20 {
		return errors.New("queue observation history too large")
	}
	var stored stuckState
	if json.Unmarshal(b, &stored) != nil || stored.Observed == nil || len(stored.Observed) > 4000 {
		return errors.New("invalid queue observation history")
	}
	pattern := regexp.MustCompile(`^(sonarr|radarr):[1-9][0-9]{0,12}$`)
	state := map[string]time.Time{}
	sources := sourceKeys(services)
	for key, at := range stored.Observed {
		if !pattern.MatchString(key) || at.IsZero() || at.After(m.host.Now().Add(5*time.Minute)) {
			return errors.New("invalid queue observation history")
		}
		kind, _, _ := strings.Cut(key, ":")
		if sources[kind] != "" && sources[kind] == stored.Sources[kind] {
			state[key] = at
		}
	}
	m.stuck = state
	return nil
}
func (m *Media) saveStuck(next map[string]time.Time) error {
	if m.host.CacheDir() == "" {
		return errors.New("queue observation cache directory unavailable")
	}
	if len(next) > 4000 {
		return errors.New("queue observation history limit reached")
	}
	b, e := json.Marshal(stuckState{Sources: sourceKeys(m.services), Observed: next})
	if e != nil {
		return errors.New("invalid queue observation history")
	}
	if e = fsx.WriteAtomic(filepath.Join(m.host.CacheDir(), "stuck.json"), b, 0600); e != nil {
		return errors.New("queue observation history could not be saved")
	}
	return nil
}
