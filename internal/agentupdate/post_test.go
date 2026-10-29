package agentupdate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlainPosterPreservesPartialAndRefusesFailures(t *testing.T) {
	raw := `{"schema_version":"5","host":{"hostname":"host-a"},"docker":null,"diagnostics":[{"severity":"error","message":"partial"}]}`
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		b, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/inventory" || r.Header.Get("Authorization") != "Bearer throwaway" || string(b) != raw {
			t.Error("poster changed inventory or request")
		}
		w.WriteHeader(201)
	}))
	defer api.Close()
	dir := t.TempDir()
	executable := filepath.Join(t.TempDir(), "discover")
	for _, tc := range []struct {
		body string
		ok   bool
	}{
		{"printf '%s' '" + raw + "'", true},
		{"printf '%s' '" + raw + "'; exit 2", false},
		{"printf '{'", false},
		{"head -c 16777217 /dev/zero", false},
	} {
		if err := os.WriteFile(executable, []byte("#!/bin/sh\n"+tc.body+"\n"), 0755); err != nil {
			t.Fatal(err)
		}
		r := Runner{Dir: dir, URL: api.URL, Token: "throwaway"}
		if err := r.Post(context.Background(), executable); (err == nil) != tc.ok {
			t.Fatalf("ok=%v err=%v", tc.ok, err)
		}
		files, _ := filepath.Glob(filepath.Join(dir, ".inventory-*"))
		if len(files) > 0 {
			t.Fatal("temporary inventory leaked")
		}
	}
	if calls != 1 {
		t.Fatalf("posted failed collection: %d", calls)
	}
}
func TestTokenAndOriginRefusals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	for _, tc := range []struct {
		value string
		mode  os.FileMode
		ok    bool
	}{{"", 0600, false}, {"one\ntwo", 0600, false}, {"safe", 0644, false}, {"safe\n", 0600, true}} {
		os.Remove(path)
		if err := os.WriteFile(path, []byte(tc.value), tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadToken(path); (err == nil) != tc.ok {
			t.Fatal("unexpected token acceptance")
		}
	}
	for _, url := range []string{"file:///tmp/x", "https://user:password@example.org", "https://example.org/?token=x", "https://example.org/path"} {
		if ValidateURL(url, false) == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
	for _, tc := range []struct {
		url       string
		allow, ok bool
	}{
		{"http://example.org", false, false}, {"http://192.0.2.10:8080", false, false},
		{"http://192.0.2.10:8080", true, true}, {"http://127.0.0.1:8080", false, true},
		{"http://[::1]:8080", false, true}, {"http://localhost:8080", false, true},
	} {
		if err := ValidateURL(tc.url, tc.allow); (err == nil) != tc.ok {
			t.Errorf("URL %s allow=%v: %v", tc.url, tc.allow, err)
		}
	}

	if ValidateURL("https://example.org:8080/", false) != nil {
		t.Fatal("valid origin refused")
	}
}
func TestPlainPosterRefusesRedirectAndHTTPError(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "discover")
	os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s' '{\"schema_version\":\"5\",\"host\":{\"hostname\":\"host-a\"}}'"), 0755)
	for _, status := range []int{302, 401, 500} {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "/inventory")
			w.WriteHeader(status)
		}))
		r := Runner{Dir: t.TempDir(), URL: api.URL, Token: "throwaway"}
		err := r.Post(context.Background(), executable)
		api.Close()
		if err == nil || strings.Contains(err.Error(), "throwaway") {
			t.Fatal("HTTP failure not safely reported")
		}
	}
}
