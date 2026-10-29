package httpx

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// EmptyToken says what an unset token means. There is deliberately no default:
// every caller states it, because the services disagree on purpose.
type EmptyToken int

const (
	// OpenWhenUnset admits every request when the token is empty (explicit
	// local development mode only).
	OpenWhenUnset EmptyToken = iota + 1
	// DenyWhenUnset refuses every request when the token is empty (services
	// that can write, whose misconfiguration must fail closed).
	DenyWhenUnset
)

// BearerOK reports whether r carries exactly "Bearer <token>". The scheme is
// matched case-sensitively and the token in constant time. An empty token is
// resolved by policy, never by convention; an unknown policy denies.
func BearerOK(r *http.Request, token string, policy EmptyToken) bool {
	if token == "" {
		return policy == OpenWhenUnset
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
