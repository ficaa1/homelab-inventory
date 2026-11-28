package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
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
	data["accounts"] = nil
	data["routing"] = nil
	v = checkedView(t, p, "proxy", nil)
	assertText(t, v, "Accounts not reported", "Not reported")
	for state, word := range map[string]string{"not_configured": "Proxy not set up", "unreachable": "Proxy unreachable", "key_rejected": "Management key rejected", "another_state": "another_state"} {
		data["proxy"] = map[string]any{"state": state, "message": "Check <connection>"}
		v = checkedView(t, p, "proxy", nil)
		if v.Blocks[0].Tone != "warn" || v.Blocks[0].Text != word+" · Check connection" {
			t.Fatal("incorrect proxy notice", state)
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
	var key, signURL string
	signURL = "https://signin.example.com/start?session=public-fixture"
	status := http.StatusServiceUnavailable
	state := "waiting"
	p, h := actionHub(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/proxy/accounts/acc_a":
			fmt.Fprint(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","proxy":{"state":"ok"},"account":{"id":"acc_a","label":"…@example.com"}}`)
		case "/v1/actions/proxy_reauth_start":
			var body struct{ Params map[string]string }
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Params) != 1 || body.Params["account"] != "acc_a" {
				t.Error("wrong start body")
			}
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","receipt":{"summary":"Session started","reauth":{"session":"ras_a","url":%q,"redirect_hint":"localhost:1455","expires_at":"2026-12-01T09:20:00Z"}}}`, signURL)
		case "/v1/proxy/sign-in/ras_a":
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:15:00Z","session":"ras_a","account":"acc_a","state":%q,"expires_at":"2026-12-01T09:20:00Z"}`, state)
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
			if status != 202 {
				fmt.Fprintf(w, `{"error":{"field":"redirect_url","message":%q}}`, "Retry "+callback)
				return
			}
			fmt.Fprintf(w, `{"api_version":1,"generated_at":"2026-12-01T09:16:00Z","receipt":{"summary":%q,"state":"working","hosts":[]}}`, "Finishing "+callback)
		default:
			t.Error("unknown sign-in route")
		}
	})
	private := func(raw []byte) {
		t.Helper()
		if bytes.Contains(raw, []byte(callback)) || bytes.Contains(raw, []byte("private-fixture-code")) {
			t.Fatal("sign-in credential disclosed")
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
	for _, s := range []string{"waiting", "working", "ok", "error", "expired", "future"} {
		state = s
		v = checkedView(t, p, "sign-in", url.Values{"session": {session}})
		check(v)
		if (v.RefreshSeconds == 5) != (s == "waiting" || s == "working") || (len(v.Actions) > 0) != (s == "waiting") || v.Blocks[2].Items[0].Link.Params["id"] != "acc_a" {
			t.Fatal("wrong session controls", s)
		}
		if s == "waiting" && (v.Actions[0].Values["session"] != session || v.Actions[0].Fields[1].Type != plugin.Secret) {
			t.Fatal("wrong finish form")
		}
	}
	values := plugin.Config{"session": session, "redirect_url": callback}
	_, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	if err == nil {
		t.Fatal("missing upstream error")
	}
	private([]byte(err.Error()))
	files, err := os.ReadDir(filepath.Join(h.dir, "receipts"))
	want := sha256.Sum256([]byte("proxy_reauth_finish\x00" + `{"session":"ras_a"}`))
	if err != nil || len(files) != 1 || files[0].Name() != fmt.Sprintf("%x", want) {
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
	status = 202
	v, err = p.Action(t.Context(), "proxy_reauth_finish", values)
	if err != nil {
		t.Fatal(err)
	}
	check(v)
	if !strings.HasPrefix(v.Title, "Working: ") {
		t.Fatal("missing pending title")
	}
	files, err = os.ReadDir(filepath.Join(h.dir, "receipts"))
	if err != nil || len(files) != 0 {
		t.Fatal("completed receipt retained")
	}
	private(h.logs.Bytes())
	signURL = "javascript:alert(1)"
	if _, err = p.Action(t.Context(), "proxy_reauth_start", plugin.Config{"account": "acc_a"}); err == nil {
		t.Fatal("unsafe sign-in link")
	}
	p.operatorToken = ""
	state = "waiting"
	if len(checkedView(t, p, "sign-in", url.Values{"session": {session}}).Actions) > 0 {
		t.Fatal("read-only session offers writes")
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
	check(map[string]string{"proxy_renewal/acc_a": "warning", "proxy_sign_in/acc_c": "warning", "proxy_cooling/provider-a": "critical"})
	a := data["accounts"].([]any)[1].(map[string]any)
	a["disabled"] = false
	a["status"] = "active"
	data["accounts"].([]any)[0].(map[string]any)["access_expires_at"] = "2026-12-02T09:15:01Z"
	check(map[string]string{"proxy_sign_in/acc_c": "warning"})
	data["truncated"] = true
	if _, err := p.Alerts(t.Context()); err == nil {
		t.Fatal("truncated account list resolved alerts")
	}
	for _, state := range []string{"unreachable", "key_rejected", "not_configured"} {
		data["proxy"] = map[string]any{"state": state}
		if state == "not_configured" {
			check(nil)
		} else {
			check(map[string]string{"proxy_unreachable/proxy": "warning"})
		}
	}
	status = 404
	check(nil)
	status = 503
	if _, err := p.Alerts(t.Context()); err == nil {
		t.Fatal("upstream failure silently resolved alerts")
	}
}
