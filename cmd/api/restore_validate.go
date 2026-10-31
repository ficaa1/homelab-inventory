package main

import "fmt"

// Resolve lazy durable managers while the install can still roll back. This
// performs no provider probes, polls, notifications, or recommendation runs.
func (s *server) validateRestoredState() error {
	if _, err := s.readSettings(); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if s.plugins != nil {
		if err := s.plugins.ValidateTasteState(); err != nil {
			return fmt.Errorf("taste state: %w", err)
		}
		for _, p := range s.plugins.List() {
			if p.Enabled && p.State != "ok" {
				return fmt.Errorf("plugin %s requires valid local configuration", p.ID)
			}
		}
	}
	if _, err := s.duskStore(); err != nil {
		return fmt.Errorf("TV state: %w", err)
	}
	return nil
}
