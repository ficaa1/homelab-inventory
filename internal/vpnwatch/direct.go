package vpnwatch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/execallow"
)

// Proxy is deliberately nil: this request uses the dashboard process's own
// network, even when HTTP_PROXY is configured. No redirects or retries are sent.
var directHTTP = &http.Client{
	Timeout:       5 * time.Second,
	Transport:     &http.Transport{DisableKeepAlives: true, MaxResponseHeaderBytes: 64 << 10},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func directCheck(ctx context.Context) error {
	target, err := execallow.VPNCheckURL()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errors.New("direct VPN check unavailable")
	}
	res, err := directHTTP.Do(req)
	if err != nil {
		return errors.New("direct VPN check failed")
	}
	defer res.Body.Close()
	const limit = 64 << 10
	n, err := io.Copy(io.Discard, io.LimitReader(res.Body, limit+1))
	if err != nil || n > limit || res.StatusCode < 200 || res.StatusCode >= 300 {
		return errors.New("direct VPN check failed")
	}
	return nil
}
