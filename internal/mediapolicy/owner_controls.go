package mediapolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// OwnerControlRequest uses zero-based positions in waiting queues. Position is
// a pointer so omitting it cannot accidentally move an item to the front.
type OwnerControlRequest struct {
	ID          string `json:"id,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Key         string `json:"key,omitempty"`
	Position    *int   `json:"position,omitempty"`
	Paused      *bool  `json:"paused,omitempty"`
	Apply       bool   `json:"apply"`
	RequestedBy string `json:"requestedBy,omitempty"`
}

// ValidOwnerControl is also the API's narrow payload allowlist.
func ValidOwnerControl(path string, in OwnerControlRequest) bool {
	if !in.Apply {
		return false
	}
	switch path {
	case "/archive/pause":
		return in.Paused != nil && in.ID == "" && in.Hash == "" && in.Key == "" && in.Position == nil
	case "/archive/job/approve", "/archive/job/drop":
		return in.ID != "" && in.Hash == "" && in.Key == "" && in.Position == nil && in.Paused == nil
	case "/seeding/stop":
		return ValidSeedHash(in.Hash) && in.ID == "" && in.Key == "" && in.Position == nil && in.Paused == nil
	case "/seeding/queue/reorder":
		return ValidSeedHash(in.Hash) && in.ID == "" && in.Key == "" && in.Position != nil && *in.Position >= 0 && in.Paused == nil
	case "/archive/search/reorder", "/archive/approvals/reorder":
		return in.Key != "" && in.ID == "" && in.Hash == "" && in.Position != nil && *in.Position >= 0 && in.Paused == nil
	}
	return false
}

func (s *Service) handleOwnerControl(w http.ResponseWriter, r *http.Request) {
	var in OwnerControlRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || !ValidOwnerControl(r.URL.Path, in) {
		writeJSON(w, 400, map[string]string{"error": "Invalid control request"})
		return
	}
	if in.RequestedBy == "" {
		in.RequestedBy = "worker bearer token"
	}
	if len(in.RequestedBy) > 200 {
		in.RequestedBy = in.RequestedBy[:200]
	}
	if s.DryRun {
		writeJSON(w, 409, map[string]string{"error": "Applied service required"})
		return
	}
	if err := s.Store.WriteAllowed(); err != nil {
		writeJSON(w, 409, map[string]string{"error": "Progress could not save"})
		return
	}
	// Pause and Stop must remain available during a move or long cycle. Their
	// fields live outside runner snapshots and each read/save preserves them.
	path := r.URL.Path
	var err error
	switch path {
	case "/archive/pause":
		err = s.Store.AuditDetail("archive_pause_requested", nil, in)
		if err == nil {
			err = s.Store.UpdateSeeding(func(st *State) error { st.ArchivePaused = *in.Paused; return nil })
		}
	case "/seeding/stop":
		err = s.Store.AuditDetail("seeding_stop_requested", nil, in)
		if err == nil {
			err = s.Store.UpdateSeeding(func(st *State) error {
				if st.Seeding == nil || st.Seeding.Plan.Hash != in.Hash || !seedPhaseActive(st.Seeding.Phase) {
					return fmt.Errorf("Move is not running")
				}
				st.Seeding.StopRequested = true
				return nil
			})
		}
	case "/seeding/queue/reorder", "/archive/search/reorder", "/archive/approvals/reorder":
		err = s.Store.AuditDetail("owner_control_requested", nil, map[string]any{"action": path, "request": in})
		if err == nil {
			err = s.reorderOwnerQueue(path, in)
		}
	default:
		if !s.lockTick(r.Context()) {
			writeJSON(w, 503, map[string]string{"error": archiveBusyMessage})
			return
		}
		defer s.tick.Unlock()
		err = s.Store.AuditDetail("owner_control_requested", nil, map[string]any{"action": path, "request": in})
		if err == nil {
			if strings.HasPrefix(path, "/archive/job/") {
				a, ok := s.Archive.(interface {
					DecideJob(context.Context, time.Time, string, bool) (archivetier.State, error)
				})
				if !ok || !s.Config.ArchiveEnabled {
					err = fmt.Errorf("Archive controls unavailable")
				} else {
					_, err = a.DecideJob(r.Context(), time.Now().UTC(), in.ID, path == "/archive/job/approve")
				}
			} else {
				err = s.reorderOwnerQueue(path, in)
			}
		}
	}
	s.publish()
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	if err := s.Store.AuditDetail("owner_control_applied", nil, map[string]any{"action": path, "request": in}); err != nil {
		writeJSON(w, 409, map[string]string{"error": "Progress could not save"})
		return
	}
	select {
	case s.wakeChan() <- struct{}{}:
	default:
	}
	if path == "/archive/job/approve" {
		_, err = s.RequestRun(in.RequestedBy)
	}
	if path == "/archive/pause" && !*in.Paused {
		s.ResumeSeedQueue()
	}
	if err != nil {
		writeJSON(w, 409, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"applied": true, "message": "Owner control saved"})
}

func titleQueueKey(t archivetier.ApprovalRequest) string {
	return fmt.Sprintf("%s:%d:%d", t.Kind, t.ID, t.Season)
}
func moveQueue[T any](rows []T, from, to int) []T {
	item := rows[from]
	if from < to {
		copy(rows[from:to], rows[from+1:to+1])
	} else {
		copy(rows[to+1:from+1], rows[to:from])
	}
	rows[to] = item
	return rows
}
func (s *Service) reorderOwnerQueue(path string, in OwnerControlRequest) error {
	return s.Store.UpdateSeeding(func(st *State) error {
		pos := *in.Position
		from := -1
		switch path {
		case "/seeding/queue/reorder":
			for i, q := range st.SeedQueue {
				if q.Hash == in.Hash {
					from = i
				}
			}
			if from < 0 || pos >= len(st.SeedQueue) {
				return fmt.Errorf("Queue item changed")
			}
			st.SeedQueue = moveQueue(st.SeedQueue, from, pos)
		case "/archive/approvals/reorder":
			for i, q := range st.Archive.Approvals {
				if titleQueueKey(q.ApprovalRequest) == in.Key {
					from = i
				}
			}
			if from < 0 || pos >= len(st.Archive.Approvals) {
				return fmt.Errorf("Queue item changed")
			}
			st.Archive.Approvals = moveQueue(st.Archive.Approvals, from, pos)
		case "/archive/search/reorder":
			// Only waiting rows take positions; searching and terminal rows stay put.
			slots := []int{}
			waiting := []ArchiveSearch{}
			for i, q := range st.ArchiveSearches {
				if q.State == "queued" {
					slots = append(slots, i)
					if titleQueueKey(q.Title) == in.Key {
						from = len(waiting)
					}
					waiting = append(waiting, q)
				}
			}
			if from < 0 || pos >= len(waiting) {
				return fmt.Errorf("Queue item changed")
			}
			waiting = moveQueue(waiting, from, pos)
			for i, slot := range slots {
				st.ArchiveSearches[slot] = waiting[i]
			}
		}
		return nil
	})
}

// keepQueueOrder merges a worker result with the owner's latest queue order.
// Removed items stay removed and new entries follow surviving queued items.
func keepQueueOrder[T any](rows, ordered []T, key func(T) string) []T {
	if len(rows) == 0 {
		return rows
	}
	byKey := map[string]T{}
	for _, row := range rows {
		byKey[key(row)] = row
	}
	out := make([]T, 0, len(rows))
	for _, row := range ordered {
		if v, ok := byKey[key(row)]; ok {
			out = append(out, v)
			delete(byKey, key(row))
		}
	}
	for _, row := range rows {
		if _, ok := byKey[key(row)]; ok {
			out = append(out, row)
			delete(byKey, key(row))
		}
	}
	return out
}
