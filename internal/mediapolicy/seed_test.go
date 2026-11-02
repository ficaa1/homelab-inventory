package mediapolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
)

const seedTestHash = "0123456789abcdef0123456789abcdef01234567"

type seedFixture struct {
	stopAction                                                          string
	stopLibrary                                                         bool
	mu                                                                  sync.Mutex
	t                                                                   *testing.T
	svc                                                                 *Service
	q, arr                                                              *httptest.Server
	download, nas                                                       string
	save, state, relative, root, library, version                       string
	complete, matched, active, cross, failCheck, ambiguous              bool
	writes                                                              []string
	checkTicks, checks                                                  int
	moved                                                               bool
	backend, downloadPath                                               string
	extensions                                                          bool
	startLocations                                                      []string
	duplicateCommand, adoptSourceOnStart, failAllChecks                 bool
	failAction                                                          string
	replaceSourceOnStop                                                 bool
	streamOnStop                                                        bool
	wrongCommandSource                                                  bool
	commands                                                            []archivetier.SeedCommand
	fallbackDownloadPath, unclearDownloadResponse, loseDownloadResponse bool
	downloadPathVerified                                                bool
	peerState, peerFile                                                 string
	peerDownloadPath                                                    string
}

func seedFake(t *testing.T) *seedFixture {
	t.Helper()
	f := &seedFixture{t: t, save: "/data/downloads/completed", state: "stalledUP", relative: "Release/video.mkv", root: "/data/media/movies", library: "/data/media/movies/Movie/video.mkv", version: "6.3.0.10514", complete: true, matched: true, commands: []archivetier.SeedCommand{}}
	base := t.TempDir()
	f.backend = "2.0.14.0"
	f.download = filepath.Join(base, "download")
	f.nas = filepath.Join(base, "nas")
	f.put("/data/downloads/completed/Release/video.mkv")
	f.put(f.library)
	os.MkdirAll(filepath.Join(f.nas, "media", ".seeding"), 0755)
	f.q = httptest.NewServer(http.HandlerFunc(f.qbit))
	f.arr = httptest.NewServer(http.HandlerFunc(f.arrHTTP))
	t.Cleanup(f.q.Close)
	t.Cleanup(f.arr.Close)
	store, e := OpenStore(filepath.Join(base, "state"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(store.Close)
	c := DefaultConfig()
	c.ArchiveEnabled = false
	c.Seeding.Enabled = true
	c.Seeding.DownloadMount = archivetier.MountMapping{AppRoot: "/data/downloads", LocalRoot: f.download}
	c.Seeding.NASMount = archivetier.MountMapping{AppRoot: "/data/media", LocalRoot: filepath.Join(f.nas, "media")}
	c.Archive.Mounts = []archivetier.MountMapping{{AppRoot: "/data/downloads", LocalRoot: f.download}, {AppRoot: "/data/media", LocalRoot: filepath.Join(f.nas, "media")}}
	h := SharedHTTP(&Transport{Apply: true, Store: store, QbitOrigin: f.q.URL, RadarrOrigin: f.arr.URL, SonarrOrigin: f.arr.URL})
	f.svc = &Service{Config: c, Store: store, LibraryClients: map[string]*archivetier.Client{}, Seeding: &SeedMover{Qbit: NewSeedQbit(f.q.URL, "fixture-user", "fixture-password", h), PollInterval: time.Millisecond, QueueRetry: time.Millisecond, Timeout: 30 * time.Second, Free: func(string) (int64, error) { return 1e12, nil }, Active: func(context.Context) (bool, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.active, nil }}}
	for _, kind := range []string{"radarr", "sonarr"} {
		cl := archivetier.NewClient(kind, f.arr.URL, "fixture-key", true)
		cl.HTTP = h
		cl.Safety = c.Archive
		f.svc.LibraryClients[kind] = cl
	}
	// Separate Sonarr fake so app/version identities can be reviewed exactly.
	son := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/system/status":
			json.NewEncoder(w).Encode(map[string]any{"appName": "Sonarr", "version": "4.0.19.2979"})
		case "/api/v3/history":
			json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 0, "records": []any{}})
		case "/api/v3/series":
			json.NewEncoder(w).Encode([]any{})
		default:
			t.Errorf("unexpected Sonarr %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(son.Close)
	f.svc.LibraryClients["sonarr"].Base = son.URL
	return f
}
func (f *seedFixture) local(p string) string {
	if strings.HasPrefix(p, "/data/downloads/") {
		return filepath.Join(f.download, strings.TrimPrefix(p, "/data/downloads/"))
	}
	return filepath.Join(f.nas, strings.TrimPrefix(p, "/data/"))
}
func (f *seedFixture) put(p string) {
	f.t.Helper()
	l := f.local(p)
	if e := os.MkdirAll(filepath.Dir(l), 0755); e != nil {
		f.t.Fatal(e)
	}
	if e := os.WriteFile(l, []byte("torrent bytes"), 0644); e != nil {
		f.t.Fatal(e)
	}
}
func (f *seedFixture) qbit(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := json.NewEncoder(w)
	if r.Method == "POST" {
		r.ParseForm()
		a := strings.TrimPrefix(r.URL.Path, "/api/v2/")
		if a == "auth/login" {
			w.WriteHeader(http.StatusNoContent) // qBittorrent 5.1+
			return
		}
		id := r.Form.Get("hashes")
		if a == "torrents/setDownloadPath" {
			id = r.Form.Get("id")
			if len(r.Form) != 2 || len(r.Form["path"]) != 1 || r.Form.Get("path") != "" {
				f.t.Error("download path write was not an exact clear")
			}
		}
		if id != seedTestHash {
			f.t.Errorf("wrong write hash")
		}
		f.writes = append(f.writes, a)
		if j := f.svc.Store.Snapshot().Seeding; j == nil || j.Intent == "" {
			f.t.Error("write without persisted intent")
		}
		if f.ambiguous && a == "torrents/setLocation" {
			w.WriteHeader(500)
			return
		}
		if a == f.failAction {
			w.WriteHeader(403)
			return
		}
		switch a {
		case "torrents/setDownloadPath":
			if f.state != "stoppedUP" || !f.complete {
				f.t.Error("download path cleared before stopped completion")
			}
			j := f.svc.Store.Snapshot().Seeding
			if j.Intent != "setDownloadPath" || j.Plan.OriginalDownloadPath != f.downloadPath {
				f.t.Error("original download path not persisted before clear")
			}
			f.downloadPath = ""
			if f.fallbackDownloadPath {
				f.downloadPath = "/data/downloads/qbittorrent/global-temp"
			}
			if f.loseDownloadResponse {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					f.t.Error(err)
				} else {
					conn.Close()
				}
				return
			}
			if f.unclearDownloadResponse {
				fmt.Fprint(w, "unexpected")
			}
		case "torrents/stop":
			if f.streamOnStop {
				f.active = true
			}
			f.state = "stoppedUP"
			if f.replaceSourceOnStop {
				os.WriteFile(f.local(filepath.Join(f.save, f.relative)), []byte("changed bytes"), 0644)
			}
		case "torrents/start":
			f.startLocations = append(f.startLocations, f.save)
			if f.adoptSourceOnStart {
				f.library = "/data/downloads/completed/Release/video.mkv"
				f.root = "/data/downloads/completed"
			}
			f.state = "stalledUP"
		case "torrents/recheck":
			f.checks++
			f.checkTicks = 2
		case "torrents/setLocation":
			if f.downloadPath != "" || !f.downloadPathVerified {
				f.t.Error("storage moved before empty download path readback")
			}
			dst := r.Form.Get("location")
			old := f.local(filepath.Join(f.save, f.relative))
			newp := f.local(filepath.Join(dst, f.relative))
			if _, e := os.Stat(newp); os.IsNotExist(e) {
				b, e := os.ReadFile(old)
				if e != nil {
					f.t.Error(e)
				}
				os.MkdirAll(filepath.Dir(newp), 0755)
				os.WriteFile(newp, b, 0644)
				os.Remove(old)
			}
			f.save = dst
			f.state = "stoppedUP"
		default:
			f.t.Errorf("forbidden qBittorrent write %s", a)
		}
		if f.stopAction == a {
			if w := f.post("/seeding/stop", OwnerControlRequest{Hash: seedTestHash, Apply: true, RequestedBy: "owner"}); w.Code != 200 {
				f.t.Errorf("stop request: %d %s", w.Code, w.Body)
			}
		}
		return
	}
	switch r.URL.Path {
	case "/api/v2/app/version":
		fmt.Fprint(w, "v5.2.3")
	case "/api/v2/app/buildInfo":
		enc.Encode(map[string]string{"libtorrent": f.backend})
	case "/api/v2/app/preferences":
		enc.Encode(map[string]any{"incomplete_files_ext": f.extensions, "temp_path_enabled": true, "temp_path": "/data/downloads/qbittorrent/global-temp"})
	case "/api/v2/torrents/info":
		if f.downloadPath == "" && f.state == "stoppedUP" {
			f.downloadPathVerified = true
		}
		state := f.state
		if f.checkTicks > 0 {
			state = "checkingUP"
			f.checkTicks--
		} else if f.failCheck && (f.checks == 1 || f.failAllChecks && f.checks > 0) {
			state = "stoppedDL"
		}
		progress, left := 1.0, int64(0)
		if !f.complete || f.failCheck && (f.checks == 1 || f.failAllChecks && f.checks > 0) && f.checkTicks == 0 && state == "stoppedDL" {
			progress, left = .5, 6
		}
		manual := false
		ts := []SeedTorrent{{Hash: seedTestHash, Name: "Fixture Movie", SavePath: f.save, DownloadPath: &f.downloadPath, AutoTMM: &manual, ContentPath: filepath.Join(f.save, "Release"), Size: 13, Progress: &progress, AmountLeft: &left, State: state}}
		if f.cross {
			other := ts[0]
			other.Hash = strings.Repeat("a", 40)
			ts = append(ts, other)
		}
		if f.peerState != "" {
			dl := f.peerDownloadPath
			peer := SeedTorrent{Hash: strings.Repeat("b", 40), Name: "Peer", SavePath: "/data/downloads/completed", DownloadPath: &dl, AutoTMM: &manual, Size: -1, Progress: new(float64), AmountLeft: new(int64), State: f.peerState}
			ts = append(ts, peer)
		}
		enc.Encode(ts)
	case "/api/v2/torrents/files":
		if r.URL.Query().Get("hash") == strings.Repeat("b", 40) {
			fs := []SeedTorrentFile{}
			if f.peerFile != "" {
				fs = append(fs, SeedTorrentFile{Index: 0, Name: f.peerFile, Size: 13, Progress: new(float64)})
			}
			enc.Encode(fs)
			return
		}
		progress := 1.0
		if f.failCheck && (f.checks == 1 || f.failAllChecks && f.checks > 0) && f.checkTicks == 0 {
			progress = .5
		}
		enc.Encode([]SeedTorrentFile{{Index: 0, Name: f.relative, Size: 13, Progress: &progress}})
	default:
		f.t.Errorf("unexpected qBittorrent read %s", r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *seedFixture) arrHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	enc := json.NewEncoder(w)
	file := archivetier.File{ID: 2, Path: f.library, Size: 13}
	title := map[string]any{"id": 1, "title": "Movie", "path": filepath.Dir(f.library), "rootFolderPath": f.root, "hasFile": true, "movieFile": file, "tags": []int{}}
	switch r.URL.Path {
	case "/api/v3/system/status":
		enc.Encode(map[string]string{"appName": "Radarr", "version": f.version})
	case "/api/v3/history":
		hs := []archivetier.HistoryItem{}
		// Like the real Arr, the download ID filter is case-sensitive.
		if id := r.URL.Query().Get("downloadId"); f.matched && (id == "" || id == strings.ToUpper(seedTestHash)) {
			hs = append(hs, archivetier.HistoryItem{ID: 1, MovieID: 1, DownloadID: strings.ToUpper(seedTestHash), EventType: "downloadFolderImported", Data: map[string]string{"fileId": "2", "droppedPath": "/data/downloads/completed/Release/video.mkv"}})
		}
		enc.Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(hs), "records": hs})
	case "/api/v3/movie/1":
		enc.Encode(title)
	case "/api/v3/movie":
		enc.Encode([]any{title})
	case "/api/v3/tag":
		enc.Encode([]any{})
	case "/api/v3/command":
		enc.Encode(f.commands)
	case "/api/v3/movie/editor":
		oldTitle := filepath.Dir(f.library)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["rootFolderPath"] != "/data/media/movies" || body["moveFiles"] != true || body["deleteFiles"] != true || len(body) != 4 {
			f.t.Errorf("broad editor write %v", body)
		}
		if j := f.svc.Store.Snapshot().Seeding; j == nil || j.Intent != "library_move:radarr" {
			f.t.Error("editor without intent")
		}
		old := f.local(f.library)
		f.root = "/data/media/movies"
		f.library = "/data/media/movies/Movie/video.mkv"
		newp := f.local(f.library)
		b, e := os.ReadFile(old)
		if e != nil {
			f.t.Error(e)
		}
		os.MkdirAll(filepath.Dir(newp), 0755)
		os.WriteFile(newp, b, 0644)
		os.Remove(old)
		f.moved = true
		cmd := archivetier.SeedCommand{ID: 99, Name: "BulkMoveMovie", Status: "completed"}
		cmd.Body.Destination = f.root
		// Decode through JSON to populate the anonymous movie-ID slice.
		if f.wrongCommandSource {
			oldTitle = "/data/downloads/Movies/Other"
		}
		b, _ = json.Marshal(map[string]any{"destinationRootFolder": f.root, "movies": []any{map[string]any{"movieId": 1, "sourcePath": oldTitle}}})
		json.Unmarshal(b, &cmd.Body)
		f.commands = append(f.commands, cmd)
		if f.duplicateCommand {
			cmd.ID++
			f.commands = append(f.commands, cmd)
		}
		if f.stopLibrary {
			if w := f.post("/seeding/stop", OwnerControlRequest{Hash: seedTestHash, Apply: true}); w.Code != 200 {
				f.t.Error("library stop failed")
			}
		}
		enc.Encode([]any{})
	default:
		f.t.Errorf("unexpected Arr %s %s", r.Method, r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *seedFixture) plan() SeedPlan {
	f.t.Helper()
	p, e := f.svc.seedPlan(context.Background(), seedTestHash)
	if e != nil {
		f.t.Fatal(e)
	}
	return p
}
func (f *seedFixture) run(p SeedPlan) *SeedJob {
	f.t.Helper()
	b, _ := json.Marshal(SeedRequest{Hash: p.Hash, PlanHash: p.PlanHash, Apply: true})
	r := httptest.NewRequest("POST", "/seeding/move", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer fixture-token")
	w := httptest.NewRecorder()
	f.svc.Handler("fixture-token").ServeHTTP(w, r)
	if w.Code != 202 {
		f.t.Fatalf("move %d %s", w.Code, w.Body.String())
	}
	f.svc.tick.Lock()
	f.svc.tick.Unlock()
	return f.svc.Store.Snapshot().Seeding
}
func TestSeedLinkSuccess(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	if p.Mode != "link" || p.NASBytesCost != 0 || p.DownloadBytesFreed != 13 {
		t.Fatal(p)
	}
	j := f.run(p)
	if j.Phase != "complete" {
		t.Fatal(j.Error)
	}
	if _, e := os.Stat(f.local(p.Files[0].Source)); !os.IsNotExist(e) {
		t.Fatal("Download not freed")
	}
	a, _ := os.Stat(f.local(f.library))
	b, _ := os.Stat(f.local(filepath.Join(p.Destination, p.Files[0].Relative)))
	if !os.SameFile(a, b) {
		t.Fatal("NAS is not a library hardlink")
	}
	if len(f.svc.Store.Snapshot().Leases) != 0 {
		t.Fatal("lease retained after completion")
	}
}
func TestSeedRecheckFailureRollsBack(t *testing.T) {
	f := seedFake(t)
	f.failCheck = true
	p := f.plan()
	j := f.run(p)
	if j.Phase != "rolled_back" {
		t.Fatal(j.Error)
	}
	if _, e := os.Stat(f.local(p.Files[0].Source)); e != nil {
		t.Fatal("Download deleted before proof")
	}
	if _, e := os.Stat(f.local(filepath.Join(p.Destination, p.Files[0].Relative))); !os.IsNotExist(e) {
		t.Fatal("failed links retained")
	}
	if f.save != p.OldPath || f.state != "stalledUP" {
		t.Fatal("torrent not restored seeding")
	}
}
func TestSeedMove(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	if p.Mode != "move" || p.NASBytesCost != 13 {
		t.Fatal(p)
	}
	j := f.run(p)
	if j.Phase != "complete" {
		t.Fatal(j.Error)
	}
	if _, e := os.Stat(f.local(p.Files[0].Source)); !os.IsNotExist(e) {
		t.Fatal("client did not free Download")
	}
}
func TestSeedDownloadLibraryStepZero(t *testing.T) {
	f := seedFake(t)
	f.root = "/data/downloads/Movies"
	f.library = "/data/downloads/Movies/Movie/video.mkv"
	p := f.local(f.library)
	os.MkdirAll(filepath.Dir(p), 0755)
	if e := os.Link(f.local("/data/downloads/completed/Release/video.mkv"), p); e != nil {
		t.Fatal(e)
	}
	plan := f.plan()
	if len(plan.LibraryMoves) != 1 || plan.NASBytesCost != 13 {
		t.Fatal(plan)
	}
	j := f.run(plan)
	if j.Phase != "complete" || !f.moved {
		t.Fatal(j.Error)
	}
	if _, e := os.Stat(p); !os.IsNotExist(e) {
		t.Fatal("Download library link retained")
	}
}
func TestSeedAmbiguousWriteHaltsAndRetainsLease(t *testing.T) {
	f := seedFake(t)
	f.ambiguous = true
	p := f.plan()
	j := f.run(p)
	if j.Phase != "halted" || j.Intent != "setLocation" {
		t.Fatal(j)
	}
	if f.svc.Store.Snapshot().Leases["radarr:1"].Owner != "seeding" {
		t.Fatal("lease lost")
	}
	if _, e := os.Stat(f.local(p.Files[0].Source)); e != nil {
		t.Fatal("ambiguous write removed source")
	}
	n := len(f.writes)
	f.svc.Tick(context.Background(), time.Now())
	if len(f.writes) != n {
		t.Fatal("ambiguous write retried")
	}
}
func TestSeedRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		change       func(*seedFixture)
	}{
		{"active", "Plex is in use", func(f *seedFixture) { f.active = true }},
		{"unconfigured", "not configured", func(f *seedFixture) { f.svc.Seeding.Qbit.User = "" }},
		{"unknown-streams", "stream status unknown", func(f *seedFixture) {
			f.svc.Seeding.Active = func(context.Context) (bool, error) { return false, fmt.Errorf("stream status unknown") }
		}},
		{"incomplete", "not complete", func(f *seedFixture) { f.complete = false }},
		{"cross", "cross-seeded", func(f *seedFixture) { f.cross = true }},
		{"escape", "escapes root", func(f *seedFixture) { f.relative = "../elsewhere" }},
		{"version", "unsupported radarr", func(f *seedFixture) { f.version = "6.4.0" }},
		{"lease", "Busy", func(f *seedFixture) {
			_, e := f.svc.Store.Acquire("radarr:1", "seeding")
			if e != nil {
				t.Fatal(e)
			}
		}},
		{"floor", "NAS space", func(f *seedFixture) {
			f.matched = false
			f.svc.Seeding.Free = func(string) (int64, error) { return 500e9, nil }
		}},
		{"archive", "Archive job in progress", func(f *seedFixture) {
			f.svc.Store.SaveArchive(archivetier.State{Job: &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 9}, Phase: archivetier.Downloading}})
		}},
		{"symlink", "symlinked", func(f *seedFixture) {
			p := f.local("/data/downloads/completed/Release/video.mkv")
			os.Remove(p)
			os.Symlink(f.local(f.library), p)
		}},
		{"hardlink", "other hardlinks", func(f *seedFixture) {
			os.Link(f.local("/data/downloads/completed/Release/video.mkv"), filepath.Join(f.download, "unknown"))
		}},
		{"outside", "Already on NAS", func(f *seedFixture) { f.save = "/data/elsewhere" }},
		{"destination", "destination already", func(f *seedFixture) { os.MkdirAll(filepath.Join(f.nas, "media", ".seeding", seedTestHash), 0755) }},
		{"poison", "save progress", func(f *seedFixture) { f.svc.Store.poison = fmt.Errorf("fixture disk failure") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedFake(t)
			tc.change(f)
			_, e := f.svc.seedPlan(context.Background(), seedTestHash)
			if e == nil || !strings.Contains(e.Error(), tc.reason) {
				t.Fatalf("wanted %q, got %v", tc.reason, e)
			}
			if len(f.writes) != 0 {
				t.Fatal("refusal wrote")
			}
		})
	}
}
func TestSeedGateMessageFollowsPhase(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	j := &SeedJob{Plan: p, Phase: "moving"}
	j.Plan.Name = "Fixture Release"
	f.svc.Store.SaveSeeding(j)
	if e := f.svc.seedGate(context.Background(), p); e == nil || !strings.Contains(e.Error(), "Another move in progress") {
		t.Fatal("running move message:", e)
	}
	for _, phase := range []string{"halted", "failed", "not_a_phase"} {
		j.Phase = phase
		f.svc.Store.SaveSeeding(j)
		e := f.svc.seedGate(context.Background(), p)
		if e == nil || !strings.Contains(e.Error(), "Previous move needs attention") {
			t.Fatalf("%s move message: %v", phase, e)
		}
		if strings.Contains(e.Error(), "running") {
			t.Fatalf("%s move reported as running: %v", phase, e)
		}
	}
}

func TestSeedExactRemoval(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	extra := f.local("/data/downloads/completed/Release/keep.txt")
	os.WriteFile(extra, []byte("keep"), 0644)
	j := f.run(p)
	if j.Phase != "complete" {
		t.Fatal(j.Error)
	}
	if _, e := os.Stat(extra); e != nil {
		t.Fatal("unlisted file removed")
	}
	outside := p.Files[0]
	outside.Source = "/data/downloads/Movies/victim"
	if _, e := f.svc.seedOld(outside); e == nil {
		t.Fatal("outside download root allowed")
	}
}
func TestSeedChangedPlanAndCSRF(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	p.PlanHash = "changed"
	b, _ := json.Marshal(SeedRequest{Hash: p.Hash, PlanHash: p.PlanHash, Apply: true})
	for _, tc := range []struct {
		ct   string
		code int
	}{{"application/json", 409}, {"text/plain", 415}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/seeding/move", strings.NewReader(string(b)))
		r.Header.Set("Content-Type", tc.ct)
		r.Header.Set("Authorization", "Bearer token")
		f.svc.Handler("token").ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if len(f.writes) > 0 {
		t.Fatal("changed plan applied")
	}
}

func TestSeedIgnoresStreamAfterMoveStarts(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	// Playback begins after the first client write.
	f.streamOnStop = true
	j := &SeedJob{Plan: p, Phase: "queued"}
	f.svc.Store.SaveSeeding(j)
	if err := f.svc.executeSeed(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if j.Phase != "complete" || len(f.writes) == 0 || !f.active {
		t.Fatal("started move did not finish", j.Phase)
	}
}

func TestSeedRestartRetainsIntent(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	j := &SeedJob{Plan: p, Phase: "moving", Intent: "setLocation", Started: time.Now()}
	f.svc.Store.SaveSeeding(j)
	dir := f.svc.Store.dir
	f.svc.Store.Close()
	st, e := OpenStore(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	snap := st.Snapshot()
	if snap.Seeding.Phase != "halted" || snap.Seeding.Intent != "setLocation" || snap.Leases["radarr:1"].Owner != "seeding" {
		t.Fatal("interrupted intent lost")
	}
	// Cleanup registered by seedFake must not unlock a reused descriptor.
	f.svc.Store.lock = nil
}
func TestSeedBusy(t *testing.T) {
	f := seedFake(t)
	f.svc.tick.Lock()
	defer f.svc.tick.Unlock()
	r := httptest.NewRequest("POST", "/seeding/preview", strings.NewReader(`{"hash":"`+seedTestHash+`"}`))
	r.Header.Set("Authorization", "Bearer token")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.svc.Handler("token").ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestSeedChangedFileCannotBeRemoved(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	old := f.local(p.Files[0].Source)
	os.Remove(old)
	os.WriteFile(old, []byte("torrent bytes"), 0644)
	if _, e := f.svc.seedOld(p.Files[0]); e == nil {
		t.Fatal("replaced inode accepted")
	}
}

func TestSeedReviewedBackendAndIncompleteStorage(t *testing.T) {
	for _, tc := range []struct {
		name, backend, download string
		extensions, allowed     bool
	}{
		{"deployed", "2.0.14.0", "", false, true},
		{"suffix", "2.0.14", "", false, true},
		{"unreviewed", "2.0.15", "", false, false},
		{"incomplete-path-cleared-on-confirmation", "2.0.14.0", "/data/downloads/incomplete", false, true},
		{"extension", "2.0.14.0", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := seedFake(t)
			f.backend = tc.backend
			f.downloadPath = tc.download
			f.extensions = tc.extensions
			_, err := f.svc.seedPlan(context.Background(), seedTestHash)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed %v, error %v", tc.allowed, err)
			}
			if len(f.writes) != 0 {
				t.Fatal("preview mutated torrent")
			}
		})
	}
}

func TestSeedClearsAndVerifiesDownloadPathBeforeMove(t *testing.T) {
	for _, mode := range []string{"link", "move", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			f := seedFake(t)
			f.downloadPath = "/data/downloads/qbittorrent/incomplete"
			f.matched = mode != "move"
			f.failCheck = mode == "rollback"
			p := f.plan()
			if p.OriginalDownloadPath != f.downloadPath || len(f.writes) != 0 {
				t.Fatal("preview lost original path or wrote")
			}
			j := f.run(p)
			want := "complete"
			if mode == "rollback" {
				want = "rolled_back"
			}
			if j.Phase != want || f.downloadPath != "" || j.Plan.OriginalDownloadPath != p.OriginalDownloadPath {
				t.Fatal(j.Phase, j.Error)
			}
			if len(f.writes) < 3 || f.writes[0] != "torrents/stop" || f.writes[1] != "torrents/setDownloadPath" || f.writes[2] != "torrents/setLocation" {
				t.Fatal("wrong write sequence", f.writes)
			}
			audit, err := os.ReadFile(filepath.Join(f.svc.Store.dir, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(audit), p.OriginalDownloadPath) || !strings.Contains(string(audit), "seeding_download_path_verified_empty") {
				t.Fatal("clear audit evidence missing")
			}
		})
	}
}

func TestSeedDownloadPathClearHaltsOnFallbackOrAmbiguousResponse(t *testing.T) {
	for _, outcome := range []string{"fallback", "unexpected-body", "lost-response", "refused"} {
		t.Run(outcome, func(t *testing.T) {
			f := seedFake(t)
			f.downloadPath = "/data/downloads/qbittorrent/incomplete"
			f.fallbackDownloadPath = outcome == "fallback"
			f.unclearDownloadResponse = outcome == "unexpected-body"
			f.loseDownloadResponse = outcome == "lost-response"
			if outcome == "refused" {
				f.failAction = "torrents/setDownloadPath"
			}
			p := f.plan()
			j := f.run(p)
			if j.Phase != "halted" || len(f.writes) != 2 || f.writes[1] != "torrents/setDownloadPath" {
				t.Fatal(j.Phase, j.Error, f.writes)
			}
			if outcome != "fallback" && j.Intent != "setDownloadPath" {
				t.Fatal("clear intent lost", j.Intent)
			}
			if f.svc.Store.Snapshot().Leases["radarr:1"].Owner != "seeding" {
				t.Fatal("halt lost lease")
			}
			if _, err := os.Stat(f.local(p.Files[0].Source)); err != nil {
				t.Fatal("source lost", err)
			}
			f.svc.Tick(context.Background(), time.Now())
			if len(f.writes) != 2 {
				t.Fatal("clear replayed")
			}
		})
	}
}

func TestSeedDownloadPathMustStillMatchConfirmation(t *testing.T) {
	f := seedFake(t)
	f.downloadPath = "/data/downloads/incomplete"
	p := f.plan()
	f.downloadPath = "/data/downloads/changed"
	if err := f.svc.executeSeed(context.Background(), &SeedJob{Plan: p}); err == nil || len(f.writes) != 0 {
		t.Fatal("changed path accepted")
	}
}

func TestSeedClearDownloadPathRequiresStoppedCompleteSource(t *testing.T) {
	for _, state := range []string{"running", "incomplete"} {
		t.Run(state, func(t *testing.T) {
			f := seedFake(t)
			f.downloadPath = "/data/downloads/incomplete"
			p := f.plan()
			if state == "incomplete" {
				f.state = "stoppedDL"
				f.complete = false
			}
			j := &SeedJob{Plan: p, Phase: "moving"}
			f.svc.Store.SaveSeeding(j)
			if err := f.svc.seedClearDownloadPath(context.Background(), j); err == nil || len(f.writes) != 0 {
				t.Fatal("unsafe clear accepted")
			}
		})
	}
}

func TestSeedListsAlreadyOnNASTorrentAsNothingToDo(t *testing.T) {
	f := seedFake(t)
	f.save = "/data/media/series/GoT S06"
	f.downloadPath = "/data/downloads/incomplete"
	r := httptest.NewRequest("GET", "/seeding", nil)
	w := httptest.NewRecorder()
	f.svc.handleSeeding(w, r)
	var out struct {
		Items []SeedPlan `json:"items"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || len(out.Items) != 1 {
		t.Fatal("NAS torrent omitted")
	}
	p := out.Items[0]
	if !strings.Contains(p.Refusal, "Already on NAS") || p.PlanHash != "" || len(p.Files) != 0 || p.DownloadBytesFreed != 0 {
		t.Fatal("NAS torrent offered")
	}
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("NAS preview accepted")
	}
	if len(f.writes) != 0 {
		t.Fatal("NAS torrent mutated")
	}
}

func TestSeedStartRequiresFreshLocationAndCompletion(t *testing.T) {
	for _, tc := range []string{"incomplete", "wrong-location", "running"} {
		t.Run(tc, func(t *testing.T) {
			f := seedFake(t)
			p := f.plan()
			j := &SeedJob{Plan: p, Phase: "rechecking"}
			f.svc.Store.SaveSeeding(j)
			switch tc {
			case "incomplete":
				f.complete = false
			case "wrong-location":
				f.save = "/data/elsewhere"
			case "running":
				f.state = "stalledUP"
			}
			if err := f.svc.seedStart(context.Background(), j); err == nil {
				t.Fatal("unverified resume accepted")
			}
			if len(f.startLocations) != 0 {
				t.Fatal("start sent before fresh proof")
			}
		})
	}
}

func TestSeedRefusesUnattributedDownloadLibrarySource(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	f.library = "/data/downloads/completed/Release/video.mkv"
	f.root = "/data/downloads/completed"
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("only library copy accepted for client deletion without import history")
	}
	if len(f.writes) != 0 {
		t.Fatal("refusal wrote")
	}
}
func TestSeedRefusesMoveThroughSymlinkedDestinationParent(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	if err := os.Remove(filepath.Join(f.nas, "media", ".seeding")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(f.nas, "media", ".seeding")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("client move could escape NAS through destination parent")
	}
}

func TestSeedDestinationParentChangesAfterConfirmation(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	if err := os.Remove(filepath.Join(f.nas, "media", ".seeding")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(f.nas, "media", ".seeding")); err != nil {
		t.Fatal(err)
	}
	j := &SeedJob{Plan: p, Phase: "queued"}
	f.svc.Store.SaveSeeding(j)
	if err := f.svc.executeSeed(context.Background(), j); err == nil {
		t.Fatal("changed parent accepted")
	}
	for _, w := range f.writes {
		if w == "torrents/setLocation" {
			t.Fatal("client move sent through symlink")
		}
	}
	if _, err := os.Stat(f.local(p.Files[0].Source)); err != nil {
		t.Fatal("source lost")
	}
}

func seedDownloadLibrary(f *seedFixture) {
	f.root = "/data/downloads/Movies"
	f.library = "/data/downloads/Movies/Movie/video.mkv"
	p := f.local(f.library)
	os.MkdirAll(filepath.Dir(p), 0755)
	if err := os.Link(f.local("/data/downloads/completed/Release/video.mkv"), p); err != nil {
		f.t.Fatal(err)
	}
}
func TestSeedAmbiguousCompletedLibraryCommands(t *testing.T) {
	f := seedFake(t)
	seedDownloadLibrary(f)
	f.duplicateCommand = true
	j := f.run(f.plan())
	if j.Phase != "halted" || !strings.Contains(j.Error, "ambiguous") {
		t.Fatal("ambiguous command accepted", j.Phase, j.Error)
	}
	if len(f.writes) != 0 {
		t.Fatal("torrent mutated after ambiguous Arr command")
	}
}
func TestSeedFreshArrReferenceBeforeUnlink(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	f.adoptSourceOnStart = true
	j := f.run(p)
	if j.Phase != "halted" {
		t.Fatal("new library source reference ignored", j.Phase)
	}
	if _, err := os.Stat(f.local(p.Files[0].Source)); err != nil {
		t.Fatal("library source deleted", err)
	}
}
func TestSeedFailedChecksNeverStartOnNAS(t *testing.T) {
	for _, tc := range []string{"rollback", "rollback-fails", "move"} {
		t.Run(tc, func(t *testing.T) {
			f := seedFake(t)
			f.failCheck = true
			f.failAllChecks = tc == "rollback-fails"
			if tc == "move" {
				f.matched = false
			}
			p := f.plan()
			j := f.run(p)
			for _, location := range f.startLocations {
				if location != p.OldPath {
					t.Fatal("incomplete NAS resumed", location)
				}
			}
			if tc != "rollback" && (j.Phase != "halted" || len(f.startLocations) != 0) {
				t.Fatal("failed verification resumed", j.Phase)
			}
		})
	}
}
func TestSeedExpiredSessionHaltsWithoutReplay(t *testing.T) {
	for _, action := range []string{"torrents/stop", "torrents/setLocation", "torrents/recheck", "torrents/start"} {
		t.Run(action, func(t *testing.T) {
			f := seedFake(t)
			f.failAction = action
			p := f.plan()
			j := f.run(p)
			if j.Phase != "halted" || j.Intent != strings.TrimPrefix(action, "torrents/") {
				t.Fatal(j.Phase, j.Intent, j.Error)
			}
			if _, err := os.Stat(f.local(p.Files[0].Source)); err != nil {
				t.Fatal("expired session caused cleanup")
			}
			n := len(f.writes)
			f.svc.Tick(context.Background(), time.Now())
			if len(f.writes) != n {
				t.Fatal("expired session replayed write")
			}
		})
	}
}

func TestSeedMoveRevalidatesSourceAfterStop(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	f.replaceSourceOnStop = true
	p := f.plan()
	j := f.run(p)
	if j.Phase != "halted" {
		t.Fatal("changed source moved", j.Phase)
	}
	for _, w := range f.writes {
		if w == "torrents/setLocation" {
			t.Fatal("client moved changed source")
		}
	}
	if _, err := os.Stat(f.local(p.Files[0].Source)); err != nil {
		t.Fatal("changed source deleted")
	}
}
func TestSeedQueuedTorrentMustStillBeComplete(t *testing.T) {
	f := seedFake(t)
	f.matched = false
	p := f.plan()
	f.complete = false
	j := &SeedJob{Plan: p, Phase: "queued"}
	f.svc.Store.SaveSeeding(j)
	if err := f.svc.executeSeed(context.Background(), j); err == nil {
		t.Fatal("incomplete torrent accepted")
	}
	if len(f.writes) != 0 {
		t.Fatal("stale queued state caused torrent mutation")
	}
}
func TestSeedLibraryMoveRequiresOriginalTitlePath(t *testing.T) {
	f := seedFake(t)
	seedDownloadLibrary(f)
	p := f.plan()
	f.library = "/data/downloads/Movies/Other/video.mkv"
	f.put(f.library)
	j := &SeedJob{Plan: p, Phase: "queued"}
	f.svc.Store.SaveSeeding(j)
	if err := f.svc.executeSeed(context.Background(), j); err == nil {
		t.Fatal("changed title accepted")
	}
	if f.moved {
		t.Fatal("Arr moved a different library folder than preview")
	}
}

func TestSeedCommandSourceMustMatchPreview(t *testing.T) {
	f := seedFake(t)
	seedDownloadLibrary(f)
	f.wrongCommandSource = true
	j := f.run(f.plan())
	if j.Phase != "halted" || !strings.Contains(j.Error, "command source") {
		t.Fatal(j.Phase, j.Error)
	}
	if len(f.writes) != 0 {
		t.Fatal("torrent changed after unattributed library command")
	}
}
func TestSeedRestartStopsEveryMutationPhase(t *testing.T) {
	for _, tc := range []struct{ phase, intent string }{
		{"queued", ""}, {"library_move", "library_move:radarr"}, {"linking", "link:Release/video.mkv"},
		{"moving", "stop"}, {"moving", "setDownloadPath"}, {"moving", "prepare_destination"}, {"moving", "setLocation"},
		{"rechecking", "recheck"}, {"rechecking", "start"}, {"rolling_back", "setLocation"},
		{"rolling_back", "unlink_rollback:Release/video.mkv"}, {"cleaning", "remove_download:Release/video.mkv"},
	} {
		t.Run(tc.phase+tc.intent, func(t *testing.T) {
			f := seedFake(t)
			p := f.plan()
			j := &SeedJob{Plan: p, Phase: tc.phase, Intent: tc.intent, Started: time.Now()}
			if err := f.svc.Store.SaveSeeding(j); err != nil {
				t.Fatal(err)
			}
			old := f.svc.Store
			old.Close()
			old.lock = nil
			st, err := OpenStore(old.dir)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			f.svc.Store = st
			snap := st.Snapshot()
			if snap.Seeding.Phase != "halted" || snap.Seeding.Intent != tc.intent || snap.Leases["radarr:1"].Owner != "seeding" {
				t.Fatal("restart lost recovery evidence")
			}
			f.svc.Tick(context.Background(), time.Now())
			if len(f.writes) != 0 {
				t.Fatal("restart replayed mutation")
			}
		})
	}
}

func TestSeedPartialPackUsesWholeTorrentMoveCost(t *testing.T) {
	f := seedFake(t)
	f.plan() // Authenticate against the original loopback fixture.
	f.put("/data/downloads/completed/Release/unimported.mkv")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.qbit(rec, r)
		switch r.URL.Path {
		case "/api/v2/torrents/files":
			var fs []SeedTorrentFile
			json.Unmarshal(rec.Body.Bytes(), &fs)
			progress := 1.0
			fs = append(fs, SeedTorrentFile{Index: 1, Name: "Release/unimported.mkv", Size: 13, Progress: &progress})
			json.NewEncoder(w).Encode(fs)
		case "/api/v2/torrents/info":
			var ts []SeedTorrent
			json.Unmarshal(rec.Body.Bytes(), &ts)
			ts[0].Size = 26
			json.NewEncoder(w).Encode(ts)
		default:
			w.WriteHeader(rec.Code)
			w.Write(rec.Body.Bytes())
		}
	}))
	defer up.Close()
	f.svc.Seeding.Qbit.Base = up.URL
	p := f.plan()
	if p.Mode != "move" || p.NASBytesCost != 26 || len(p.Files) != 2 || p.Files[1].Match != nil {
		t.Fatal("partial pack linked", p.Mode, p.NASBytesCost)
	}
}
func TestSeedReplacedArrFileFallsBackToMove(t *testing.T) {
	f := seedFake(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.arrHTTP(rec, r)
		if r.URL.Path == "/api/v3/movie/1" {
			var title map[string]any
			json.Unmarshal(rec.Body.Bytes(), &title)
			title["movieFile"].(map[string]any)["id"] = 3
			json.NewEncoder(w).Encode(title)
			return
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer up.Close()
	f.svc.LibraryClients["radarr"].Base = up.URL
	p := f.plan()
	if p.Mode != "move" || p.Files[0].Match != nil {
		t.Fatal("replaced library file linked")
	}
}
func TestSeedRelativeNamesRemainContained(t *testing.T) {
	for _, p := range []string{"/absolute", "../escape", "Release/../escape", "Release//file", "Release/./file", "Release/", "Release\\file", "Release/\x00file"} {
		if seedRelative(p) {
			t.Fatalf("unsafe relative name %q", p)
		}
	}
	if !seedRelative("Release/Епизода_日本語.mkv") {
		t.Fatal("valid unicode name refused")
	}
	f := seedFake(t)
	dir := f.local("/data/downloads/completed/Release")
	outside := filepath.Join(f.download, "outside")
	if err := os.Rename(dir, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("symlinked source parent accepted")
	}
}

func TestSeedLibraryMoveCostsWholeFolder(t *testing.T) {
	f := seedFake(t)
	seedDownloadLibrary(f)
	f.put("/data/downloads/Movies/Movie/subtitle.srt")
	p := f.plan()
	if p.NASBytesCost != 26 || p.LibraryMoves[0].Bytes != 26 {
		t.Fatal("folder sidecars omitted from NAS cost", p.NASBytesCost)
	}
	f.svc.Seeding.Free = func(string) (int64, error) { return 500e9 + 13, nil }
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("whole folder would cross NAS floor")
	}
}

func TestSeedLibraryMoveProtectsOtherTorrentFolder(t *testing.T) {
	f := seedFake(t)
	seedDownloadLibrary(f)
	f.plan()
	f.cross = true
	f.put("/data/downloads/Movies/Movie/Release/video.mkv")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.qbit(rec, r)
		if r.URL.Path == "/api/v2/torrents/info" {
			var ts []SeedTorrent
			json.Unmarshal(rec.Body.Bytes(), &ts)
			ts[1].SavePath = "/data/downloads/Movies/Movie"
			ts[1].ContentPath = "/data/downloads/Movies/Movie/Release"
			json.NewEncoder(w).Encode(ts)
			return
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer up.Close()
	f.svc.Seeding.Qbit.Base = up.URL
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("step zero could remove another torrent's source folder")
	}
}

func TestSeedRefusesScheduledArchiveCleanup(t *testing.T) {
	f := seedFake(t)
	if err := f.svc.Store.SaveArchive(archivetier.State{AbandonedDownloads: []archivetier.AbandonedDownload{{Kind: "radarr", DownloadID: strings.ToUpper(seedTestHash), CompletedAt: time.Now().Add(-10 * 24 * time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.seedPlan(context.Background(), seedTestHash); err == nil {
		t.Fatal("archive cleanup can erase halted relocation evidence")
	}
}

func TestSeedQbitLoginStatuses(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		ok     bool
	}{{204, "", true}, {200, "Ok.", true}, {200, "Fails.", false}, {403, "", false}, {500, "", false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		q := &SeedQbit{Base: srv.URL, User: "u", Password: "p", HTTP: srv.Client()}
		if e := q.Login(context.Background()); (e == nil) != c.ok {
			t.Errorf("login %d %q: err=%v, want ok=%v", c.status, c.body, e, c.ok)
		}
		srv.Close()
	}
}

func TestSeedQbitSessionExpiry(t *testing.T) {
	// qBittorrent drops an idle WebUI session after an hour; the client must log
	// in again instead of answering 403 until it is restarted.
	var sid, logins, versions int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/auth/login":
			logins++
			sid++
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: fmt.Sprint(sid), Path: "/"})
			w.WriteHeader(204)
		case "/api/v2/app/version":
			versions++
			if c, e := r.Cookie("SID"); e != nil || c.Value != fmt.Sprint(sid) {
				w.WriteHeader(403)
				return
			}
			fmt.Fprint(w, "v5.2.3")
		}
	}))
	defer srv.Close()
	q := NewSeedQbit(srv.URL, "u", "p", srv.Client())
	var v string
	for i := 0; i < 2; i++ {
		if e := q.Login(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e := q.call(context.Background(), "GET", "app/version", nil, &v); e != nil || v != "v5.2.3" {
			t.Fatalf("call %d: %q %v", i, v, e)
		}
		sid++ // the server forgets the session
	}
	if logins != 2 || versions != 3 {
		t.Fatalf("logins=%d versions=%d, want one re-login and one retry", logins, versions)
	}
	// A login that still fails ends the retry rather than looping.
	q = NewSeedQbit(srv.URL, "u", "p", srv.Client())
	q.logged = true
	sid = -100
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	if e := q.call(context.Background(), "GET", "app/version", nil, &v); e == nil {
		t.Fatal("a refused re-login must fail")
	}
}

func TestSeedPeerWithoutMetadata(t *testing.T) {
	f := seedFake(t)
	f.peerState = "metaDL"
	f.plan()
	f = seedFake(t)
	f.peerState = "stalledUP"
	if _, e := f.svc.seedPlan(context.Background(), seedTestHash); e == nil || !strings.Contains(e.Error(), "torrent files missing") {
		t.Fatalf("an empty file list on a torrent with metadata must refuse, got %v", e)
	}
}

func TestSeedPeerMissingFiles(t *testing.T) {
	// A missing-files or partly downloaded peer whose files are absent from
	// both its save and download paths shares nothing.
	f := seedFake(t)
	f.peerState, f.peerFile, f.peerDownloadPath = "missingFiles", "Gone/video.mkv", "/data/downloads/qbittorrent/incomplete"
	f.plan()

	// A peer whose download path holds the same inode still refuses.
	f = seedFake(t)
	f.peerState, f.peerFile, f.peerDownloadPath = "stalledDL", "Shared/video.mkv", "/data/downloads/qbittorrent/incomplete"
	dst := f.local("/data/downloads/qbittorrent/incomplete/Shared/video.mkv")
	os.MkdirAll(filepath.Dir(dst), 0755)
	if e := os.Link(f.local("/data/downloads/completed/Release/video.mkv"), dst); e != nil {
		t.Fatal(e)
	}
	if _, e := f.svc.seedPlan(context.Background(), seedTestHash); e == nil {
		t.Fatal("a peer sharing the inode through its download path must refuse")
	}

	// A peer path that cannot be inspected (not under any mount) still refuses.
	f = seedFake(t)
	f.peerState, f.peerFile, f.peerDownloadPath = "stalledDL", "Gone/video.mkv", "/elsewhere"
	if _, e := f.svc.seedPlan(context.Background(), seedTestHash); e == nil || !strings.Contains(e.Error(), "cannot be inspected") {
		t.Fatalf("unmapped peer path must refuse, got %v", e)
	}
}

func TestSeedLiveProgressIsReadOnlyStatus(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	half := 0.42
	f.svc.observeSeed(SeedTorrent{Hash: p.Hash, State: "checkingUP", Progress: &half})
	if e := f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "rechecking", Started: time.Now()}); e != nil {
		t.Fatal(e)
	}
	got := f.svc.Status().Seeding
	if got == nil || got.Live == nil || got.Live.State != "checkingUP" || got.Live.Progress != 0.42 {
		t.Fatalf("live progress missing: %+v", got)
	}
	if f.svc.Store.Snapshot().Seeding.Live != nil {
		t.Fatal("live progress reached stored state")
	}
	w := httptest.NewRecorder()
	f.svc.handleSeeding(w, httptest.NewRequest("GET", "/seeding", nil))
	if !strings.Contains(w.Body.String(), `"live":{"state":"checkingUP","progress":0.42`) {
		t.Fatalf("seeding list job lacks live progress: %s", w.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(f.svc.Store.dir, "state.json")); strings.Contains(string(b), `"live"`) {
		t.Fatal("live progress persisted")
	}
	other := 1.0
	f.svc.observeSeed(SeedTorrent{Hash: strings.Repeat("f", 40), State: "stalledUP", Progress: &other})
	if f.svc.Status().Seeding.Live != nil {
		t.Fatal("another torrent's reading attached")
	}
	f.svc.observeSeed(SeedTorrent{Hash: p.Hash, State: "checkingUP", Progress: &half})
	f.svc.seedLive.live.Observed = time.Now().Add(-3 * time.Minute)
	if f.svc.Status().Seeding.Live != nil {
		t.Fatal("stale reading attached")
	}
	f.svc.observeSeed(SeedTorrent{Hash: p.Hash, State: "stoppedUP", Progress: &other})
	f.svc.Store.SaveSeeding(&SeedJob{Plan: p, Phase: "halted"})
	if f.svc.Status().Seeding.Live != nil {
		t.Fatal("inactive job carries a reading")
	}
}

func TestSeedMoverReadsRecordLiveProgress(t *testing.T) {
	f := seedFake(t)
	p := f.plan()
	if _, e := f.svc.seedTorrent(context.Background(), p.Hash); e != nil {
		t.Fatal(e)
	}
	if f.svc.seedLive.hash != p.Hash || f.svc.seedLive.live.Progress != 1 || f.svc.seedLive.live.State != f.state {
		t.Fatalf("mover read not observed: %+v", f.svc.seedLive.live)
	}
	if len(f.writes) != 0 {
		t.Fatal("observation wrote to qBittorrent")
	}
}

func TestSeedConfigUsesConfiguredMounts(t *testing.T) {
	c := DefaultSeedConfig()
	c.Enabled = true
	c.DownloadRoot = "/downloads/complete"
	c.DownloadMount = archivetier.MountMapping{AppRoot: "/downloads", LocalRoot: "/writable/downloads"}
	c.NASMount = archivetier.MountMapping{AppRoot: "/library", LocalRoot: "/writable/media"}
	c.SeedRoot = "/library/seeding"
	c.LibraryRoots = map[string]string{"radarr": "/library/movies", "sonarr": "/library/series"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.SeedRoot = "/downloads/seeding"
	if c.Validate() == nil {
		t.Fatal("destination on the download disk accepted")
	}
}
