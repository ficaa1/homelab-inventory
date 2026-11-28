package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

const (
	readToken     = "read-fixture-only"
	operatorToken = "operator-fixture-only"
)

type testHost struct{ client *http.Client }

func (h testHost) HTTP() *http.Client { return h.client }
func (testHost) CacheDir() string     { return "" }
func (testHost) Logf(string, ...any)  {}
func (testHost) Now() time.Time       { return time.Date(2027, 1, 2, 12, 0, 0, 0, time.UTC) }

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeHub struct {
	*Fleet
	calls atomic.Int32
}

func newHub(t *testing.T, tls bool, answer func(http.ResponseWriter, *http.Request)) *fakeHub {
	t.Helper()
	f := new(fakeHub)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		token := r.Header.Get("Authorization")
		operator := r.URL.Path == "/v1/meta" && token == "Bearer "+operatorToken
		if !slices.Contains([]string{"/v1/meta", "/v1/overview", "/v1/hosts", "/v1/hosts/host-a", "/v1/hosts/host-b", "/v1/hosts/host-missing", "/v1/skills", "/v1/trials"}, r.URL.Path) || r.Method != "GET" || r.URL.RawQuery != "" || (!operator && token != "Bearer "+readToken) {
			t.Error("unexpected hub method, route, query or credential")
			w.WriteHeader(400)
			return
		}
		answer(w, r)
	})
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(handler)
	} else {
		s = httptest.NewServer(handler)
	}
	t.Cleanup(s.Close)
	f.Fleet = New(testHost{s.Client()}).(*Fleet)
	if err := f.Configure(plugin.Config{"hub_url": s.URL + "/", "read_token": readToken, "operator_token": operatorToken}); err != nil {
		t.Fatal(err)
	}
	return f
}
func fixtureHub(t *testing.T) *fakeHub {
	return newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/")
		name = strings.TrimPrefix(name, "hosts/")
		if name == "host-missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write(fixture(t, name))
	})
}
func assertPrivate(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{readToken, operatorToken, "private-query-value"} {
		if strings.Contains(string(b), s) {
			t.Fatal("serialized output revealed a credential or query")
		}
	}
}
func (f *fakeHub) view(t *testing.T, name string, params url.Values) plugin.View {
	t.Helper()
	v, err := f.View(t.Context(), name, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = plugin.ValidateView(v, f.Manifest()); err != nil {
		t.Fatal(name, err)
	}
	assertPrivate(t, v)
	if v.RefreshSeconds != 60 || !v.UpdatedAt.Equal(f.host.Now()) {
		t.Fatal("incorrect refresh settings")
	}
	return v
}
func TestConfigure(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"hub_url", "ftp://hub.example.lan", "http://"}, {"hub_url", "http:///x", "host name"}, {"hub_url", "https://:8790", "host name"}, {"hub_url", "https://user@hub.example.lan", "credentials"},
		{"hub_url", "https://hub.example.lan/?x=y", "query"}, {"hub_url", "https://hub.example.lan?", "query"}, {"hub_url", "https://hub.example.lan#", "fragment"},
		{"hub_url", "https://hub.example.lan/prefix/v1/", "without /v1"}, {"hub_url", "https://hub.example.lan/%761", "without /v1"},
		{"read_token", "", "Enter"}, {"read_token", "one\rtwo", "one line"}, {"operator_token", "one\ntwo", "one line"},
	} {
		t.Run(tc.key+"_"+tc.want, func(t *testing.T) {
			p := new(Fleet)
			c := plugin.Config{"hub_url": "https://hub.example.lan/base/", "read_token": readToken}
			if err := p.Configure(c); err != nil || p.base != "https://hub.example.lan/base" {
				t.Fatal("valid base", err)
			}
			c[tc.key] = tc.value
			var fe *plugin.FieldError
			if err := p.Configure(c); !errors.As(err, &fe) || fe.Key != tc.key || !strings.Contains(fe.Message, tc.want) {
				t.Fatalf("unexpected validation: %v", err)
			}
			assertPrivate(t, fe)
			if p.readToken != readToken || p.base != "https://hub.example.lan/base" {
				t.Fatal("failed save changed configuration")
			}
			p.Close()
			if p.readToken != "" || p.operatorToken != "" {
				t.Fatal("Close retained tokens")
			}
		})
	}
}

func TestConnectionChecks(t *testing.T) {
	for _, tc := range []struct {
		name, readRole, opRole string
		version                int
		tls, operator, ok      bool
	}{
		{"read only", "read", "", 1, false, false, true}, {"both roles", "operator", "operator", 1, true, true, true}, {"role mismatch", "read", "read", 1, false, true, false}, {"future API", "read", "operator", 2, false, true, false}, {"bad read role", "report", "", 1, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var credentials []string
			h := newHub(t, tc.tls, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/meta" {
					t.Error("Test used non-meta route")
				}
				role := tc.readRole
				if r.Header.Get("Authorization") == "Bearer "+operatorToken {
					role = tc.opRole
				}
				credentials = append(credentials, r.Header.Get("Authorization"))
				json.NewEncoder(w).Encode(map[string]any{"api_version": tc.version, "role": role, "generated_at": "2026-12-01T09:15:00Z", "new_field": true})
			})
			wantCredentials := []string{"Bearer " + readToken}
			if !tc.operator {
				h.operatorToken = ""
			} else {
				wantCredentials = append(wantCredentials, "Bearer "+operatorToken)
			}
			r := h.Test(t.Context())
			assertPrivate(t, r)
			if r.OK != tc.ok || !slices.Equal(wantCredentials, credentials) {
				t.Fatal("unexpected checks or tokens", r)
			}
			checks := map[string]plugin.Check{}
			for _, c := range r.Checks {
				checks[c.Name] = c
			}
			if !checks["Hub reachable"].OK || checks["API version 1"].OK != (tc.version == 1) || checks["Read token accepted"].OK != (tc.version == 1 && tc.readRole != "report") {
				t.Fatal(r)
			}
			if !checks["Connection"].OK || strings.Contains(checks["Connection"].Message, "unencrypted") == tc.tls {
				t.Fatal(r)
			}
			if tc.name == "role mismatch" && (checks["Operator token"].OK || checks["Operator token"].Message != "This token has read access only; use the operator token") {
				t.Fatal(r)
			}
		})
	}
}
func TestBadParametersStayLocal(t *testing.T) {
	h := newHub(t, false, func(http.ResponseWriter, *http.Request) { t.Error("invalid param reached the hub") })
	for _, tc := range []struct {
		view   string
		params url.Values
	}{
		{"host", nil}, {"host", url.Values{"name": {"../meta"}}}, {"host", url.Values{"name": {"host-a", "../meta"}}}, {"host", url.Values{"name": {"HOST-A"}}},
		{"skills", url.Values{"source": {""}}}, {"skills", url.Values{"mode": {""}}}, {"skills", url.Values{"source": {"../other"}}}, {"skills", url.Values{"source": {"example-skills", "/bad"}}}, {"skills", url.Values{"mode": {"unknown"}}}, {"skills", url.Values{"mode": {"model", "/bad"}}},
	} {
		v := h.view(t, tc.view, tc.params)
		if len(v.Blocks) != 1 || v.Blocks[0].Type != "notice" {
			t.Fatal(v)
		}
	}
	for _, name := range []string{"usage", "proxy", "actions"} {
		_, err := h.View(t.Context(), name, nil)
		if !errors.Is(err, plugin.ErrNotFound) {
			t.Fatalf("unexpected view %s", name)
		}
	}
	if h.calls.Load() != 0 {
		t.Fatal("unexpected HTTP")
	}
}
func TestSafeHubErrors(t *testing.T) {
	for _, status := range []int{401, 500, 307} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHub(t, false, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/overview" {
					t.Error("unexpected redirect")
				}
				w.Header().Set("Location", "/v1/meta")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "Check <settings>\u0001 " + readToken + " " + operatorToken + " https://hub.example.lan/log?private-query-value"}})
			})
			v, err := h.View(t.Context(), "overview", nil)
			assertPrivate(t, v)
			if err == nil {
				t.Fatal("hub failure hidden")
			}
			assertPrivate(t, err.Error())
			if strings.ContainsAny(err.Error(), "<>\u0001?") {
				t.Fatal("unsafe message")
			}
			if status == 401 && err.Error() != "The hub rejected the read token. Check it in Admin → Plugins." {
				t.Fatal(err)
			}
			if status == 500 && !strings.Contains(err.Error(), "Check settings") {
				t.Fatal("missing hub fix-it text")
			}
			if h.calls.Load() != 1 {
				t.Fatal("redirect followed")
			}
		})
	}
	h := fixtureHub(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := h.View(ctx, "skills", nil)
	if err == nil || err.Error() != "Fleet hub unavailable. Check hub_url and that the hub is running." {
		t.Fatal(err)
	}
	assertPrivate(t, err.Error())
}

func TestFleetRegistryLifecycle(t *testing.T) {
	h := fixtureHub(t)
	r, err := plugin.Open(t.TempDir(), []plugin.Builtin{{ID: "fleet", New: New}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Update("fleet", plugin.Update{Settings: plugin.Config{"hub_url": h.base, "read_token": nil, "operator_token": nil}}); err != nil {
		t.Fatal("save disabled settings", err)
	}
	if _, err := r.Test(t.Context(), "fleet", nil); err == nil {
		t.Fatal("missing required token accepted")
	}
	on := true
	if _, err := r.Update("fleet", plugin.Update{Enabled: &on, Settings: plugin.Config{"read_token": readToken}}); err != nil {
		t.Fatal(err)
	}
	if h.calls.Load() != 0 {
		t.Fatal("configuration performed network I/O")
	}
	result, err := r.Test(t.Context(), "fleet", nil)
	if err != nil || !result.OK {
		t.Fatal(result, err)
	}
	b, err := r.View(t.Context(), "fleet", "hosts", nil)
	if err != nil || !strings.Contains(string(b), "host-a") {
		t.Fatal("guarded host read failed", err)
	}
	on = false
	if _, err := r.Update("fleet", plugin.Update{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.View(t.Context(), "fleet", "hosts", nil); !errors.Is(err, plugin.ErrNotFound) {
		t.Fatal("disabled view available")
	}
	on = true
	if _, err := r.Update("fleet", plugin.Update{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.View(t.Context(), "fleet", "overview", nil); err != nil {
		t.Fatal("re-enable lost credentials", err)
	}
}
