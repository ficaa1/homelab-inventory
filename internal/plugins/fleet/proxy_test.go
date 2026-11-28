package fleet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

func proxyData(t *testing.T) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(fixture(t, "proxy"), &data); err != nil {
		t.Fatal(err)
	}
	return data
}
func checkedView(t *testing.T, p *Fleet, name string, params url.Values) plugin.View {
	t.Helper()
	v, err := p.View(t.Context(), name, params)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = plugin.ValidateView(v, p.Manifest()); err != nil {
		t.Fatal(err)
	}
	assertPrivate(t, v)
	return v
}
func TestProxyViews(t *testing.T) {
	data := proxyData(t)
	p, _ := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/proxy":
			json.NewEncoder(w).Encode(data)
		case "/v1/proxy/accounts/acc_a", "/v1/proxy/accounts/acc_b":
			idx := 0
			if strings.HasSuffix(r.URL.Path, "acc_b") {
				idx = 1
			}
			account := data["accounts"].([]any)[idx]
			json.NewEncoder(w).Encode(map[string]any{"api_version": 1, "generated_at": data["generated_at"], "proxy": data["proxy"], "account": account})
		default:
			t.Error("unexpected route")
		}
	})
	p.panelURL = "https://panel.example.com/manage"
	v := checkedView(t, p, "proxy", nil)
	if v.Actions[0].Values["etag"] != `"r-41"` || v.Actions[0].Fields[0].Default != "round-robin" {
		t.Fatal("missing strategy defaults")
	}
	assertText(t, v.Blocks, "Round robin", "Cooling down, 25 min left", "Renewal failed", "Quota 80% used", "Priority 0 · weight 1 · 1204 ok, 31 failed", "https://panel.example.com/manage", "Needs sign-in")
	if v.Blocks[1].Items[0].Link.Params["id"] != "acc_a" || v.Blocks[1].Items[0].Badges[2].Tone != "warn" {
		t.Fatal("wrong account link or quota tone")
	}
	v = checkedView(t, p, "account", url.Values{"id": {"acc_a"}})
	if len(v.Actions) != 5 || v.Actions[0].Fields[1].Default != 0 || v.Actions[1].Values["enabled"] != false || v.Actions[1].Destructive {
		t.Fatal("incorrect account controls")
	}
	assertText(t, v.Blocks, "Whole account", "5 min ago", "Requests, last 200 minutes", "Succeeded", "Failed")
	if v.Blocks[1].Rows[0]["used"] != 0.42 || len(v.Blocks[3].Series[0].Points) != 1 {
		t.Fatal("quota or chart")
	}
	v = checkedView(t, p, "account", url.Values{"id": {"acc_b"}})
	assertText(t, v.Blocks, "This provider doesn't report quota.", "No cooldowns", "Not reported")
	if len(v.Actions) != 4 || v.Actions[1].Values["enabled"] != true || v.Actions[1].Label != "Enable account" {
		t.Fatal("incorrect actions for disabled account")
	}
	p.operatorToken = ""
	for _, name := range []string{"account", "proxy"} {
		if len(checkedView(t, p, name, url.Values{"id": {"acc_a"}}).Actions) > 0 {
			t.Fatal("read-only account offers writes")
		}
	}
	active := data["accounts"].([]any)[1].(map[string]any)
	active["status"], active["disabled"] = "active", false
	if checkedView(t, p, "proxy", nil).Blocks[0].Items[1].Value != "1" {
		t.Fatal("zero weight should still route under round robin")
	}
	data["routing"].(map[string]any)["strategy"] = "weighted-round-robin"
	if checkedView(t, p, "proxy", nil).Blocks[0].Items[1].Value != "0" {
		t.Fatal("zero weight cannot route under weighted round robin")
	}
	data["routing"] = map[string]any{"strategy": "another_strategy", "etag": "opaque"}
	data["accounts"].([]any)[0].(map[string]any)["status"] = "another_state"
	v = checkedView(t, p, "proxy", nil)
	if v.Blocks[1].Items[0].Tone != "muted" {
		t.Fatal("unknown status tone")
	}
	assertText(t, v, "another_strategy", "another_state")
	accounts := data["accounts"].([]any)
	data["accounts"] = nil
	data["routing"] = nil
	v = checkedView(t, p, "proxy", nil)
	assertText(t, v, "Accounts not reported", "Not reported")
	for state, word := range map[string]string{"not_configured": "Proxy not set up", "unreachable": "Proxy unreachable", "key_rejected": "Management key rejected", "another_state": "another_state"} {
		data["proxy"] = map[string]any{"state": state, "message": "Check <connection>"}
		v = checkedView(t, p, "proxy", nil)
		tone := "warn"
		if state == "another_state" {
			tone = "muted"
		}
		if v.Blocks[0].Tone != tone || v.Blocks[0].Text != word+" · Check connection" {
			t.Fatal("incorrect proxy notice", state)
		}
	}
	account := accounts[0].(map[string]any)
	data["accounts"] = accounts
	data["proxy"] = map[string]any{"state": "ok"}
	for renewal, word := range map[string]string{"ok": "OK", "renewing": "Renewing", "failed": "Failed", "unknown": "Unknown", "future": "Unknown"} {
		account["renewal"] = renewal
		v = checkedView(t, p, "account", url.Values{"id": {"acc_a"}})
		found := false
		for _, item := range v.Blocks[0].Items {
			if item.Label == "Access renewal" && item.Value == word {
				found = true
			}
		}
		if !found {
			t.Fatal("missing renewal state", renewal)
		}
	}

}

func TestProxyRequests(t *testing.T) {
	checkRequests(t, []requestCase{
		{"proxy_set_strategy", plugin.Config{"strategy": "fill-first", "etag": `"r-41"`}, `{"strategy":"fill-first","etag":"\"r-41\""}`},
		{"proxy_set_account_routing", plugin.Config{"account": "acc_a", "priority": float64(-1), "weight": float64(0)}, `{"account":"acc_a","priority":-1,"weight":0}`},
		{"proxy_set_account_enabled", plugin.Config{"account": "acc_a", "enabled": false}, `{"account":"acc_a","enabled":false}`},
		{"proxy_reset_cooldown", plugin.Config{"account": "acc_a"}, `{"account":"acc_a"}`},
		{"proxy_refresh_account", plugin.Config{"account": "acc_a"}, `{"account":"acc_a"}`},
	})
	p, _ := actionHub(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid routing reached hub") })
	for _, n := range [][2]float64{{1.5, 0}, {1, -1}, {1, 1000001}} {
		_, err := p.Action(t.Context(), "proxy_set_account_routing", plugin.Config{"account": "acc_a", "priority": n[0], "weight": n[1]})
		if err == nil {
			t.Fatal("accepted invalid routing")
		}
	}
}
func TestProxySignInFlow(t *testing.T) {
	const callback = "http://localhost:1455/complete?code=private-fixture-code"
	const session = "ras_a"
	const encodedCode = "%70rivate-fixture-code"
	echo := strings.Join([]string{callback, url.QueryEscape(callback), encodedCode}, " ")
	var key string
	signURL := "https://signin.example.com/start?session=public-fixture&state=kept%2Bvalue"
	status, viewStatus := http.StatusServiceUnavailable, http.StatusOK
	state, code, receiptState := "waiting", "upstream_failed", "working"
	p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/proxy/accounts/acc_a":
			fmt.Fprint(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","proxy":{"state":"ok"},"account":{"id":"acc_a","label":"…@example.com"}}`)
		case "/v1/actions/proxy_reauth_start":
			var body struct{ Params map[string]string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Params) != 1 || body.Params["account"] != "acc_a" {
				t.Error("wrong start body")
			}
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","receipt":{"id":"rcp_start","action":"proxy_reauth_start","state":"done","summary":"Session started","reauth":{"session":"ras_a","url":%q,"redirect_hint":"localhost:1455","expires_at":"2026-12-01T09:20:00Z"}}}`, signURL)
		case "/v1/proxy/sign-in/ras_a":
			w.WriteHeader(viewStatus)
			if viewStatus != http.StatusOK {
				fmt.Fprintf(w, `{"error":{"code":"not_found","message":%q}}`, echo)
				return
			}
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","session":"ras_a","account":"acc_a","state":%q,"message":%q,"expires_at":"2026-12-01T09:20:00Z"}`, state, echo)
		case "/v1/actions/proxy_reauth_finish":
			var body struct{ Params map[string]string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Params) != 2 || body.Params["redirect_url"] != callback || body.Params["session"] != session {
				t.Error("wrong finish body")
			}
			if key != "" && key != r.Header.Get("Idempotency-Key") {
				t.Error("changed sign-in retry key")
			}
			key = r.Header.Get("Idempotency-Key")
			w.WriteHeader(status)
			if status >= 400 {
				fmt.Fprintf(w, `{"error":{"code":%q,"field":"redirect_url","message":%q}}`, code, echo)
				return
			}
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:16:00Z","receipt":{"id":"rcp_finish","action":"proxy_reauth_finish","summary":%q,"state":%q,"error":{"code":"conflict","message":%q},"commit":%q,"hosts":[{"host":%q,"state":"pending"}]}}`, echo, receiptState, echo, echo, echo)
		default:
			t.Error("unknown sign-in route")
		}
	})
	private := func(raw []byte) {
		t.Helper()
		for _, fragment := range []string{callback, url.QueryEscape(callback), "private-fixture-code", encodedCode} {
			if bytes.Contains(raw, []byte(fragment)) {
				t.Fatal("sign-in credential disclosed")
			}
		}
	}
	check := func(v plugin.View) {
		t.Helper()
		raw, err := plugin.ValidateView(v, p.Manifest())
		if err != nil {
			t.Fatal(err)
		}
		private(raw)
	}
	v, err := p.Action(t.Context(), "proxy_reauth_start", plugin.Config{"account": "acc_a"})
	if err != nil {
		t.Fatal(err)
	}
	check(v)
	if v.Title != "Sign in again: …@example.com" || v.Blocks[1].Items[0].Link.Href != signURL || v.Blocks[1].Items[1].Link.Params["session"] != session {
		t.Fatal("missing sign-in links")
	}
	assertText(t, v, "localhost:1455", "2026-12-01T09:20:00Z")
	for s, text := range map[string]string{
		"waiting": "Waiting for the address from the sign-in page.", "working": "Checking the sign-in.",
		"ok": "Signed in. The account is usable again.", "error": "The sign-in failed. Start again with Sign in again.",
		"expired": "This sign-in expired after 5 minutes. Start again with Sign in again.", "future": "The sign-in state is unknown.",
	} {
		state = s
		v = checkedView(t, p, "sign-in", url.Values{"session": {session}})
		check(v)
		if v.Blocks[1].Text != text {
			t.Fatal("wrong sign-in status copy", s)
		}
		if (v.RefreshSeconds == 5) != (s == "waiting" || s == "working") || (len(v.Actions) > 0) != (s == "waiting") || v.Blocks[2].Items[0].Link.Params["id"] != "acc_a" {
			t.Fatal("wrong session controls", s)
		}
		if s == "waiting" && (v.Actions[0].Values["session"] != session || v.Actions[0].Fields[1].Type != plugin.Secret) {
			t.Fatal("wrong finish form")
		}
	}
	viewStatus = http.StatusNotFound
	_, err = p.View(t.Context(), "sign-in", url.Values{"session": {session}})
	if err == nil {
		t.Fatal("missing sign-in view error")
	}
	private([]byte(err.Error()))
	values := plugin.Config{"session": session, "redirect_url": callback}
	_, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	if err == nil {
		t.Fatal("missing upstream error")
	}
	private([]byte(err.Error()))
	files, err := os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 1 {
		t.Fatal("unexpected pending receipt")
	}
	err = filepath.WalkDir(h.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		private(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	status, code = 422, "invalid_param"
	_, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	var field *plugin.FieldError
	if !errors.As(err, &field) || field.Key != "redirect_url" || field.Message != redirectAddressHelp {
		t.Fatal("missing fixed redirect address help")
	}
	private([]byte(err.Error()))
	key = ""
	for _, status = range []int{202, 200} {
		v, err = p.Action(t.Context(), "proxy_reauth_finish", values)
		if err != nil {
			t.Fatal(err)
		}
		check(v)
		if v.Title != "Working: Checking the sign-in." || v.Blocks[0].Text != "Still working. The result shows here when it lands." {
			t.Fatal("missing pending copy")
		}
	}
	receiptState = "done"
	v, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	if err != nil || v.Title != "Signed in. The account is usable again." {
		t.Fatal("missing success copy")
	}
	check(v)
	receiptState, key = "failed", ""
	_, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	if err == nil {
		t.Fatal("missing failed receipt error")
	}
	private([]byte(err.Error()))
	files, err = os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 0 {
		t.Fatal("validated receipt retained")
	}
	private(h.logs.Bytes())
	for _, unsafe := range unsafeFleetLinks() {
		signURL = unsafe
		if _, err = p.Action(t.Context(), "proxy_reauth_start", plugin.Config{"account": "acc_a"}); err == nil {
			t.Fatal("unsafe sign-in link")
		}
	}
	viewStatus = http.StatusOK
	p.operatorToken = ""
	state = "waiting"
	if len(checkedView(t, p, "sign-in", url.Values{"session": {session}}).Actions) > 0 {
		t.Fatal("read-only session offers writes")
	}
}

func TestSignInCallbacksHaveSeparatePrivateReservations(t *testing.T) {
	const prefix = "http://localhost:1455/callback?code="
	codes := []string{"first-synthetic-code", "second-synthetic-code"}
	keys := make(chan string, len(codes))
	p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"code":"upstream_failed"}}`)
	})
	for _, code := range codes {
		values := plugin.Config{"session": "ras_a", "redirect_url": prefix + code}
		if _, err := p.Action(t.Context(), "proxy_reauth_finish", values); err == nil {
			t.Fatal("expected an upstream error")
		}
	}
	if <-keys == <-keys {
		t.Fatal("different callback reused the same key")
	}
	dir := filepath.Join(h.dir, "receipts")
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != len(codes) {
		t.Fatal("expected a reservation for each callback")
	}
	for _, file := range files {
		raw, err := os.ReadFile(filepath.Join(dir, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, code := range codes {
			for _, part := range []string{prefix + code, code} {
				if strings.Contains(file.Name(), part) || bytes.Contains(raw, []byte(part)) {
					t.Fatal("callback credential persisted")
				}
			}
		}
	}
}
func unsafeFleetLinks() []string {
	encoded := func(token string) string { return fmt.Sprintf("%%%02X%s", token[0], token[1:]) }
	return []string{
		"javascript:alert(1)", "https://user:password@example.com/", "https://example.com/" + encoded(readToken),
		"https://example.com/?" + encoded(operatorToken) + "=fixture",
		"https://example.com/?fixture=" + encoded(readToken), "https://example.com/#" + encoded(operatorToken),
	}
}

func TestProxyPanelURLChecksDecodedTokens(t *testing.T) {
	p := &Fleet{}
	for _, unsafe := range unsafeFleetLinks() {
		var field *plugin.FieldError
		err := p.Configure(plugin.Config{"read_token": readToken, "operator_token": operatorToken, "proxy_panel_url": unsafe})
		if !errors.As(err, &field) || field.Key != "proxy_panel_url" {
			t.Fatal("unsafe panel link")
		}
	}
	const good = "https://example.com/p%61nel?q=public%2Btest#test"
	if err := p.Configure(plugin.Config{"read_token": readToken, "operator_token": operatorToken, "proxy_panel_url": good}); err != nil || p.panelURL != good {
		t.Fatal("valid panel URL changed")
	}
}

func TestProxyAlerts(t *testing.T) {
	data := proxyData(t)
	overview := fixture(t, "overview")
	status := 200
	p, _ := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/overview":
			w.Write(overview)
		case "/v1/proxy":
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(data)
		default:
			t.Error("unexpected alert route")
		}
	})
	check := func(want map[string]string) {
		t.Helper()
		alerts, err := p.Alerts(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, a := range alerts {
			if strings.HasPrefix(a.Rule, "proxy_") {
				count++
				if want[a.Rule+"/"+a.Subject] != a.Severity {
					t.Fatal("unexpected proxy alert", a.Rule, a.Subject)
				}
				if a.Rule == "proxy_sign_in" && (!strings.Contains(a.Summary, "…@example.com") || !strings.Contains(a.Summary, "provider-b")) {
					t.Fatal("missing account identity")
				}
			}
		}
		if count != len(want) {
			t.Fatal("wrong proxy alert count", count)
		}
	}
	complete := map[string]string{"proxy_renewal/acc_a": "warning", "proxy_sign_in/acc_c": "warning", "proxy_cooling/provider-a": "critical"}
	check(complete)
	outage := maps.Clone(complete)
	outage["proxy_unreachable/proxy"] = "warning"
	accounts := data["accounts"]
	data["accounts"] = nil
	check(outage)
	for _, state := range []string{"unreachable", "key_rejected", "not_configured", "future", ""} {
		data["proxy"] = map[string]any{"state": state}
		check(outage)
	}
	status = 404
	check(complete)
	status = 503
	if _, err := p.Alerts(t.Context()); err == nil {
		t.Fatal("upstream failure silently resolved alerts")
	}
	status = 200
	data["proxy"] = map[string]any{"state": "ok"}
	data["accounts"], data["truncated"] = accounts, true
	if _, err := p.Alerts(t.Context()); err == nil {
		t.Fatal("truncated account list resolved alerts")
	}
	data["truncated"] = false
	data["accounts"] = []any{}
	check(nil)
	data["accounts"] = accounts
	check(complete)
	base := p.base
	configure := func(base string) {
		t.Helper()
		if err := p.Configure(plugin.Config{"hub_url": base, "read_token": readToken, "operator_token": operatorToken}); err != nil {
			t.Fatal(err)
		}
	}
	data["accounts"] = nil
	configure(base + "/")
	check(outage)
	configure(base + "/other-hub")
	if len(p.proxyAlerts) != 0 {
		t.Fatal("different hub retained account alerts")
	}
	configure(base)
	check(map[string]string{"proxy_unreachable/proxy": "warning"})
	data["accounts"] = accounts
	check(complete)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	configure(base)
	data["accounts"] = nil
	check(map[string]string{"proxy_unreachable/proxy": "warning"})
}
