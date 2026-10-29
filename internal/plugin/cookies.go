package plugin

import (
	"context"
	"net/http"
)

type cookieJarKey struct{}
type cookieTransport struct {
	base http.RoundTripper
	jar  http.CookieJar
}

func (t cookieTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.WithContext(context.WithValue(r.Context(), cookieJarKey{}, t.jar)))
}

// WithCookieJar copies a guarded client and scopes its cookies, including
// redirects inside the host guard, to one operation. The outbound guard,
// timeout, redirect policy and response cap are preserved.
func WithCookieJar(base *http.Client, jar http.CookieJar) *http.Client {
	client := *base
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Jar = jar
	client.Transport = cookieTransport{transport, jar}
	return &client
}

type noRedirectKey struct{}
type noRedirectTransport struct{ base http.RoundTripper }

func (t noRedirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.base.RoundTrip(r.WithContext(context.WithValue(r.Context(), noRedirectKey{}, true)))
}

// WithoutRedirects keeps fixed-endpoint integrations on their declared paths,
// including redirects normally followed inside the host's transport guard.
func WithoutRedirects(base *http.Client) *http.Client {
	client := *base
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = noRedirectTransport{transport}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}
