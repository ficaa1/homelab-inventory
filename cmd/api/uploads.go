package main

import (
	"errors"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func (s *server) handleUploadTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	targets := []plugin.TargetStatus{}
	if s.plugins != nil {
		targets = s.plugins.UploadTargets()
	}
	writeJSON(w, 200, targets)
}
func (s *server) handlePluginUpload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	fail := func(err error) {
		var invalid *plugin.UploadError
		if errors.As(err, &invalid) {
			writeError(w, invalid.Status, invalid.Message)
		} else {
			s.pluginError(w, err)
		}
	}
	if s.plugins == nil {
		fail(plugin.ErrNotFound)
		return
	}
	name, err := plugin.UploadName(r.Header.Get("X-Upload-Name"))
	if err != nil {
		fail(err)
		return
	}
	// Both deadlines are per request: ordinary routes retain the server limits.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(plugin.UploadTimeout))
	_ = controller.SetWriteDeadline(time.Now().Add(plugin.UploadTimeout))
	body := http.MaxBytesReader(w, r.Body, plugin.MaxUploadBytes)
	result, err := s.plugins.Upload(r.Context(), r.PathValue("id"), r.PathValue("target"), name, r.ContentLength, body, requestActor(r))
	if err != nil {
		fail(err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "name": result.Name, "message": result.Message})
}
