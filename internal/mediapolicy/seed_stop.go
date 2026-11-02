package mediapolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var errOwnerStopped = errors.New("Stopped by owner")

// seedOwnerStop is called between steps, never from a wait for an accepted
// write. A verified NAS result commits start/cleanup even after Stop. At the
// original path it proves every source and restarts a stopped torrent there.
// Between location and verification it halts without touching either copy.
func (s *Service) seedOwnerStop(ctx context.Context, j *SeedJob) error {
	if j.stopping {
		return nil
	}
	saved := s.Store.Snapshot().Seeding
	if saved != nil && saved.Plan.Hash == j.Plan.Hash {
		j.StopRequested = j.StopRequested || saved.StopRequested
	}
	if !j.StopRequested || j.NASVerified {
		return nil
	}
	if j.Intent != "" {
		return errOwnerStopped
	}
	t, err := s.Seeding.Qbit.Torrents(ctx)
	if err != nil {
		return errOwnerStopped
	}
	for _, torrent := range t {
		if torrent.Hash != j.Plan.Hash {
			continue
		}
		if !seedAtLocation(torrent, j.Plan, j.Plan.OldPath) || torrent.AutoTMM == nil || *torrent.AutoTMM || torrent.DownloadPath == nil || (*torrent.DownloadPath != "" && *torrent.DownloadPath != j.Plan.OriginalDownloadPath) {
			return errOwnerStopped
		}
		if torrent.State != "stoppedUP" && torrent.State != "uploading" && torrent.State != "stalledUP" && torrent.State != "queuedUP" && torrent.State != "forcedUP" {
			return errOwnerStopped
		}
		if err := s.seedSourceTorrent(ctx, j, *torrent.DownloadPath); err != nil {
			return errOwnerStopped
		}
		for _, f := range j.Plan.Files {
			if _, err := s.seedOld(f); err != nil {
				return errOwnerStopped
			}
		}
		if strings.HasPrefix(torrent.State, "stopped") {
			j.stopping = true
			defer func() { j.stopping = false }()
			if err := s.seedWrite(ctx, j, "start", ""); err != nil {
				return fmt.Errorf("%w: resume unavailable", errOwnerStopped)
			}
			if err := s.Seeding.wait(ctx, func() (bool, error) {
				fresh, err := s.seedTorrentWithDownloadPath(ctx, j.Plan.Hash, *torrent.DownloadPath)
				if err != nil {
					return false, err
				}
				if !seedAtLocation(fresh, j.Plan, j.Plan.OldPath) || *fresh.Progress != 1 || *fresh.AmountLeft != 0 {
					return false, errOwnerStopped
				}
				switch fresh.State {
				case "uploading", "stalledUP", "queuedUP", "forcedUP":
					return true, nil
				}
				return false, nil
			}); err != nil {
				return fmt.Errorf("%w: resume unverified", errOwnerStopped)
			}
		}
		j.Phase = "stopped"
		return errOwnerStopped
	}
	return errOwnerStopped
}
