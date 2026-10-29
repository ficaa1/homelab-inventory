package overview

import (
	"context"
	"errors"
	"fmt"
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"testing"
	"time"
)

func TestRecentWindowBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		first          *time.Time
		silenced       bool
		count, unknown int
	}{
		{"boundary", date(now.Add(-24 * time.Hour)), false, 1, 0},
		{"outside", date(now.Add(-24*time.Hour - time.Nanosecond)), false, 0, 0},
		{"now", date(now), false, 1, 0},
		{"future", date(now.Add(time.Nanosecond)), false, 0, 1},
		{"missing", nil, false, 0, 1},
		{"zero", date(time.Time{}), false, 0, 1},
		{"silenced", date(now), true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := recentAlerts([]alert.Alert{{Host: "host-a", Rule: "test", Subject: "one", FirstSeen: tc.first, Silenced: tc.silenced}}, nil, now)
			if *r.Count != tc.count || r.UnknownAgeCount != tc.unknown || *r.HasRecent != (tc.count > 0) || len(r.Items) != tc.count {
				t.Fatal(r)
			}
			if tc.unknown > 0 && r.Status != "partial" {
				t.Fatal(r.Status)
			}
		})
	}
}
func date(t time.Time) *time.Time { return &t }
func TestRecentLimitAndUnavailable(t *testing.T) {
	alerts := []alert.Alert{}
	for i := 0; i < 8; i++ {
		alerts = append(alerts, alert.Alert{Host: "host-a", Rule: "test", Subject: fmt.Sprint(i), Level: alert.LevelInfo, FirstSeen: date(now.Add(-time.Duration(i) * time.Hour))})
	}
	r := recentAlerts(alerts, nil, now)
	if *r.Count != 8 || len(r.Items) != 5 || r.Items[0].Subject != "0" {
		t.Fatal(r)
	}
	r = recentAlerts(alerts, errors.New("unreadable"), now)
	if r.Status != "unavailable" || r.Count != nil || r.HasRecent != nil || r.Items != nil {
		t.Fatal(r)
	}
}
func TestLedgerAgeValidity(t *testing.T) {
	for _, tc := range []struct {
		entry seen.Entry
		valid bool
	}{
		{seen.Entry{First: now.Add(-time.Hour), Last: now}, true},
		{seen.Entry{First: now.Add(-3 * time.Hour), Last: now.Add(-seen.Grace)}, true},
		{seen.Entry{First: now.Add(-3 * time.Hour), Last: now.Add(-seen.Grace - time.Nanosecond)}, false},
		{seen.Entry{First: now, Last: now.Add(time.Second)}, false},
		{seen.Entry{First: now, Last: now.Add(-time.Second)}, false},
		{seen.Entry{Last: now}, false},
	} {
		if got := validAge(tc.entry, now); got != tc.valid {
			t.Fatalf("%+v: %v", tc.entry, got)
		}
	}
}
func TestRecentOverviewReadsLedgerWithoutSeeding(t *testing.T) {
	v := inv("host-a")
	v.CollectedAt = now.Add(-time.Hour)
	s := saved(t, v)
	first := now.Add(-24 * time.Hour)
	key := seen.Key(alert.Alert{Host: "host-a", Rule: "host.stale", Subject: "host-a"})
	s.WriteState(seen.StateName, seen.Book{Entries: map[string]seen.Entry{key: {First: first, Last: now}}})
	r := (Builder{Store: s}).Build(context.Background(), now)
	recent := r.Fleet.(Fleet).Alerts.Recent
	if *recent.Count != 1 || len(recent.Items) != 1 || !recent.Items[0].Since.Equal(first) {
		t.Fatal(recent)
	}
	var after seen.Book
	s.ReadState(seen.StateName, &after)
	if len(after.Entries) != 1 || !after.Entries[key].First.Equal(first) {
		t.Fatal("ledger mutated")
	}
}
