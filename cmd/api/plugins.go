package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
	"github.com/ficaa1/homelab-inventory/internal/plugins/archive"
	"github.com/ficaa1/homelab-inventory/internal/plugins/cloudflare"
	"github.com/ficaa1/homelab-inventory/internal/plugins/dusk"
	"github.com/ficaa1/homelab-inventory/internal/plugins/media"
	"github.com/ficaa1/homelab-inventory/internal/plugins/network"
	"github.com/ficaa1/homelab-inventory/internal/plugins/security"
	"github.com/ficaa1/homelab-inventory/internal/plugins/taste"
	"github.com/ficaa1/homelab-inventory/internal/plugins/tdarr"
	"github.com/ficaa1/homelab-inventory/internal/plugins/uploads"
)

// builtInPlugins is the explicit composition root. No implicit registration or
// external plugin loading is supported.
func builtInPlugins() []plugin.Builtin {
	return []plugin.Builtin{{ID: "tdarr", New: tdarr.New}, {ID: "security", New: security.New}, {ID: "cloudflare", New: cloudflare.New}, {ID: "taste", New: taste.New}, {ID: "archive", New: archive.New}, {ID: "dusk", New: dusk.New}, {ID: "media", New: media.New}, {ID: "uploads", New: uploads.New}, {ID: "network", New: network.New}}
}
func openPlugins(data string, logger *log.Logger) (*plugin.Registry, error) {
	r, err := plugin.Open(data, builtInPlugins(), logger)
	if err == nil {
		r.BindTasteSource()
	}
	return r, err
}
func (s *server) pluginRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /uploads/targets", s.auth(s.handleUploadTargets))
	mux.HandleFunc("POST /plugins/{id}/upload/{target}", s.auth(s.handlePluginUpload))
	mux.HandleFunc("POST /plugins/{id}/actions/{action}", s.handlePluginAction)
	mux.HandleFunc("GET /plugins", s.auth(s.handlePlugins))
	mux.HandleFunc("PUT /plugins/{id}", s.auth(s.handlePluginUpdate))
	mux.HandleFunc("POST /plugins/{id}/test", s.auth(s.handlePluginTest))
	mux.HandleFunc("GET /plugins/{id}/views/{name}", s.auth(s.handlePluginView))
	mux.HandleFunc("GET /plugins/{id}/images/{ref}", s.auth(s.handlePluginImage))
}
func (s *server) pluginCapabilities() []plugin.Capability {
	if s.plugins == nil {
		return []plugin.Capability{}
	}
	return s.plugins.Capabilities()
}
func (s *server) handlePlugins(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		writeJSON(w, 200, []plugin.ItemStatus{})
		return
	}
	writeJSON(w, 200, s.plugins.List())
}
func pluginBody(w http.ResponseWriter, r *http.Request, v any, optional bool) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	var raw json.RawMessage
	err := dec.Decode(&raw)
	if optional && err == io.EOF {
		return true
	}
	if err != nil || len(raw) == 0 || raw[0] != '{' || dec.Decode(new(any)) != io.EOF {
		writeError(w, 400, "expected one JSON object")
		return false
	}
	// Decode again with a strict schema. Error messages deliberately omit input.
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		writeError(w, 400, "invalid plugin request")
		return false
	}
	return true
}
func (s *server) pluginError(w http.ResponseWriter, err error) {
	var validation *plugin.ValidationError
	switch {
	case errors.As(err, &validation):
		writeJSON(w, 422, validation)
	case errors.Is(err, plugin.ErrNotFound):
		writeError(w, 404, err.Error())
	case errors.Is(err, plugin.ErrBusy):
		writeError(w, 429, err.Error())
	default:
		writeError(w, 502, err.Error())
	}
}
func (s *server) handlePluginUpdate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		s.pluginError(w, plugin.ErrNotFound)
		return
	}
	var in plugin.Update
	if !pluginBody(w, r, &in, false) {
		return
	}
	out, err := s.plugins.Update(r.PathValue("id"), in)
	if err != nil {
		s.pluginError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func (s *server) handlePluginTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		s.pluginError(w, plugin.ErrNotFound)
		return
	}
	var in struct {
		Settings plugin.Config `json:"settings"`
	}
	if !pluginBody(w, r, &in, true) {
		return
	}
	out, err := s.plugins.Test(r.Context(), r.PathValue("id"), in.Settings)
	if err != nil {
		s.pluginError(w, err)
		return
	}
	writeJSON(w, 200, out)
}
func (s *server) handlePluginView(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		s.pluginError(w, plugin.ErrNotFound)
		return
	}
	data, err := s.plugins.View(r.Context(), r.PathValue("id"), r.PathValue("name"), r.URL.Query())
	if err != nil {
		s.pluginError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}
func (s *server) handlePluginImage(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		s.pluginError(w, plugin.ErrNotFound)
		return
	}
	ct, data, err := s.plugins.Image(r.Context(), r.PathValue("id"), r.PathValue("ref"))
	if err != nil {
		s.pluginError(w, err)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// SVG is an image too, but must remain inert when directly navigated to.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Write(data)
}

func (s *server) handlePluginAction(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.plugins == nil {
		s.pluginError(w, plugin.ErrNotFound)
		return
	}
	spec, err := s.plugins.ActionSpec(r.PathValue("id"), r.PathValue("action"))
	if err != nil {
		s.pluginError(w, err)
		return
	}
	if spec.Operator {
		if _, ok := s.agentSession(r); !ok {
			writeError(w, 403, "operator credential required")
			return
		}
	}
	if !spec.Operator {
		s.auth(s.handlePluginActionAuthenticated)(w, r)
		return
	}
	s.handlePluginActionAuthenticated(w, r)
}
func (s *server) handlePluginActionAuthenticated(w http.ResponseWriter, r *http.Request) {
	spec, err := s.plugins.ActionSpec(r.PathValue("id"), r.PathValue("action"))
	if err != nil {
		s.pluginError(w, err)
		return
	}
	var in struct {
		Confirmed bool          `json:"confirmed"`
		Values    plugin.Config `json:"values"`
	}
	if !pluginBody(w, r, &in, false) {
		return
	}
	out, err := s.plugins.Action(r.Context(), r.PathValue("id"), spec.ID, in.Confirmed, in.Values)
	if err != nil {
		s.pluginError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}
