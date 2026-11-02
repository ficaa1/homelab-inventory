package mediapolicy

import (
	"encoding/json"
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

func libraryService(t *testing.T) (*Service, *sync.Map) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := &Service{Store: store, Config: Config{ArchiveEnabled: true}, LibraryClients: map[string]*archivetier.Client{}}
	calls := &sync.Map{}
	for _, kind := range []string{"radarr", "sonarr"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := kind + " " + r.Method + " " + r.URL.Path
			n, _ := calls.LoadOrStore(key, 0)
			calls.Store(key, n.(int)+1)
			switch r.URL.Path {
			case "/api/v3/tag":
				w.Write([]byte(`[{"id":7,"label":"archive"},{"id":19,"label":"4k"}]`))
			case "/api/v3/system/status":
				name := "Radarr"
				if kind == "sonarr" {
					name = "Sonarr"
				}
				json.NewEncoder(w).Encode(map[string]string{"appName": name, "version": archivetier.DefaultConfig().ReviewedVersions[kind]})
			case "/api/v3/movie":
				w.Write([]byte(`[{"id":1,"title":"A movie","year":2025,"path":"/movies/a","tags":[],"sizeOnDisk":100,"movieFile":{"id":10,"size":100}}]`))
			case "/api/v3/series":
				w.Write([]byte(`[{"id":2,"title":"A series","year":2024,"path":"/tv/a","tags":[],"statistics":{"sizeOnDisk":900},"seasons":[{"seasonNumber":1,"statistics":{"sizeOnDisk":600}},{"seasonNumber":2,"statistics":{"sizeOnDisk":300}}]}]`))
			case "/api/v3/movie/editor", "/api/v3/series/editor":
				w.WriteHeader(204)
			default:
				t.Errorf("unexpected upstream request: %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		c := archivetier.NewClient(kind, srv.URL, "fixture", true)
		tr := &Transport{Apply: true, Store: store}
		if kind == "radarr" {
			tr.RadarrOrigin = srv.URL
		} else {
			tr.SonarrOrigin = srv.URL
		}
		c.HTTP = SharedHTTP(tr)
		s.LibraryClients[kind] = c
	}
	return s, calls
}
func libraryRequest(s *Service, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler("fixture").ServeHTTP(rec, req)
	return rec
}
func count(c *sync.Map, key string) int {
	n, _ := c.Load(key)
	if n == nil {
		return 0
	}
	return n.(int)
}

func TestLibraryCacheAndLiveState(t *testing.T) {
	s, calls := libraryService(t)
	fetch := func(p string) Library {
		t.Helper()
		rec := libraryRequest(s, "GET", p, "", "fixture")
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body)
		}
		var out Library
		json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	out := fetch("/library")
	if len(out.Items) != 2 || out.Items[0].Kind != "sonarr" || out.Items[0].SizeBytes != 900 || len(out.Items[0].Seasons) != 2 {
		t.Fatalf("sort/inventory: %+v", out)
	}
	if out.Items[1].ArchiveStatus.Status != "none" {
		t.Fatal("unexpected status")
	}
	if err := s.Store.SaveArchive(archivetier.State{Job: &archivetier.Job{Target: archivetier.Target{Kind: "sonarr", ID: 2, Season: 1}, Phase: archivetier.Downloading}}); err != nil {
		t.Fatal(err)
	}
	out = fetch("/library")
	if out.Items[0].ArchiveStatus.Status != "active" || out.Items[0].Seasons[0].ArchiveStatus.Status != "active" || out.Items[0].Seasons[1].ArchiveStatus.Status != "none" {
		t.Fatal("cached state was not refreshed")
	}
	if count(calls, "radarr GET /api/v3/movie") != 1 || count(calls, "sonarr GET /api/v3/series") != 1 {
		t.Fatal("cache did not prevent reads")
	}
	// Mutating a served copy must not contaminate the cached inventory.
	out.Items[0].Title = "changed"
	if fetch("/library").Items[0].Title == "changed" {
		t.Fatal("cache shares mutable data")
	}
	fetch("/library?refresh=1")
	if count(calls, "radarr GET /api/v3/movie") != 2 {
		t.Fatal("refresh ignored")
	}
	s.libraryCache.Fetched = time.Now().Add(-6 * time.Minute)
	fetch("/library")
	if count(calls, "radarr GET /api/v3/movie") != 3 {
		t.Fatal("expired cache reused")
	}
}

func TestLibraryStatus(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Second)
	job := archivetier.Job{Target: archivetier.Target{Kind: "sonarr", ID: 2, Season: 1, Files: []archivetier.File{{Size: 1000}}}, Phase: archivetier.Complete, ActualBytes: 200, Completed: at}
	st := archivetier.State{History: []archivetier.HistoryEntry{{Job: job, Status: archivetier.HistoryAvailable}, {Job: job, Status: archivetier.HistoryAvailable}}, Job: &job}
	got := libraryStatus(st, "sonarr", 2, -1)
	if got.Status != "shrunk" || got.SavedBytes != 800 || got.Date != at.Format(time.RFC3339) {
		t.Fatalf("shrunk: %+v", got)
	}
	st.Approvals = []archivetier.Approval{{ApprovalRequest: archivetier.ApprovalRequest{Kind: "radarr", ID: 9}}, {ApprovalRequest: archivetier.ApprovalRequest{Kind: "sonarr", ID: 2, Season: 2}, Blocked: true}}
	if libraryStatus(st, "sonarr", 2, -1).Status != "pending_approval" || libraryStatus(st, "sonarr", 2, 1).Status != "shrunk" {
		t.Fatal("approval matching")
	}
	st.Job.Phase = archivetier.Failed
	if libraryStatus(st, "sonarr", 2, 1).Status != "failed" {
		t.Fatal("failed treated as shrunk")
	}
	st.Job = nil
	st.Approvals = nil
	st.History[0].Status = archivetier.HistoryRestored
	if libraryStatus(st, "sonarr", 2, 1).Status != "none" {
		t.Fatal("restored title marked shrunk")
	}
}

func TestTagEndpointAuthDryRunValidationAndAudit(t *testing.T) {
	s, calls := libraryService(t)
	body := `{"kind":"radarr","id":1,"archive":true,"fourK":false}`
	for _, methodPath := range []struct{ method, path string }{{"GET", "/library"}, {"POST", "/archive/tag"}} {
		if rec := libraryRequest(s, methodPath.method, methodPath.path, body, ""); rec.Code != 401 {
			t.Fatal("endpoint did not require auth")
		}
	}
	s.DryRun = true
	if rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture"); rec.Code != 409 {
		t.Fatalf("dry-run: %d", rec.Code)
	}
	if count(calls, "radarr GET /api/v3/tag") != 0 {
		t.Fatal("dry-run reached Arr")
	}
	s.DryRun = false
	for _, invalid := range []string{`{}`, `{"kind":"evil","id":1,"archive":true,"fourK":false}`, `{"kind":"radarr","id":0,"archive":true,"fourK":false}`, `{"kind":"radarr","id":1}`, body + ` {}`, `{"kind":"radarr","id":1,"archive":true,"fourK":false,"profile":9}`} {
		if rec := libraryRequest(s, "POST", "/archive/tag", invalid, "fixture"); rec.Code != 400 {
			t.Errorf("accepted %s", invalid)
		}
	}
	libraryRequest(s, "GET", "/library", "", "fixture")
	if rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture"); rec.Code != 200 {
		t.Fatalf("tag: %d %s", rec.Code, rec.Body)
	}
	if count(calls, "radarr PUT /api/v3/movie/editor") != 2 || !s.libraryCache.Fetched.IsZero() {
		t.Fatal("tag write or invalidation missing")
	}
	audit, err := os.ReadFile(filepath.Join(s.Store.dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), "archive_tag_requested") || !strings.Contains(string(audit), "archive_tag_result") {
		t.Fatal("audit missing")
	}
	if err := s.Store.SaveArchive(archivetier.State{Job: &archivetier.Job{Target: archivetier.Target{Kind: "radarr", ID: 1}, Phase: archivetier.Downloading}}); err != nil {
		t.Fatal(err)
	}
	if rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture"); rec.Code != 409 || count(calls, "radarr PUT /api/v3/movie/editor") != 2 {
		t.Fatal("active job allowed tag changes")
	}
	if err := s.Store.SaveArchive(archivetier.State{Approvals: []archivetier.Approval{{ApprovalRequest: archivetier.ApprovalRequest{Kind: "sonarr", ID: 2, Season: 1}}, {ApprovalRequest: archivetier.ApprovalRequest{Kind: "radarr", ID: 1}, Blocked: true}}}); err != nil {
		t.Fatal(err)
	}
	if rec := libraryRequest(s, "POST", "/archive/tag", body, "fixture"); rec.Code != 409 || count(calls, "radarr PUT /api/v3/movie/editor") != 2 {
		t.Fatal("queued approval allowed tag changes")
	}
}
