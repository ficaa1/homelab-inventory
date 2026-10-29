package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call sends one JSON-RPC request line and returns the decoded response.
func call(t *testing.T, srv *Server, req map[string]any) map[string]any {
	t.Helper()
	in := bytes.NewBuffer(nil)
	out := bytes.NewBuffer(nil)
	_ = json.NewEncoder(in).Encode(req)
	ss := New(srv.baseURL, srv.token, in, out)
	ss.Tools = srv.Tools
	if err := ss.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	sc := bufio.NewScanner(out)
	if !sc.Scan() {
		t.Fatalf("no response line for %v", req["method"])
	}
	var resp map[string]any
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		t.Fatalf("bad response JSON %q: %v", sc.Text(), err)
	}
	return resp
}

func fakeAPI(t *testing.T, token string) (*httptest.Server, *[]string) {
	t.Helper()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.String())
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"error":"missing or invalid bearer token"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/hosts":
			w.Write([]byte(`{"generated_at":"x","hosts":["host-b","host-a","host-c"]}`))
		case "/hosts/host-c":
			w.Write([]byte(`{"name":"host-c","schema_version":"5"}`))
		case "/hosts/host-a/drift":
			w.Write([]byte(`{"changed":["firewall"],"from":"a","to":"b"}`))
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func TestInitializeEchoesProtocolVersion(t *testing.T) {
	srv := New("http://example.test", "", nil, nil)
	resp := call(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "test"}},
	})
	if resp["id"] != float64(1) {
		t.Fatalf("id = %v, want 1", resp["id"])
	}
	res := resp["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("protocolVersion = %v, want client's version echoed", res["protocolVersion"])
	}
	info := res["serverInfo"].(map[string]any)
	if info["name"] != "homelab-bridge" {
		t.Fatalf("serverInfo = %v", info)
	}
}

func TestToolsListHasSchemaForOptions(t *testing.T) {
	srv := New("http://example.test", "", nil, nil)
	resp := call(t, srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := resp["result"].(map[string]any)["tools"].([]any)
	byName := map[string]map[string]any{}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		byName[tool["name"].(string)] = tool
	}
	host, ok := byName["host"]
	if !ok {
		t.Fatalf("no host tool; have %d tools", len(tools))
	}
	props := host["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["at"]; !ok {
		t.Fatalf("host tool schema missing optional at option: %v", props)
	}
	fw, ok := byName["firewall_plan"]
	if !ok {
		t.Fatal("no firewall_plan tool")
	}
	fwProps := fw["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if p, ok := fwProps["ssh_port"]; !ok || p.(map[string]any)["type"] != "integer" {
		t.Fatalf("firewall_plan ssh_port schema wrong: %v", fwProps)
	}
}

func TestToolCallHitsAPIWithToken(t *testing.T) {
	api, hits := fakeAPI(t, "sekrit")
	srv := New(api.URL, "sekrit", nil, nil)
	resp := call(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "host", "arguments": map[string]any{"name": "host-c"}},
	})
	res := resp["result"].(map[string]any)
	if res["isError"] != nil && res["isError"] == true {
		t.Fatalf("unexpected error result: %v", res)
	}
	content := res["content"].([]any)[0].(map[string]any)
	if !strings.Contains(content["text"].(string), `"name":"host-c"`) {
		t.Fatalf("body not passed through: %v", content["text"])
	}
	if len(*hits) != 1 || (*hits)[0] != "/hosts/host-c" {
		t.Fatalf("hits = %v, want exactly /hosts/host-c", *hits)
	}
}

func TestToolCallSendsQueryParams(t *testing.T) {
	api, hits := fakeAPI(t, "sekrit")
	srv := New(api.URL, "sekrit", nil, nil)
	for _, tc := range []struct {
		tool string
		args map[string]any
		path string
	}{
		{"host_drift", map[string]any{"name": "host-a", "from": "aaa", "to": "bbb"}, "/hosts/host-a/drift?from=aaa&to=bbb"},
		{"host_packages", map[string]any{"name": "host-a", "q": "ssl", "scope": "web", "limit": 10, "offset": 20}, "/hosts/host-a/packages?limit=10&offset=20&q=ssl&scope=web"},
		{"package_changes", map[string]any{"name": "host-a", "limit": 7}, "/hosts/host-a/packages/changes?limit=7"},
		{"package_search", map[string]any{"name": "libssl*"}, "/packages?name=libssl%2A"},
	} {
		call(t, srv, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tc.tool, "arguments": tc.args},
		})
		if got := (*hits)[len(*hits)-1]; got != tc.path {
			t.Fatalf("%s hit %s, want %s", tc.tool, got, tc.path)
		}
	}
}

func TestToolCallRequiresSafeName(t *testing.T) {
	api, hits := fakeAPI(t, "fake")
	srv := New(api.URL, "fake", nil, nil)
	for _, args := range []map[string]any{nil, {}, {"name": nil}, {"name": 3}, {"name": ""}, {"name": "."}, {"name": ".."}, {"name": "../host-c"}, {"name": "a/b"}} {
		resp := call(t, srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "host", "arguments": args}})
		e, ok := resp["error"].(map[string]any)
		if !ok || e["code"] != float64(-32602) {
			t.Fatalf("expected invalid arguments: %v", resp)
		}
	}
	if len(*hits) != 0 {
		t.Fatal("invalid name reached API")
	}
}

func TestTokenFileMustBePrivateAndReadable(t *testing.T) {
	t.Setenv("API_TOKEN", "")
	file := filepath.Join(t.TempDir(), "token")
	t.Setenv("API_TOKEN_FILE", file)
	if _, err := EnvToken(); err == nil {
		t.Fatal("missing file ignored")
	}
	for _, mode := range []os.FileMode{0644, 0640, 0604, 0600} {
		os.Remove(file)
		if err := os.WriteFile(file, []byte("obviously-fake-test-token\n"), mode); err != nil {
			t.Fatal(err)
		}
		token, err := EnvToken()
		if (err == nil) != (mode == 0600) {
			t.Fatalf("mode %o error=%v", mode, err)
		}
		if mode == 0600 && token != "obviously-fake-test-token" {
			t.Fatal("bad token read")
		}
	}
}

func TestAPIErrorBecomesIsError(t *testing.T) {
	api, _ := fakeAPI(t, "sekrit")
	// The bridge holds a different token than the API expects: every request
	// must come back as a 401 isError result, never a panic or a cache hit.
	srv := New(api.URL, "wrongtoken", nil, nil)
	resp := call(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "hosts", "arguments": map[string]any{}},
	})
	res := resp["result"].(map[string]any)
	if res["isError"] != true {
		t.Fatalf("want isError=true for a 401, got %v", res)
	}
}

func TestCollectionToolWithoutName(t *testing.T) {
	api, hits := fakeAPI(t, "sekrit")
	srv := New(api.URL, "sekrit", nil, nil)
	resp := call(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "hosts", "arguments": map[string]any{}},
	})
	if resp["error"] != nil {
		t.Fatalf("collection tool without name should work, got %v", resp)
	}
	if len(*hits) != 1 || (*hits)[0] != "/hosts" {
		t.Fatalf("hits = %v, want /hosts", *hits)
	}
}

func TestNotificationWithoutIDDoesNotPanic(t *testing.T) {
	// A JSON-RPC 2.0 request without an id is a notification: the server must
	// not reply. Before the guard was widened beyond the notifications/ prefix,
	// an id-less "ping" dereferenced a nil *json.RawMessage and crashed the
	// whole stdio process.
	in := bytes.NewBufferString(`{"jsonrpc":"2.0","method":"ping"}` + "\n")
	out := bytes.NewBuffer(nil)
	srv := New("http://example.test", "", in, out)
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "" {
		t.Fatalf("notification got a response %q, want none", got)
	}
}

func TestStorageToolUsesAuthenticatedGET(t *testing.T) {
	hits := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Method != http.MethodGet || r.URL.Path != "/storage" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer test" {
			t.Error("storage tool crossed GET summary boundary")
		}
		w.Write([]byte(`{"rows":[]}`))
	}))
	defer api.Close()
	srv := New(api.URL, "test", nil, nil)
	resp := call(t, srv, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "storage", "arguments": map[string]any{}},
	})
	if resp["error"] != nil || hits != 1 {
		t.Fatal("storage tool failed")
	}
}

func TestPluginToolsReadOnlyProjectionAndQueryEscaping(t *testing.T) {
	var received string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("plugin tool is not a GET")
		}
		received = r.URL.RequestURI()
		if r.URL.Path == "/plugins" {
			w.Write([]byte(`[{"id":"sample","settings":{"key":{"set":true}},"settings_schema":[{"key":"key"}]}]`))
		} else {
			w.Write([]byte(`{"title":"Now","blocks":[]}`))
		}
	}))
	defer api.Close()
	srv := New(api.URL, "", nil, nil)
	for _, tc := range []struct {
		name string
		args map[string]any
		path string
	}{
		{"plugins_list", map[string]any{}, "/plugins"},
		{"plugin_view", map[string]any{"plugin": "sample", "view": "main", "params": map[string]any{"q": "a&b"}}, "/plugins/sample/views/main?q=a%26b"},
	} {
		resp := call(t, srv, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": tc.name, "arguments": tc.args}})
		if resp["error"] != nil || received != tc.path {
			t.Fatal("plugin GET route or escaping failed")
		}
		data, _ := json.Marshal(resp)
		if tc.name == "plugins_list" && bytes.Contains(data, []byte("settings")) {
			t.Fatal("MCP list includes settings")
		}
	}
	tool := pluginViewTool()
	for _, args := range []map[string]any{{"plugin": "../sample", "view": "main"}, {"plugin": "sample", "view": "../main"}, {"plugin": "sample", "view": "main", "params": map[string]any{"q": true}}} {
		if _, _, err := tool.Path(args); err == nil {
			t.Fatal("invalid plugin arguments accepted")
		}
	}
}
