package mediapolicy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

type SeedConfig struct {
	Enabled      bool   `json:"enabled"`
	SeedRoot     string `json:"seedRoot"`
	DownloadRoot string `json:"downloadRoot"`
	// Writable views. NASMount must be the same mount for library and links:
	// linkat cannot cross two bind mounts, even when their device IDs match.
	DownloadMount archivetier.MountMapping `json:"downloadMount"`
	NASMount      archivetier.MountMapping `json:"nasMount"`
	NASMinFreeGB  float64                  `json:"nasMinFreeGB"`
	LibraryRoots  map[string]string        `json:"libraryRoots"`
}

func DefaultSeedConfig() SeedConfig {
	return SeedConfig{SeedRoot: "/data/media/.seeding", DownloadMount: archivetier.MountMapping{AppRoot: "/data/downloads"}, NASMount: archivetier.MountMapping{AppRoot: "/data/media"}, DownloadRoot: "/data/downloads/completed", NASMinFreeGB: 500, LibraryRoots: map[string]string{"radarr": "/data/media/movies", "sonarr": "/data/media/series"}}
}
func (c SeedConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !seedUnder(c.DownloadRoot, c.DownloadMount.AppRoot) || !seedUnder(c.SeedRoot, c.NASMount.AppRoot) || seedUnder(c.SeedRoot, c.DownloadMount.AppRoot) || seedUnder(c.DownloadMount.AppRoot, c.SeedRoot) || !seedUnder(c.DownloadMount.LocalRoot, c.DownloadMount.LocalRoot) || !seedUnder(c.NASMount.LocalRoot, c.NASMount.LocalRoot) || math.IsNaN(c.NASMinFreeGB) || math.IsInf(c.NASMinFreeGB, 0) || c.NASMinFreeGB < 0 || c.NASMinFreeGB > 1e6 {
		return fmt.Errorf("invalid seeding configuration")
	}
	for _, k := range []string{"radarr", "sonarr"} {
		if !seedUnder(c.LibraryRoots[k], c.NASMount.AppRoot) || seedUnder(c.LibraryRoots[k], c.SeedRoot) || seedUnder(c.SeedRoot, c.LibraryRoots[k]) || seedUnder(c.LibraryRoots[k], c.DownloadMount.AppRoot) || seedUnder(c.DownloadMount.AppRoot, c.LibraryRoots[k]) {
			return fmt.Errorf("invalid seeding library root")
		}
	}
	return nil
}

type SeedFile struct {
	Relative        string                  `json:"relative"`
	Source          string                  `json:"source"`
	Size            int64                   `json:"size"`
	Match           *archivetier.SeedImport `json:"match,omitempty"`
	Evidence        archivetier.Evidence    `json:"evidence"`
	LibraryEvidence *archivetier.Evidence   `json:"libraryEvidence,omitempty"`
}
type SeedLibraryMove struct {
	Kind  string `json:"kind"`
	ID    int    `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Bytes int64  `json:"bytes"`
}
type SeedPlan struct {
	Hash        string `json:"hash"`
	Name        string `json:"name"`
	OldPath     string `json:"oldPath"`
	ContentPath string `json:"contentPath"`
	// Retained for audit and operator reconciliation; never restored automatically.
	OriginalDownloadPath string            `json:"originalDownloadPath"`
	Destination          string            `json:"destination"`
	Mode                 string            `json:"mode"`
	Size                 int64             `json:"size"`
	DownloadBytesFreed   int64             `json:"nvmeBytesFreed"` // Wire name retained for compatibility.
	NASBytesCost         int64             `json:"nasBytesCost"`
	Files                []SeedFile        `json:"files"`
	LibraryMoves         []SeedLibraryMove `json:"libraryMoves"`
	Titles               []string          `json:"titles"`
	Refusal              string            `json:"refusal,omitempty"`
	PlanHash             string            `json:"planHash"`
	DryRun               bool              `json:"dryRun"`
}
type SeedJob struct {
	StopRequested bool `json:"stopRequested,omitempty"`
	NASVerified   bool `json:"nasVerified,omitempty"`
	stopping      bool
	Plan          SeedPlan  `json:"plan"`
	Phase         string    `json:"phase"`
	Intent        string    `json:"intent,omitempty"`
	Started       time.Time `json:"started"`
	Finished      time.Time `json:"finished,omitempty"`
	Error         string    `json:"error,omitempty"`
	// Live is the mover's latest torrent read, attached to status copies only
	// and never persisted.
	Live *SeedLive `json:"live,omitempty"`
}

// SeedLive is qBittorrent's state and completed fraction for the moving
// torrent, as the mover last read it. During a recheck the fraction is the
// share of pieces checked so far.
type SeedLive struct {
	State    string    `json:"state"`
	Progress float64   `json:"progress"`
	Observed time.Time `json:"observed"`
}

// seedLiveMaxAge drops a reading the mover has not refreshed recently.
const seedLiveMaxAge = 2 * time.Minute

type seedObservation struct {
	mu   sync.Mutex
	hash string
	live SeedLive
}

func (s *Service) observeSeed(t SeedTorrent) {
	if t.Progress == nil {
		return
	}
	s.seedLive.mu.Lock()
	defer s.seedLive.mu.Unlock()
	s.seedLive.hash = t.Hash
	s.seedLive.live = SeedLive{State: t.State, Progress: *t.Progress, Observed: time.Now().UTC()}
}

// withSeedLive returns a copy of an active job carrying the latest reading of
// its own torrent, or the job unchanged.
func (s *Service) withSeedLive(j *SeedJob) *SeedJob {
	if j == nil || !seedPhaseActive(j.Phase) {
		return j
	}
	s.seedLive.mu.Lock()
	hash, live := s.seedLive.hash, s.seedLive.live
	s.seedLive.mu.Unlock()
	if hash != j.Plan.Hash || time.Since(live.Observed) > seedLiveMaxAge {
		return j
	}
	c := *j
	c.Live = &live
	return &c
}

func (j *SeedJob) holds() bool {
	return j != nil && j.Phase != "complete" && j.Phase != "rolled_back" && j.Phase != "stopped"
}

func seedPhaseActive(phase string) bool {
	switch phase {
	case "queued", "library_move", "linking", "moving", "rechecking", "rolling_back", "cleaning":
		return true
	}
	return false
}
func ValidSeedHash(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	b, e := hex.DecodeString(s)
	return e == nil && len(b)*2 == len(s) && strings.ToLower(s) == s
}
func seedHash(p SeedPlan) string {
	p.PlanHash = ""
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type SeedMover struct {
	Qbit   *SeedQbit
	Active func(context.Context) (bool, error)
	// Context is the daemon lifetime; cancellation halts without replay.
	Context      context.Context
	PollInterval time.Duration
	// QueueRetry spaces a queued move's waits on Plex or a busy title;
	// default 30s.
	QueueRetry time.Duration
	Timeout    time.Duration
	// Free is injectable for filesystem fixture tests, production uses statfs.
	Free func(string) (int64, error)
}

func (s *Service) seedGate(ctx context.Context, p SeedPlan) error {
	if !s.Config.Seeding.Enabled || s.Seeding == nil || s.Seeding.Qbit == nil {
		return fmt.Errorf("seeding mover is not configured")
	}
	if s.Store.WriteAllowed() != nil {
		return fmt.Errorf("Could not save progress")
	}
	st := s.Store.Snapshot()
	if st.ArchivePaused {
		return fmt.Errorf("Paused by owner")
	}
	if st.Seeding.holds() {
		if seedPhaseActive(st.Seeding.Phase) {
			return fmt.Errorf("Another move in progress")
		}
		return fmt.Errorf("Previous move needs attention")
	}
	if st.Archive.Job != nil && st.Archive.Job.HoldsLease() {
		if st.Archive.Job.Phase == archivetier.Downloading {
			return fmt.Errorf("Archive job in progress")
		}
		return fmt.Errorf("Archive needs attention")
	}
	for _, j := range st.Archive.FailedJobs {
		if strings.EqualFold(j.DownloadID, p.Hash) {
			return fmt.Errorf("Archive needs attention")
		}
	}
	for _, d := range st.Archive.AbandonedDownloads {
		if strings.EqualFold(d.DownloadID, p.Hash) {
			return fmt.Errorf("Cleanup needs attention")
		}
	}
	for _, t := range p.Titles {
		if l, ok := st.Leases[t]; ok {
			s.Store.mu.Lock()
			err := s.Store.leaseBusyLocked(t, l)
			s.Store.mu.Unlock()
			return err
		}
	}
	if err := s.seedLibraryOwnership(ctx, p, true); err != nil {
		return err
	}
	if s.Seeding.Active == nil {
		return fmt.Errorf("Could not check Plex")
	}
	active, e := s.Seeding.Active(ctx)
	if e != nil {
		return e
	}
	if active {
		return fmt.Errorf("Plex is in use")
	}
	return s.Seeding.Qbit.Reviewed(ctx)
}
func (s *Service) seedFree(p SeedPlan) error {
	free, e := s.Seeding.free(s.Config.Seeding.NASMount.LocalRoot)
	if e != nil {
		return e
	}
	if p.NASBytesCost > 0 && (free < p.NASBytesCost || free-p.NASBytesCost < int64(s.Config.Seeding.NASMinFreeGB*1e9)) {
		return fmt.Errorf("Not enough NAS space")
	}
	return nil
}
func (m *SeedMover) free(p string) (int64, error) {
	if m.Free != nil {
		return m.Free(p)
	}
	v, e := (archivetier.LocalFilesystem{}).Volume(p)
	return v.Free, e
}
func (s *Service) seedPlan(ctx context.Context, hash string) (SeedPlan, error) {
	p := SeedPlan{Hash: hash, Mode: "move", Files: []SeedFile{}, LibraryMoves: []SeedLibraryMove{}, Titles: []string{}, DryRun: s.DryRun}
	if !ValidSeedHash(hash) {
		return p, fmt.Errorf("invalid torrent hash")
	}
	if !s.Config.Seeding.Enabled || s.Seeding == nil {
		return p, fmt.Errorf("seeding mover is not configured")
	}
	q := s.Seeding.Qbit
	ts, e := q.Torrents(ctx)
	if e != nil {
		return p, e
	}
	var torrent *SeedTorrent
	for i := range ts {
		if ts[i].Hash == hash {
			torrent = &ts[i]
		}
	}
	if torrent == nil {
		return p, fmt.Errorf("torrent is no longer on the download disk")
	}
	p.Name, p.OldPath, p.ContentPath = torrent.Name, torrent.SavePath, torrent.ContentPath
	p.OriginalDownloadPath = *torrent.DownloadPath
	p.Destination = filepath.Join(s.Config.Seeding.SeedRoot, hash)
	cfg := s.Config.Seeding
	if !seedUnder(p.OldPath, cfg.DownloadRoot) {
		p.Size, p.Mode, p.Destination = torrent.Size, "", ""
		return p, fmt.Errorf("Already on NAS")
	}
	if !seedUnder(p.ContentPath, p.OldPath) {
		return p, fmt.Errorf("Outside the download folder")
	}
	if *torrent.Progress != 1 || *torrent.AmountLeft != 0 {
		return p, fmt.Errorf("torrent is not complete")
	}
	switch torrent.State {
	case "uploading", "stalledUP", "queuedUP", "forcedUP", "stoppedUP":
	default:
		return p, fmt.Errorf("Waiting for seeding")
	}
	if *torrent.AutoTMM {
		return p, fmt.Errorf("Turn off automatic management")
	}
	fs, e := q.Files(ctx, hash)
	if e != nil {
		return p, e
	}
	imports := []archivetier.SeedImport{}
	for _, kind := range []string{"radarr", "sonarr"} {
		c := s.LibraryClients[kind]
		if c == nil {
			return p, fmt.Errorf("library applications are not configured")
		}
		is, err := c.SeedImports(ctx, hash)
		if err != nil {
			return p, err
		}
		imports = append(imports, is...)
	}
	titles := map[string]bool{}
	moves := map[string]SeedLibraryMove{}
	seen := map[string]bool{}
	for _, f := range fs {
		if !seedRelative(f.Name) || f.Size <= 0 || *f.Progress != 1 || seen[f.Name] {
			return p, fmt.Errorf("torrent file path escapes root, is repeated or incomplete")
		}
		seen[f.Name] = true
		src := filepath.Join(p.OldPath, f.Name)
		if !seedUnder(src, p.ContentPath) {
			return p, fmt.Errorf("torrent file outside its content path")
		}
		local, err := seedMap(src, s.Config.Archive.Mounts)
		if err != nil {
			return p, err
		}
		ev, err := seedEvidence(local, f.Size)
		if err != nil {
			return p, err
		}
		sf := SeedFile{Relative: f.Name, Source: src, Size: f.Size, Evidence: ev}
		for _, im := range imports {
			if im.Source != src || im.Size != f.Size {
				continue
			}
			if sf.Match != nil && (sf.Match.Path != im.Path || sf.Match.Kind != im.Kind || sf.Match.ID != im.ID) {
				return p, fmt.Errorf("ambiguous library match")
			}
			if !seedUnder(im.Path, im.TitlePath) || !seedUnder(im.TitlePath, im.Root) {
				return p, fmt.Errorf("library file path outside its root")
			}
			if !seedUnder(im.Root, cfg.DownloadMount.AppRoot) && !seedUnder(im.Root, cfg.LibraryRoots[im.Kind]) {
				return p, fmt.Errorf("library root is not approved")
			}
			lp, err := seedMap(im.Path, s.Config.Archive.Mounts)
			if err != nil {
				return p, err
			}
			le, err := seedEvidence(lp, im.Size)
			if err != nil {
				return p, err
			}
			sf.Match = &im
			sf.LibraryEvidence = &le
			key := archivetier.TitleKey(im.Kind, im.ID)
			titles[key] = true
			if seedUnder(im.Root, cfg.DownloadMount.AppRoot) {
				if seedUnder(src, im.TitlePath) || seedUnder(im.TitlePath, p.ContentPath) {
					return p, fmt.Errorf("library move overlaps torrent content path")
				}
				// Moving the whole item may include files outside this torrent. Account
				// for the entire item's library bytes, never just this matched file.
				if _, ok := moves[key]; !ok {
					size, err := s.seedLibraryMoveSize(im.TitlePath)
					if err != nil {
						return p, err
					}
					moves[key] = SeedLibraryMove{im.Kind, im.ID, im.TitlePath, cfg.LibraryRoots[im.Kind], size}
				}
			}
		}
		n, err := seedLinks(local)
		if err != nil {
			return p, err
		}
		if n > 1 && (sf.Match == nil || sf.LibraryEvidence.Device != ev.Device || sf.LibraryEvidence.Inode != ev.Inode || n != 2 || !seedUnder(sf.Match.Root, cfg.DownloadMount.AppRoot)) {
			return p, fmt.Errorf("Download disk file has other hardlinks; bytes cannot be freed safely")
		}
		p.Size += f.Size
		p.DownloadBytesFreed += f.Size
		p.Files = append(p.Files, sf)
	}
	if p.Size != torrent.Size {
		return p, fmt.Errorf("torrent total size differs from file inventory")
	}
	// Check every client hash, including ones outside the configured root. A
	// failed or escaping peer inventory cannot prove exclusive ownership.
	for _, other := range ts {
		if other.Hash == hash {
			continue
		}
		ofs, err := q.PeerFiles(ctx, other)
		if err != nil {
			return p, err
		}
		for _, f := range ofs {
			ops, err := seedPeerPaths(other, f.Name)
			if err != nil {
				return p, err
			}
			for _, op := range ops {
				for _, move := range moves {
					if seedUnder(op, move.From) {
						return p, fmt.Errorf("another torrent references the library move folder")
					}
				}
				for _, mine := range p.Files {
					if mine.Source == op {
						return p, fmt.Errorf("cross-seeded torrent shares files")
					}
				}
				oe, ok, err := seedPeerEvidence(op, s.Config.Archive.Mounts)
				if err != nil {
					return p, err
				}
				for _, mine := range p.Files {
					if ok && oe.Device == mine.Evidence.Device && oe.Inode == mine.Evidence.Inode {
						return p, fmt.Errorf("cross-seeded torrent shares files")
					}
				}
			}
		}
	}
	p.Mode = "link"
	for _, f := range p.Files {
		if f.Match == nil {
			p.Mode = "move"
		}
	}
	if p.Mode == "move" {
		p.NASBytesCost = p.Size
	}
	for t := range titles {
		p.Titles = append(p.Titles, t)
	}
	sort.Strings(p.Titles)
	for _, t := range p.Titles {
		if m, ok := moves[t]; ok {
			p.LibraryMoves = append(p.LibraryMoves, m)
			p.NASBytesCost += m.Bytes
		}
	}
	// Fresh destination must not exist, including a symlink or stale operation.
	dest, err := seedMap(p.Destination, []archivetier.MountMapping{cfg.NASMount})
	if err != nil {
		return p, err
	}
	if err = seedAbsentDestination(dest); err != nil {
		return p, err
	}
	if e = s.seedGate(ctx, p); e != nil {
		return p, e
	}
	if e = s.seedFree(p); e != nil {
		return p, e
	}
	p.PlanHash = seedHash(p)
	return p, nil
}

func (s *Service) seedLibraryMoveSize(appPath string) (int64, error) {
	local, err := seedMap(appPath, s.Config.Archive.Mounts)
	if err != nil {
		return 0, err
	}
	files, err := (archivetier.LocalFilesystem{}).Walk(local)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, file := range files {
		if file.Size < 0 || total > math.MaxInt64-file.Size {
			return 0, fmt.Errorf("library move size exceeds limit")
		}
		total += file.Size
	}
	if total <= 0 {
		return 0, fmt.Errorf("download disk library move size unknown")
	}
	return total, nil
}

type SeedRequest struct {
	Hash     string `json:"hash"`
	PlanHash string `json:"planHash,omitempty"`
	Apply    bool   `json:"apply,omitempty"`
}

func (s *Service) handleSeeding(w http.ResponseWriter, r *http.Request) {
	if s.Seeding == nil || !s.Config.Seeding.Enabled {
		writeJSON(w, 503, map[string]string{"error": "seeding mover is not configured"})
		return
	}
	if r.Method == "GET" {
		ts, e := s.Seeding.Qbit.Torrents(r.Context())
		if e != nil {
			writeJSON(w, 503, map[string]string{"error": e.Error()})
			return
		}
		out := []SeedPlan{}
		for _, t := range ts {
			p, e := s.seedPlan(r.Context(), t.Hash)
			if e != nil {
				p.Refusal = e.Error()
				p.PlanHash = ""
			}
			out = append(out, p)
		}
		snap := s.Store.Snapshot()
		queue, results := seedQueueView(snap)
		writeJSON(w, 200, map[string]any{"items": out, "job": s.withSeedLive(snap.Seeding), "queue": queue, "queueResults": results})
		return
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return
	}
	var in SeedRequest
	d := json.NewDecoder(io.LimitReader(r.Body, 4096))
	d.DisallowUnknownFields()
	if d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF || !ValidSeedHash(in.Hash) {
		writeJSON(w, 400, map[string]string{"error": "invalid seeding request"})
		return
	}
	if !s.tick.TryLock() {
		writeJSON(w, 503, map[string]string{"error": archiveBusyMessage})
		return
	}
	locked := true
	defer func() {
		if locked {
			s.tick.Unlock()
		}
	}()
	p, e := s.seedPlan(r.Context(), in.Hash)
	if e != nil {
		writeJSON(w, 409, map[string]string{"error": e.Error()})
		return
	}
	if r.URL.Path == "/seeding/preview" {
		if in.Apply || in.PlanHash != "" {
			writeJSON(w, 400, map[string]string{"error": "preview accepts only hash"})
			return
		}
		writeJSON(w, 200, p)
		return
	}
	if !in.Apply || s.DryRun {
		writeJSON(w, 409, map[string]string{"error": "confirmation and applied service required"})
		return
	}
	if in.PlanHash == "" || in.PlanHash != p.PlanHash {
		writeJSON(w, 409, map[string]string{"error": "plan changed; review a new preview"})
		return
	}
	if len(s.Store.Snapshot().SeedQueue) > 0 {
		writeJSON(w, 409, map[string]string{"error": "Queue in progress"})
		return
	}
	j, release, e := s.beginSeed(p, false)
	if e != nil {
		writeJSON(w, 409, map[string]string{"error": e.Error()})
		return
	}
	locked = false
	s.seedWG.Add(1)
	go func() {
		defer s.seedWG.Done()
		defer s.tick.Unlock()
		defer release()
		s.runSeed(j)
	}()
	writeJSON(w, 202, map[string]any{"queued": true, "hash": p.Hash})
}

func (s *Service) WaitSeeding() { s.seedWG.Wait() }
