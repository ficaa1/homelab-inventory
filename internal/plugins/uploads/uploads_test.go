package uploads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func configured(t *testing.T, c plugin.Config) (*plugin.Registry, *Uploads, string) {
	t.Helper()
	data := t.TempDir()
	var instance *Uploads
	registry, err := plugin.Open(data, []plugin.Builtin{{ID: "uploads", New: func(h plugin.Host) plugin.Plugin { instance = New(h).(*Uploads); return instance }}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.Close)
	on := true
	if _, err = registry.Update("uploads", plugin.Update{Enabled: &on, Settings: c}); err != nil {
		t.Fatal(err)
	}
	return registry, instance, data
}
func TestFolderNeverOverwritesAndKeepsBoundedHistory(t *testing.T) {
	folder := t.TempDir()
	registry, u, data := configured(t, plugin.Config{"folder_path": folder})
	ctx := context.Background()
	for _, expected := range []string{"a.pdf", "a (1).pdf"} {
		result, err := registry.Upload(ctx, "uploads", "folder", "a.pdf", 3, strings.NewReader("pdf"), "bearer")
		if err != nil || result.Name != expected {
			t.Fatalf("%+v %v", result, err)
		}
		stat, err := os.Stat(filepath.Join(folder, expected))
		if err != nil || stat.Mode().Perm() != 0644 {
			t.Fatal("published file mode")
		}
	}
	if _, err := registry.Upload(ctx, "uploads", "folder", "short.pdf", 4, strings.NewReader("pdf"), "bearer"); err == nil {
		t.Fatal("short body published")
	}
	if _, err := os.Stat(filepath.Join(folder, "short.pdf")); !os.IsNotExist(err) {
		t.Fatal("incomplete file exists")
	}
	entries, _ := os.ReadDir(folder)
	if len(entries) != 2 {
		t.Fatal("temporary files leaked")
	}
	on := true
	for _, invalid := range []string{data, filepath.Join(data, "plugins"), "relative", "/tmp/../tmp"} {
		if _, err := registry.Update("uploads", plugin.Update{Enabled: &on, Settings: plugin.Config{"folder_path": invalid}}); err == nil {
			t.Fatal("unsafe destination accepted")
		}
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(data, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Update("uploads", plugin.Update{Settings: plugin.Config{"folder_path": alias}}); err == nil {
		t.Fatal("data alias accepted")
	}
	for i := 0; i < 101; i++ {
		_, _ = u.Upload(ctx, "folder", "recent.txt", 0, strings.NewReader(""))
	}
	raw, err := os.ReadFile(filepath.Join(data, "plugins", "uploads", "recent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recent []record
	if json.Unmarshal(raw, &recent) != nil || len(recent) != 100 {
		t.Fatal("history unbounded")
	}
	if test := u.Test(ctx); !test.OK || len(test.Checks) != 1 {
		t.Fatal(test)
	}
	if _, err := registry.View(ctx, "uploads", "recent", nil); err != nil {
		t.Fatal(err)
	}
}
func TestRemoteDestinationsGuardedFormsAndSafeFailures(t *testing.T) {
	const token = "fixture-token"
	const password = "fixture-password"
	reject := false
	uploads := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/":
			if r.Header.Get("Authorization") != "Token "+token {
				t.Error("missing API token")
			}
			io.WriteString(w, `{}`)
		case "/api/documents/post_document/":
			if r.Header.Get("Authorization") != "Token "+token {
				t.Error("missing API token")
			}
			if reject {
				http.Error(w, token+password, 403)
				return
			}
			if r.ContentLength < 3 {
				t.Error("multipart length missing")
			}
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				return
			}
			part, err := reader.NextPart()
			if err != nil {
				t.Error(err)
				return
			}
			raw, _ := io.ReadAll(part)
			if part.FormName() != "document" || part.FileName() != "a.pdf" || string(raw) != "pdf" {
				t.Error("wrong Paperless part")
			}
			uploads++
			io.WriteString(w, `"task-123"`)
		case "/login":
			if r.Method == "GET" {
				http.SetCookie(w, &http.Cookie{Name: "csrf", Value: "seed", Path: "/"})
				io.WriteString(w, `<input value='csrf-value' name='csrf_token'>`)
				return
			}
			if r.FormValue("username") != "reader" || r.FormValue("password") != password || r.FormValue("csrf_token") != "csrf-value" {
				t.Error("wrong login form")
			}
			if _, err := r.Cookie("csrf"); err != nil {
				t.Error("missing login cookie")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "signed-in", Path: "/"})
			http.Redirect(w, r, "/", 302)
		case "/":
			if _, err := r.Cookie("session"); err != nil {
				http.Redirect(w, r, "/login", 302)
				return
			}
			io.WriteString(w, `<form id="form-upload"><input name="csrf_token" value="upload-value"></form>`)
			if reject {
				io.WriteString(w, `<div class="alert alert-danger">Refused <b>duplicate</b> fixture-password fixture-token</div>`)
			}
		case "/upload":
			if _, err := r.Cookie("session"); err != nil {
				t.Error("missing upload session")
			}
			if r.FormValue("csrf_token") != "upload-value" {
				t.Error("missing upload csrf")
			}
			file, header, err := r.FormFile("btn-upload")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			raw, _ := io.ReadAll(file)
			if header.Filename != "a.epub" || string(raw) != "book" {
				t.Error("wrong book")
			}
			if reject {
				io.WriteString(w, `{"location":"/"}`)
			} else {
				uploads++
				io.WriteString(w, `{"location":"/book/42"}`)
			}
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	registry, u, _ := configured(t, plugin.Config{"paperless_url": fake.URL, "paperless_token": token, "calibre_url": fake.URL, "calibre_user": "reader", "calibre_password": password})
	ctx := context.Background()
	for _, test := range []struct{ target, name, body, message string }{{"paperless", "a.pdf", "pdf", "queued task task-123"}, {"calibre", "a.epub", "book", "book 42"}} {
		result, err := registry.Upload(ctx, "uploads", test.target, test.name, int64(len(test.body)), strings.NewReader(test.body), "bearer")
		if err != nil || !strings.Contains(result.Message, test.message) {
			t.Fatalf("%+v %v", result, err)
		}
	}
	if uploads != 2 {
		t.Fatal("missing uploads")
	}
	if result := u.Test(ctx); !result.OK || len(result.Checks) != 2 {
		t.Fatal(result)
	}
	reject = true
	for _, target := range []string{"paperless", "calibre"} {
		name := "a.pdf"
		body := "pdf"
		if target == "calibre" {
			name = "a.epub"
			body = "book"
		}
		_, err := registry.Upload(ctx, "uploads", target, name, int64(len(body)), strings.NewReader(body), "bearer")
		if err == nil || strings.Contains(err.Error(), token) || strings.Contains(err.Error(), password) {
			t.Fatal("unsafe failure")
		}
		if target == "calibre" && !strings.Contains(err.Error(), "Refused duplicate") {
			t.Fatal("flash message missing")
		}
	}
	view, err := registry.View(ctx, "uploads", "recent", nil)
	if err != nil || strings.Contains(string(view), token) || strings.Contains(string(view), password) {
		t.Fatal("unsafe recent history")
	}
}
