package mediapolicy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

// arr is a read-mostly application fake for supervisor tests. Every write is
// counted; reads fail if their context has already expired.
type arr struct {
	status          string
	original        archivetier.Target
	current         archivetier.Target
	searches, grabs int
	imports         int
	queue           []archivetier.QueueItem
	queueReads      int
}

func (a *arr) live(ctx context.Context) error { return ctx.Err() }
func (a *arr) Targets(ctx context.Context) ([]archivetier.Target, error) {
	if a.original.ID == 0 {
		return []archivetier.Target{}, a.live(ctx)
	}
	return []archivetier.Target{a.original}, a.live(ctx)
}
func (a *arr) Current(ctx context.Context, _ archivetier.Target) (archivetier.Target, error) {
	return a.current, a.live(ctx)
}
func (a *arr) Queue(ctx context.Context) ([]archivetier.QueueItem, error) {
	a.queueReads++
	if a.queue != nil {
		return a.queue, a.live(ctx)
	}
	return []archivetier.QueueItem{}, a.live(ctx)
}
func (a *arr) Search(ctx context.Context, _ archivetier.Target) ([]archivetier.Release, error) {
	a.searches++
	return nil, a.live(ctx)
}
func (a *arr) Grab(context.Context, archivetier.Release) error { a.grabs++; return nil }
func (a *arr) Manual(ctx context.Context, _ archivetier.QueueItem) ([]archivetier.ManualItem, error) {
	return nil, a.live(ctx)
}
func (a *arr) Import(context.Context, []map[string]any) (int, error) { a.imports++; return 1, nil }
func (a *arr) Command(ctx context.Context, _ int) (string, error)    { return a.status, a.live(ctx) }
func (a *arr) Preflight(ctx context.Context) error                   { return a.live(ctx) }
func (a *arr) Present(ctx context.Context, _ archivetier.Target) (bool, error) {
	return true, a.live(ctx)
}
func (a *arr) Opted(ctx context.Context, _ archivetier.Target) (bool, error) {
	return true, a.live(ctx)
}
func (a *arr) TargetPreflight(ctx context.Context, _ archivetier.Target, _ []archivetier.ManualItem, _ int64) (string, []archivetier.Evidence, error) {
	return "", nil, errors.New("no new writes expected")
}
func (a *arr) Recovery(ctx context.Context, j *archivetier.Job) ([]archivetier.Evidence, error) {
	return []archivetier.Evidence{{Path: "/data/.recycle/radarr/Example Movie (2025)/old.mkv", Size: j.Target.Files[0].Size}}, a.live(ctx)
}
func (a *arr) Lifecycle(ctx context.Context, _ *archivetier.Job, _ archivetier.Target) error {
	return a.live(ctx)
}

func (a *arr) GrabbedDownload(ctx context.Context, j *archivetier.Job) (string, error) {
	return j.DownloadID, a.live(ctx)
}

type quietPlex struct{}

func (quietPlex) Active(context.Context) (bool, error) { return false, nil }
func (quietPlex) Recent(context.Context, time.Time, int) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func archiveConfig() archivetier.Config {
	c := archivetier.DefaultConfig()
	c.Start, c.End = "00:00", "05:00"
	c.DownloadRoots = []string{"/data/downloads/completed"}
	c.LibraryRoots = []string{"/data/media/movies"}
	c.IndexerIDs = map[string]int{"sonarr": 10, "radarr": 11}
	c.PlexKeys = map[string]string{"radarr:293": "15850"}
	return c
}

// supervised builds the real archive runner behind the supervisor, with a
// persisted job when phase is set.
func supervised(t *testing.T, phase archivetier.Phase, started time.Time) (*Service, *Store, *arr) {
	t.Helper()
	s, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	old := archivetier.Target{Kind: "radarr", ID: 293, Title: "Example Movie", Files: []archivetier.File{{ID: 292, Path: "/data/media/movies/Example Movie (2025)/Example Movie.2025.2160p.mkv", Size: 77253337510}}}
	old.Files[0].Quality.Quality.Resolution = 2160
	fake := &arr{status: "completed", original: old, current: archivetier.Target{Kind: "radarr", ID: 293, Title: "Example Movie", Files: []archivetier.File{{ID: 300, Path: "/data/media/movies/Example Movie (2025)/Example Movie.2025.1080p.mkv", Size: 7 * archivetier.GB, MediaInfo: &archivetier.MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}}}}}
	fake.current.Files[0].Quality.Quality.Resolution = 1080
	fake.current.Files[0].Quality.Quality.Name = "Bluray-1080p"
	if phase != "" {
		job := &archivetier.Job{Mode: archivetier.Mode1080p, PolicyVersion: archivetier.PolicyVersion, Target: old, Phase: phase, Started: started, CommandID: 7, DownloadID: "selected", RecycleRoot: "/data/.recycle/radarr", RecycleBaseline: []archivetier.Evidence{}}
		if err = s.SaveArchive(archivetier.State{Job: job}); err != nil {
			t.Fatal(err)
		}
	}
	runner := &archivetier.Runner{Probe: func(_ context.Context, f archivetier.File) (*archivetier.MediaInfo, error) { return f.MediaInfo, nil }, Config: archiveConfig(), Store: &archivetier.Store{LoadState: s.LoadArchive, SaveState: s.SaveArchive, AuditRecord: s.Audit}, Arr: map[string]archivetier.ArrAPI{"radarr": fake, "sonarr": &arr{}}, Plex: quietPlex{}, DState: func() (int, error) { return 0, nil }, Acquire: func(t archivetier.Target) (func(), error) { return s.Acquire(t.Key(), "archive") }}
	svc := &Service{Store: s, Config: Config{ArchiveEnabled: true, Archive: archiveConfig()}, Archive: runner, Interval: time.Minute}
	return svc, s, fake
}

func TestDisabledArchiveCannotHideUnfinishedJob(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _, _ := supervised(t, archivetier.Verifying, now.Add(-time.Hour))
	svc.Config.ArchiveEnabled = false
	if err := svc.Tick(context.Background(), now); err == nil || svc.Status().Healthy {
		t.Fatal("unfinished job hidden by disabling archive")
	}
}
