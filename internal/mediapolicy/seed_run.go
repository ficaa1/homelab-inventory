package mediapolicy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

func (s *Store) SaveSeeding(j *SeedJob) error {
	return s.UpdateSeeding(func(st *State) error {
		if st.Seeding != nil && st.Seeding.Plan.Hash == j.Plan.Hash {
			j.StopRequested = j.StopRequested || st.Seeding.StopRequested
		}
		st.Seeding = clone(j)
		return nil
	})
}
func (s *Service) seedIntent(ctx context.Context, j *SeedJob, action string, write func() error) error {
	if err := s.seedOwnerStop(ctx, j); err != nil {
		return err
	}
	if s.DryRun {
		return fmt.Errorf("dry-run write refused")
	}
	if e := s.Store.WriteAllowed(); e != nil {
		return e
	}
	j.Intent = action
	if e := s.Store.SaveSeeding(j); e != nil {
		return e
	}
	if e := s.Store.AuditDetail("seeding_intent", nil, j); e != nil {
		return e
	}
	if e := write(); e != nil {
		return e
	} // Intent deliberately retained on every error.
	j.Intent = ""
	if e := s.Store.SaveSeeding(j); e != nil {
		return e
	}
	return s.Store.AuditDetail("seeding_write_accepted", nil, map[string]any{"hash": j.Plan.Hash, "action": action})
}
func (s *Service) seedPhase(j *SeedJob, phase string) error {
	j.Phase = phase
	return s.Store.SaveSeeding(j)
}
func (s *Service) seedIOGate(ctx context.Context, j *SeedJob) error {
	if err := s.seedOwnerStop(ctx, j); err != nil {
		return err
	}
	if err := s.Seeding.Qbit.Reviewed(ctx); err != nil {
		return err
	}
	for _, c := range s.LibraryClients {
		if err := c.Preflight(ctx); err != nil {
			return err
		}
	}
	return s.seedFree(j.Plan)
}
func (m *SeedMover) interval() time.Duration {
	if m.PollInterval > 0 {
		return m.PollInterval
	}
	return 250 * time.Millisecond
}
func (m *SeedMover) wait(ctx context.Context, predicate func() (bool, error)) error {
	ticker := time.NewTicker(m.interval())
	defer ticker.Stop()
	for {
		done, e := predicate()
		if e != nil {
			return e
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation timed out or stopped; inspect before retrying")
		case <-ticker.C:
		}
	}
}
func (s *Service) seedTorrent(ctx context.Context, hash string) (SeedTorrent, error) {
	return s.seedTorrentWithDownloadPath(ctx, hash, "")
}
func (s *Service) seedTorrentWithDownloadPath(ctx context.Context, hash, downloadPath string) (SeedTorrent, error) {
	ts, e := s.Seeding.Qbit.Torrents(ctx)
	if e != nil {
		return SeedTorrent{}, e
	}
	for _, t := range ts {
		if t.Hash == hash {
			s.observeSeed(t)
			if *t.AutoTMM || *t.DownloadPath != downloadPath {
				return SeedTorrent{}, fmt.Errorf("torrent storage management changed")
			}
			return t, nil
		}
	}
	return SeedTorrent{}, fmt.Errorf("torrent disappeared; inspect before retrying")
}
func (s *Service) seedWrite(ctx context.Context, j *SeedJob, action, location string) error {
	return s.seedIntent(ctx, j, action, func() error { return s.Seeding.Qbit.Write(ctx, action, j.Plan.Hash, location) })
}
func (s *Service) seedStop(ctx context.Context, j *SeedJob) error {
	if e := s.seedWrite(ctx, j, "stop", ""); e != nil {
		return e
	}
	return s.Seeding.wait(ctx, func() (bool, error) {
		t, e := s.seedTorrentWithDownloadPath(ctx, j.Plan.Hash, j.Plan.OriginalDownloadPath)
		return strings.HasPrefix(t.State, "stopped"), e
	})
}

func (s *Service) seedClearDownloadPath(ctx context.Context, j *SeedJob) error {
	if err := s.seedIOGate(ctx, j); err != nil {
		return err
	}
	if err := s.seedInitialTorrent(ctx, j); err != nil {
		return err
	}
	t, err := s.seedTorrentWithDownloadPath(ctx, j.Plan.Hash, j.Plan.OriginalDownloadPath)
	if err != nil {
		return err
	}
	if t.State != "stoppedUP" {
		return fmt.Errorf("clearing download path requires a stopped, complete torrent")
	}
	if j.Plan.OriginalDownloadPath != "" {
		if err = s.seedWrite(ctx, j, "setDownloadPath", ""); err != nil {
			return err
		}
	}
	// Positive readback is mandatory: a no-op or fallback to the global temp
	// path cannot pass this gate. Leave the path empty even after rollback.
	if err = s.seedSourceTorrent(ctx, j, ""); err != nil {
		return err
	}
	t, err = s.seedTorrent(ctx, j.Plan.Hash)
	if err != nil {
		return err
	}
	if t.State != "stoppedUP" {
		return fmt.Errorf("torrent did not remain stopped after clearing download path")
	}
	return s.Store.AuditDetail("seeding_download_path_verified_empty", nil, j)
}
func (s *Service) seedLocation(ctx context.Context, j *SeedJob, location string) error {
	if e := s.seedIOGate(ctx, j); e != nil {
		return e
	}
	current := j.Plan.OldPath
	if j.Phase == "rolling_back" {
		current = j.Plan.Destination
	}
	t, e := s.seedTorrent(ctx, j.Plan.Hash)
	if e != nil {
		return e
	}
	if !strings.HasPrefix(t.State, "stopped") || !seedAtLocation(t, j.Plan, current) {
		return fmt.Errorf("location write requires stopped storage at the expected source")
	}
	if current == j.Plan.OldPath {
		if t.State != "stoppedUP" || *t.Progress != 1 || *t.AmountLeft != 0 {
			return fmt.Errorf("source torrent is no longer complete")
		}
		for _, f := range j.Plan.Files {
			if _, e := s.seedOld(f); e != nil {
				return e
			}
		}
	}
	if e := s.seedWrite(ctx, j, "setLocation", location); e != nil {
		return e
	}
	return s.Seeding.wait(ctx, func() (bool, error) {
		t, e := s.seedTorrent(ctx, j.Plan.Hash)
		if e != nil {
			return false, e
		}
		if t.State == "moving" || strings.HasPrefix(t.State, "checking") {
			return false, nil
		}
		if !strings.HasPrefix(t.State, "stopped") {
			return false, fmt.Errorf("location change did not leave torrent stopped")
		}
		return seedAtLocation(t, j.Plan, location), nil
	})
}

func seedAtLocation(t SeedTorrent, p SeedPlan, location string) bool {
	rel, err := filepath.Rel(p.OldPath, p.ContentPath)
	return err == nil && t.SavePath == location && t.ContentPath == filepath.Join(location, rel)
}

// Force recheck must be observed in progress before accepting its result.
// A lost/tiny unobserved check halts safely rather than trusting stale progress.
func (s *Service) seedRecheck(ctx context.Context, j *SeedJob) (bool, error) {
	if e := s.seedIOGate(ctx, j); e != nil {
		return false, e
	}
	location := j.Plan.Destination
	if j.Phase == "rolling_back" {
		location = j.Plan.OldPath
	}
	t, e := s.seedTorrent(ctx, j.Plan.Hash)
	if e != nil {
		return false, e
	}
	if !strings.HasPrefix(t.State, "stopped") || !seedAtLocation(t, j.Plan, location) {
		return false, fmt.Errorf("recheck requires stopped storage at the planned location")
	}
	if e := s.seedWrite(ctx, j, "recheck", ""); e != nil {
		return false, e
	}
	saw := false
	verified := false
	e = s.Seeding.wait(ctx, func() (bool, error) {
		t, e := s.seedTorrent(ctx, j.Plan.Hash)
		if e != nil {
			return false, e
		}
		if !seedAtLocation(t, j.Plan, location) {
			return false, fmt.Errorf("torrent storage changed during recheck")
		}
		if strings.HasPrefix(t.State, "checking") {
			saw = true
			return false, nil
		}
		if !saw {
			return false, nil
		}
		if !strings.HasPrefix(t.State, "stopped") {
			return false, fmt.Errorf("recheck did not leave torrent stopped")
		}
		fs, e := s.Seeding.Qbit.Files(ctx, j.Plan.Hash)
		if e != nil {
			return false, e
		}
		verified = *t.Progress == 1 && *t.AmountLeft == 0
		if len(fs) != len(j.Plan.Files) {
			return false, fmt.Errorf("torrent file inventory changed")
		}
		for i, f := range fs {
			if f.Name != j.Plan.Files[i].Relative || f.Size != j.Plan.Files[i].Size {
				return false, fmt.Errorf("torrent file identity changed")
			}
			verified = verified && *f.Progress == 1
		}
		return true, nil
	})
	return verified, e
}
func (s *Service) seedStart(ctx context.Context, j *SeedJob) error {
	if e := s.seedIOGate(ctx, j); e != nil {
		return e
	}
	location := j.Plan.Destination
	if j.Phase == "rolling_back" {
		location = j.Plan.OldPath
	}
	t, e := s.seedTorrent(ctx, j.Plan.Hash)
	if e != nil {
		return e
	}
	if t.State != "stoppedUP" || !seedAtLocation(t, j.Plan, location) || *t.Progress != 1 || *t.AmountLeft != 0 {
		return fmt.Errorf("resume requires a stopped, complete torrent at the verified location")
	}
	fs, e := s.Seeding.Qbit.Files(ctx, j.Plan.Hash)
	if e != nil {
		return e
	}
	if len(fs) != len(j.Plan.Files) {
		return fmt.Errorf("torrent file inventory changed before resume")
	}
	for i, f := range fs {
		if f.Name != j.Plan.Files[i].Relative || f.Size != j.Plan.Files[i].Size || *f.Progress != 1 {
			return fmt.Errorf("torrent files are not verified complete before resume")
		}
	}
	if e := s.seedWrite(ctx, j, "start", ""); e != nil {
		return e
	}
	return s.Seeding.wait(ctx, func() (bool, error) {
		t, e := s.seedTorrent(ctx, j.Plan.Hash)
		if e != nil {
			return false, e
		}
		if !seedAtLocation(t, j.Plan, location) || *t.Progress != 1 || *t.AmountLeft != 0 {
			return false, fmt.Errorf("torrent lost verified completion")
		}
		switch t.State {
		case "uploading", "stalledUP", "queuedUP", "forcedUP":
			return true, nil
		}
		return false, nil
	})
}
func (s *Service) seedLibraryMoves(ctx context.Context, j *SeedJob) error {
	for _, m := range j.Plan.LibraryMoves {
		if e := s.seedIOGate(ctx, j); e != nil {
			return e
		}
		c := s.LibraryClients[m.Kind]
		if e := s.seedBeforeLibraryMove(ctx, j, m); e != nil {
			return e
		}
		before, e := c.SeedCommands(ctx)
		if e != nil {
			return e
		}
		ids := map[int]bool{}
		for _, v := range before {
			ids[v.ID] = true
		}
		if e = s.seedIntent(ctx, j, "library_move:"+m.Kind, func() error { return c.MoveSeedLibrary(ctx, m.ID, m.To) }); e != nil {
			return e
		}
		name := "BulkMoveMovie"
		if m.Kind == "sonarr" {
			name = "BulkMoveSeries"
		}
		cmdID := 0
		e = s.Seeding.wait(ctx, func() (bool, error) {
			cs, e := c.SeedCommands(ctx)
			if e != nil {
				return false, e
			}
			status := ""
			for _, v := range cs {
				if ids[v.ID] || v.Name != name || v.Body.Destination != m.To {
					continue
				}
				matched := len(v.Body.Movies) == 1 && v.Body.Movies[0].ID == m.ID
				if m.Kind == "sonarr" {
					matched = len(v.Body.Series) == 1 && v.Body.Series[0].ID == m.ID
				}
				if !matched {
					continue
				}
				source := ""
				if m.Kind == "sonarr" {
					source = v.Body.Series[0].Source
				} else {
					source = v.Body.Movies[0].Source
				}
				if source != m.From {
					return false, fmt.Errorf("library move command source differs from preview")
				}
				if cmdID != 0 && cmdID != v.ID {
					return false, fmt.Errorf("ambiguous library move command")
				}
				cmdID = v.ID
				status = v.Status
			}
			// Inspect the whole response before accepting completion: a second
			// matching command makes attribution ambiguous regardless of order.
			switch status {
			case "completed":
				return true, nil
			case "failed", "aborted", "cancelled":
				return false, fmt.Errorf("library move command failed")
			case "":
				if cmdID != 0 {
					return false, fmt.Errorf("library move command disappeared")
				}
			case "queued", "started":
			default:
				return false, fmt.Errorf("library move command status unknown")
			}
			return false, nil
		})
		if e != nil {
			return e
		}
	}
	// Resolve paths again after Arr completes. Each previously matched file
	// must retain its file and title identities, and now be under the NAS root.
	for _, kind := range []string{"radarr", "sonarr"} {
		imports, e := s.LibraryClients[kind].SeedImports(ctx, j.Plan.Hash)
		if e != nil {
			return e
		}
		for i := range j.Plan.Files {
			f := &j.Plan.Files[i]
			if f.Match == nil || f.Match.Kind != kind {
				continue
			}
			var match *archivetier.SeedImport
			for _, im := range imports {
				if im.FileID == f.Match.FileID && im.ID == f.Match.ID && im.Source == f.Source && im.Size == f.Size {
					if match != nil && match.Path != im.Path {
						return fmt.Errorf("library match changed ambiguously")
					}
					match = &im
				}
			}
			if match == nil || !seedUnder(match.Path, s.Config.Seeding.LibraryRoots[kind]) {
				return fmt.Errorf("library did not move to approved NAS root")
			}
			local, e := seedMap(match.Path, []archivetier.MountMapping{s.Config.Seeding.NASMount})
			if e != nil {
				return e
			}
			ev, e := seedEvidence(local, f.Size)
			if e != nil {
				return e
			}
			f.Match = match
			f.LibraryEvidence = &ev
		}
	}
	return s.Store.SaveSeeding(j)
}

func (s *Service) seedBeforeLibraryMove(ctx context.Context, j *SeedJob, m SeedLibraryMove) error {
	if e := s.seedInitialTorrent(ctx, j); e != nil {
		return e
	}
	if e := s.seedOwnership(ctx, j); e != nil {
		return e
	}
	refs, err := s.LibraryClients[m.Kind].SeedLibraryReferences(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, ref := range refs {
		if ref.ID != m.ID || ref.FileID != 0 {
			continue
		}
		if ref.TitlePath != m.From || !seedUnder(ref.Root, s.Config.Seeding.DownloadMount.AppRoot) {
			return fmt.Errorf("library title path changed before move")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("library title disappeared before move")
	}
	size, err := s.seedLibraryMoveSize(m.From)
	if err != nil {
		return err
	}
	if size != m.Bytes {
		return fmt.Errorf("library move byte cost changed since preview")
	}
	dest, err := seedMap(m.To, []archivetier.MountMapping{s.Config.Seeding.NASMount})
	if err != nil {
		return err
	}
	ev, err := (archivetier.LocalFilesystem{}).Stat(dest)
	if err != nil || !ev.Directory {
		return fmt.Errorf("library move destination root is unavailable or symlinked")
	}
	for _, f := range j.Plan.Files {
		if f.Match == nil || f.Match.Kind != m.Kind || f.Match.ID != m.ID {
			continue
		}
		matched := false
		for _, ref := range refs {
			if ref.ID == m.ID && ref.FileID == f.Match.FileID && ref.Path == f.Match.Path && ref.Size == f.Size {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("library file identity changed before move")
		}
		local, err := seedMap(f.Match.Path, s.Config.Archive.Mounts)
		if err != nil {
			return err
		}
		ev, err := seedEvidence(local, f.Size)
		if err != nil || ev.Device != f.LibraryEvidence.Device || ev.Inode != f.LibraryEvidence.Inode || ev.Modified != f.LibraryEvidence.Modified {
			return fmt.Errorf("library file changed before move")
		}
	}
	return nil
}

func (s *Service) seedInitialTorrent(ctx context.Context, j *SeedJob) error {
	return s.seedSourceTorrent(ctx, j, j.Plan.OriginalDownloadPath)
}
func (s *Service) seedSourceTorrent(ctx context.Context, j *SeedJob, downloadPath string) error {
	t, err := s.seedTorrentWithDownloadPath(ctx, j.Plan.Hash, downloadPath)
	if err != nil {
		return err
	}
	if !seedAtLocation(t, j.Plan, j.Plan.OldPath) || *t.Progress != 1 || *t.AmountLeft != 0 {
		return fmt.Errorf("source torrent changed since confirmation")
	}
	switch t.State {
	case "uploading", "stalledUP", "queuedUP", "forcedUP", "stoppedUP":
	default:
		return fmt.Errorf("source torrent is no longer in a complete seeding state")
	}
	fs, err := s.Seeding.Qbit.Files(ctx, j.Plan.Hash)
	if err != nil {
		return err
	}
	if len(fs) != len(j.Plan.Files) {
		return fmt.Errorf("source file inventory changed since confirmation")
	}
	for i, f := range fs {
		if f.Name != j.Plan.Files[i].Relative || f.Size != j.Plan.Files[i].Size || *f.Progress != 1 {
			return fmt.Errorf("source file identity changed since confirmation")
		}
	}
	return nil
}
func (s *Service) seedDestination(f SeedFile, p SeedPlan) (string, error) {
	return seedMap(filepath.Join(p.Destination, f.Relative), []archivetier.MountMapping{s.Config.Seeding.NASMount})
}
func (s *Service) seedOld(f SeedFile) (string, error) {
	if !seedUnder(f.Source, s.Config.Seeding.DownloadRoot) {
		return "", fmt.Errorf("removal outside the download root")
	}
	local, e := seedMap(f.Source, []archivetier.MountMapping{s.Config.Seeding.DownloadMount})
	if e != nil {
		return "", e
	}
	got, e := seedEvidence(local, f.Size)
	if e != nil || got.Inode != f.Evidence.Inode || got.Device != f.Evidence.Device || got.Modified != f.Evidence.Modified {
		return "", fmt.Errorf("download disk source identity changed")
	}
	return local, nil
}
func (s *Service) seedOwnership(ctx context.Context, j *SeedJob) error {
	ts, e := s.Seeding.Qbit.Torrents(ctx)
	if e != nil {
		return e
	}
	for _, t := range ts {
		if t.Hash == j.Plan.Hash {
			continue
		}
		fs, e := s.Seeding.Qbit.PeerFiles(ctx, t)
		if e != nil {
			return e
		}
		for _, f := range fs {
			ps, e := seedPeerPaths(t, f.Name)
			if e != nil {
				return e
			}
			for _, p := range ps {
				for _, move := range j.Plan.LibraryMoves {
					if seedUnder(p, move.From) {
						return fmt.Errorf("another torrent references the library move folder")
					}
				}
				ev, ok, e := seedPeerEvidence(p, s.Config.Archive.Mounts)
				if e != nil {
					return e
				}
				for _, mine := range j.Plan.Files {
					if p == mine.Source || ok && ev.Device == mine.Evidence.Device && ev.Inode == mine.Evidence.Inode {
						return fmt.Errorf("cross-seeded torrent shares files")
					}
				}
			}
		}
	}
	return nil
}

func (s *Service) seedLibraryOwnership(ctx context.Context, p SeedPlan, allowMoves bool) error {
	for _, kind := range []string{"radarr", "sonarr"} {
		refs, err := s.LibraryClients[kind].SeedLibraryReferences(ctx)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			if !seedUnder(ref.TitlePath, ref.Root) || (ref.Path != "" && !seedUnder(ref.Path, ref.TitlePath)) {
				return fmt.Errorf("library ownership path escapes its title root")
			}
			for _, f := range p.Files {
				if seedUnder(f.Source, ref.TitlePath) || seedUnder(ref.TitlePath, p.ContentPath) {
					return fmt.Errorf("Arr still references torrent content as a library path")
				}
			}
			if ref.Path == "" || !seedUnder(ref.Path, s.Config.Seeding.DownloadMount.AppRoot) {
				continue
			}
			local, err := seedMap(ref.Path, s.Config.Archive.Mounts)
			if err != nil {
				return err
			}
			ev, err := seedEvidence(local, ref.Size)
			if err != nil {
				return err
			}
			for _, f := range p.Files {
				if ev.Inode != f.Evidence.Inode || ev.Device != f.Evidence.Device {
					continue
				}
				accounted := false
				if allowMoves && f.Match != nil && f.Match.Kind == kind && f.Match.ID == ref.ID && f.Match.FileID == ref.FileID && f.Match.Path == ref.Path {
					for _, move := range p.LibraryMoves {
						if move.Kind == kind && move.ID == ref.ID {
							accounted = true
						}
					}
				}
				if !accounted {
					return fmt.Errorf("Arr still references download disk source inode")
				}
			}
		}
	}
	return nil
}
func (s *Service) runSeed(j *SeedJob) {
	ctx := s.Seeding.Context
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := s.Seeding.Timeout
	if timeout <= 0 {
		timeout = 12 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := s.executeSeed(ctx, j)
	if err != nil {
		if !errors.Is(err, errOwnerStopped) || j.Phase != "stopped" {
			j.Phase = "halted"
		}
		if errors.Is(err, errOwnerStopped) {
			j.Error = "Stopped by owner"
		} else {
			j.Error = err.Error() + "; preserve state and inspect before retrying"
		}
	}
	j.Finished = time.Now().UTC()
	s.Store.SaveSeeding(j)
	s.Store.AuditDetail("seeding_result", nil, j)
}
func (s *Service) executeSeed(ctx context.Context, j *SeedJob) error {
	if e := s.seedIOGate(ctx, j); e != nil {
		return e
	}
	if e := s.seedInitialTorrent(ctx, j); e != nil {
		return e
	}
	if e := s.seedOwnership(ctx, j); e != nil {
		return e
	}
	for _, f := range j.Plan.Files {
		if _, e := s.seedOld(f); e != nil {
			return e
		}
	}
	if e := s.seedPhase(j, "library_move"); e != nil {
		return e
	}
	if len(j.Plan.LibraryMoves) > 0 {
		if e := s.seedLibraryMoves(ctx, j); e != nil {
			return e
		}
	}
	if e := s.seedLibraryOwnership(ctx, j.Plan, false); e != nil {
		return e
	}
	if e := s.seedIOGate(ctx, j); e != nil {
		return e
	}
	if j.Plan.Mode == "link" {
		if e := s.seedPhase(j, "linking"); e != nil {
			return e
		}
		for _, f := range j.Plan.Files {
			src, e := seedMap(f.Match.Path, []archivetier.MountMapping{s.Config.Seeding.NASMount})
			if e != nil {
				return e
			}
			dst, e := s.seedDestination(f, j.Plan)
			if e != nil {
				return e
			}
			if e = s.seedIntent(ctx, j, "link:"+f.Relative, func() error { return seedLink(src, dst, *f.LibraryEvidence) }); e != nil {
				return e
			}
		}
	} else {
		// qBittorrent owns the complete copy/removal in move mode. Never delete
		// anything ourselves here; destination absence prevents collision adoption.
		dest, e := seedMap(j.Plan.Destination, []archivetier.MountMapping{s.Config.Seeding.NASMount})
		if e != nil {
			return e
		}
		if e = seedAbsentDestination(dest); e != nil {
			return e
		}
	}
	if e := s.seedPhase(j, "moving"); e != nil {
		return e
	}
	if e := s.seedStop(ctx, j); e != nil {
		return e
	}
	if e := s.seedClearDownloadPath(ctx, j); e != nil {
		return e
	}
	if e := s.seedIOGate(ctx, j); e != nil {
		return e
	}
	if e := s.seedLibraryOwnership(ctx, j.Plan, false); e != nil {
		return e
	}
	if j.Plan.Mode == "link" {
		for _, f := range j.Plan.Files {
			if _, e := s.seedOld(f); e != nil {
				return e
			}
			dst, e := s.seedDestination(f, j.Plan)
			if e != nil {
				return e
			}
			ev, e := seedEvidence(dst, f.Size)
			if e != nil || ev.Device != f.LibraryEvidence.Device || ev.Inode != f.LibraryEvidence.Inode || ev.Modified != f.LibraryEvidence.Modified {
				return fmt.Errorf("link destination changed before location write")
			}
		}
	} else {
		dest, e := seedMap(j.Plan.Destination, []archivetier.MountMapping{s.Config.Seeding.NASMount})
		if e != nil {
			return e
		}
		if e = seedAbsentDestination(dest); e != nil {
			return e
		}
		if e = s.seedIntent(ctx, j, "prepare_destination", func() error { return seedPrepareDestination(dest) }); e != nil {
			return e
		}
	}
	if e := s.seedLocation(ctx, j, j.Plan.Destination); e != nil {
		return e
	}
	if e := s.seedPhase(j, "rechecking"); e != nil {
		return e
	}
	ok, e := s.seedRecheck(ctx, j)
	if e != nil {
		return e
	}
	if !ok {
		if j.Plan.Mode != "link" {
			return fmt.Errorf("moved torrent recheck failed")
		}
		if e = s.seedPhase(j, "rolling_back"); e != nil {
			return e
		}
		for _, f := range j.Plan.Files {
			if _, e = s.seedOld(f); e != nil {
				return e
			}
		}
		if e = s.seedLocation(ctx, j, j.Plan.OldPath); e != nil {
			return e
		}
		ok, e = s.seedRecheck(ctx, j)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("download disk rollback recheck failed")
		}
		if e = s.seedStart(ctx, j); e != nil {
			return e
		}
		for _, f := range j.Plan.Files {
			dst, e := s.seedDestination(f, j.Plan)
			if e != nil {
				return e
			}
			if e = s.seedIntent(ctx, j, "unlink_rollback:"+f.Relative, func() error { return seedRemove(dst, *f.LibraryEvidence, false) }); e != nil {
				return e
			}
		}
		j.Error = "NAS recheck failed; rolled back to verified download disk data"
		return s.seedPhase(j, "rolled_back")
	}
	j.NASVerified = true
	if e = s.Store.SaveSeeding(j); e != nil {
		return e
	}
	if e = s.seedStart(ctx, j); e != nil {
		return e
	}
	if j.Plan.Mode == "link" {
		if e = s.seedPhase(j, "cleaning"); e != nil {
			return e
		}
		if e = s.seedOwnership(ctx, j); e != nil {
			return e
		}
		for _, f := range j.Plan.Files {
			t, e := s.seedTorrent(ctx, j.Plan.Hash)
			if e != nil {
				return e
			}
			if !seedAtLocation(t, j.Plan, j.Plan.Destination) || *t.Progress != 1 || *t.AmountLeft != 0 {
				return fmt.Errorf("torrent no longer references verified NAS data")
			}
			if e = s.seedLibraryOwnership(ctx, j.Plan, false); e != nil {
				return e
			}
			old, e := s.seedOld(f)
			if e != nil {
				return e
			}
			dst, e := s.seedDestination(f, j.Plan)
			if e != nil {
				return e
			}
			ev, e := seedEvidence(dst, f.Size)
			if e != nil || ev.Inode != f.LibraryEvidence.Inode || ev.Device != f.LibraryEvidence.Device || ev.Modified != f.LibraryEvidence.Modified {
				return fmt.Errorf("verified NAS file changed before cleanup")
			}
			if e = s.seedIntent(ctx, j, "remove_download:"+f.Relative, func() error { return seedRemove(old, f.Evidence, true) }); e != nil {
				return e
			}
		}
	}
	return s.seedPhase(j, "complete")
}
