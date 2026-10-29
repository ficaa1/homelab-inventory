package main

import (
	"github.com/ficaa1/homelab-inventory/internal/overview"
	"net/http"
	"time"
)

func (s *server) handleOverview(w http.ResponseWriter, r *http.Request) {
	b := overview.Builder{Store: s.store, Cache: &s.overviewCache, Runs: &s.overviewRuns, Evaluate: s.evaluateAlerts}
	writeJSON(w, http.StatusOK, b.Build(r.Context(), time.Now()))
}
