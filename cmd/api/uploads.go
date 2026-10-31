package main

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
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

const previewCSP = "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"

const (
	previewTextMax  = 2 << 20
	previewImageMax = 25 << 20
)

type previewType struct {
	mime string
	max  int64
}

// previewTypes is the whole preview allowlist. SVG, PDF, HEIC and media are
// deliberately absent: they are download-only.
var previewTypes = map[string]previewType{
	".md":       {"text/plain; charset=utf-8", previewTextMax},
	".markdown": {"text/plain; charset=utf-8", previewTextMax},
	".txt":      {"text/plain; charset=utf-8", previewTextMax},
	".log":      {"text/plain; charset=utf-8", previewTextMax},
	".csv":      {"text/plain; charset=utf-8", previewTextMax},
	".yaml":     {"text/plain; charset=utf-8", previewTextMax},
	".yml":      {"text/plain; charset=utf-8", previewTextMax},
	".conf":     {"text/plain; charset=utf-8", previewTextMax},
	".json":     {"text/plain; charset=utf-8", previewTextMax},
	".html":     {"text/html; charset=utf-8", previewTextMax},
	".htm":      {"text/html; charset=utf-8", previewTextMax},
	".jpg":      {"image/jpeg", previewImageMax},
	".jpeg":     {"image/jpeg", previewImageMax},
	".png":      {"image/png", previewImageMax},
	".gif":      {"image/gif", previewImageMax},
	".webp":     {"image/webp", previewImageMax},
	".avif":     {"image/avif", previewImageMax},
}

// handleUploadPreview shares the folder download resolver, but only serves
// bounded text and raster images. The response stays inert on direct navigation.
func (s *server) handleUploadPreview(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if s.plugins == nil {
		writeError(w, 404, "uploads unavailable")
		return
	}
	name := r.PathValue("name")
	t, ok := previewTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		writeError(w, 415, "no preview for this type")
		return
	}
	f, size, err := s.plugins.OpenFolderFile(r.Context(), name)
	if err != nil {
		s.pluginError(w, err)
		return
	}
	defer f.Close()
	if size > t.max {
		writeError(w, 413, "too large to preview")
		return
	}
	h := w.Header()
	h.Set("Content-Type", t.mime)
	h.Set("Content-Length", fmt.Sprint(size))
	h.Set("Content-Security-Policy", previewCSP)
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": name}))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(plugin.UploadTimeout))
	// Keep the served length pinned to the descriptor size checked above.
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
