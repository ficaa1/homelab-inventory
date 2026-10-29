package store

import "path/filepath"

// AgentOffersPath is the API-owned offer state/audit journal, outside releases.
func (s *Store) AgentOffersPath() string { return filepath.Join(s.dir, "agent-offers.jsonl") }
