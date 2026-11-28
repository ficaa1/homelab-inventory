package fleet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/fsx"
	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

var skillID = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,200}$`)

func field(key, label string, t plugin.FieldType) plugin.Field {
	return plugin.Field{Key: key, Label: label, Type: t, Required: true}
}
func choice(key, label string, options ...plugin.Option) plugin.Field {
	f := field(key, label, plugin.Select)
	f.Options = options
	return f
}
func action(id, label string, destructive bool, fields ...plugin.Field) plugin.ActionSpec {
	return plugin.ActionSpec{ID: id, Label: label, Operator: true, Result: "toast", Destructive: destructive, Fields: fields}
}
func fleetActions() []plugin.ActionSpec {
	source := field("source", "Source", plugin.Text)
	return []plugin.ActionSpec{
		action("add_trial_source", "Add trial source", false, field("name", "Source name", plugin.Text), field("url", "Repository URL", plugin.Text),
			plugin.Field{Key: "glob", Label: "Glob", Type: plugin.Text, Help: "Enter a glob or a manifest path."},
			plugin.Field{Key: "manifest", Label: "Manifest path", Type: plugin.Text},
			plugin.Field{Key: "include", Label: "Skills to include", Type: plugin.Text, Help: "Comma-separated skill names. Leave empty for all skills."},
			plugin.Field{Key: "hosts", Label: "Hosts", Type: plugin.Text, Help: "Comma-separated host names. Leave empty for every host."},
			plugin.Field{Key: "model_invoked", Label: "Make skills model-invoked", Type: plugin.Bool}),
		action("set_skill_mode", "Set skill mode", false, field("skill", "Skill", plugin.Text), choice("mode", "Mode", plugin.Option{Value: "model", Label: modes["model"]}, plugin.Option{Value: "user", Label: modes["user"]}, plugin.Option{Value: "exclude", Label: modes["exclude"]})),
		action("end_trial", "End trial", true, source, choice("undecided", "Undecided skills", plugin.Option{Value: "user", Label: "User-only"}, plugin.Option{Value: "exclude", Label: "Excluded"}, plugin.Option{Value: "suggestion", Label: "Each skill's suggestion"})),
		action("remove_source", "Remove source", true, source),
		action("force_file", "Overwrite file", true, field("host", "Host", plugin.Text), field("item", "Item", plugin.Text), field("live_sha256", "Current file hash", plugin.Text)),
	}
}

// Only copy complete, unchanged identifiers into a request. Display text may be shortened.
func (p *Fleet) offer(v *plugin.View, id, label, confirm string, presets, defaults plugin.Config) {
	if p.operatorToken == "" || len(v.Actions) >= 50 {
		return
	}
	for _, value := range presets {
		if s, ok := value.(string); ok && (s == "" || s != p.clean(s, 512)) {
			return
		}
	}
	for _, a := range p.Manifest().Actions {
		if a.ID != id {
			continue
		}
		if label != "" {
			a.Label = label
		}
		a.Confirm = confirm
		a.Values = presets
		for i, f := range a.Fields {
			if d, ok := defaults[f.Key]; ok {
				a.Fields[i].Default = d
			}
		}
		v.Actions = append(v.Actions, a)
		return
	}
}

func csv(s string) []string {
	values := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	return values
}
func badField(key, message string) error { return &plugin.FieldError{Key: key, Message: message} }
func actionParams(id string, in plugin.Config) (plugin.Config, error) {
	params := maps.Clone(in)
	get := func(k string) string { v, _ := params[k].(string); return v }
	switch id {
	case "add_trial_source":
		if !sourceID.MatchString(get("name")) {
			return nil, badField("name", "Use 1–64 lowercase letters, digits, dots, underscores or hyphens. Start with a letter or digit.")
		}
		u, err := url.Parse(get("url"))
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return nil, badField("url", "Use a URL that starts with https:// and contains no credentials.")
		}
		glob, manifest := strings.TrimSpace(get("glob")), strings.TrimSpace(get("manifest"))
		if (glob == "") == (manifest == "") {
			return nil, badField("glob", "Enter exactly one glob or manifest path.")
		}
		if glob != "" {
			params["glob"] = glob
			delete(params, "manifest")
		} else {
			params["manifest"] = manifest
			delete(params, "glob")
		}
		params["include"] = csv(get("include"))
		hosts := csv(get("hosts"))
		for _, h := range hosts {
			if !hostID.MatchString(h) {
				return nil, badField("hosts", "Use host names of up to 32 lowercase letters, digits or hyphens, starting with a letter.")
			}
		}
		params["hosts"] = nil
		if len(hosts) > 0 {
			params["hosts"] = hosts
		}
		if _, ok := params["model_invoked"]; !ok {
			params["model_invoked"] = false
		}
	case "end_trial", "remove_source":
		if !sourceID.MatchString(get("source")) {
			return nil, badField("source", "Choose a valid source from Skills or Trials.")
		}
	case "set_skill_mode":
		if !skillID.MatchString(get("skill")) {
			return nil, badField("skill", "Choose a skill from Skills.")
		}
	case "force_file":
		if !hostID.MatchString(get("host")) {
			return nil, badField("host", "Choose a host from Hosts.")
		}
		if len([]rune(get("item"))) > 512 || get("item") == "" {
			return nil, badField("item", "Choose a file from the host's attention list.")
		}
	case "proxy_set_strategy":
		// The registry checks the strategy option and requires the opaque etag.
	case "proxy_set_account_routing", "proxy_set_account_enabled", "proxy_refresh_account", "proxy_reset_cooldown", "proxy_reauth_start":
		if !proxyID.MatchString(get("account")) {
			return nil, badField("account", "Choose an account from Proxy.")
		}
		if id == "proxy_set_account_routing" {
			priority, ok := params["priority"].(float64)
			if !ok || priority != math.Trunc(priority) {
				return nil, badField("priority", "Enter a whole number for priority.")
			}
			weight, ok := params["weight"].(float64)
			if !ok || weight < 0 || weight > 1000000 {
				return nil, badField("weight", "Enter a weight from 0 to 1000000.")
			}
		}
	case "proxy_reauth_finish":
		if !proxyID.MatchString(get("session")) {
			return nil, badField("session", "Open the sign-in session from your account.")
		}
	default:
		return nil, plugin.ErrNotFound
	}
	return params, nil
}

type pendingReceipt struct {
	Key     string    `json:"key"`
	Created time.Time `json:"created_at"`
}

func (p *Fleet) reserve(a plugin.ActionSpec, params plugin.Config) (path, key string, err error) {
	public := maps.Clone(params)
	for _, f := range a.Fields {
		if f.Type == plugin.Secret {
			delete(public, f.Key)
		}
	}
	canonical, err := json.Marshal(public)
	if err != nil {
		return "", "", err
	}
	hash := sha256.Sum256(append([]byte(p.base+"\x00"+a.ID+"\x00"), canonical...))
	dir := filepath.Join(p.host.CacheDir(), "receipts")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, hex.EncodeToString(hash[:]))
	data, err := os.ReadFile(path)
	now := p.host.Now()
	if err == nil {
		var saved pendingReceipt
		if json.Unmarshal(data, &saved) != nil || saved.Created.IsZero() || saved.Key == "" {
			return "", "", errors.New("Cannot read the pending fleet receipt.")
		}
		if now.Sub(saved.Created) < 30*24*time.Hour {
			return path, saved.Key, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	var random [16]byte
	rand.Read(random[:])
	key = hex.EncodeToString(random[:])
	data, err = json.Marshal(pendingReceipt{key, now})
	if err != nil {
		return "", "", err
	}
	return path, key, fsx.WriteAtomic(path, data, 0600)
}

type receiptResponse struct {
	envelope
	Receipt *struct {
		Reauth                             *reauthReceipt
		ID, Action, Summary, Commit, State string
		Hosts                              []struct{ Host, State string }
		Force                              *struct{ State string }
		Error                              *hubProblem
	}
}

func definitiveActionError(code string) bool {
	switch code {
	case "bad_request", "unauthorized", "forbidden", "not_found", "conflict", "not_editable",
		"idempotency_mismatch", "too_large", "invalid_param", "rate_limited", "busy":
		return true
	}
	return false
}

func (r receiptResponse) valid(action string) bool {
	if r.Receipt == nil || strings.TrimSpace(r.Receipt.ID) == "" || r.Receipt.Action != action {
		return false
	}
	switch r.Receipt.State {
	case "failed":
		return r.Receipt.Error != nil && r.Receipt.Error.Code != ""
	case "done", "working":
		return strings.TrimSpace(r.Receipt.Summary) != ""
	}
	return false
}

func releaseReceipt(path string) error {
	if err := os.Remove(path); err != nil {
		return errors.New("The hub answered. Check the current state and the fleet plugin cache permissions.")
	}
	return nil
}

func (p *Fleet) Action(ctx context.Context, id string, in plugin.Config) (plugin.View, error) {
	var v plugin.View
	if p.operatorToken == "" || p.base == "" {
		return v, errors.New("Set the hub URL and operator token in Admin → Plugins.")
	}
	var spec plugin.ActionSpec
	for _, a := range p.Manifest().Actions {
		if a.ID == id {
			spec = a
			break
		}
	}
	if spec.ID == "" {
		return v, plugin.ErrNotFound
	}
	params, err := actionParams(id, in)
	if err != nil {
		return v, err
	}
	var accountLabel string
	if id == "proxy_reauth_start" {
		account := params["account"].(string)
		var d proxyResponse
		if err := p.get(ctx, "proxy/accounts/"+account, p.readToken, &d); err != nil {
			return v, err
		}
		if d.Account == nil || d.Account.ID != account {
			return v, errResponse
		}
		accountLabel = p.word(d.Account.Label)
	}
	payload, err := json.Marshal(map[string]any{"params": params})
	if err != nil {
		return v, errResponse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/v1/actions/"+id, bytes.NewReader(payload))
	if err != nil {
		return v, errResponse
	}
	// A transport retry would bypass the user's decision to reconcile the receipt.
	req.GetBody = nil
	path, key, err := p.reserve(spec, params)
	if err != nil {
		return v, errors.New("Could not save the receipt. Check the fleet plugin cache permissions and free space.")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.operatorToken)
	req.Header.Set("Idempotency-Key", key)
	resp, err := plugin.WithoutRedirects(p.host.HTTP()).Do(req)
	if err != nil {
		return v, errors.New("Fleet hub unavailable. Check the hub, then retry to reconcile this change.")
	}
	defer resp.Body.Close()
	// A timeout can arrive after the headers. Keep the key until the full reply has arrived.
	const maxReply = 4 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
	if err != nil {
		return v, errors.New("The hub reply was interrupted. Check the hub, then retry to reconcile this change.")
	}
	if len(body) > maxReply {
		return v, errResponse
	}
	reply := &http.Response{StatusCode: resp.StatusCode, Body: io.NopCloser(bytes.NewReader(body))}
	var response receiptResponse
	finish := id == "proxy_reauth_finish"
	if err = p.decode(reply, p.operatorToken, &response, finish); err != nil {
		var failure *hubError
		if errors.As(err, &failure) {
			if definitiveActionError(failure.code) {
				if removeErr := releaseReceipt(path); removeErr != nil {
					return v, removeErr
				}
			}
			if finish {
				if failure.status == http.StatusUnprocessableEntity && failure.field == "redirect_url" {
					return v, badField("redirect_url", redirectAddressHelp)
				}
			} else if failure.status/100 == 4 && failure.field != "" {
				return v, badField(failure.field, failure.message)
			}
		}
		return v, err
	}
	if !response.valid(id) {
		return v, errResponse
	}
	r := response.Receipt
	if id == "proxy_reauth_start" && r.State != "failed" {
		v, err = p.signInStarted(response, accountLabel)
		if err != nil {
			return v, err
		}
	}
	if err = releaseReceipt(path); err != nil {
		return plugin.View{}, err
	}
	if r.State == "failed" {
		if finish {
			if r.Error.Code == "invalid_param" && r.Error.Field == "redirect_url" {
				return v, badField("redirect_url", redirectAddressHelp)
			}
			return v, errors.New(signInError(r.Error.Code))
		}
		if r.Error.Message != "" {
			return v, errors.New(p.clean(r.Error.Message, 400))
		}
		return v, errors.New("The hub could not apply the change.")
	}
	if id == "proxy_reauth_start" {
		return v, nil
	}
	v = plugin.View{UpdatedAt: response.Generated}
	if finish {
		v.Title, _ = signInCopy("ok")
	} else {
		v.Title = p.clean(r.Summary, 400)
	}
	if r.State == "working" {
		if finish {
			v.Title, _ = signInCopy("working")
		}
		v.Title = "Working: " + v.Title
		v.Blocks = []plugin.Block{notice("info", "Still working. The result shows here when it lands.")}
		return v, nil
	}
	if finish {
		return v, nil
	}
	detail := "Applied"
	var pending []string
	for _, h := range r.Hosts {
		if h.State == "pending" {
			pending = append(pending, p.word(h.Host))
		}
	}
	if len(pending) > 0 {
		detail = "Pending on " + strings.Join(pending, ", ")
	}
	if r.Commit != "" {
		runes := []rune(p.word(r.Commit))
		detail = join(detail, "Commit "+string(runes[:min(7, len(runes))]))
	}
	if id == "force_file" && r.Force != nil {
		detail = join(detail, p.enum(r.Force.State, forceStates).Text)
	}
	v.Blocks = []plugin.Block{notice("info", detail)}
	return v, nil
}
