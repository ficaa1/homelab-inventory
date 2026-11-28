// Package fleet reads host reports and skill decisions from a fleet hub.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

type Fleet struct {
	host                           plugin.Host
	base, readToken, operatorToken string
}

func (*Fleet) Manifest() plugin.Manifest {
	return plugin.Manifest{
		ID: "fleet", Name: "Fleet", Version: "1", Description: "Hosts, drift, skills and trials from a fleet hub.",
		Settings: []plugin.Field{
			{Key: "hub_url", Label: "Hub URL", Type: plugin.URL, Required: true, Placeholder: "https://hub.example.lan:8790", Help: "The hub's base URL, without /v1."},
			{Key: "read_token", Label: "Read token", Type: plugin.Secret, Required: true, Help: "Bearer token for views and alerts."},
			{Key: "operator_token", Label: "Operator token", Type: plugin.Secret, Help: "Optional. Lets this dashboard change fleet through the hub. Leave it empty to keep it read-only."},
		},
		Tab: &plugin.Tab{Title: "Fleet", Icon: "grid", Views: []plugin.ViewRef{
			{Name: "overview", Title: "Overview"}, {Name: "hosts", Title: "Hosts"}, {Name: "skills", Title: "Skills"}, {Name: "trials", Title: "Trials"}, {Name: "host", Title: "Host"},
		}},
	}
}

func (p *Fleet) Configure(c plugin.Config) error {
	base, _ := c["hub_url"].(string)
	// The registry omits unset fields when saving a disabled plugin and
	// enforces required settings before enabling or testing one.
	if _, present := c["hub_url"]; present {
		u, err := url.Parse(base)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return &plugin.FieldError{Key: "hub_url", Message: "Use an http:// or https:// URL with a host name."}
		}
		if u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(base, "#") {
			return &plugin.FieldError{Key: "hub_url", Message: "Remove credentials, query and fragment from the hub URL."}
		}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
			return &plugin.FieldError{Key: "hub_url", Message: "Use the hub's base URL without /v1"}
		}
		base = strings.TrimRight(u.String(), "/")
	}
	read, _ := c["read_token"].(string)
	operator, _ := c["operator_token"].(string)
	if _, present := c["read_token"]; present && strings.TrimSpace(read) == "" {
		return &plugin.FieldError{Key: "read_token", Message: "Enter the hub's read token."}
	}
	for _, key := range []string{"read_token", "operator_token"} {
		value, _ := c[key].(string)
		if strings.ContainsAny(value, "\r\n") {
			return &plugin.FieldError{Key: key, Message: "Enter the token on one line."}
		}
	}
	p.base, p.readToken, p.operatorToken = base, read, operator
	return nil
}

func (p *Fleet) Close() error {
	p.base, p.readToken, p.operatorToken = "", "", ""
	return nil
}

var hostID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var sourceID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var queryText = regexp.MustCompile(`\?[^\s]*`)

func (p *Fleet) clean(s string, limit int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
	for _, token := range []string{p.readToken, p.operatorToken} {
		if token != "" {
			s = strings.ReplaceAll(s, token, "[redacted]")
		}
	}
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

func (p *Fleet) word(s string) string { return p.clean(s, 200) }

type envelope struct {
	Version   int       `json:"api_version"`
	Generated time.Time `json:"generated_at"`
}

func (e envelope) header() envelope { return e }

type hubError struct {
	status  int
	message string
}

func (e *hubError) Error() string { return e.message }

var errVersion = errors.New("Use a fleet hub that supports API version 1.")
var errResponse = errors.New("Check that hub_url points to a fleet hub returning a complete JSON response.")

func (p *Fleet) get(ctx context.Context, path, token string, out interface{ header() envelope }) error {
	if p.base == "" || token == "" {
		return errors.New("Set the hub URL and read token in Admin → Plugins.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/v1/"+path, nil)
	if err != nil {
		return errResponse
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := plugin.WithoutRedirects(p.host.HTTP()).Do(req)
	if err != nil {
		return errors.New("Fleet hub unavailable. Check hub_url and that the hub is running.")
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := fmt.Sprintf("Fleet hub returned HTTP %d. Check the hub and try again.", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized {
			message = "The hub rejected the read token. Check it in Admin → Plugins."
			if token == p.operatorToken && token != p.readToken {
				message = "The hub rejected the operator token. Check it in Admin → Plugins."
			}
		} else {
			var body struct{ Error struct{ Message string } }
			if dec.Decode(&body) == nil {
				if s := strings.TrimSpace(p.clean(queryText.ReplaceAllString(body.Error.Message, ""), 400)); s != "" {
					message = s
				}
			}
		}
		return &hubError{resp.StatusCode, message}
	}
	if dec.Decode(out) != nil || dec.Decode(new(any)) != io.EOF {
		return errResponse
	}
	if out.header().Version != 1 {
		return errVersion
	}
	if out.header().Generated.IsZero() {
		return errResponse
	}
	return nil
}

func (p *Fleet) Test(ctx context.Context) plugin.TestResult {
	type meta struct {
		envelope
		Role string
	}
	var m meta
	err := p.get(ctx, "meta", p.readToken, &m)
	result := plugin.TestResult{OK: true, Message: "Fleet hub checks passed."}
	check := func(name string, ok bool, message string) {
		result.Checks = append(result.Checks, plugin.Check{Name: name, OK: ok, Message: message})
		if !ok && result.OK {
			result.OK = false
			result.Message = message
		}
	}
	var httpErr *hubError
	reachable := err == nil || errors.Is(err, errVersion) || errors.Is(err, errResponse) || errors.As(err, &httpErr)
	message := "The hub answered."
	if !reachable {
		message = err.Error()
	}
	check("Hub reachable", reachable, message)
	versionMessage := "API version 1."
	if err != nil {
		versionMessage = err.Error()
	}
	check("API version 1", err == nil, versionMessage)
	accepted := err == nil && (m.Role == "read" || m.Role == "operator")
	message = "Read access confirmed."
	if err != nil {
		message = err.Error()
	} else if !accepted {
		message = "Use a token with read or operator access."
	}
	check("Read token accepted", accepted, message)
	if p.operatorToken != "" {
		var op meta
		err := p.get(ctx, "meta", p.operatorToken, &op)
		message := "Operator access confirmed."
		if err != nil {
			message = err.Error()
		} else if op.Role != "operator" {
			message = "This token has read access only; use the operator token"
		}
		check("Operator token", err == nil && op.Role == "operator", message)
	}
	message = "HTTPS encrypts the tokens."
	if strings.HasPrefix(p.base, "http://") {
		message = "Plain HTTP sends the tokens unencrypted. Use HTTPS to encrypt them."
	}
	check("Connection", true, message)
	return result
}
