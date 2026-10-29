package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/actions"
	"github.com/ficaa1/homelab-inventory/internal/execallow"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func TestActionsRoutesShareGuard(t *testing.T) {
	s := testServer(t, "test-token")
	inv := &inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: time.Now(), Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "homelab-api", ComposeProject: "api"}}}}
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.actionPolicy = execallow.Policy{Host: "host-a"}
	s.offerOperator = func(*http.Request) (string, bool) { return "operator", true }
	s.exec = &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return s.store.Latest("host-a") }, Execute: func(context.Context, string, map[string]string) execallow.Result {
		calls++
		return execallow.Result{ExitCode: 0}
	}}
	var err error
	s.actions, err = actions.Open(t.TempDir(), s.exec)
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	cases := []struct {
		method, path, body, ct, token string
		status                        int
	}{
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api"}`, "application/json", "", 401},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api"}`, "text/plain", "test-token", 415},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api"}`, "application/json", "test-token", 200},
		{"POST", "/actions/execute", `{"kind":"container_restart","target":"homelab-api"}`, "application/json", "test-token", 400},
		{"POST", "/actions/execute", fmt.Sprintf(`{"kind":"container_restart","target":"homelab-api","confirm":true,"container_set_hash":%q}`, actions.ContainerSetHash([]string{"homelab-api"})), "application/json", "test-token", 200},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api","host":"host-a"}`, "application/json", "test-token", 200},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api","host":"host-b"}`, "application/json", "test-token", 400},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api","host":""}`, "application/json", "test-token", 400},
		{"POST", "/actions/preview", `{"kind":"container_restart","target":"homelab-api","host":null}`, "application/json", "test-token", 400},
		{"POST", "/actions/execute", fmt.Sprintf(`{"kind":"container_restart","target":"homelab-api","host":"host-b","confirm":true,"container_set_hash":%q}`, actions.ContainerSetHash([]string{"homelab-api"})), "application/json", "test-token", 400},
		{"POST", "/actions/execute", fmt.Sprintf(`{"kind":"container_restart","target":"homelab-api","host":"host-a","confirm":true,"container_set_hash":%q}`, actions.ContainerSetHash([]string{"homelab-api"})), "application/json", "test-token", 200},
		{"GET", "/actions/stacks", "", "", "test-token", 200},
		{"GET", "/actions/stacks?host=host-a", "", "", "test-token", 200},
		{"GET", "/actions/stacks?host=host-b", "", "", "test-token", 200},
		{"GET", "/actions/log?limit=1", "", "", "test-token", 200},
		{"GET", "/actions/log?limit=101", "", "", "test-token", 400},
		{"POST", "/actions/execute", `{"kind":"container_restart","target":"homelab-api","confirm":true,"container_set_hash":"stale"} {}`, "application/json", "test-token", 400},
		{"POST", "/actions/execute", `{"kind":"container_restart","target":"homelab-api","confirm":true,"extra":1}`, "application/json", "test-token", 400},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", tc.ct)
		if tc.token != "" {
			req.Header.Set("Authorization", "Bearer "+tc.token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Fatalf("%s %s: got %d want %d: %s", tc.method, tc.path, rec.Code, tc.status, rec.Body)
		}
		if strings.HasPrefix(tc.path, "/actions/stacks") {
			var stacks []actions.Stack
			want := 1
			if tc.path == "/actions/stacks?host=host-b" {
				want = 0
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &stacks); err != nil || stacks == nil || len(stacks) != want {
				t.Fatalf("%s: unexpected stacks %s", tc.path, rec.Body)
			}
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	if s.actions.Log(1)[0].Actor != "operator" {
		t.Fatal("missing operator audit actor")
	}
}
func TestActionExecuteChangedPreviewAndPartialResponse(t *testing.T) {
	s := testServer(t, "test-token")
	inv := &inventory.Inventory{SchemaVersion: inventory.SchemaVersion, CollectedAt: time.Now(), Host: inventory.Host{Hostname: "host-a"}, Docker: &inventory.Docker{Available: true, Containers: []inventory.Container{{Name: "a", ComposeProject: "custom-project"}}}}
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.actionPolicy = execallow.Policy{Host: "host-a"}
	s.offerOperator = func(*http.Request) (string, bool) { return "operator", true }
	s.exec = &execallow.Runner{Policy: execallow.Policy{Host: "host-a"}, Inventory: func() (*inventory.Inventory, error) { return s.store.Latest("host-a") }, Execute: func(_ context.Context, cmd string, args map[string]string) execallow.Result {
		calls++
		if args["container"] == "b" {
			return execallow.Result{ExitCode: -1, Error: "fake daemon unavailable"}
		}
		return execallow.Result{}
	}}
	var err error
	s.actions, err = actions.Open(t.TempDir(), s.exec)
	if err != nil {
		t.Fatal(err)
	}
	h := s.routes()
	post := func(path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	req := actionRequest{Kind: "stack_restart", Target: "custom-project"}
	rec := post("/actions/preview", req)
	var preview actions.Preview
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &preview) != nil {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	req.ContainerSetHash, req.Confirm = preview.ContainerSetHash, true
	inv.CollectedAt = inv.CollectedAt.Add(time.Second)
	inv.Docker.Containers = append(inv.Docker.Containers, inventory.Container{Name: "b", ComposeProject: "custom-project"}, inventory.Container{Name: "c", ComposeProject: "custom-project"})
	if _, err := s.store.Save(inv); err != nil {
		t.Fatal(err)
	}
	rec = post("/actions/execute", req)
	var conflict struct {
		Error   string
		Preview actions.Preview
	}
	if rec.Code != 409 || json.Unmarshal(rec.Body.Bytes(), &conflict) != nil || !reflect.DeepEqual(conflict.Preview.Containers, []string{"a", "b", "c"}) || calls != 0 {
		t.Fatalf("conflict: %d %s calls=%d", rec.Code, rec.Body, calls)
	}
	// Fresh explicit confirmation uses only the returned inventory members.
	req.ContainerSetHash = conflict.Preview.ContainerSetHash
	rec = post("/actions/execute", req)
	var result actions.Entry
	if rec.Code != 500 || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Result != "partial" || calls != 2 {
		t.Fatalf("execute: %d %s calls=%d", rec.Code, rec.Body, calls)
	}
	want := []actions.ContainerResult{{Container: "a", Status: "restarted"}, {Container: "b", Status: "failed", Error: "fake daemon unavailable"}, {Container: "c", Status: "skipped"}}
	if !reflect.DeepEqual(result.Results, want) || !reflect.DeepEqual(s.actions.Log(1)[0].Results, want) {
		t.Fatalf("results: %+v", result.Results)
	}
}
