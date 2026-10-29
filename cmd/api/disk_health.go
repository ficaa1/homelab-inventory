package main

import (
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
	"time"
)

// Serialize evidence updates across dashboard, digest and notifier evaluations.
// Errors abort evaluation so a state read/write failure cannot announce recovery.
func (s *server) evaluateAlerts(invs []*inventory.Inventory, now time.Time) (*alert.Report, error) {
	s.diskHealthMu.Lock()
	defer s.diskHealthMu.Unlock()
	book := alert.DiskHealthBook{}
	if err := s.store.ReadState(alert.DiskHealthStateName, &book); err != nil {
		return nil, err
	}
	if book == nil {
		book = alert.DiskHealthBook{}
	}
	effective := book.Apply(invs)
	if err := s.store.WriteState(alert.DiskHealthStateName, book); err != nil {
		return nil, err
	}
	rep := alert.Evaluate(effective, alert.DefaultThresholds(), now)
	if s.plugins != nil {
		rep.Add(s.plugins.Alerts()...)
	}
	return rep, nil
}
