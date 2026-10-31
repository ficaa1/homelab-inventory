package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ficaa1/homelab-inventory/internal/deviceauth"
	"github.com/ficaa1/homelab-inventory/internal/duskpersonal"
	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/mediadata"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type duskState struct {
	mu       sync.Mutex
	devices  *deviceauth.Store
	personal *duskpersonal.Store
	path     string
	saved    duskSaved
}
type duskReceipt struct {
	Target string `json:"target"`
	State  string `json:"state"`
}
type duskSaved struct {
	Enrichment map[string]int                     `json:"enrichment"`
	Policies   map[string]mediadata.RequestPolicy `json:"policies"`
	Receipts   map[string]duskReceipt             `json:"receipts"`
}

func (s *server) duskStore() (*duskState, error) {
	s.duskOnce.Do(func() {
		if s.store == nil {
			s.duskErr = errors.New("storage unavailable")
			return
		}
		dir := filepath.Join(s.store.Dir(), "plugins", "dusk", "core")
		if err := os.MkdirAll(dir, 0700); err != nil {
			s.duskErr = err
			return
		}
		d := &duskState{path: filepath.Join(dir, "policy.json"), saved: duskSaved{Policies: map[string]mediadata.RequestPolicy{}, Receipts: map[string]duskReceipt{}, Enrichment: map[string]int{}}}
		d.devices, s.duskErr = deviceauth.New(dir)
		if s.duskErr != nil {
			return
		}
		d.personal, s.duskErr = duskpersonal.New(dir)
		if s.duskErr != nil {
			return
		}
		b, err := os.ReadFile(d.path)
		if err == nil {
			if json.Unmarshal(b, &d.saved) != nil || d.saved.Policies == nil || d.saved.Receipts == nil {
				s.duskErr = errors.New("invalid dusk state")
				return
			}
		} else if !os.IsNotExist(err) {
			s.duskErr = err
			return
		}
		if d.saved.Enrichment == nil {
			d.saved.Enrichment = map[string]int{}
		}
		s.dusk = d
	})
	return s.dusk, s.duskErr
}
func (d *duskState) save() error {
	b, err := json.Marshal(d.saved)
	if err != nil {
		return err
	}
	return fsx.WriteAtomic(d.path, b, 0600)
}
func (s *server) duskRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /dusk/admin", s.auth(s.duskAdmin))
	m.HandleFunc("GET /dusk/options", s.auth(s.duskOptions))
	m.HandleFunc("GET /dusk/enrichment", s.auth(s.duskEnrichment))
	m.HandleFunc("PUT /dusk/enrichment/{id}", s.auth(s.duskEnrichmentWrite))
	for _, route := range []string{"POST /dusk/profiles", "DELETE /dusk/profiles/{id}", "POST /dusk/codes", "DELETE /dusk/devices/{id}", "PUT /dusk/policies/{id}", "POST /dusk/import/{id}"} {
		m.HandleFunc(route, s.auth(s.duskAdminWrite))
	}
	m.HandleFunc("POST /tv/v1/pair", s.duskPair)
	for _, method := range []string{"GET", "POST"} {
		m.HandleFunc(method+" /tv/{version}/{operation}", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if r.PathValue("version") != "v1" {
				writeError(w, 400, "Unsupported TV protocol version; this server supports version 1")
				return
			}
			writeError(w, 404, "Unknown TV protocol route")
		})
	}
	for _, route := range []string{"GET /tv/v1/capabilities", "GET /tv/v1/media", "GET /tv/v1/browse", "GET /tv/v1/discover", "GET /tv/v1/search", "GET /tv/v1/title/{kind}/{id}", "GET /tv/v1/similar/{kind}/{id}", "GET /tv/v1/taste/picks", "GET /tv/v1/taste/familiar", "POST /tv/v1/request", "POST /tv/v1/preference", "POST /tv/v1/discover/refresh", "POST /tv/v1/taste/verdict"} {
		m.HandleFunc(route, s.duskTV)
	}
}
func (s *server) duskEnabled() bool {
	if s.plugins == nil {
		return false
	}
	_, ok := s.plugins.DuskSettings()
	return ok
}
func (s *server) duskAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d, err := s.duskStore()
	if err != nil {
		writeError(w, 503, "Dusk storage unavailable")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	writeJSON(w, 200, map[string]any{"profiles": d.devices.Profiles(), "devices": d.devices.Devices(), "policies": d.saved.Policies, "enrichment": d.saved.Enrichment})
}
func (s *server) duskOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		writeError(w, 503, "Media not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	v, err := s.plugins.MediaOptions(ctx)
	if err != nil {
		writeError(w, 502, "Request options unavailable; check Media connections")
		return
	}
	writeJSON(w, 200, v)
}
func (s *server) duskAdminWrite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	d, err := s.duskStore()
	if err != nil {
		writeError(w, 503, "Dusk storage unavailable")
		return
	}
	id := r.PathValue("id")
	switch {
	case r.Method == "POST" && r.URL.Path == "/dusk/profiles":
		var v deviceauth.Profile
		if !pluginBody(w, r, &v, false) {
			return
		}
		out, e := d.devices.SaveProfile(v)
		if e != nil {
			writeError(w, 400, "Invalid profile")
			return
		}
		writeJSON(w, 201, out)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/dusk/profiles/"):
		if d.devices.DeleteProfile(id) != nil {
			writeError(w, 400, "Invalid profile")
			return
		}
		d.mu.Lock()
		delete(d.saved.Policies, id)
		delete(d.saved.Enrichment, id)
		e := d.save()
		d.mu.Unlock()
		if e != nil {
			writeError(w, 503, "Policy storage unavailable")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.Method == "DELETE":
		if d.devices.Revoke(id) != nil {
			writeError(w, 400, "Invalid device")
			return
		}
		writeJSON(w, 200, map[string]bool{"ok": true})
	case r.URL.Path == "/dusk/codes":
		if !s.duskEnabled() {
			writeError(w, 503, "Dusk integration disabled")
			return
		}
		var v struct {
			ProfileID string   `json:"profile_id"`
			Scopes    []string `json:"scopes"`
		}
		if !pluginBody(w, r, &v, false) {
			return
		}
		out, e := d.devices.CreateCode(v.ProfileID, v.Scopes)
		if e != nil {
			writeError(w, 400, "Invalid profile, permissions or code limit")
			return
		}
		writeJSON(w, 201, out)
	case strings.HasPrefix(r.URL.Path, "/dusk/import/"):
		var v struct {
			CSV string `json:"csv"`
		}
		if !pluginBody(w, r, &v, false) {
			return
		}
		if !hasProfile(d, id) {
			writeError(w, 400, "Invalid profile")
			return
		}
		out, e := d.personal.Import(id, v.CSV)
		if e != nil {
			writeError(w, 400, "Invalid CSV import")
			return
		}
		writeJSON(w, 200, out)
	default:
		var v mediadata.RequestPolicy
		if !pluginBody(w, r, &v, false) {
			return
		}
		if !hasProfile(d, id) || len(v.Label) > 100 || (v.Approval != "manual" && v.Approval != "automatic") || v.RequesterID <= 0 {
			writeError(w, 400, "Invalid request policy")
			return
		}
		if s.plugins == nil {
			writeError(w, 422, "Configure Media first")
			return
		}
		e := s.plugins.MediaValidatePolicy(r.Context(), v)
		if e != nil {
			writeError(w, 422, "Select configured destinations and a requester whose approval permissions match the policy")
			return
		}
		d.mu.Lock()
		old, had := d.saved.Policies[id]
		d.saved.Policies[id] = v
		e = d.save()
		if e != nil {
			if had {
				d.saved.Policies[id] = old
			} else {
				delete(d.saved.Policies, id)
				delete(d.saved.Enrichment, id)
			}
		}
		d.mu.Unlock()
		if e != nil {
			writeError(w, 503, "Policy storage unavailable")
			return
		}
		writeJSON(w, 200, v)
	}
}
func hasProfile(d *duskState, id string) bool {
	for _, p := range d.devices.Profiles() {
		if p.ID == id {
			return true
		}
	}
	return false
}
func (s *server) duskPair(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.duskEnabled() {
		writeError(w, 503, "Dusk integration disabled")
		return
	}
	var v struct {
		Code string `json:"code"`
		Name string `json:"device_name"`
	}
	if !tvBody(w, r, &v) {
		return
	}
	d, e := s.duskStore()
	if e != nil {
		writeError(w, 503, "Dusk storage unavailable")
		return
	}
	ip, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		ip = r.RemoteAddr
	}
	device, token, e := d.devices.Redeem(v.Code, v.Name, ip)
	if e != nil {
		status := 400
		if errors.Is(e, deviceauth.ErrRateLimited) {
			status = 429
		}
		writeError(w, status, "Invalid or expired pairing code; try again later")
		return
	}
	writeJSON(w, 201, map[string]any{"api_version": 1, "device_id": device.ID, "token": token, "capabilities": s.duskCapabilities(d, device)})
}
func tvBody(w http.ResponseWriter, r *http.Request, v any) bool {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		writeError(w, 415, "JSON required")
		return false
	}
	return pluginBody(w, r, v, false)
}
func scope(d deviceauth.Device, k string) bool {
	for _, v := range d.Scopes {
		if v == k {
			return true
		}
	}
	return false
}
func (s *server) duskCapabilities(d *duskState, dev deviceauth.Device) map[string]any {
	c, _ := s.plugins.DuskSettings()
	m := s.plugins.MediaCapabilities()
	profile := deviceauth.Profile{ID: dev.ProfileID}
	for _, p := range d.devices.Profiles() {
		if p.ID == dev.ProfileID {
			profile = p
		}
	}
	discovery := c.Discovery
	d.mu.Lock()
	p, has := d.saved.Policies[dev.ProfileID]
	d.mu.Unlock()
	features := map[string]bool{"media": m.Media && scope(dev, "media"), "upcoming": m.Upcoming && scope(dev, "upcoming"), "discover": m.Discover && discovery && scope(dev, "discover"), "search": m.Search && discovery && scope(dev, "search"), "requests": m.Requests && has && c.Requests && scope(dev, "requests"), "preferences": m.Discover && discovery && scope(dev, "preferences"), "refresh": m.Discover && discovery && scope(dev, "refresh"), "taste": m.Discover && c.Taste && scope(dev, "taste")}
	out := map[string]any{"api_version": 1, "server_name": c.ServerName, "device_id": dev.ID, "profile": profile, "features": features}
	if features["requests"] {
		out["request_policy"] = map[string]string{"label": p.Label, "approval": p.Approval}
	}
	return out
}
func (s *server) duskTV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.duskEnabled() {
		writeError(w, 503, "Dusk integration disabled")
		return
	}
	d, err := s.duskStore()
	if err != nil {
		writeError(w, 503, "Dusk storage unavailable")
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(r.Header.Values("Authorization")) != 1 {
		writeError(w, 401, "Invalid device credential")
		return
	}
	dev, err := d.devices.Authenticate(token)
	if err != nil {
		writeError(w, 401, "Invalid device credential")
		return
	}
	op := strings.TrimPrefix(r.URL.Path, "/tv/v1/")
	caps := s.duskCapabilities(d, dev)
	if op == "capabilities" {
		writeJSON(w, 200, caps)
		return
	}
	permission := op
	switch {
	case op == "browse":
		permission = "media"
	case strings.HasPrefix(op, "title/"):
		permission = "search"
	case strings.HasPrefix(op, "similar/"):
		permission = "discover"
	case strings.HasPrefix(op, "taste/"):
		permission = "taste"
	case op == "preference":
		permission = "preferences"
	case op == "request":
		permission = "requests"
	case op == "discover/refresh":
		permission = "refresh"
	}
	if !caps["features"].(map[string]bool)[permission] {
		writeError(w, 403, "Feature unavailable or device permission denied")
		return
	}
	kind := r.PathValue("kind")
	id := 0
	if r.PathValue("id") != "" {
		id, err = strconv.Atoi(r.PathValue("id"))
		if err != nil || !validTitle(kind, id) {
			writeError(w, 400, "Invalid title")
			return
		}
	}
	q := r.URL.Query().Get("q")
	if op == "search" && (strings.TrimSpace(q) == "" || len([]rune(q)) > 80 || strings.IndexFunc(q, unicode.IsControl) >= 0) {
		writeError(w, 400, "Enter a title up to 80 characters")
		return
	}
	if strings.HasPrefix(op, "taste/") && r.Method == "GET" {
		kind = r.URL.Query().Get("kind")
		if kind != "movie" && kind != "tv" {
			writeError(w, 400, "Invalid kind")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 19*time.Second)
	defer cancel()
	if r.Method == "POST" {
		s.duskWrite(w, r, ctx, d, dev, op)
		return
	}
	readOp := strings.Split(op, "/")[0]
	if readOp == "taste" {
		readOp = "discover"
	}
	out, err := s.plugins.MediaRead(ctx, readOp, kind, id, q)
	if err != nil {
		writeError(w, 502, "Media source unavailable")
		return
	}
	if report, ok := out.(mediadata.Discovery); ok {
		report, err = s.personalize(ctx, d, dev.ProfileID, report)
		if err != nil {
			writeError(w, 503, "Profile feedback unavailable")
			return
		}
		out = report
		if strings.HasPrefix(op, "taste/") {
			out = d.personal.Taste(dev.ProfileID, kind, report, op == "taste/familiar")
		}
	}
	if titles, ok := out.(mediadata.Titles); ok {
		titles = d.personal.Explicit(dev.ProfileID, titles)
		out = titles
	}
	if detail, ok := out.(mediadata.Detail); ok {
		detail.RequestsEnabled = caps["features"].(map[string]bool)["requests"]
		b, _ := json.Marshal(detail)
		v := map[string]any{}
		_ = json.Unmarshal(b, &v)
		if detail.RequestsEnabled {
			v["request_policy"] = caps["request_policy"]
		}
		out = v
	}
	if feed, ok := out.(map[string]any); ok && op == "media" && !caps["features"].(map[string]bool)["upcoming"] {
		feed["upcoming"] = nil
	}
	writeJSON(w, 200, out)
}

var duskOperationID = regexp.MustCompile(`^[A-Za-z0-9-]{16,80}$`)

func validTitle(k string, id int) bool {
	return (k == "movie" || k == "tv") && id > 0 && id < 1000000000
}
func (s *server) personalize(ctx context.Context, d *duskState, profile string, report mediadata.Discovery) (mediadata.Discovery, error) {
	d.mu.Lock()
	userID := d.saved.Enrichment[profile]
	d.mu.Unlock()
	if userID > 0 {
		history, e := s.plugins.MediaHistory(ctx, userID)
		if e != nil {
			report.Partial = true
		} else {
			report.History = history
		}
	}
	e := d.personal.Enrich(ctx, profile, func(ctx context.Context, q string) ([]mediadata.Title, error) {
		v, e := s.plugins.MediaRead(ctx, "search", "", 0, q)
		if e != nil {
			return nil, e
		}
		titles, ok := v.(mediadata.Titles)
		if !ok {
			return nil, errors.New("invalid search")
		}
		return titles.Titles, nil
	})
	if e != nil {
		report.Partial = true
	}
	return d.personal.Apply(ctx, profile, report, func(ctx context.Context, k string, id int) (mediadata.Discovery, error) {
		v, e := s.plugins.MediaRead(ctx, "similar", k, id, "")
		if e != nil {
			return mediadata.Discovery{}, e
		}
		rep, ok := v.(mediadata.Discovery)
		if !ok {
			return rep, errors.New("invalid discovery")
		}
		return rep, nil
	})
}
func (s *server) duskWrite(w http.ResponseWriter, r *http.Request, ctx context.Context, d *duskState, dev deviceauth.Device, op string) {
	switch op {
	case "preference":
		var v struct {
			Kind   string `json:"kind"`
			ID     int    `json:"id"`
			Hidden *bool  `json:"hidden"`
		}
		if !tvBody(w, r, &v) {
			return
		}
		if !validTitle(v.Kind, v.ID) || v.Hidden == nil {
			writeError(w, 400, "Invalid preference")
			return
		}
		if d.personal.SetHidden(dev.ProfileID, v.Kind, v.ID, *v.Hidden) != nil {
			writeError(w, 503, "Preference storage unavailable")
			return
		}
	case "taste/verdict":
		var v struct {
			Kind    string `json:"kind"`
			ID      int    `json:"id"`
			Verdict string `json:"verdict"`
			Title   string `json:"title"`
			Year    string `json:"year"`
		}
		if !tvBody(w, r, &v) {
			return
		}
		if !validTitle(v.Kind, v.ID) || d.personal.Verdict(dev.ProfileID, v.Kind, v.ID, v.Verdict, v.Title, v.Year) != nil {
			writeError(w, 400, "Invalid verdict")
			return
		}
	case "discover/refresh":
		if !tvBody(w, r, &struct{}{}) {
			return
		}
		v, e := s.plugins.MediaRead(ctx, "refresh", "", 0, "")
		if e != nil {
			writeError(w, 502, "Discovery unavailable")
			return
		}
		rep, ok := v.(mediadata.Discovery)
		if !ok {
			writeError(w, 502, "Discovery unavailable")
			return
		}
		rep, e = s.personalize(ctx, d, dev.ProfileID, rep)
		if e != nil {
			writeError(w, 503, "Profile feedback unavailable")
			return
		}
		writeJSON(w, 200, rep)
		return
	case "request":
		var v mediadata.Request
		if !tvBody(w, r, &v) {
			return
		}
		if !validTitle(v.Kind, v.ID) || !v.Confirmed || v.Season < 0 || v.Season > 1000 || (v.Kind == "tv" && v.Season < 1) || (v.Kind == "movie" && v.Season != 0) || !duskOperationID.MatchString(v.Operation) {
			writeError(w, 400, "Invalid confirmed request")
			return
		}
		target := dev.ProfileID + ":" + v.Kind + ":" + strconv.Itoa(v.ID) + ":" + strconv.Itoa(v.Season)
		key := dev.ProfileID + ":op:" + v.Operation
		d.mu.Lock()
		if receipt, exists := d.saved.Receipts[key]; exists {
			d.mu.Unlock()
			if receipt.Target != target {
				writeError(w, 409, "Operation already belongs to another title")
				return
			}
			writeJSON(w, 200, mediadata.RequestResult{State: receipt.State})
			return
		}
		for _, receipt := range d.saved.Receipts {
			if receipt.Target == target && receipt.State != "rejected" && receipt.State != "configuration" {
				d.mu.Unlock()
				writeJSON(w, 200, mediadata.RequestResult{State: receipt.State})
				return
			}
		}
		if len(d.saved.Receipts) >= 20000 {
			d.mu.Unlock()
			writeError(w, 503, "Request receipt limit reached")
			return
		}
		policy := d.saved.Policies[dev.ProfileID]
		d.saved.Receipts[key] = duskReceipt{Target: target, State: "unknown"}
		if d.save() != nil {
			delete(d.saved.Receipts, key)
			d.mu.Unlock()
			writeError(w, 503, "Request receipt storage unavailable")
			return
		}
		d.mu.Unlock()
		out, _ := s.plugins.MediaRequest(ctx, policy, v)
		if out.State == "" {
			out.State = "unknown"
		}
		d.mu.Lock()
		d.saved.Receipts[key] = duskReceipt{Target: target, State: out.State}
		if d.save() != nil {
			out.State = "unknown"
			d.saved.Receipts[key] = duskReceipt{Target: target, State: "unknown"}
		}
		d.mu.Unlock()
		writeJSON(w, 200, out)
		return
	default:
		writeError(w, 404, "Unknown action")
		return
	}
	writeJSON(w, 200, map[string]string{"state": "ok"})
}

func (s *server) duskEnrichment(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		writeJSON(w, 200, []mediadata.Choice{})
		return
	}
	v, e := s.plugins.MediaProfiles(r.Context())
	if e != nil {
		writeError(w, 502, "History profiles unavailable")
		return
	}
	writeJSON(w, 200, v)
}
func (s *server) duskEnrichmentWrite(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var v struct {
		UserID int `json:"user_id"`
	}
	if !pluginBody(w, r, &v, false) {
		return
	}
	d, e := s.duskStore()
	if e != nil {
		writeError(w, 503, "Dusk storage unavailable")
		return
	}
	id := r.PathValue("id")
	if !hasProfile(d, id) || v.UserID < 0 {
		writeError(w, 400, "Invalid profile")
		return
	}
	if v.UserID > 0 {
		if s.plugins == nil {
			writeError(w, 422, "Media is not configured")
			return
		}
		choices, e := s.plugins.MediaProfiles(r.Context())
		found := false
		for _, c := range choices {
			if c.ID == v.UserID {
				found = true
			}
		}
		if e != nil || !found {
			writeError(w, 422, "Select a configured history profile")
			return
		}
	}
	d.mu.Lock()
	old := d.saved.Enrichment[id]
	d.saved.Enrichment[id] = v.UserID
	e = d.save()
	if e != nil {
		d.saved.Enrichment[id] = old
	}
	d.mu.Unlock()
	if e != nil {
		writeError(w, 503, "Profile storage unavailable")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
