package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const MaxHTTPBody = 8 << 20

// host owns its transport; neither proxies nor a caller's DefaultTransport can
// bypass the configured authority allowlist. Every request (including pooled
// connections) and every dial checks the current configuration.
type host struct {
	id, dir   string
	logger    *log.Logger
	mu        sync.RWMutex
	allowed   map[string]bool
	secrets   []string
	client    *http.Client
	transport *http.Transport
}

func newHost(data, id string, logger *log.Logger) *host {
	h := &host{id: id, dir: filepath.Join(data, "plugins", id), logger: logger, allowed: map[string]bool{}}
	h.transport = &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !h.permits(addr) {
				return nil, errors.New("plugin outbound host is not configured")
			}
			conn, err := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, errors.New("plugin connection failed")
			}
			return conn, nil
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       time.Minute,
	}
	h.client = &http.Client{Timeout: 10 * time.Second, Transport: hostTransport{h}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 || !strings.EqualFold(req.URL.Host, via[0].URL.Host) || req.URL.Scheme != via[0].URL.Scheme {
			// net/http wraps redirect errors with the original request URL. Scrub
			// every shared URL before that wrapper is constructed.
			for _, previous := range via {
				scrubRequestURL(previous)
			}
			scrubRequestURL(req)
			return errors.New("plugin redirect must stay on the original host and scheme")
		}
		return nil
	}}
	return h
}
func (h *host) HTTP() *http.Client { return h.client }
func (h *host) CacheDir() string   { return h.dir }
func (h *host) Now() time.Time     { return time.Now() }
func (h *host) Logf(format string, args ...any) {
	if h.logger != nil {
		h.logger.Printf("plugin %s: %s", h.id, h.safe(fmt.Sprintf(format, args...)))
	}
}
func authority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(net.JoinHostPort(u.Hostname(), port))
}
func (h *host) permits(addr string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.allowed[strings.ToLower(addr)]
}
func (h *host) configure(m Manifest, c Config) {
	allowed := map[string]bool{}
	var secrets []string
	for _, f := range m.Settings {
		s, _ := c[f.Key].(string)
		if f.Type == URL && s != "" {
			if u, err := url.Parse(s); err == nil {
				allowed[authority(u)] = true
			}
		}
		if f.Type == Secret && s != "" {
			secrets = append(secrets, s)
		}
	}
	h.mu.Lock()
	h.allowed = allowed
	for _, secret := range secrets {
		known := false
		for _, old := range h.secrets {
			known = known || old == secret
		}
		if !known {
			h.secrets = append(h.secrets, secret)
		}
	}
	h.mu.Unlock()
	h.transport.CloseIdleConnections()
}

var urlInText = regexp.MustCompile(`https?://[^\s"'<>]+`)

func (h *host) safe(s string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, secret := range h.secrets {
		s = strings.ReplaceAll(s, secret, "[redacted]")
	}
	return urlInText.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[URL]"
		}
		u.User = nil
		u.RawQuery = ""
		u.ForceQuery = false
		u.Fragment = ""
		return u.String()
	})
}
func scrubRequestURL(req *http.Request) {
	if req.URL != nil {
		req.URL.RawQuery = ""
		req.URL.ForceQuery = false
		req.URL.User = nil
		req.URL.Fragment = ""
	}
}

type hostTransport struct{ h *host }

func (t hostTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	defer func() {
		if err != nil {
			scrubRequestURL(req)
		}
	}()
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || !t.h.permits(authority(req.URL)) || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return nil, errors.New("plugin outbound host is not configured")
	}
	// Handle redirects inside the guard so net/http cannot leak a Location
	// query while constructing its own error. The outer client still supplies
	// the ten-second deadline through the request context.
	wire := req.Clone(req.Context())
	jar, _ := req.Context().Value(cookieJarKey{}).(http.CookieJar)
	redirect := t.h.client.CheckRedirect
	if stop, _ := req.Context().Value(noRedirectKey{}).(bool); stop {
		redirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	inner := &http.Client{Transport: guardedTransport{t.h}, CheckRedirect: redirect, Jar: jar}
	resp, err = inner.Do(wire)
	if err != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return nil, errors.New("plugin HTTP request failed")
	}
	resp.Body = &cappedBody{body: resp.Body, left: MaxHTTPBody}
	return resp, nil
}

// guardedTransport also checks pooled requests: the dial guard alone would
// let a previously configured host remain reachable over a live connection.
type guardedTransport struct{ h *host }

func (t guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || !t.h.permits(authority(req.URL)) || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return nil, errors.New("plugin outbound host is not configured")
	}
	return t.h.transport.RoundTrip(req)
}

type cappedBody struct {
	body io.ReadCloser
	left int64
}

func (b *cappedBody) Close() error { return b.body.Close() }
func (b *cappedBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.left == 0 {
		var extra [1]byte
		n, err := b.body.Read(extra[:])
		if n > 0 {
			return 0, errors.New("plugin HTTP body exceeds 8 MiB")
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.body.Read(p)
	b.left -= int64(n)
	if err != nil && err != io.EOF {
		return n, errors.New("reading plugin HTTP body failed")
	}
	return n, err
}
