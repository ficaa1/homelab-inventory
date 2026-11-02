package archivetier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// restoreFixture lays out a real library, recycle bin and replacement under a
// temporary directory mapped at /data, as Radarr would leave them after a
// copy-mode replacement of Example Movie.
type restoreFixture struct {
	root, original, recycled, replacement string
	job                                   Job
	inode                                 uint64
	posts                                 []map[string]any
	srv                                   *httptest.Server
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	st, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := &restoreFixture{root: t.TempDir()}
	movie := filepath.Join(f.root, "media", "movies", "Example Movie (2025)")
	bin := filepath.Join(f.root, ".recycle", "radarr", "Example Movie (2025)")
	for _, d := range []string{movie, bin} {
		if err := os.MkdirAll(d, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	name := "Example Movie.2025.2160p.UHD.Blu-ray.Remux.DV.HDR.HEVC.TrueHD.Atmos.7.1-CiNEPHiLES.mkv"
	f.original = filepath.Join(movie, name)
	f.recycled = filepath.Join(bin, name)
	f.replacement = filepath.Join(movie, "Example Movie.2025.1080p.BluRay.x265-GRP.mkv")
	if err := os.WriteFile(f.recycled, []byte("original 2160p bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.replacement, []byte("1080p"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f.recycled, f.replacement} {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.inode = inode(t, f.recycled)
	size := int64(len("original 2160p bytes"))
	f.job = Job{Phase: Complete, RecycleRoot: "/data/.recycle/radarr",
		Target:   Target{Kind: "radarr", ID: 293, Title: "Example Movie", Files: []File{{ID: 292, Path: "/data/media/movies/Example Movie (2025)/" + name, Size: size}}},
		Recovery: []Evidence{{Path: "/data/.recycle/radarr/Example Movie (2025)/" + name, Size: size}}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["method"], body["path"] = r.Method, r.URL.Path
		f.posts = append(f.posts, body)
		w.Write([]byte(`{"id":77}`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *restoreFixture) restorer(apply bool) *Restorer {
	return &Restorer{Mounts: []MountMapping{{AppRoot: "/data", LocalRoot: f.root}}, LibraryRoots: []string{"/data/media/movies"},
		Quarantine: []string{"/data/media/movies/Example Movie (2025)/" + filepath.Base(f.replacement)}, QuarantineRoot: "/data/.archive-quarantine",
		Apply: apply, Rescan: true, Client: NewClient("radarr", f.srv.URL, "key", apply)}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestRestoreOnRealFilesystem(t *testing.T) {
	f := newRestoreFixture(t)
	res, err := f.restorer(false).Restore(context.Background(), f.job)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || !exists(f.recycled) || exists(f.original) || !exists(f.replacement) || len(f.posts) != 0 || !strings.HasPrefix(res.Rescan, "would POST") {
		t.Fatalf("dry-run changed something: %+v posts=%v", res, f.posts)
	}
	if len(res.Restore) != 1 || res.Restore[0].Mode != "-rw-r--r--" || res.Restore[0].Inode != f.inode {
		t.Fatalf("dry-run plan incomplete: %+v", res)
	}
	replacementInode := inode(t, f.replacement)

	res, err = f.restorer(true).Restore(context.Background(), f.job)
	if err != nil {
		t.Fatal(err)
	}
	if exists(f.recycled) || inode(t, f.original) != f.inode {
		t.Fatal("original not moved back as the same file")
	}
	if b, _ := os.ReadFile(f.original); string(b) != "original 2160p bytes" {
		t.Fatal("restored bytes differ")
	}
	quarantined := filepath.Join(f.root, ".archive-quarantine", filepath.Base(f.replacement))
	if exists(f.replacement) || inode(t, quarantined) != replacementInode {
		t.Fatal("replacement not moved aside intact")
	}
	if st, _ := os.Stat(f.original); st.Mode().Perm() != 0o644 {
		t.Fatal("permissions changed")
	}
	if len(f.posts) != 1 || f.posts[0]["method"] != "POST" || f.posts[0]["path"] != "/api/v3/command" || f.posts[0]["name"] != "RescanMovie" || f.posts[0]["movieId"] != float64(293) {
		t.Fatalf("rescan not requested once after the restore: %v", f.posts)
	}
	if !res.Applied || !res.Restore[0].Done || !res.Quarantine[0].Done || !strings.HasPrefix(res.Rescan, "accepted as command 77") {
		t.Fatalf("result does not say what was done: %+v", res)
	}
}

// The rescan is a write: a client without apply never sends it, even when
// the files were moved.
func TestRestoreRescanIsDryRunGated(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.restorer(true)
	r.Client = NewClient("radarr", f.srv.URL, "key", false)
	if _, err := r.Restore(context.Background(), f.job); err == nil || len(f.posts) != 0 {
		t.Fatalf("rescan escaped a dry-run client: %v %v", err, f.posts)
	}
}

func TestRestoreRefusesBeforeChangingAnything(t *testing.T) {
	for _, scenario := range []string{"destination occupied", "wrong size", "symlinked original", "unreadable for library owner", "outside library", "phase", "quarantine outside library", "different filesystem"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.restorer(true)
			job := f.job
			switch scenario {
			case "destination occupied":
				// The replacement would be quarantined first: a late refusal
				// would leave that move behind.
				if err := os.WriteFile(f.original, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "wrong size":
				job.Target.Files[0].Size++
				job.Recovery[0].Size++
			case "symlinked original":
				real := f.recycled + ".real"
				os.Rename(f.recycled, real)
				os.Symlink(real, f.recycled)
			case "unreadable for library owner":
				os.Chmod(f.recycled, 0o200)
			case "outside library":
				r.LibraryRoots = []string{"/data/media/series"}
				r.Quarantine = nil
			case "phase":
				job.Phase = Downloading
			case "quarantine outside library":
				r.Quarantine = []string{"/data/.recycle/radarr/Example Movie (2025)/" + filepath.Base(f.recycled)}
			case "different filesystem":
				shm, err := os.MkdirTemp("/dev/shm", "restore-test-")
				if err != nil {
					t.Skip("no second filesystem")
				}
				t.Cleanup(func() { os.RemoveAll(shm) })
				var a, b syscall.Stat_t
				if syscall.Stat(shm, &a) != nil || syscall.Stat(f.root, &b) != nil || a.Dev == b.Dev {
					t.Skip("no second filesystem")
				}
				os.MkdirAll(filepath.Join(shm, "radarr", "Example Movie (2025)"), 0o775)
				os.Rename(f.recycled, filepath.Join(shm, "radarr", "Example Movie (2025)", filepath.Base(f.recycled)))
				if !exists(filepath.Join(shm, "radarr", "Example Movie (2025)", filepath.Base(f.recycled))) {
					data, _ := os.ReadFile(f.recycled)
					os.WriteFile(filepath.Join(shm, "radarr", "Example Movie (2025)", filepath.Base(f.recycled)), data, 0o644)
				}
				r.Mounts = append(r.Mounts, MountMapping{AppRoot: "/recycle", LocalRoot: shm})
				job.RecycleRoot = "/recycle/radarr"
				job.Recovery[0].Path = "/recycle/radarr/Example Movie (2025)/" + filepath.Base(f.recycled)
			}
			before := map[string]bool{f.recycled: exists(f.recycled), f.original: exists(f.original), f.replacement: exists(f.replacement)}
			if _, err := r.Restore(context.Background(), job); err == nil {
				t.Fatal("unsafe restore accepted")
			}
			for p, was := range before {
				if exists(p) != was {
					t.Fatalf("refused restore still changed %s", p)
				}
			}
			if len(f.posts) != 0 {
				t.Fatal("refused restore requested a rescan")
			}
		})
	}
}
