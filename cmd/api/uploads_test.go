package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type routeUploader struct{ routePlugin }

func (*routeUploader) UploadTargets() []plugin.UploadTarget {
	return []plugin.UploadTarget{{ID: "docs", Label: "Documents", Accept: []string{".pdf"}, MaxBytes: 32}}
}
func (*routeUploader) Upload(_ context.Context, _ string, name string, _ int64, body io.Reader) (plugin.UploadResult, error) {
	_, err := io.Copy(io.Discard, body)
	return plugin.UploadResult{Name: name, Message: "Stored"}, err
}
func TestUploadsValidationAndAudit(t *testing.T) {
	s := testServer(t, strings.Repeat("a", 32))
	data := s.store.Dir()
	p := &routeUploader{}
	var err error
	s.plugins, err = plugin.Open(data, []plugin.Builtin{{ID: "sample", New: func(plugin.Host) plugin.Plugin { return p }}}, s.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.plugins.Close)
	on := true
	if _, err = s.plugins.Update("sample", plugin.Update{Enabled: &on, Settings: plugin.Config{"endpoint": "http://example.org"}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		size   int64
		status int
	}{
		{"", 3, 400}, {"../file.pdf", 3, 422}, {".file.pdf", 3, 422}, {"a\\file.pdf", 3, 422}, {"a\x00.pdf", 3, 422}, {"a\n.pdf", 3, 422}, {"a.exe", 3, 415}, {"a.pdf", 33, 413}, {"a.pdf", plugin.MaxUploadBytes + 1, 413}, {"a.pdf", -1, 411}, {"a.pdf", 3, 200}, {"snow 雪+.PDF", 3, 200},
	} {
		req := httptest.NewRequest("POST", "/plugins/sample/upload/docs", strings.NewReader("pdf"))
		req.ContentLength = test.size
		req.Header.Set("Authorization", "Bearer "+s.token)
		if test.name != "" {
			req.Header.Set("X-Upload-Name", url.PathEscape(test.name))
		}
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("%q: got %d want %d: %s", test.name, rec.Code, test.status, rec.Body.String())
		}
	}
	raw, err := os.ReadFile(filepath.Join(data, "upload-audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected two intent/outcome pairs: %d", len(lines))
	}
	sum := sha256.Sum256([]byte("pdf"))
	for i, line := range lines {
		var a map[string]any
		if json.Unmarshal([]byte(line), &a) != nil {
			t.Fatal("invalid audit JSON")
		}
		if a["actor"] != "bearer" || a["bytes"] != float64(3) {
			t.Fatal(a)
		}
		if i%2 == 1 && (a["sha256"] != hex.EncodeToString(sum[:]) || a["event"] != "outcome") {
			t.Fatal("bad streaming digest")
		}
	}
	stat, _ := os.Stat(filepath.Join(data, "upload-audit.jsonl"))
	if stat.Mode().Perm() != 0600 {
		t.Fatal("audit permissions")
	}
	for _, token := range []string{"", strings.Repeat("b", 32)} {
		if rec := pluginRequest(s, "GET", "/uploads/targets", "", token); rec.Code != 401 {
			t.Fatal("target list requires admin")
		}
	}
	if rec := pluginRequest(s, "GET", "/uploads/targets", "", s.token); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"plugin_name"`) {
		t.Fatal("missing targets")
	}
	for _, path := range []string{"/plugins/unknown/upload/docs", "/plugins/sample/upload/unknown"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("pdf"))
		req.Header.Set("Authorization", "Bearer "+s.token)
		req.Header.Set("X-Upload-Name", "a.pdf")
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatal("unknown upload target served")
		}
	}
	if _, err := plugin.UploadName(strings.Repeat("x", 201)); err == nil {
		t.Fatal("long file name accepted")
	}
	// An empty POST without Content-Length differs from an explicit zero-byte file.
	req := httptest.NewRequest("POST", "/plugins/sample/upload/docs", nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	req.Header.Set("X-Upload-Name", "empty.pdf")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 411 {
		t.Fatal("missing zero-byte Content-Length accepted")
	}
	req.Header.Set("Content-Length", "0")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatal("explicit zero-byte file refused")
	}

	// A real server with short ordinary deadlines must still accept a slow file.
	live := httptest.NewUnstartedServer(s.routes())
	live.Config.ReadTimeout = 50 * time.Millisecond
	live.Config.WriteTimeout = 50 * time.Millisecond
	live.Start()
	defer live.Close()
	reader, writer := io.Pipe()
	go func() { time.Sleep(150 * time.Millisecond); _, _ = io.WriteString(writer, "pdf"); _ = writer.Close() }()
	upload, err := http.NewRequest("POST", live.URL+"/plugins/sample/upload/docs", reader)
	if err != nil {
		t.Fatal(err)
	}
	upload.ContentLength = 3
	upload.Header.Set("Authorization", "Bearer "+s.token)
	upload.Header.Set("X-Upload-Name", "slow.pdf")
	upload.Header.Set("Expect", "100-continue")
	client := live.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Do(upload)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatal("upload inherited the short server deadline")
	}
}
