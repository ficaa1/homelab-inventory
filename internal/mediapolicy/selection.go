package mediapolicy

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

const invalidSelectionWarning = "Invalid archive scoring overrides; using file policy"

type SelectionSettings struct {
	Warning          string                         `json:"warning,omitempty"`
	InvalidOverrides json.RawMessage                `json:"invalidOverrides,omitempty"`
	File             archivetier.Selection          `json:"file"`
	Effective        archivetier.Selection          `json:"effective"`
	Overrides        archivetier.SelectionOverrides `json:"overrides"`
}
type SelectionRequest struct {
	Overrides   *archivetier.SelectionOverrides `json:"overrides,omitempty"`
	Reset       bool                            `json:"reset,omitempty"`
	RequestedBy string                          `json:"requestedBy,omitempty"`
}

func (s *Service) selectionSettings() (SelectionSettings, error) {
	st := s.Store.Snapshot()
	file, err := (archivetier.SelectionOverrides{}).Apply(s.Config.Archive.Selection)
	if err != nil {
		return SelectionSettings{}, err
	}
	o := st.SelectionOverrides
	effective, err := o.Apply(file)
	settings := SelectionSettings{File: file, Effective: effective, Overrides: o}
	if err != nil || st.SelectionOverridesWarning != "" {
		settings.Effective = file
		settings.Overrides = archivetier.SelectionOverrides{}
		settings.Warning = invalidSelectionWarning
		settings.InvalidOverrides = st.SelectionOverridesRaw
		if len(settings.InvalidOverrides) == 0 {
			settings.InvalidOverrides, _ = json.Marshal(o)
		}
	}
	return settings, nil
}

// syncSelection runs only under tick, leaving the immutable file config and
// clients' filesystem safety configuration alone. Older jobs are pinned before
// installing a new policy; new jobs persist their policy at grab intent.
func (s *Service) syncSelection() error {
	settings, err := s.selectionSettings()
	if err != nil {
		return err
	}
	if runner, ok := s.Archive.(*archivetier.Runner); ok {
		runner.Config.Selection = settings.Effective
		runner.OwnerPaused = func() bool { return s.Store.Snapshot().ArchivePaused }
	}
	return nil
}

func (s *Store) saveSelection(o archivetier.SelectionOverrides, archive archivetier.State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := clone(s.state)
	s.state.SelectionOverrides = clone(o)
	s.state.SelectionOverridesRaw = nil
	s.state.SelectionOverridesWarning = ""
	s.state.Archive = clone(archive)
	if err := s.saveLocked(); err != nil {
		s.state = previous
		return err
	}
	return nil
}

func (s *Service) handleSelection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if ct != "application/json" {
			writeJSON(w, 415, map[string]string{"error": "Invalid request"})
			return
		}
	}
	if !s.lockTick(r.Context()) {
		writeJSON(w, 503, map[string]string{"error": archiveBusyMessage})
		return
	}
	defer s.tick.Unlock()
	before, err := s.selectionSettings()
	if err != nil && r.Method == http.MethodGet {
		writeJSON(w, 409, map[string]string{"error": "Invalid value"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, 200, before)
		return
	}
	var in SelectionRequest
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&in); err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = fmt.Errorf("extra JSON")
		}
	}
	if err != nil || in.Reset && in.Overrides != nil || !in.Reset && in.Overrides == nil || len(in.RequestedBy) > 200 {
		writeJSON(w, 400, map[string]string{"error": "Invalid value"})
		return
	}
	if in.RequestedBy == "" {
		in.RequestedBy = "mediapolicy bearer token"
	}
	o := archivetier.SelectionOverrides{}
	if in.Overrides != nil {
		o = *in.Overrides
	}
	effective, err := o.Apply(before.File)
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "Invalid value"})
		return
	}
	current, proposed := s.Config.Archive, s.Config.Archive
	current.Selection, proposed.Selection = before.Effective, effective
	archive := s.Store.Snapshot().Archive
	// An active job keeps the entire persisted selection policy even when caps
	// or codec factors change. Backfill older jobs with the preceding policy.
	if archive.Job != nil && archive.Job.Selection == nil {
		archive.Job.Selection = &before.Effective
	}
	preview := archivetier.RescoreCached(&archive, current, proposed)
	if r.Method == http.MethodPost {
		writeJSON(w, 200, preview)
		return
	}
	detail := map[string]any{"actor": in.RequestedBy, "before": before.Effective, "after": effective, "beforeOverrides": before.Overrides, "beforeInvalidOverrides": before.InvalidOverrides, "afterOverrides": o}
	if err = s.Store.AuditDetail("selection_settings_intent", nil, detail); err == nil {
		err = s.Store.saveSelection(o, archive)
	}
	if err == nil {
		err = s.Store.AuditDetail("selection_settings_saved", nil, detail)
	}
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": "Save failed"})
		return
	}
	s.syncSelection()
	s.publish()
	writeJSON(w, 200, SelectionSettings{File: before.File, Effective: effective, Overrides: o})
}
