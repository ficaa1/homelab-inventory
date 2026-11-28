package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type actionTestHost struct {
	testHost
	dir  string
	logs bytes.Buffer
}

func (h *actionTestHost) CacheDir() string                { return h.dir }
func (h *actionTestHost) Logf(format string, args ...any) { fmt.Fprintf(&h.logs, format, args...) }
func actionHub(t *testing.T, handler http.HandlerFunc) (*Fleet, *actionTestHost) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "Bearer " + readToken
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/actions/") {
			expected = "Bearer " + operatorToken
			if !regexp.MustCompile(`^[A-Za-z0-9-]{16,64}$`).MatchString(r.Header.Get("Idempotency-Key")) || r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing action headers")
			}
		} else if r.Method != "GET" {
			t.Error("unexpected request method")
		}
		if r.Header.Get("Authorization") != expected {
			t.Error("wrong credential")
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	h := &actionTestHost{testHost: testHost{server.Client()}, dir: t.TempDir()}
	p := New(h).(*Fleet)
	if err := p.Configure(plugin.Config{"hub_url": server.URL, "read_token": readToken, "operator_token": operatorToken}); err != nil {
		t.Fatal(err)
	}
	return p, h
}

const doneReceipt = `{"api_version":1,"generated_at":"2026-12-01T09:16:02Z","receipt":{"state":"done","summary":"<Saved>","commit":"0123456789","hosts":[{"host":"host-a","state":"pending"}],"force":{"state":"queued"}}}`

type requestCase struct {
	id   string
	in   plugin.Config
	want string
}

func checkRequests(t *testing.T, cases []requestCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p, _ := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/actions/"+tc.id || r.Method != "POST" {
					t.Error("unexpected route")
				}
				var got, want map[string]any
				if json.NewDecoder(r.Body).Decode(&got) != nil || json.Unmarshal([]byte(tc.want), &want) != nil || !reflect.DeepEqual(got, map[string]any{"params": want}) {
					t.Error("wrong action body")
				}
				fmt.Fprint(w, doneReceipt)
			})
			v, err := p.Action(t.Context(), tc.id, tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = plugin.ValidateView(v, p.Manifest()); err != nil {
				t.Fatal(err)
			}
			if v.Title != "Saved" || !strings.Contains(v.Blocks[0].Text, "Pending on host-a · Commit 0123456") {
				t.Fatal("receipt display")
			}
			if tc.id == "force_file" && !strings.Contains(v.Blocks[0].Text, "Queued") {
				t.Fatal("missing force state")
			}
		})
	}
}
func TestFleetRequests(t *testing.T) {
	checkRequests(t, []requestCase{
		{"add_trial_source", plugin.Config{"name": "example-skills", "url": "https://git.example.com/skills", "glob": "skills/*", "include": " a, ,b", "hosts": "host-a,host-b,", "model_invoked": true}, `{"name":"example-skills","url":"https://git.example.com/skills","glob":"skills/*","include":["a","b"],"hosts":["host-a","host-b"],"model_invoked":true}`},
		{"add_trial_source", plugin.Config{"name": "example-skills", "url": "https://git.example.com/skills", "manifest": "skills.json"}, `{"name":"example-skills","url":"https://git.example.com/skills","manifest":"skills.json","include":[],"hosts":null,"model_invoked":false}`},
		{"end_trial", plugin.Config{"source": "example-skills", "undecided": "suggestion"}, `{"source":"example-skills","undecided":"suggestion"}`},
		{"remove_source", plugin.Config{"source": "example-skills"}, `{"source":"example-skills"}`},
		{"set_skill_mode", plugin.Config{"skill": "example-skills/lint", "mode": "model"}, `{"skill":"example-skills/lint","mode":"model"}`},
		{"force_file", plugin.Config{"host": "host-a", "item": "item-a", "live_sha256": "123"}, `{"host":"host-a","item":"item-a","live_sha256":"123"}`},
	})
}

func TestFleetActionInstances(t *testing.T) {
	h := fixtureHub(t)
	host := h.view(t, "host", url.Values{"name": {"host-a"}})
	if len(host.Actions) != 2 || host.Actions[0].Values["item"] == host.Actions[1].Values["item"] || !strings.Contains(host.Actions[0].Confirm, "+4 −1") {
		t.Fatal("wrong overwrite confirmations")
	}
	trials := h.view(t, "trials", nil)
	if len(trials.Actions) != 2 || trials.Actions[0].Values["source"] == trials.Actions[1].Values["source"] {
		t.Fatal("wrong trial confirmations")
	}
	skills := h.view(t, "skills", url.Values{"source": {"example-skills"}})
	if len(skills.Actions) != 2 || skills.Actions[1].ID != "remove_source" {
		t.Fatal("missing source action")
	}
	params := url.Values{"id": {"example-skills/lint-docs"}}
	v := h.view(t, "skill", params)
	if len(v.Actions) != 1 || v.Actions[0].Values["skill"] != params.Get("id") || v.Actions[0].Fields[1].Default != "model" || len(v.Blocks[1].Rows) != 2 {
		t.Fatal("wrong skill detail")
	}
	if v = h.view(t, "skill", url.Values{"id": {"missing"}}); v.Blocks[0].Type != "notice" {
		t.Fatal("missing skill notice")
	}
	h.operatorToken = ""
	for name, params := range map[string]url.Values{"skills": {"source": {"example-skills"}}, "trials": nil, "host": {"name": {"host-a"}}, "skill": params} {
		if len(h.view(t, name, params).Actions) > 0 {
			t.Fatal("action offered without operator token")
		}
	}
}

func TestActionReconcilesTimeout(t *testing.T) {
	keys := make(chan string, 4)
	release := make(chan struct{})
	defer close(release)
	p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		if r.Header.Get("Idempotency-Key") != "" && r.URL.Path == "/v1/actions/remove_source" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			case <-time.After(time.Millisecond * 150):
			}
		}
		fmt.Fprint(w, doneReceipt)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	defer cancel()
	values := plugin.Config{"source": "example-skills"}
	if _, err := p.Action(ctx, "remove_source", values); err == nil {
		t.Fatal("expected timeout")
	}
	var key string
	select {
	case key = <-keys:
	case <-time.After(time.Second):
		t.Fatal("hub did not receive first request")
	}
	files, err := os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing pending receipt")
	}
	info, err := files[0].Info()
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt permissions")
	}
	if _, err = p.Action(t.Context(), "remove_source", values); err != nil {
		t.Fatal(err)
	}
	if <-keys != key {
		t.Fatal("retry changed key")
	}
	files, err = os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 0 {
		t.Fatal("definitive receipt retained")
	}
}

func TestActionLocalValidation(t *testing.T) {
	p, _ := actionHub(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached hub") })
	for _, tc := range []struct {
		id, key string
		params  plugin.Config
	}{
		{"remove_source", "source", plugin.Config{"source": "Bad source"}},
		{"add_trial_source", "name", plugin.Config{"name": "bad name"}},
		{"add_trial_source", "url", plugin.Config{"name": "example-skills", "url": "http://example.com"}},
		{"add_trial_source", "glob", plugin.Config{"name": "example-skills", "url": "https://example.com", "glob": "*", "manifest": "manifest"}},
	} {
		_, err := p.Action(t.Context(), tc.id, tc.params)
		var field *plugin.FieldError
		if !errors.As(err, &field) || field.Key != tc.key {
			t.Error("missing field error", tc.key)
		}
	}
}
