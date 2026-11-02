package mediapolicy

import (
	"context"
	"fmt"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// Queue persistence is independent of runner saves, which replace Archive.
func (s *Store) saveArchiveSearches(rows []ArchiveSearch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.state.ArchiveSearches
	s.state.ArchiveSearches = clone(keepQueueOrder(rows, s.state.ArchiveSearches, func(row ArchiveSearch) string { return titleQueueKey(row.Title) }))
	if err := s.saveLocked(); err != nil {
		s.state.ArchiveSearches = previous
		return err
	}
	return nil
}

func (s *Service) queueArchiveSearches(in TagRequest, item archivetier.LibraryItem, before archivetier.State) error {
	rows := s.Store.Snapshot().ArchiveSearches
	keep := rows[:0]
	for _, row := range rows {
		// Withdrawn seasons must not perform later tracker lookups.
		if row.Title.Kind == in.Kind && row.Title.ID == in.ID && row.State == "queued" {
			selected := in.Kind == "radarr" || in.Seasons == nil
			for _, n := range in.Seasons {
				selected = selected || n == row.Title.Season
			}
			if !*in.Archive || !selected {
				continue
			}
		}
		if row.Finished.IsZero() || time.Since(row.Finished) < 10*time.Second {
			keep = append(keep, row)
		}
	}
	rows = keep
	if *in.Archive {
		seasons := []int{0}
		if in.Kind == "sonarr" {
			seasons = in.Seasons
			if seasons == nil {
				for _, season := range item.Seasons {
					if season.Season > 0 {
						seasons = append(seasons, season.Season)
					}
				}
			}
		}
		for _, season := range seasons {
			already := false
			if item.Tags.Archive && in.Kind == "sonarr" {
				chosen, ok := before.Seasons[archivetier.TitleKey(in.Kind, in.ID)]
				already = !ok
				for _, n := range chosen {
					already = already || n == season
				}
			}
			title := archivetier.ApprovalRequest{Kind: in.Kind, ID: in.ID, Season: season}
			for _, row := range rows {
				already = already || row.Title == title && row.Finished.IsZero()
			}
			if !already {
				rows = append(rows, ArchiveSearch{Title: title, Name: item.Title, State: "queued"})
			}
		}
	}
	if err := s.Store.AuditDetail("archive_search_queue_intent", nil, rows); err != nil {
		return err
	}
	return s.Store.saveArchiveSearches(rows)
}

// After reconciliation, the normal cycle attempts at most one queued lookup,
// under the same tick lock as manual search and nightly discovery. This bounds
// cycle work even if the rolling-minute rate window expires during a search;
// the runner still owns persisted rate admission and never waits for capacity.
func (s *Service) drainArchiveSearches(ctx context.Context) {
	a, ok := s.Archive.(interface {
		SearchTitleCached(context.Context, time.Time, archivetier.ApprovalRequest) (archivetier.State, error)
	})
	if s.Store.Snapshot().ArchivePaused || !ok || ctx.Err() != nil || s.DryRun || !s.Config.ArchiveEnabled || s.Store.WriteAllowed() != nil {
		return
	}
	rows := s.Store.Snapshot().ArchiveSearches
	for i := range rows {
		if rows[i].State != "queued" {
			continue
		}
		title := rows[i].Title
		if s.Store.UpdateSeeding(func(st *State) error {
			if st.ArchivePaused {
				return fmt.Errorf("Paused by owner")
			}
			for n, row := range st.ArchiveSearches {
				if row.Title == title && row.State == "queued" {
					st.ArchiveSearches[n].State = "searching"
					return nil
				}
			}
			return fmt.Errorf("Queue item changed")
		}) != nil {
			return
		}
		_, err := a.SearchTitleCached(ctx, time.Now().UTC(), title)
		s.Store.UpdateSeeding(func(st *State) error {
			for n, row := range st.ArchiveSearches {
				if row.Title != title || row.State != "searching" {
					continue
				}
				if err != nil && !archivetier.IsSearchRefused(err) {
					row.State = "queued"
					st.ArchiveSearches = append(append(st.ArchiveSearches[:n], st.ArchiveSearches[n+1:]...), row)
				} else {
					row.State = "done"
					if err != nil {
						row.State = "failed"
					}
					row.Finished = time.Now().UTC()
					st.ArchiveSearches[n] = row
				}
				return nil
			}
			return fmt.Errorf("Queue item changed")
		})
		s.publish()
		return
	}
}
