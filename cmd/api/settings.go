package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/alert"
	"github.com/ficaa1/homelab-inventory/internal/notify"
	"github.com/ficaa1/homelab-inventory/internal/settings"
	"github.com/ficaa1/homelab-inventory/internal/silence"
	"github.com/ficaa1/homelab-inventory/internal/store"
)

// stored reads the saved settings. A missing or unreadable blob is not an
// error: it means nothing has been configured from the page yet, and every
// field falls through to the environment.
func (s *server) stored() settings.Stored {
	st, _ := s.readSettings()
	return st
}

func (s *server) readSettings() (settings.Stored, error) {
	var st settings.Stored
	err := s.store.ReadState(settings.StateName, &st)
	return st, err
}

// effective is the configuration in force: the environment, with anything
// saved from the page layered over it.
func (s *server) effective() settings.Effective {
	return settings.Resolve(s.stored(), s.defaults)
}

// handleGetSettings reports the configuration and — the part that matters —
// why notifications have or have not been arriving.
//
// The motivating bug was not a bug at all. Notifications were enabled, the
// floor was `critical`, and the fleet had ten warnings and no criticals, so
// nothing was ever sent. Every individual field said something true and the
// combination was still unreadable. So this endpoint does the arithmetic:
// how many alerts sit at or above the floor right now, and what each other
// floor would have covered.
func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	eff := s.effective()
	now := time.Now()

	type levelCoverage struct {
		Level alert.Level `json:"level"`
		// Count is how many current alerts this floor would cover.
		Count int `json:"count"`
		// Current marks the floor in force.
		Current bool `json:"current"`
	}

	out := struct {
		RunRetention      store.Policy    `json:"run_retention"`
		NotifyLevel       alert.Level     `json:"notify_level"`
		NotifyInterval    string          `json:"notify_interval"`
		DigestInterval    string          `json:"digest_interval"`
		NotifyEnabled     bool            `json:"notify_enabled"`
		DashboardName     string          `json:"dashboard_name"`
		IconsFetch        bool            `json:"icons_fetch"`
		AppriseSet        bool            `json:"apprise_set"`
		AppriseHint       string          `json:"apprise_hint,omitempty"`
		DigestAppriseSet  bool            `json:"digest_apprise_set"`
		DigestAppriseHint string          `json:"digest_apprise_hint,omitempty"`
		Sending           bool            `json:"sending"`
		Source            map[string]bool `json:"configured_here"`
		Coverage          []levelCoverage `json:"coverage"`
		// AtOrAboveFloor is how many current, unsilenced alerts are eligible
		// to notify. Zero here with Sending true is the "working, and quiet"
		// case that looked like a fault.
		AtOrAboveFloor int           `json:"at_or_above_floor"`
		TotalAlerts    int           `json:"total_alerts"`
		Status         notify.Status `json:"status"`
		Log            notify.Log    `json:"log"`
		UpdatedAt      *time.Time    `json:"updated_at,omitempty"`
	}{
		RunRetention:      eff.RunRetention,
		NotifyLevel:       eff.NotifyLevel,
		NotifyInterval:    eff.NotifyInterval.String(),
		DigestInterval:    eff.DigestInterval.String(),
		NotifyEnabled:     eff.NotifyEnabled,
		DashboardName:     eff.DashboardName,
		IconsFetch:        eff.IconsFetch,
		AppriseSet:        eff.AppriseURL != "",
		AppriseHint:       settings.Redacted(eff.AppriseURL),
		DigestAppriseSet:  eff.DigestAppriseURL != "",
		DigestAppriseHint: settings.Redacted(eff.DigestAppriseURL),
		Sending:           eff.Sending(),
		Coverage:          []levelCoverage{},
	}

	// Which fields a human has set here, so the page can show what is coming
	// from the environment and what is an override.
	st := s.stored()
	out.Source = map[string]bool{
		"notify_level":       st.NotifyLevel != nil,
		"notify_interval":    st.NotifyInterval != nil,
		"digest_interval":    st.DigestInterval != nil,
		"apprise_url":        st.AppriseURL != nil && *st.AppriseURL != "",
		"digest_apprise_url": st.DigestAppriseURL != nil && *st.DigestAppriseURL != "",
		"notify_enabled":     st.NotifyEnabled != nil,
		"dashboard_name":     st.DashboardName != nil,
		"icons_fetch":        st.IconsFetch != nil,
	}
	if !st.UpdatedAt.IsZero() {
		t := st.UpdatedAt
		out.UpdatedAt = &t
	}

	// Coverage is computed over the same silenced-and-filtered set the
	// notifier itself uses, so the number on the page is the number that
	// governs delivery rather than a similar-looking one.
	if invs, err := s.store.LatestAll(); err == nil {
		rep, err := s.evaluateAlerts(invs, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "loading disk health evidence")
			return
		}
		if book, err := s.silences(); err == nil {
			rep = silence.Apply(rep, &book, now)
		}
		live := silence.Unsilenced(rep.Alerts)
		out.TotalAlerts = len(live)
		for _, lv := range []alert.Level{alert.LevelCritical, alert.LevelWarning, alert.LevelInfo} {
			n := 0
			for _, a := range live {
				if a.Level.AtLeast(lv) {
					n++
				}
			}
			out.Coverage = append(out.Coverage, levelCoverage{
				Level: lv, Count: n, Current: lv == eff.NotifyLevel,
			})
			if lv == eff.NotifyLevel {
				out.AtOrAboveFloor = n
			}
		}
	}

	if s.notifier != nil {
		out.Status = s.notifier.Status()
		out.Log = s.notifier.Recorder.Read()
	}
	writeJSON(w, http.StatusOK, out)
}

// handlePutSettings applies a partial update.
//
// Partial rather than whole-document because the apprise URL is write-only:
// a page that had to send back everything it was given could not preserve a
// field it was never shown. Omitting a field leaves it alone.
func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCurationBytes)

	var in settings.Update
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "decoding: "+err.Error())
		return
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	next, err := settings.Apply(s.stored(), in, time.Now(), s.defaults)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.WriteState(settings.StateName, next); err != nil {
		s.log.Printf("settings: %v", err)
		writeError(w, http.StatusInternalServerError, "saving settings")
		return
	}

	// Logged without the URL, which is a credential — only whether one is now
	// present. The rest is worth having in the log: a notification floor that
	// silently changed is a thing you want to be able to find later.
	eff := settings.Resolve(next, s.defaults)
	if s.icons != nil {
		s.icons.SetFetch(eff.IconsFetch)
	}
	s.log.Printf("settings updated: level=%s interval=%s digest=%s enabled=%t apprise=%t digest_apprise=%t",
		eff.NotifyLevel, eff.NotifyInterval, eff.DigestInterval, eff.NotifyEnabled, eff.AppriseURL != "", eff.DigestAppriseURL != "")

	s.handleGetSettings(w, r)
}

// handleResetSettings drops every override and returns to the environment.
//
// Worth having as its own operation: "put it back how it was" is otherwise a
// guess about what the environment held, which is exactly the knowledge a
// person editing settings from a phone does not have.
func (s *server) handleResetSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	st := s.stored()
	if err := s.store.WriteState(settings.StateName, settings.Stored{Auth: st.Auth, AuthGeneration: st.AuthGeneration}); err != nil {
		s.log.Printf("settings reset: %v", err)
		writeError(w, http.StatusInternalServerError, "resetting settings")
		return
	}
	s.log.Printf("settings reset to the environment defaults")
	if s.icons != nil {
		s.icons.SetFetch(s.defaults.IconsFetch)
	}
	s.handleGetSettings(w, r)
}
