// Package httpx holds HTTP client helpers. It is stdlib-only because the
// separately pinned agentupdate binary imports it.
package httpx

import (
	"errors"
	"net/http"
)

// RefuseRedirects returns a CheckRedirect that refuses every redirect, so
// credentials are never replayed against a host the service was pointed away to.
func RefuseRedirects() func(*http.Request, []*http.Request) error {
	return func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
}
