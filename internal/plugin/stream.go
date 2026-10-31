package plugin

import (
	"context"
	"net/http"
)

type responseLimitKey struct{}
type streamingTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t streamingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.WithContext(context.WithValue(r.Context(), responseLimitKey{}, t.limit)))
}

// StreamingHTTP keeps every authority, redirect and dial guard while admitting
// up to 128 MiB per response for the bounded network throughput measurement.
func StreamingHTTP(base *http.Client) *http.Client {
	c := *base
	t := c.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	c.Transport = streamingTransport{t, 128 << 20}
	c.Timeout = 0
	return &c
}

type longActionKey struct{}
type longActionTransport struct{ base http.RoundTripper }

func (t longActionTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.WithContext(context.WithValue(r.Context(), longActionKey{}, true)))
}

// LongActionHTTP extends only the response-header wait for an explicit bounded
// sidecar action. The core action deadline, authority guard and body cap remain.
func LongActionHTTP(base *http.Client) *http.Client {
	c := *base
	t := c.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	c.Transport = longActionTransport{t}
	return &c
}

// MetadataHTTP admits the bounded 64 MiB Tdarr metadata snapshot while keeping
// the regular timeout and all configured-authority restrictions.
func MetadataHTTP(base *http.Client) *http.Client {
	c := *base
	t := c.Transport
	if t == nil {
		t = http.DefaultTransport
	}
	c.Transport = streamingTransport{t, 64 << 20}
	return &c
}
