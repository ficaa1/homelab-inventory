package mediapolicy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEditorAllowlist(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer upstream.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	client := SharedHTTP(&Transport{Apply: true, Store: store, RadarrOrigin: upstream.URL, SonarrOrigin: "http://sonarr.example.org"})
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"PUT", "/api/v3/movie/editor", true},
		{"PUT", "/api/v3/movie/2", false},
		{"PUT", "/api/v3/movie/editor/extra", false},
		{"POST", "/api/v3/movie/editor", false},
		{"DELETE", "/api/v3/movie/editor", false},
		{"PUT", "/api/v3/series/editor", false},
	} {
		req, _ := http.NewRequest(tc.method, upstream.URL+tc.path, nil)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (err == nil) != tc.allowed {
			t.Errorf("%s %s: %v", tc.method, tc.path, err)
		}
	}
	if calls != 1 {
		t.Fatal("unexpected upstream write")
	}
}

func TestSeedDownloadPathClearAllowlist(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer up.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr := &Transport{Apply: true, Store: store, QbitOrigin: up.URL}
	for _, tc := range []struct {
		body    string
		allowed bool
	}{
		{"id=" + seedTestHash + "&path=", true},
		{"id=" + seedTestHash + "&path=/data/downloads/temp", false},
		{"hashes=" + seedTestHash + "&path=", false},
		{"id=all&path=", false},
		{"id=" + seedTestHash + "%7C" + seedTestHash + "&path=", false},
		{"id=" + seedTestHash, false},
		{"id=" + seedTestHash + "&path=&path=", false},
		{"id=" + seedTestHash + "&path=&extra=1", false},
	} {
		req, _ := http.NewRequest("POST", up.URL+"/api/v2/torrents/setDownloadPath", strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := SharedHTTP(tr).Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (err == nil) != tc.allowed {
			t.Errorf("clear allowed=%v: %v", tc.allowed, err)
		}
	}
	tr.Apply = false
	req, _ := http.NewRequest("POST", up.URL+"/api/v2/torrents/setDownloadPath", strings.NewReader("id="+seedTestHash+"&path="))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := SharedHTTP(tr).Do(req); err == nil {
		t.Fatal("dry-run clear accepted")
	}
	if calls != 1 {
		t.Fatalf("%d writes reached upstream", calls)
	}
}

func TestSeedDownloadPathClearChecksTransmittedBody(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer up.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr := &Transport{Apply: true, Store: store, QbitOrigin: up.URL}
	// Replacing Body leaves the constructor's GetBody replay copy unchanged.
	clear := "id=" + seedTestHash + "&path="
	set := clear + "%2Fdownload%2Ftemp"
	req, _ := http.NewRequest("POST", up.URL+"/api/v2/torrents/setDownloadPath", strings.NewReader(clear))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Body = io.NopCloser(strings.NewReader(set))
	req.ContentLength = int64(len(set))
	resp, err := SharedHTTP(tr).Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || calls != 0 {
		t.Fatal("nonempty transmitted download path passed the write barrier")
	}
}

func TestSonarrExactEditorAllowlist(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer upstream.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr := &Transport{Apply: true, Store: store, SonarrOrigin: upstream.URL, RadarrOrigin: "http://radarr.example.org"}
	client := SharedHTTP(tr)
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"PUT", "/api/v3/series/editor", true}, {"PUT", "/api/v3/series/1", false},
		{"PUT", "/api/v3/series/editor/extra", false}, {"PUT", "/api/v3/series/1/extra", false},
		{"PUT", "/api/v3/series/", false}, {"PUT", "/api/v3/series/foo", false},
		{"PUT", "/api/v3/series/0", false}, {"PUT", "/api/v3/series/-1", false},
		{"POST", "/api/v3/series/editor", false}, {"DELETE", "/api/v3/series/editor", false},
		{"PUT", "/api/v3/movie/editor", false}, {"POST", "/api/v3/tag", false},
	} {
		req, _ := http.NewRequest(tc.method, upstream.URL+tc.path, nil)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (err == nil) != tc.allowed {
			t.Errorf("%s %s: %v", tc.method, tc.path, err)
		}
	}
	if calls != 1 {
		t.Fatal("unexpected write reached Sonarr")
	}
	tr.Apply = false
	req, _ := http.NewRequest("PUT", upstream.URL+"/api/v3/series/editor", nil)
	if _, err := client.Do(req); err == nil || calls != 1 {
		t.Fatal("dry-run editor write")
	}
}

func TestCleanupDeleteTransportAllowlist(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer upstream.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tr := &Transport{Apply: true, Store: store, RadarrOrigin: upstream.URL}
	client := SharedHTTP(tr)
	for _, tc := range []struct {
		path    string
		allowed bool
	}{
		{"/api/v3/queue/7?removeFromClient=true&blocklist=true&skipRedownload=true", true},
		{"/api/v3/queue/7?removeFromClient=true&blocklist=true&skipRedownload=true&skipRedownload=false", false},
		{"/api/v3/queue/7?removeFromClient=true&blocklist=true&skipRedownload=true&blocklist=true", false},
		{"/api/v3/queue/7?removeFromClient=true&blocklist=true&skipRedownload=true&bad%zz=1", false},
		{"/api/v3/queue/7?removeFromClient=true&blocklist=false", false},
		{"/api/v3/queue/7?blocklist=true", false},
		{"/api/v3/queue/07?removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/api/v3/queue/0?removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/api/v3/queue/bulk?removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/api/v3/queue/7/extra?removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/api/v3/queue/%37?removeFromClient=true&blocklist=true&skipRedownload=true", false},
		{"/api/v3/movie/7?removeFromClient=true&blocklist=true&skipRedownload=true", false},
	} {
		req, _ := http.NewRequest("DELETE", upstream.URL+tc.path, nil)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (err == nil) != tc.allowed {
			t.Errorf("%s: %v", tc.path, err)
		}
	}
	for _, origin := range []string{upstream.URL, "http://unapproved.example.org"} {
		tr.Apply = false
		if origin != upstream.URL {
			tr.Apply = true
		}
		req, _ := http.NewRequest("DELETE", origin+"/api/v3/queue/7?removeFromClient=true&blocklist=true&skipRedownload=true", nil)
		if _, err := client.Do(req); err == nil {
			t.Fatal("unapproved cleanup accepted")
		}
	}
	if calls != 1 {
		t.Fatalf("%d requests reached upstream", calls)
	}
}

func TestSeedHTTPAllowlist(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer up.Close()
	store, e := OpenStore(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	tr := &Transport{Apply: true, Store: store, QbitOrigin: up.URL}
	client := SharedHTTP(tr)
	for _, tc := range []struct {
		method, path string
		allowed      bool
	}{
		{"POST", "/api/v2/auth/login", true}, {"POST", "/api/v2/torrents/stop", true}, {"POST", "/api/v2/torrents/start", true}, {"POST", "/api/v2/torrents/setLocation", true}, {"POST", "/api/v2/torrents/recheck", true},
		{"POST", "/api/v2/torrents/delete", false}, {"POST", "/api/v2/torrents/add", false}, {"POST", "/api/v2/torrents/reannounce", false}, {"POST", "/api/v2/torrents/setSavePath", false}, {"POST", "/api/v2/torrents/setDownloadPath", false}, {"DELETE", "/api/v2/torrents/stop", false}, {"POST", "/api/v2/torrents/stop?hashes=all", false}, {"POST", "/api/v2/torrents/%73top", false},
	} {
		req, _ := http.NewRequest(tc.method, up.URL+tc.path, nil)
		resp, e := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (e == nil) != tc.allowed {
			t.Errorf("%s %s %v", tc.method, tc.path, e)
		}
	}
	tr.Apply = false
	for _, path := range []string{"auth/login", "torrents/stop"} {
		req, _ := http.NewRequest("POST", up.URL+"/api/v2/"+path, nil)
		resp, e := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if (e == nil) != (path == "auth/login") {
			t.Fatal("dry-run login/torrent barrier", e)
		}
	}
}
