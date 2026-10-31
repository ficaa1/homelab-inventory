package plugin

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"regexp"
	"time"
)

// MaxAPIRequestBytes and MaxAPIResponseBytes bound a page API exchange.
const (
	MaxAPIRequestBytes  = 1 << 20
	MaxAPIResponseBytes = 8 << 20
)

// APISource is the optional JSON surface a built-in plugin offers its own
// dashboard page under /plugins/{id}/api/{path}. Core authenticates the
// caller, bounds the body and serializes the call with the plugin's other
// operations; the plugin routes Path itself and must never let a caller
// choose an upstream URL. Third-party plugins have no page and keep the
// declarative views.
type APISource interface {
	API(context.Context, APIRequest) (APIResponse, error)
}

// APITimeouts optionally lengthens the 10-second deadline for named routes,
// for example a reviewed batch that waits on an external service.
type APITimeouts interface {
	APITimeout(method, path string) time.Duration
}

// APIOperator optionally names routes that, like operator actions, need the
// separate operator credential instead of the dashboard token. Core checks it
// before dispatch; the plugin then sees Actor "operator".
type APIOperator interface {
	APIOperator(method, path string) bool
}

// APIOperator reports whether a page route needs the operator credential.
func (r *Registry) APIOperator(id, method, path string) bool {
	e := r.entries[id]
	if e == nil {
		return false
	}
	o, ok := e.plugin.(APIOperator)
	return ok && o.APIOperator(method, path)
}

type APIRequest struct {
	Method string
	Path   string // no leading slash, already unescaped
	Query  url.Values
	Body   []byte
	Actor  string
}

// APIResponse is JSON unless ContentType says otherwise; images are the only
// other type a page needs.
type APIResponse struct {
	Status      int
	ContentType string
	Body        []byte
}

var apiPath = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[A-Za-z0-9._:-]+)*$`)

// API dispatches one page request. Error messages are redacted like view errors.
func (r *Registry) API(ctx context.Context, id string, req APIRequest) (APIResponse, error) {
	e := r.entries[id]
	if e == nil || !apiPath.MatchString(req.Path) {
		return APIResponse{}, ErrNotFound
	}
	e.op.Lock()
	defer e.op.Unlock()
	src, ok := e.plugin.(APISource)
	if !ok || !e.stored.Enabled {
		return APIResponse{}, ErrNotFound
	}
	if !e.ready {
		return APIResponse{}, errors.New(e.message)
	}
	timeout := 10 * time.Second
	if t, ok := e.plugin.(APITimeouts); ok {
		if d := t.APITimeout(req.Method, req.Path); d > 0 && d <= 6*time.Minute {
			timeout = d
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := src.API(ctx, req)
	if err != nil {
		var validation *ValidationError
		if errors.As(err, &validation) || errors.Is(err, ErrNotFound) {
			return APIResponse{}, err
		}
		return APIResponse{}, errors.New(e.host.safe(err.Error()))
	}
	if len(out.Body) > MaxAPIResponseBytes {
		return APIResponse{}, errors.New("plugin response too large")
	}
	if out.Status == 0 {
		out.Status = 200
	}
	if out.ContentType == "" {
		out.ContentType = "application/json"
	}
	if out.ContentType == "application/json" {
		out.Body = e.host.redact(out.Body)
	} else if out.ContentType, _, err = validateImage(out.ContentType, out.Body); err != nil {
		return APIResponse{}, err
	}
	return out, nil
}

// redact replaces configured secret values in a response body. URLs are left
// alone: page data legitimately carries links.
func (h *host) redact(b []byte) []byte {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, secret := range h.secrets {
		if secret != "" {
			b = bytes.ReplaceAll(b, []byte(secret), []byte("[redacted]"))
		}
	}
	return b
}
