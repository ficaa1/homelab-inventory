package overview

import (
	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/seen"
	"sort"
	"time"
)

func validAge(e seen.Entry, now time.Time) bool {
	return !e.First.IsZero() && !e.Last.IsZero() && !e.First.After(e.Last) && !e.Last.After(now) && now.Sub(e.Last) <= seen.Grace
}

// Recent means newly observed and still active, not notifications or escalations.
func recentAlerts(alerts []alert.Alert, ledgerErr error, now time.Time) Recent {
	r := Recent{Status: "ok", WindowS: 86400, Items: []RecentItem{}}
	if ledgerErr != nil {
		r.Status, r.Error, r.Items = "unavailable", "alert ages could not be read", nil
		return r
	}
	count := 0
	for _, a := range alerts {
		if a.Silenced {
			continue
		}
		if a.FirstSeen == nil || a.FirstSeen.IsZero() || a.FirstSeen.After(now) {
			r.UnknownAgeCount++
			continue
		}
		if a.FirstSeen.Before(now.Add(-24 * time.Hour)) {
			continue
		}
		count++
		r.Items = append(r.Items, RecentItem{a.Host, a.Rule, a.Subject, a.FirstSeen.UTC()})
	}
	sort.Slice(r.Items, func(i, j int) bool {
		a, b := r.Items[i], r.Items[j]
		if !a.Since.Equal(b.Since) {
			return a.Since.After(b.Since)
		}
		return seen.Key(alert.Alert{Host: a.Host, Rule: a.Rule, Subject: a.Subject}) < seen.Key(alert.Alert{Host: b.Host, Rule: b.Rule, Subject: b.Subject})
	})
	if len(r.Items) > 5 {
		r.Items = r.Items[:5]
	}
	has := count > 0
	r.Count, r.HasRecent = &count, &has
	if r.UnknownAgeCount > 0 {
		r.Status = "partial"
	}
	return r
}
