package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestBearerOK(t *testing.T) {
	cases := []struct {
		token, auth string
		policy      EmptyToken
		want        bool
	}{
		{"", "", OpenWhenUnset, true},
		{"", "Bearer ", OpenWhenUnset, true},
		{"", "", DenyWhenUnset, false},
		{"", "Bearer ", DenyWhenUnset, false},
		{"", "Bearer ", 0, false},
		{"tok", "Bearer tok", OpenWhenUnset, true},
		{"tok", "Bearer tok", DenyWhenUnset, true},
		{"tok", "", OpenWhenUnset, false},
		{"tok", "bearer tok", DenyWhenUnset, false},
		{"tok", "Bearer tok ", DenyWhenUnset, false},
		{"tok", "Bearer  tok", DenyWhenUnset, false},
		{"tok", "Bearer to", DenyWhenUnset, false},
		{"tok", "tok", DenyWhenUnset, false},
		{"tok", "Basic tok", DenyWhenUnset, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/", nil)
		if c.auth != "" {
			r.Header.Set("Authorization", c.auth)
		}
		if got := BearerOK(r, c.token, c.policy); got != c.want {
			t.Errorf("token %q header %q policy %d: %v, want %v", c.token, c.auth, c.policy, got, c.want)
		}
	}
}
