package main

import (
	"github.com/ficaa1/homelab-inventory/internal/capacity"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"net/http"
)

// handleStorage serves the fleet-wide capacity snapshot: every PVE storage,
// ZFS pool and real filesystem across all hosts, with the delta against the
// previous stored run. It is the "space left" answer for the digest, and the
// same rows the dashboard's host pages show, normalized to one flat list.
func (s *server) handleStorage(w http.ResponseWriter, r *http.Request) {
	latest, err := s.store.LatestAll()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	previous := make(map[string]*inventory.Inventory, len(latest))
	for _, inv := range latest {
		records, err := s.store.History(inv.Host.Hostname)
		if err != nil {
			continue
		}
		if len(records) > 1 {
			if prev, err := s.store.At(inv.Host.Hostname, records[1].ID); err == nil {
				previous[inv.Host.Hostname] = prev
			}
		}
	}
	writeJSON(w, http.StatusOK, capacity.Build(latest, previous))
}
