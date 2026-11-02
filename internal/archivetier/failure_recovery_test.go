package archivetier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A fixture view exposes real temporary files through the read-only
// inspection interface; the production client never mutates this view.
type readonlyFixtureFS struct{ LocalFilesystem }

func (f readonlyFixtureFS) Volume(p string) (Volume, error) {
	v, e := f.LocalFilesystem.Volume(p)
	v.ReadOnly = true
	return v, e
}

type failureRecoveryArr struct {
	*fakeArr
	client     *Client
	command    string
	currentErr error
	scans      int
	t          *testing.T
}

func (a *failureRecoveryArr) Command(context.Context, int) (string, error) { return a.command, nil }
func (a *failureRecoveryArr) Current(ctx context.Context, t Target) (Target, error) {
	if a.currentErr != nil {
		return Target{}, a.currentErr
	}
	return a.fakeArr.Current(ctx, t)
}
func (a *failureRecoveryArr) Recovery(ctx context.Context, j *Job) ([]Evidence, error) {
	if j.Phase == Failed {
		a.t.Fatal("failure committed before recovery inspection")
	}
	a.scans++
	return a.client.Recovery(ctx, j)
}

func TestFailedImportAndVerificationRetainRestorableOriginals(t *testing.T) {
	for _, scenario := range []string{"command failed", "command aborted", "command cancelled", "verification failed", "inspection unavailable", "file missing", "timeout", "ambiguous import"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRestoreFixture(t)
			r, _, _, now := runner(t)
			r.Config.LibraryRoots = []string{"/data/media/movies"}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != "GET" || req.URL.Path != "/api/v3/config/mediamanagement" {
					t.Error("unexpected inspection request")
					w.WriteHeader(400)
					return
				}
				w.Write([]byte(`{"recycleBin":"/data/.recycle/radarr","useScriptImport":false}`))
			}))
			defer srv.Close()
			client := NewClient("radarr", srv.URL, "fixture", false)
			client.Safety.Mounts = []MountMapping{{AppRoot: "/data", LocalRoot: f.root}}
			client.FS = readonlyFixtureFS{}
			root, _, err := client.inspect(f.job.RecycleRoot)
			if err != nil {
				t.Fatal(err)
			}
			j := f.job
			j.Mode = Mode1080p
			j.PolicyVersion = PolicyVersion
			j.Recovery = nil
			j.RecycleBaseline = []Evidence{root}
			j.Started = now
			j.Phase = Verifying
			j.CommandID = 42
			a := &failureRecoveryArr{fakeArr: &fakeArr{current: j.Target, present: true}, client: client, command: "completed", t: t}
			switch scenario {
			case "command failed":
				a.command = "failed"
			case "command aborted":
				a.command = "aborted"
			case "command cancelled":
				a.command = "cancelled"
			case "inspection unavailable":
				a.currentErr = errors.New("unavailable")
			case "file missing":
				a.current.Files = append([]File(nil), j.Target.Files...)
				a.current.Files[0].ID += 100
				a.current.Files[0].Path = "/data/media/movies/Example Movie (2025)/replacement.mkv"
				a.current.Files[0].Size = 5
				a.current.Files[0].Quality = qual(1080)
				a.current.Files[0].MediaInfo = &MediaInfo{AudioChannels: 5.1, AudioCodec: "EAC3", VideoCodec: "HEVC"}
				a.present = false
				if !Verified(j.Target, a.current, r.Config, Mode1080p) {
					t.Fatal("file existence fixture must pass replacement verification first")
				}
			case "timeout":
				a.command = "started"
				j.Started = now.Add(-time.Duration(r.Config.TimeoutHours+1) * time.Hour)
			case "ambiguous import":
				j.Phase = ImportIntent
			}
			r.Arr["radarr"] = a
			if err := r.Store.Save(State{Job: &j}); err != nil {
				t.Fatal(err)
			}
			st, err := r.Check(context.Background(), now)
			if err == nil || testStateJob(st).Phase != Failed || a.scans != 1 {
				t.Fatalf("failure not inspected: scans=%d job=%+v err=%v", a.scans, testStateJob(st), err)
			}
			persisted, err := r.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if len(testStateJob(persisted).Recovery) != 1 {
				t.Fatalf("original evidence lost: %+v", testStateJob(persisted))
			}
			if _, err = f.restorer(false).Restore(context.Background(), *testStateJob(persisted)); err != nil {
				t.Fatal(err)
			}
			res, err := f.restorer(true).Restore(context.Background(), *testStateJob(persisted))
			if err != nil || len(res.Restore) != 1 || !res.Restore[0].Done || inode(t, f.original) != f.inode || exists(f.recycled) {
				t.Fatalf("failed job restore: %+v %v", res, err)
			}
			if !exists(filepath.Join(f.root, ".archive-quarantine", filepath.Base(f.replacement))) {
				t.Fatal("replacement not quarantined")
			}
		})
	}
}

func TestRestoreMissingEvidencePrintsInspectionProcedure(t *testing.T) {
	f := newRestoreFixture(t)
	f.job.Phase = Failed
	f.job.Recovery = nil
	_, err := f.restorer(false).Restore(context.Background(), f.job)
	if err == nil {
		t.Fatal("missing evidence accepted")
	}
	for _, text := range []string{"stop the service", "state.json/audit.jsonl", "target.files", "recycleBaseline", "seven-day", "without -apply"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("missing inspection guidance %q: %v", text, err)
		}
	}
}
