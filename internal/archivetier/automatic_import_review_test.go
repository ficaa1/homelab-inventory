package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type automaticRecoveryArr struct {
	*automaticArr
	recoveryClient *Client
}

func (a *automaticRecoveryArr) Recovery(ctx context.Context, j *Job) ([]Evidence, error) {
	return a.recoveryClient.Recovery(ctx, j)
}

// Serve the synthetic import contract and real recycle inspection without writes.
func recoveryImportClient(t *testing.T, bin string, mount MountMapping, history []map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != "GET" {
			t.Error("unexpected write")
			w.WriteHeader(400)
			return
		}
		switch req.URL.Path {
		case "/api/v3/manualimport":
			if req.URL.Query().Get("downloadId") != "hash" {
				t.Error("wrong selected download")
			}
			w.Write([]byte(`[]`))
		case "/api/v3/config/mediamanagement":
			json.NewEncoder(w).Encode(map[string]any{"recycleBin": bin, "useScriptImport": false})
		case "/api/v3/queue":
			w.Write(pagedBody(t, req, []json.RawMessage{}))
		case "/api/v3/history":
			records := []json.RawMessage{}
			for _, h := range history {
				b, _ := json.Marshal(h)
				records = append(records, b)
			}
			w.Write(pagedBody(t, req, records))
		default:
			t.Error("unexpected path", req.URL.Path)
			w.WriteHeader(400)
		}
	}))
	t.Cleanup(srv.Close)
	kind := "radarr"
	if mount.AppRoot == "/library" {
		kind = "sonarr"
	}
	client := NewClient(kind, srv.URL, "fixture", false)
	client.Safety.Mounts = []MountMapping{mount}
	client.FS = readonlyFixtureFS{}
	return client
}

func TestUnverifiableLibraryChangesCollectRealRecycleEvidence(t *testing.T) {
	for _, scenario := range []string{"missing fileId", "grab mismatch", "expired before observation", "rejected probe during import", "grab conflict", "missing grab", "queue conflict"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRestoreFixture(t)
			r, fake, _, now := runner(t)
			r.Config.LibraryRoots = []string{"/data/media/movies"}
			j := f.job
			j.Phase, j.Mode, j.PolicyVersion, j.Started, j.DownloadID = Downloading, Mode1080p, PolicyVersion, now, "hash"
			j.Recovery = nil
			j.Release.Title, j.Release.IndexerID = "selected release", 11
			fake.current = j.Target
			fake.current.Files = append([]File(nil), j.Target.Files...)
			fake.current.Files[0].ID += 100
			fake.current.Files[0].Size, fake.current.Files[0].Path = 5, "/data/media/movies/Example Movie (2025)/"+filepath.Base(f.replacement)
			history := automaticHistory(t, fake.current, now.Add(time.Minute))
			if scenario == "rejected probe during import" {
				r.Config.PlexKeys["radarr:293"] = "901"
				imported := fake.current
				fake.current = j.Target
				fake.queue = []QueueItem{{ID: 1, MovieID: j.Target.ID, DownloadID: "hash", State: "importPending", Status: "completed"}}
				j.ReleaseID = "selected"
				fake.items = fake.items[:1]
				fake.items[0].Movie.ID = j.Target.ID
				r.Probe = func(context.Context, File) (*MediaInfo, error) {
					fake.current, fake.queue = imported, []QueueItem{}
					return &MediaInfo{AudioChannels: 2, AudioCodec: "AAC", VideoCodec: "HEVC"}, nil
				}
			}
			switch scenario {
			case "grab conflict":
				j.Phase, fake.grabbedID = GrabIntent, "other"
			case "missing grab":
				j.Phase, j.DownloadID, j.Started, j.GrabChecks = GrabIntent, "", now.Add(-16*time.Minute), 3
				fake.noGrabHistory = true
			case "queue conflict":
				fake.queue = []QueueItem{{MovieID: j.Target.ID, DownloadID: "other"}}
			}
			if scenario == "missing fileId" {
				delete(history[0]["data"].(map[string]any), "fileId")
			}
			if scenario == "grab mismatch" {
				fake.grabbedID = "other"
			}
			if scenario == "expired before observation" {
				j.Started = now.Add(-25 * time.Hour)
			}
			client := recoveryImportClient(t, j.RecycleRoot, MountMapping{AppRoot: "/data", LocalRoot: f.root}, history)
			root, _, err := client.inspect(j.RecycleRoot)
			if err != nil {
				t.Fatal(err)
			}
			j.RecycleBaseline = []Evidence{root}
			r.Arr["radarr"] = &automaticRecoveryArr{automaticArr: &automaticArr{fakeArr: fake, client: client, t: t}, recoveryClient: client}
			if err := r.Store.Save(State{Job: &j, StreamQuietSince: now.Add(-5 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			st, err := r.Check(context.Background(), now.Add(2*time.Minute))
			if scenario == "queue conflict" {
				if err != nil || !testStateJob(st).HasImportIntent() || len(st.AbandonedDownloads) != 0 {
					t.Fatalf("changed library ended cleanly: %+v %v", st, err)
				}
				st, err = r.Check(context.Background(), now.Add(25*time.Hour))
			}
			if err == nil || testStateJob(st).Phase != Failed || !testStateJob(st).HoldsLease() || testStateJob(st).ImportedAt.IsZero() || !testStateJob(st).HasImportIntent() || len(testStateJob(st).Recovery) != 1 || len(st.History) != 1 || len(st.AbandonedDownloads) != 0 || strings.Contains(testStateJob(st).Summary, "Original library files retained") || fake.imports != 0 {
				t.Fatalf("early failure lost originals: %+v %v", testStateJob(st), err)
			}
			if _, err := RestorePlan(*testStateJob(st)); err != nil {
				t.Fatal("real originals not restorable", err)
			}
			if _, err := r.ClearFailed(context.Background(), now.Add(time.Hour), JobID(*testStateJob(st))); err == nil {
				t.Fatal("unattributed library change bypassed Clear", err)
			}
		})
	}
}

type productionGrabAutomaticArr struct{ *automaticArr }

func (a *productionGrabAutomaticArr) ReconcileDownload(ctx context.Context, j *Job) (string, error) {
	return a.client.ReconcileDownload(ctx, j)
}

func TestAutomaticImportUsesProductionGrabIdentity(t *testing.T) {
	for _, scenario := range []string{"success", "wrong indexer", "wrong title"} {
		t.Run(scenario, func(t *testing.T) {
			r, fake, _, now := runner(t)
			st := check(t, r, now)
			fake.current = replacement()
			history := automaticHistory(t, fake.current, now.Add(time.Minute))
			grab := map[string]any{"id": 2000, "seriesId": testStateJob(st).Target.ID, "date": now.Add(time.Second).Format(time.RFC3339), "sourceTitle": testStateJob(st).Release.Title, "eventType": "grabbed", "downloadId": "hash", "data": map[string]string{"indexerId": "10"}}
			if scenario == "wrong indexer" {
				grab["data"] = map[string]string{"indexerId": "99"}
			}
			if scenario == "wrong title" {
				grab["sourceTitle"] = "unrelated release"
			}
			history = append(history, grab)
			srv := lifecycleServer(t, history, []json.RawMessage{})
			defer srv.Close()
			r.Arr["sonarr"] = &productionGrabAutomaticArr{automaticArr: &automaticArr{fakeArr: fake, client: NewClient("sonarr", srv.URL, "fixture", false), t: t}}
			st, err := r.Check(context.Background(), now.Add(2*time.Minute))
			if scenario == "success" {
				if err != nil || testStateJob(st).Phase != Complete {
					t.Fatalf("production identity failed: %+v %v", testStateJob(st), err)
				}
			} else if err == nil || testStateJob(st).Phase != Failed || !testStateJob(st).HoldsLease() {
				t.Fatalf("wrong identity accepted: %+v %v", testStateJob(st), err)
			}
			if fake.grabs != 1 || fake.imports != 0 {
				t.Fatal("reconciliation issued writes")
			}
		})
	}
}

type importDuringPreflightArr struct {
	*automaticRecoveryArr
	root      string
	baseline  []Evidence
	localRoot string
}

func (a *importDuringPreflightArr) TargetPreflight(ctx context.Context, old Target, items []ManualItem, _ int64) (string, []Evidence, error) {
	if len(items) == 0 {
		return a.root, a.baseline, nil
	}
	// Arr recycles originals while Archive is inspecting the download. The
	// refreshed pre-import snapshot now includes those newly recycled files.
	for _, f := range old.Files {
		p := filepath.Join(a.localRoot, "recycle", filepath.Base(f.Path))
		file, err := os.Create(p)
		if err != nil {
			return "", nil, err
		}
		err = file.Truncate(f.Size)
		closeErr := file.Close()
		if err != nil {
			return "", nil, err
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
	}
	a.current, a.queue = replacement(), []QueueItem{}
	entries, err := a.recoveryClient.recycleEvidence(a.root)
	return a.root, append(append([]Evidence{}, a.baseline...), entries...), err
}

func TestAutomaticImportDuringPreflightKeepsPreGrabRecycleBaseline(t *testing.T) {
	r, fake, _, now := runner(t)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "recycle"), 0o755); err != nil {
		t.Fatal(err)
	}
	history := automaticHistory(t, replacement(), now.Add(time.Minute))
	client := recoveryImportClient(t, "/library/recycle", MountMapping{AppRoot: "/library", LocalRoot: root}, history)
	bin, _, err := client.inspect("/library/recycle")
	if err != nil {
		t.Fatal(err)
	}
	a := &importDuringPreflightArr{automaticRecoveryArr: &automaticRecoveryArr{automaticArr: &automaticArr{fakeArr: fake, client: client, t: t}, recoveryClient: client}, root: "/library/recycle", baseline: []Evidence{bin}, localRoot: root}
	r.Arr["sonarr"] = a
	check(t, r, now)
	fake.queue = []QueueItem{{ID: 1, SeriesID: 1, DownloadID: "hash", State: "importPending"}}
	st := check(t, r, now.Add(2*time.Minute))
	if st.Job.Phase != Complete || len(st.Job.Recovery) != 2 || fake.imports != 0 {
		t.Fatalf("preflight baseline hid automatic recovery: %+v", st.Job)
	}
	if _, err := RestorePlan(*st.Job); err != nil {
		t.Fatal("recycled originals not restorable", err)
	}
}

type importDuringManualListingArr struct {
	*automaticRecoveryArr
	replacement Target
}

func (a *importDuringManualListingArr) ReconcileDownload(ctx context.Context, j *Job) (string, error) {
	return a.client.ReconcileDownload(ctx, j)
}
func (a *importDuringManualListingArr) Manual(ctx context.Context, q QueueItem) ([]ManualItem, error) {
	// The tracked download has finished between the ready queue read and this GET.
	a.fakeArr.current, a.fakeArr.queue = a.replacement, []QueueItem{}
	return a.client.Manual(ctx, q)
}

func TestAutomaticImportCompletesDuringManualListing(t *testing.T) {
	f := newRestoreFixture(t)
	r, fake, _, now := runner(t)
	r.Config.LibraryRoots = []string{"/data/media/movies"}
	j := f.job
	j.Phase, j.Mode, j.PolicyVersion, j.Started, j.DownloadID = Downloading, Mode1080p, PolicyVersion, now, "hash"
	j.ManuallyApproved = true
	j.Release.Title, j.Release.IndexerID = "selected release", 11
	j.Recovery = nil
	fake.current = j.Target
	fake.queue = []QueueItem{{ID: 1, MovieID: j.Target.ID, DownloadID: "hash", State: "importPending", Status: "completed"}}
	imported := j.Target
	imported.Files = append([]File(nil), j.Target.Files...)
	imported.Files[0].ID += 100
	imported.Files[0].Size, imported.Files[0].Path = 5, "/data/media/movies/Example Movie (2025)/"+filepath.Base(f.replacement)
	history := automaticHistory(t, imported, now.Add(time.Minute))
	history = append(history, map[string]any{"id": 2000, "movieId": j.Target.ID, "eventType": "grabbed", "date": now.Add(time.Second).Format(time.RFC3339), "sourceTitle": j.Release.Title, "downloadId": "hash", "data": map[string]string{"indexerId": "11"}})
	client := recoveryImportClient(t, j.RecycleRoot, MountMapping{AppRoot: "/data", LocalRoot: f.root}, history)
	bin, _, err := client.inspect(j.RecycleRoot)
	if err != nil {
		t.Fatal(err)
	}
	j.RecycleBaseline = []Evidence{bin}
	a := &importDuringManualListingArr{automaticRecoveryArr: &automaticRecoveryArr{automaticArr: &automaticArr{fakeArr: fake, client: client, t: t}, recoveryClient: client}, replacement: imported}
	r.Arr["radarr"] = a
	if err := r.Store.Save(State{Job: &j, StreamQuietSince: now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	st, err := r.Check(context.Background(), now.Add(2*time.Minute))
	_, restoreErr := RestorePlan(*st.Job)
	if err != nil || st.Job.Phase != Complete || st.Job.CommandID != 0 || st.Job.ImportedAt.IsZero() || len(st.Job.Recovery) != 1 || restoreErr != nil || fake.imports != 0 {
		t.Fatalf("completed selected auto-import was not reconciled")
	}
}
