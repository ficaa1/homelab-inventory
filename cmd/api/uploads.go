package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
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
func (s *server) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, 404, "uploads unavailable")
		return
	}
	files, err := s.plugins.FolderFiles(r.Context())
	if err != nil {
		s.pluginError(w, err)
		return
	}
	writeJSON(w, 200, files)
}
func (s *server) handleUploadFile(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, 404, "uploads unavailable")
		return
	}
	f, size, err := s.plugins.OpenFolderFile(r.Context(), r.PathValue("name"))
	if err != nil {
		s.pluginError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(size))
	name := strings.NewReplacer(`"`, "", "\r", "", "\n", "").Replace(r.PathValue("name"))
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	io.CopyN(w, f, size)
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
	if r.ContentLength == 0 && r.Header.Get("Content-Length") == "" {
		writeError(w, 411, "Content-Length is required")
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
