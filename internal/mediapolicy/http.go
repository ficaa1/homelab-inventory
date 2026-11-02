package mediapolicy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/archivetier"
	"github.com/ficaa1/homelab-inventory/internal/httpx"
)

var ErrWriteRejected = errors.New("policy write rejected")

// Transport is the last write barrier. Policies cannot override dry-run, a
// poisoned state store, service boundaries or the narrow write allowlist.
type Transport struct {
	Base         http.RoundTripper
	Apply        bool
	Store        *Store
	SonarrOrigin string
	RadarrOrigin string
	QbitOrigin   string
	NoSearch     bool
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.NoSearch && strings.TrimRight(req.URL.Path, "/") == "/api/v3/release" && req.Method == http.MethodGet {
		return nil, fmt.Errorf("release searches disabled: %w", ErrWriteRejected)
	}
	if req.Method != http.MethodGet && !(req.Method == http.MethodPost && req.URL.Scheme+"://"+req.URL.Host == t.QbitOrigin && t.QbitOrigin != "" && req.URL.Path == "/api/v2/auth/login" && req.URL.EscapedPath() == req.URL.Path && req.URL.RawQuery == "") {
		if !t.Apply {
			return nil, fmt.Errorf("dry-run HTTP write refused: %w", ErrWriteRejected)
		}
		if t.Store == nil {
			return nil, fmt.Errorf("durable state required for writes: %w", ErrWriteRejected)
		}
		if err := t.Store.WriteAllowed(); err != nil {
			return nil, fmt.Errorf("%v: %w", err, ErrWriteRejected)
		}
		origin := req.URL.Scheme + "://" + req.URL.Host
		sonarr := origin == t.SonarrOrigin
		arr := sonarr || origin == t.RadarrOrigin
		qbit := origin == t.QbitOrigin && t.QbitOrigin != ""
		qbitPath := req.URL.Path == "/api/v2/auth/login" || req.URL.Path == "/api/v2/torrents/stop" || req.URL.Path == "/api/v2/torrents/start" || req.URL.Path == "/api/v2/torrents/setLocation" || req.URL.Path == "/api/v2/torrents/recheck"
		allowed := qbit && req.Method == http.MethodPost && qbitPath && req.URL.RawQuery == "" && req.URL.EscapedPath() == req.URL.Path
		if qbit && req.Method == http.MethodPost && req.URL.Path == "/api/v2/torrents/setDownloadPath" && req.URL.EscapedPath() == req.URL.Path && req.URL.RawQuery == "" {
			// Clone request fields before replacing the consumed body. GetBody
			// may be stale and cannot establish what will reach the upstream.
			req = req.Clone(req.Context())
			allowed = seedClearDownloadPathAllowed(req)
		}
		if arr && req.Method == http.MethodPost && req.URL.Path == "/api/v3/tag" && req.URL.EscapedPath() == req.URL.Path && req.URL.RawQuery == "" && !req.URL.ForceQuery {
			req = req.Clone(req.Context())
			body, valid := archiveTagCreateBody(req)
			allowed = valid
			if allowed {
				kind := "radarr"
				if sonarr {
					kind = "sonarr"
				}
				if err := t.Store.AuditDetail("archive_tag_create_intent", nil, map[string]any{"kind": kind, "body": json.RawMessage(body)}); err != nil {
					return nil, fmt.Errorf("tag intent could not be saved: %w", ErrWriteRejected)
				}
			}
		}
		allowed = allowed || arr && req.Method == http.MethodPost && (req.URL.Path == "/api/v3/command" || req.URL.Path == "/api/v3/release")
		allowed = allowed || (sonarr && req.Method == http.MethodPut && req.URL.Path == "/api/v3/series/editor")
		allowed = allowed || (origin == t.RadarrOrigin && req.Method == http.MethodPut && req.URL.Path == "/api/v3/movie/editor")
		q, queryErr := url.ParseQuery(req.URL.RawQuery)
		allowed = allowed || arr && req.Method == http.MethodDelete && queryErr == nil && req.URL.EscapedPath() == req.URL.Path && archivetier.QueueDeleteAllowed(strings.TrimPrefix(req.URL.Path, "/api/v3"), q) && strings.HasPrefix(req.URL.Path, "/api/v3/queue/")
		if !allowed {
			return nil, fmt.Errorf("HTTP write outside policy allowlist refused: %w", ErrWriteRejected)
		}
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("upstream transport failed")
	}
	return resp, nil
}

// The mover may clear one torrent's path; it cannot set a new path or target
// multiple hashes. Buffer the actual body so only inspected bytes are sent.
func seedClearDownloadPathAllowed(req *http.Request) bool {
	ct, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || ct != "application/x-www-form-urlencoded" || req.Body == nil {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(req.Body, 1025))
	req.Body.Close()
	if err != nil || len(b) > 1024 {
		return false
	}
	v, err := url.ParseQuery(string(b))
	if err != nil || len(v) != 2 || len(v["id"]) != 1 || !ValidSeedHash(v.Get("id")) || len(v["path"]) != 1 || v.Get("path") != "" {
		return false
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(b))), nil }
	req.Body, _ = req.GetBody()
	req.ContentLength = int64(len(b))
	return true
}
func SharedHTTP(t *Transport) *http.Client {
	return &http.Client{Transport: t, Timeout: 90 * time.Second, CheckRedirect: httpx.RefuseRedirects()}
}

// Inspect and restore the actual transmitted body, never a stale GetBody copy.
func archiveTagCreateBody(req *http.Request) ([]byte, bool) {
	ct, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || ct != "application/json" || req.Body == nil {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(req.Body, 1025))
	req.Body.Close()
	if err != nil || len(b) > 1024 || !archivetier.TagCreateAllowed(b) {
		return nil, false
	}
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(string(b))), nil }
	req.Body, _ = req.GetBody()
	req.ContentLength = int64(len(b))
	return b, true
}
