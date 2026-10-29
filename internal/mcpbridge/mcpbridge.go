// Package mcpbridge exposes generic inventory API reads as typed MCP tools.
// Requests use fixed paths and escaped host names. Redirects are refused to
// keep bearer credentials scoped to the configured API. Transport is stdio.
package mcpbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/agentupdate"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

// Option declares one argument a tool accepts, beyond the optional host
// name. Kind is "string", "int", "boolean" or "strings" (a JSON array of
// strings).
type Option struct {
	Name  string
	Kind  string // "string", "int", "boolean" or "strings"
	Notes string
}

// Tool is one read-only API endpoint exposed over MCP.
type Tool struct {
	Name        string
	Description string
	Method      string
	// Path is called with the validated arguments and returns the URL path
	// (with a {name} placeholder) and query values for the endpoint.
	Path    func(args map[string]any) (string, url.Values, error)
	options []Option
}

// NewTool builds a read-only GET tool. options describe the query parameters
// beyond the mandatory host name (empty for collection endpoints).
func NewTool(name, description, path string, options ...Option) Tool {
	t := Tool{
		Name:        name,
		Description: description,
		Method:      http.MethodGet,
		options:     options,
	}
	t.Path = func(args map[string]any) (string, url.Values, error) {
		q := url.Values{}
		for _, o := range options {
			v, ok := args[o.Name]
			if !ok || v == nil {
				continue
			}
			s := fmt.Sprint(v)
			if s == "" {
				continue
			}
			if o.Kind == "int" {
				n, err := strconv.Atoi(s)
				if err != nil {
					return "", nil, fmt.Errorf("%s must be a number, got %q", o.Name, s)
				}
				q.Set(o.Name, strconv.Itoa(n))
				continue
			}
			q.Set(o.Name, s)
		}
		return path, q, nil
	}
	return t
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t Tool) InputSchema() map[string]any {
	props := map[string]any{
		"name": map[string]any{
			"type":        "string",
			"description": "host name, as shown by GET /hosts; omit for fleet-wide tools",
		},
	}
	for _, o := range t.options {
		desc := o.Notes
		if desc == "" {
			desc = o.Name
		}
		switch o.Kind {
		case "strings":
			props[o.Name] = map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": desc,
			}
		default:
			// JSON Schema has no "int" type; translate the internal kind.
			schemaType := o.Kind
			if schemaType == "int" {
				schemaType = "integer"
			}
			props[o.Name] = map[string]any{
				"type":        schemaType,
				"description": desc,
			}
		}
	}
	return map[string]any{
		"type":       "object",
		"properties": props,
	}
}

// Server is an MCP stdio server proxying read-only GET endpoints of the
// dashboard API.
type Server struct {
	baseURL string
	token   string
	client  *http.Client
	in      io.Reader
	out     io.Writer
	// Tools is the catalog served by tools/list. It is a field so tests can
	// substitute a fake; production uses DefaultTools.
	Tools []Tool
}

// New returns a Server talking to the API at baseURL (for example
// "http://localhost:8080"). token, when non-empty, is sent as a bearer
// token on every request.
func New(baseURL, token string, in io.Reader, out io.Writer) *Server {
	return &Server{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		client: &http.Client{
			Timeout: 60 * time.Second,
			// Never follow a redirect: the token is scoped to baseURL and
			// must not be replayed against another host.
			CheckRedirect: httpx.RefuseRedirects(),
		},
		in:    in,
		out:   out,
		Tools: DefaultTools(),
	}
}

// DefaultTools is the catalog of API endpoints this server exposes. Every
// entry is a GET; adding a mutating endpoint here would violate the package
// contract and the repository's read-only discovery invariant.
func DefaultTools() []Tool {
	return []Tool{
		NewTool("hosts", "List every host the dashboard has collected an inventory for.", "/hosts"),
		NewTool("host",
			"Full latest inventory JSON for one host (system, storage, network, services, shares, firewall state). May be large.",
			"/hosts/{name}",
			Option{Name: "at", Kind: "string", Notes: "run id from host_history to fetch a specific past inventory instead of the latest"}),
		NewTool("host_packages",
			"Installed packages from the latest daily host snapshot, with explicit collection status and freshness. Image sets sample one running container per image ID.",
			"/hosts/{name}/packages",
			Option{Name: "q", Kind: "string", Notes: "substring of package name"},
			Option{Name: "scope", Kind: "string", Notes: "host, image ID, or container name"},
			Option{Name: "limit", Kind: "int", Notes: "default 200, maximum 1000, across selected sets"},
			Option{Name: "offset", Kind: "int", Notes: "nonnegative pagination offset"}),
		NewTool("package_changes", "Package changes between daily software snapshots, newest first, with incomplete-set notes.", "/hosts/{name}/packages/changes", Option{Name: "limit", Kind: "int", Notes: "default 30, maximum 365"}),
		NewTool("package_search",
			"Search packages across latest daily fleet snapshots; includes collection coverage and unknown hosts.",
			"/packages",
			Option{Name: "name", Kind: "string", Notes: "required exact package name or prefix ending in *"}),
		NewTool("host_history",
			"Stored inventory runs for one host, newest first, with timestamps and ids. Use the ids with at/from/to on other tools.",
			"/hosts/{name}/history"),
		NewTool("host_drift",
			"What changed on one host between two runs (default: the previous run against the latest). The core 'what broke since this morning' answer.",
			"/hosts/{name}/drift",
			Option{Name: "from", Kind: "string", Notes: "run id of the older run"},
			Option{Name: "to", Kind: "string", Notes: "run id of the newer run"}),
		NewTool("host_trend",
			"Capacity over time for one host: pools, storage and filesystems, with a fill-date forecast when the data can support one.",
			"/hosts/{name}/trend"),
		NewTool("host_deps",
			"What breaks if a container on this host stops: the dependency graph, per container.",
			"/hosts/{name}/deps"),
		NewTool("host_compose",
			"Docker compose files on this host as version history.",
			"/hosts/{name}/compose"),
		NewTool("compose_diff",
			"Unified diff between two revisions of a compose project on this host.",
			"/hosts/{name}/compose/diff",
			Option{Name: "project", Kind: "string", Notes: "compose project name"},
			Option{Name: "path", Kind: "string", Notes: "file path within the project"},
			Option{Name: "from", Kind: "string", Notes: "revision id"},
			Option{Name: "to", Kind: "string", Notes: "revision id"}),
		NewTool("firewall_plan",
			"Suggested inbound firewall ruleset for one host, generated from the ports it actually uses. Text only — nothing is applied.",
			"/hosts/{name}/firewall-plan",
			Option{Name: "lan", Kind: "string", Notes: "CIDR such as 192.0.2.24/24 to treat as the local network"},
			Option{Name: "ssh_port", Kind: "int", Notes: "management SSH port to keep open"}),
		NewTool("images",
			"Image references across the fleet, and how firmly each is pinned.",
			"/images"),
		NewTool("ports",
			"Port map across all hosts: who listens on what, and which ports are free.",
			"/ports"),
		NewTool("alerts",
			"Threshold alerts currently firing across the fleet, with severity.",
			"/alerts"),
		NewTool("services",
			"The curated, categorised service catalog (the dashboard launcher).",
			"/services"),
		NewTool("notify_status",
			"Notification state: apprise endpoint health and the transition state the notifier keeps.",
			"/notify"),
		NewTool("silences",
			"Acknowledgements: alerts that are acknowledged without being hidden.",
			"/silences"),

		NewTool("storage",
			"Fleet capacity snapshot: every PVE storage, ZFS pool and real filesystem across all hosts, with used/available/percent and the delta against the previous run. The 'space left' answer.",
			"/storage"),
	}
}

const maxResponse = 2 << 20 // 2 MiB: a whole inventory fits, a runaway does not

// Run reads newline-delimited JSON-RPC messages from in until EOF or ctx
// cancellation, and writes responses to out. It returns nil on a clean EOF.
func (s *Server) Run(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	enc := json.NewEncoder(s.out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			_ = enc.Encode(rpcError{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   rpcErrorBody{Code: -32700, Message: "parse error"},
			})
			continue
		}
		if msg.Method == "" { // a response or malformed request; nothing to do
			continue
		}
		// A JSON-RPC notification is any request without an id: the server
		// must not reply. Guard here so every dispatch path can safely assume
		// msg.ID is non-nil.
		if msg.ID == nil {
			continue
		}
		if err := enc.Encode(s.dispatch(ctx, msg)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return nil
}

type rpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
}

type rpcError struct {
	JSONRPC string       `json:"jsonrpc"`
	ID      any          `json:"id"`
	Error   rpcErrorBody `json:"error"`
}

type rpcErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) dispatch(ctx context.Context, msg rpcMessage) any {
	switch msg.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(msg.Params, &p)
		// Echo the client's version; servers that hard-code a version get
		// negotiation failures with newer clients.
		version := p.ProtocolVersion
		if version == "" {
			version = "2024-11-05"
		}
		return rpcResponse{JSONRPC: "2.0", ID: *msg.ID, Result: map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "homelab-bridge", "version": "1.0.0"},
		}}
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: *msg.ID, Result: map[string]any{}}
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: *msg.ID, Result: map[string]any{"tools": s.toolList()}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			return rpcError{JSONRPC: "2.0", ID: *msg.ID, Error: rpcErrorBody{Code: -32602, Message: "invalid params: " + err.Error()}}
		}
		return s.call(ctx, *msg.ID, p.Name, p.Arguments)
	default:
		return rpcError{JSONRPC: "2.0", ID: *msg.ID, Error: rpcErrorBody{Code: -32601, Message: "method not found: " + msg.Method}}
	}
}

func (s *Server) toolList() []map[string]any {
	out := make([]map[string]any, 0, len(s.Tools))
	for _, t := range s.Tools {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.InputSchema(),
		})
	}
	return out
}

func (s *Server) call(ctx context.Context, id json.RawMessage, name string, args map[string]any) any {
	var tool *Tool
	for i := range s.Tools {
		if s.Tools[i].Name == name {
			tool = &s.Tools[i]
			break
		}
	}
	if tool == nil {
		return rpcError{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: -32602, Message: "unknown tool: " + name}}
	}
	host, validName := args["name"].(string)
	host = strings.TrimSpace(host)
	path, query, err := tool.Path(args)
	if err != nil {
		return rpcError{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: -32602, Message: err.Error()}}
	}
	if strings.Contains(path, "{name}") {
		if !validName || host == "" {
			return rpcError{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: -32602, Message: "name is required and must be a string"}}
		}
		if host == "." || host == ".." || strings.Contains(host, "/") {
			return rpcError{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: -32602, Message: "name must be a host name without slash or dot path components"}}
		}
	}

	path = strings.ReplaceAll(path, "{name}", url.PathEscape(host))
	u := s.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	body, status, err := s.fetch(ctx, u)
	if err != nil {
		return rpcError{JSONRPC: "2.0", ID: id, Error: rpcErrorBody{Code: -32603, Message: "fetch failed: " + err.Error()}}
	}
	result := map[string]any{
		"content": []map[string]any{{
			"type": "text",
			"text": truncate(body),
		}},
	}
	if status >= 400 {
		result["isError"] = true
		result["content"] = []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf("API returned HTTP %d: %s", status, truncate(body)),
		}}
	}
	return rpcResponse{JSONRPC: "2.0", ID: id, Result: result}
}

func (s *Server) fetch(ctx context.Context, u string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if s.token != "" {
		req.Header.Set("Authorization", "Bearer "+s.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, 0, err
	}
	return b, resp.StatusCode, nil
}

func truncate(b []byte) string {
	if len(b) <= maxResponse {
		return string(b)
	}
	return string(b[:maxResponse]) + "\n… (truncated at 2 MiB)"
}

// EnvToken reads the bearer token from API_TOKEN, or from the file named
// by API_TOKEN_FILE (mode 0600). The file path exists so the token never
// has to appear in an argv or process list.
func EnvToken() (string, error) {
	if t := os.Getenv("API_TOKEN"); t != "" {
		return t, nil
	}
	if f := os.Getenv("API_TOKEN_FILE"); f != "" {
		token, err := agentupdate.ReadToken(f)
		if err != nil {
			return "", fmt.Errorf("API_TOKEN_FILE: %w", err)
		}
		return token, nil
	}
	return "", nil
}
