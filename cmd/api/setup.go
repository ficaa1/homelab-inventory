package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/auth"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
	"github.com/ficaa1/homelab-inventory/internal/setup"
	"github.com/ficaa1/homelab-inventory/internal/store"
	"github.com/ficaa1/homelab-inventory/scripts"
)

func (s *server) apiToken() string {
	if s.setup != nil {
		return s.setup.Tokens().API
	}
	return s.token
}
func (s *server) agentCredential() string {
	if s.setup != nil {
		return s.setup.Tokens().Agent
	}
	return s.agentToken
}
func (s *server) setupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /setup/status", s.handleSetupStatus)
	mux.HandleFunc("POST /setup/claim", s.handleSetupClaim)
	mux.HandleFunc("POST /setup/enroll", s.setupAuth(s.handleSetupEnroll))
	mux.HandleFunc("GET /setup/enroll", s.setupAuth(s.handleSetupEnrollList))
	mux.HandleFunc("POST /agent/enroll", s.handleAgentEnroll)
	mux.HandleFunc("GET /install.sh", s.handleInstaller)
	mux.HandleFunc("GET /agent/bin/SHA256SUMS", s.handleAgentBin)
	mux.HandleFunc("GET /agent/bin/{platform}/{binary}", s.handleAgentBin)
}
func (s *server) setupAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := "bearer"
		if len(r.Header.Values("Authorization")) == 0 && s.browserAuth != nil {
			// A signed-in owner can enroll hosts from the dashboard; the code it
			// creates grants only the agent's narrower authority.
			if session, ok := s.browserAuth.Session(r); ok {
				if !auth.CSRF(r, s.authConfig()) {
					writeError(w, 403, "missing CSRF header")
					return
				}
				actor = session.Email
			}
		}
		if actor == "bearer" && !httpx.BearerOK(r, s.apiToken(), httpx.DenyWhenUnset) {
			writeError(w, 401, "missing or invalid bearer token")
			return
		}
		r = withActor(r, actor)
		if s.setup == nil {
			writeError(w, 503, "enrollment unavailable in development mode")
			return
		}
		next(w, r)
	}
}
func (s *server) setupGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.setup != nil {
			state, _ := s.setup.Status()
			if state != "claimed" {
				p := r.URL.Path
				public := p == "/auth/status" || p == "/auth/login" || p == "/auth/callback" || p == "/auth/logout" || p == "/" || p == "/healthz" || p == "/manifest.webmanifest" || p == "/sw.js" || p == "/icon.svg" || strings.HasPrefix(p, "/setup/") || p == "/install.sh" || strings.HasPrefix(p, "/agent/bin/")
				if !public {
					writeError(w, 503, "setup required")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
func (s *server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	state, left := "claimed", 0
	if s.setup != nil {
		state, left = s.setup.Status()
	}
	out := map[string]any{"state": state}
	if state == "unclaimed" {
		origin, err := s.setupURL(r)
		if err != nil {
			writeError(w, 400, "invalid API host")
			return
		}
		out["window_seconds_left"] = left
		out["window_seconds_total"] = int(s.setup.Window() / time.Second)
		out["api_url"] = origin
		out["http"] = strings.HasPrefix(origin, "http:")
	}
	writeJSON(w, 200, out)
}
func setupBody(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(w, 415, "Content-Type must be application/json")
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	// Require an object (rather than accepting JSON null as an empty request).
	var raw json.RawMessage
	if dec.Decode(&raw) != nil || len(raw) == 0 || raw[0] != '{' || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one JSON object")
		return false
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		writeError(w, 400, "invalid request")
		return false
	}
	return true
}
func (s *server) handleSetupClaim(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.setup == nil {
		writeError(w, 409, "already claimed")
		return
	}
	var in struct{}
	if !setupBody(w, r, &in) {
		return
	}
	t, err := s.setup.Claim()
	switch {
	case errors.Is(err, setup.ErrClaimed):
		writeError(w, 409, err.Error())
	case errors.Is(err, setup.ErrExpired):
		writeError(w, 410, err.Error())
	case err != nil:
		writeError(w, 500, "saving setup credentials")
	default:
		s.log.Printf("setup claimed from %s", r.RemoteAddr)
		writeJSON(w, 200, map[string]string{"api_token": t.API, "agent_token": t.Agent})
	}
}
func (s *server) handleSetupEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var in struct{}
	if !setupBody(w, r, &in) {
		return
	}
	origin, err := s.setupURL(r)
	if err != nil {
		writeError(w, 400, "invalid API host")
		return
	}
	code, id, expires, err := s.setup.Enroll()
	if err != nil {
		status := 503
		if errors.Is(err, setup.ErrLimit) {
			status = 429
			err = errors.New("too many open codes")
		}
		writeError(w, status, err.Error())
		return
	}
	command := "curl -fsSL " + shellQuote(origin+"/install.sh") + " | sudo sh -s -- --enroll " + shellQuote(code)
	if strings.HasPrefix(origin, "http:") {
		command += " --allow-http"
	}
	if !s.binariesAvailable() {
		command = "printf '%s\\n' " + shellQuote("Agent binaries unavailable. Use the packaged image, configure AGENT_BIN_DIR, or follow manual setup in docs/install.md.") + " >&2; false"
	}
	writeJSON(w, 200, map[string]any{"id": id, "code": code, "expires_at": expires, "api_url": origin,
		"http": strings.HasPrefix(origin, "http:"), "commands": map[string]string{"linux": command}})
}
func (s *server) handleSetupEnrollList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	out := make([]map[string]any, 0)
	for _, c := range s.setup.List() {
		out = append(out, map[string]any{"id": c.ID, "code_hint": c.Hint, "expires_at": c.Expires, "used_by": c.UsedBy})
	}
	writeJSON(w, 200, map[string]any{"outstanding": out})
}
func (s *server) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.setup == nil {
		writeError(w, 403, "invalid enrollment code")
		return
	}
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if !s.setup.AllowAttempt(peer) {
		w.Header().Set("Retry-After", "60")
		writeError(w, 429, "enrollment rate limit exceeded")
		return
	}
	var in struct {
		Code string `json:"code"`
		Host string `json:"hostname"`
	}
	// Keep malformed, unknown, used and expired credentials indistinguishable.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&in) != nil || dec.Decode(new(any)) != io.EOF || store.ValidateHost(in.Host) != nil {
		writeError(w, 403, "invalid enrollment code")
		return
	}
	origin, err := s.setupURL(r)
	if err != nil {
		writeError(w, 400, "invalid API host")
		return
	}
	t, err := s.setup.Redeem(in.Code, in.Host)
	if err != nil {
		if errors.Is(err, setup.ErrCode) {
			writeError(w, 403, "invalid enrollment code")
		} else {
			writeError(w, 500, "saving enrollment")
		}
		return
	}
	writeJSON(w, 200, map[string]string{"agent_token": t.Agent, "api_url": origin})
}

var dnsHost = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?$`)

func validSetupHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	name := host
	if strings.Contains(host, ":") {
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			return net.ParseIP(host[1:len(host)-1]) != nil
		}
		h, p, e := net.SplitHostPort(host)
		if e != nil {
			return false
		}
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return false
		}
		name = h
	}
	if ip := net.ParseIP(name); ip != nil {
		return true
	}
	if !dnsHost.MatchString(name) {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if len(part) == 0 || len(part) > 63 || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return false
		}
	}
	return true
}
func setupOrigin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || !validSetupHost(u.Host) {
		return "", errors.New("expected an http(s) origin with a valid host and optional port")
	}
	return u.Scheme + "://" + u.Host, nil
}
func (s *server) setupURL(r *http.Request) (string, error) {
	if s.publicURL != "" {
		return setupOrigin(s.publicURL)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if s.trustedProxy {
		if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
			scheme = v
		}
		if v := r.Header.Get("X-Forwarded-Host"); v != "" {
			host = v
		}
	}
	if !validSetupHost(host) {
		return "", errors.New("invalid host")
	}
	return setupOrigin(scheme + "://" + host)
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
func (s *server) binariesAvailable() bool {
	for _, name := range []string{"SHA256SUMS", "linux-amd64/discover", "linux-amd64/agentupdate", "linux-arm64/discover", "linux-arm64/agentupdate"} {
		info, err := os.Stat(filepath.Join(s.agentBinDir, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return s.agentBinDir != ""
}
func (s *server) handleInstaller(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.binariesAvailable() {
		http.Error(w, "Agent binaries unavailable. Set AGENT_BIN_DIR to the packaged agent directory, or build with make dist and use manual setup (docs/install.md).", 404)
		return
	}
	origin, err := s.setupURL(r)
	if err != nil {
		writeError(w, 400, "invalid API host")
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	script := strings.Replace(scripts.Installer, "#!/bin/sh\n", "#!/bin/sh\nINSTALL_API_URL="+shellQuote(origin)+"\nINSTALL_DOWNLOAD=1\n", 1)
	io.WriteString(w, script)
}
func (s *server) handleAgentBin(w http.ResponseWriter, r *http.Request) {
	name := "SHA256SUMS"
	if r.URL.Path != "/agent/bin/SHA256SUMS" {
		p, b := r.PathValue("platform"), r.PathValue("binary")
		if p != "linux-amd64" && p != "linux-arm64" || b != "discover" && b != "agentupdate" {
			http.NotFound(w, r)
			return
		}
		name = filepath.Join(p, b)
	}
	path := filepath.Join(s.agentBinDir, name)
	info, err := os.Stat(path)
	if s.agentBinDir == "" || err != nil || !info.Mode().IsRegular() {
		http.Error(w, "Agent binary unavailable; configure AGENT_BIN_DIR or use manual setup (docs/install.md).", 404)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}
