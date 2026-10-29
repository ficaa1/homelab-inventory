package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Provider struct {
	Issuer        string   `json:"issuer"`
	Authorization string   `json:"authorization_endpoint"`
	Token         string   `json:"token_endpoint"`
	Userinfo      string   `json:"userinfo_endpoint"`
	AuthMethods   []string `json:"token_endpoint_auth_methods_supported"`
}

// readJSON caps every provider response, refuses redirects and omits remote text
// from errors so neither provider URLs nor credentials can reach logs or clients.
func (m *Manager) readJSON(req *http.Request, out any) error {
	resp, err := m.client.Do(req)
	if err != nil {
		return errors.New("provider request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("provider refused request")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(b) > 1<<20 || json.Unmarshal(b, out) != nil {
		return errors.New("invalid provider response")
	}
	return nil
}
func (m *Manager) Discover(ctx context.Context, c Config) (Provider, error) {
	var p Provider
	if !ValidURL(c.Issuer) {
		return p, errors.New("invalid issuer")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return p, errors.New("invalid issuer")
	}
	if err := m.readJSON(req, &p); err != nil {
		return p, err
	}
	if p.Issuer != c.Issuer {
		return p, errors.New("discovery issuer does not match")
	}
	if !ValidURL(p.Authorization) || !ValidURL(p.Token) || !ValidURL(p.Userinfo) {
		return p, errors.New("discovery requires valid authorization, token and userinfo endpoints")
	}
	return p, nil
}
func (m *Manager) exchange(ctx context.Context, c Config, p Provider, code, verifier string) (string, string) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {c.RedirectURI()}, "code_verifier": {verifier}}
	post := len(p.AuthMethods) == 1 && p.AuthMethods[0] == "client_secret_post"
	if post {
		form.Set("client_id", c.ClientID)
		form.Set("client_secret", c.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return "", "provider"
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// OAuth basic credentials are form-escaped before base64 encoding (RFC 6749).
	if !post {
		req.SetBasicAuth(url.QueryEscape(c.ClientID), url.QueryEscape(c.ClientSecret))
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if m.readJSON(req, &token) != nil || token.AccessToken == "" || strings.ContainsAny(token.AccessToken, "\r\n") {
		return "", "provider"
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, p.Userinfo, nil)
	if err != nil {
		return "", "provider"
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	var user struct {
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
	}
	if m.readJSON(req, &user) != nil || !validAddress(user.Email) {
		return "", "provider"
	}
	if c.RequireVerified && !user.Verified {
		return "", "unverified"
	}
	email := strings.ToLower(user.Email)
	if !Allowed(email, c.Allowed) {
		return "", "not_allowed"
	}
	return email, ""
}
