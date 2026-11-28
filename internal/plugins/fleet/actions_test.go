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

const receiptFixture = `{"api_version":1,"generated_at":"2026-12-01T09:16:02Z","receipt":{"id":"rcp_fixture","action":%q,"state":"done","summary":"<Saved>","commit":"0123456789","hosts":[{"host":"host-a","state":"pending"}],"force":{"state":"queued"}}}`

func doneReceipt(action string) string { return fmt.Sprintf(receiptFixture, action) }

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
				fmt.Fprint(w, doneReceipt(strings.TrimPrefix(r.URL.Path, "/v1/actions/")))
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

func hostWithOverwrites(t *testing.T) []byte {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(fixture(t, "host-a"), &data); err != nil {
		t.Fatal(err)
	}
	var items []any
	for i := 1; i <= 20; i++ {
		path := fmt.Sprintf("skills/demo-%02d/SKILL.md", i)
		items = append(items, map[string]any{"id": "claude:" + path, "harness": "claude", "kind": "skill",
			"path": path, "state": "refused", "live_sha256": "example-live-hash", "forceable": true})
	}
	data["report"].(map[string]any)["items"] = items
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestFleetHostOffersTwentyOverwrites(t *testing.T) {
	data := hostWithOverwrites(t)
	p, _ := actionHub(t, func(w http.ResponseWriter, r *http.Request) { w.Write(data) })
	v := checkedView(t, p, "host", url.Values{"name": {"host-a"}})
	if len(v.Actions) != 20 {
		t.Fatal("wrong number of host actions", len(v.Actions))
	}
	for i, a := range v.Actions {
		if a.ID != "force_file" || !a.Destructive || a.Label != fmt.Sprintf("Overwrite skills/demo-%02d/SKILL.md", i+1) {
			t.Fatal("wrong overwrite action or order")
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
		fmt.Fprint(w, doneReceipt(strings.TrimPrefix(r.URL.Path, "/v1/actions/")))
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

func TestReceiptLifetimeAndHubScope(t *testing.T) {
	p, h := actionHub(t, func(http.ResponseWriter, *http.Request) { t.Error("reservation made a hub request") })
	values := plugin.Config{"source": "example-skills"}
	reserve := func() (string, string) {
		t.Helper()
		path, key, err := p.reserve("remove_source", values)
		if err != nil {
			t.Fatal(err)
		}
		return path, key
	}
	configure := func(base string) {
		t.Helper()
		if err := p.Configure(plugin.Config{"hub_url": base, "read_token": readToken, "operator_token": operatorToken}); err != nil {
			t.Fatal(err)
		}
	}
	base := p.base
	path, key := reserve()
	backdate := func(age time.Duration) {
		t.Helper()
		data, err := json.Marshal(pendingReceipt{key, h.Now().Add(-age)})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	backdate(30*24*time.Hour - time.Second)
	configure(base + "/")
	if normalized, reused := reserve(); normalized != path || reused != key {
		t.Fatal("expected reuse on the same hub within 30 days")
	}
	configure("https://another-hub.example.com")
	if other, otherKey := reserve(); other == path || otherKey == key {
		t.Fatal("different hub reused the pending reservation")
	}
	configure(base)
	backdate(30 * 24 * time.Hour)
	if same, fresh := reserve(); same != path || fresh == key {
		t.Fatal("expired reservation was reused")
	}
	p.operatorToken = "different-synthetic-operator-token"
	if other, otherKey := reserve(); other == path || otherKey == key {
		t.Fatal("different operator token reused the pending reservation")
	}
}

func TestActionKeepsReservationUntilDefinitiveReply(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		released bool
	}{
		{"valid receipt", 200, doneReceipt("remove_source"), true},
		{"empty 2xx", 204, "", false},
		{"garbage 2xx", 200, "broken", false},
		{"interrupted 2xx", 200, doneReceipt("remove_source"), false},
		{"trailing data", 200, doneReceipt("remove_source") + "{}", false},
		{"oversized 2xx", 200, doneReceipt("remove_source") + strings.Repeat(" ", 4<<20), false},
		{"wrong envelope", 200, strings.Replace(doneReceipt("remove_source"), "api_version", "version", 1), false},
		{"no timestamp", 200, strings.Replace(doneReceipt("remove_source"), "generated_at", "time", 1), false},
		{"missing receipt", 200, `{"api_version":1,"generated_at":"2026-12-01T09:16:02Z"}`, false},
		{"missing identity", 200, strings.Replace(doneReceipt("remove_source"), "rcp_fixture", "", 1), false},
		{"wrong action", 200, doneReceipt("force_file"), false},
		{"unknown state", 200, strings.Replace(doneReceipt("remove_source"), "done", "unknown", 1), false},
		{"missing summary", 200, strings.Replace(doneReceipt("remove_source"), "<Saved>", "", 1), false},
		{"missing failure code", 200, strings.Replace(doneReceipt("remove_source"), "done", "failed", 1), false},
		{"valid failure receipt", 200, strings.Replace(doneReceipt("remove_source"), `"state":"done"`, `"state":"failed","error":{"code":"conflict"}`, 1), true},
		{"undocumented 422", 422, `{"error":{"message":"Check the source"}}`, false},
		{"unusable 503", 503, "broken", false},
		{"unknown 500", 500, `{"error":{"code":"unknown"}}`, false},
		{"upstream failure", 502, `{"error":{"code":"upstream_failed"}}`, false},
		{"bad_request", 400, `{"error":{"code":"bad_request"}}`, true},
		{"unauthorized", 401, `{"error":{"code":"unauthorized"}}`, false},
		{"forbidden", 403, `{"error":{"code":"forbidden"}}`, false},
		{"not_found", 404, `{"error":{"code":"not_found"}}`, true},
		{"conflict", 409, `{"error":{"code":"conflict"}}`, true},
		{"not_editable", 409, `{"error":{"code":"not_editable"}}`, true},
		{"idempotency_mismatch", 409, `{"error":{"code":"idempotency_mismatch"}}`, true},
		{"too_large", 413, `{"error":{"code":"too_large"}}`, true},
		{"invalid_param", 422, `{"error":{"code":"invalid_param","message":"Check <source>","field":"source"}}`, true},
		{"rate_limited", 429, `{"error":{"code":"rate_limited"}}`, false},
		{"busy", 503, `{"error":{"code":"busy"}}`, false},
		{"validation code with server error", 500, `{"error":{"code":"invalid_param"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
				if tc.name == "interrupted 2xx" {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)+1))
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			})
			_, err := p.Action(t.Context(), "remove_source", plugin.Config{"source": "example-skills"})
			if tc.name == "invalid_param" {
				var field *plugin.FieldError
				if !errors.As(err, &field) || field.Key != "source" || field.Message != "Check source" {
					t.Fatal("expected a sanitized field error")
				}
			}
			if tc.name == "unauthorized" && (err == nil || err.Error() != "The hub rejected the operator token. Check it in Admin → Plugins.") {
				t.Fatal("wrong unauthorized message")
			}
			files, readErr := os.ReadDir(filepath.Join(h.dir, "receipts"))
			if readErr != nil || (len(files) == 0) != tc.released {
				t.Fatal("wrong reservation state after reply", len(files), readErr)
			}
		})
	}
}

func TestActionReplaysAfterLostReplyAndRejectedRetry(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			keys := make(chan string, 3)
			p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
				keys <- r.Header.Get("Idempotency-Key")
				switch len(keys) {
				case 1:
					body := doneReceipt("remove_source")
					w.Header().Set("Content-Length", fmt.Sprint(len(body)))
					w.WriteHeader(http.StatusAccepted)
					fmt.Fprint(w, body[:len(body)/2])
				case 2:
					w.WriteHeader(status)
					code := map[int]string{http.StatusUnauthorized: "unauthorized", http.StatusTooManyRequests: "rate_limited"}[status]
					fmt.Fprintf(w, `{"error":{"code":%q}}`, code)
				case 3:
					fmt.Fprint(w, doneReceipt("remove_source"))
				}
			})
			values := plugin.Config{"source": "example-skills"}
			for i := 0; i < 2; i++ {
				if _, err := p.Action(t.Context(), "remove_source", values); err == nil {
					t.Fatal("expected error on incomplete or rejected reply")
				}
				files, err := os.ReadDir(filepath.Join(h.dir, "receipts"))
				if err != nil || len(files) != 1 {
					t.Fatal("pending receipt was lost")
				}
			}
			if _, err := p.Action(t.Context(), "remove_source", values); err != nil {
				t.Fatal(err)
			}
			first := <-keys
			if <-keys != first || <-keys != first {
				t.Fatal("retry changed key")
			}
		})
	}
}

func TestActionWorkingReceiptOnAcceptedAndReplay(t *testing.T) {
	const id = "proxy_refresh_account"
	keys := make(chan string, 4)
	p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		body := doneReceipt(id)
		if len(keys) == 1 {
			w.WriteHeader(http.StatusAccepted)
		}
		if len(keys) <= 2 {
			body = strings.Replace(body, "done", "working", 1)
		}
		fmt.Fprint(w, body)
	})
	values := plugin.Config{"account": "acc_a"}
	for range 2 {
		v, err := p.Action(t.Context(), id, values)
		if err != nil || v.Title != "Working: Saved" || len(v.Blocks) != 1 || v.Blocks[0].Text != "Still working. The result shows here when it lands." {
			t.Fatal("working receipt was not shown as pending")
		}
	}
	v, err := p.Action(t.Context(), id, values)
	if err != nil || v.Title != "Saved" {
		t.Fatal("final replay was not shown as done")
	}
	files, err := os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 0 {
		t.Fatal("done replay did not release key")
	}
	if _, err = p.Action(t.Context(), id, values); err != nil {
		t.Fatal(err)
	}
	first := <-keys
	if <-keys != first || <-keys != first || <-keys == first {
		t.Fatal("expected key reuse through the done replay, then a new key")
	}
}
