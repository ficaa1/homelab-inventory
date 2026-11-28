package fleet

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/plugin"
)

var proxyID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var proxyStates = map[string]string{"unreachable": "Proxy unreachable", "key_rejected": "Management key rejected", "not_configured": "Proxy not set up"}
var strategies = map[string]string{"round-robin": "Round robin", "fill-first": "Fill first", "weighted-round-robin": "Weighted round robin"}
var accountStates = map[string]plugin.Badge{
	"active": badge("Active", "ok"), "cooling_down": badge("Cooling down", "warn"), "disabled": badge("Disabled", "muted"), "needs_sign_in": badge("Needs sign-in", "warn"), "error": badge("Error", "crit"),
}

type proxyResponse struct {
	envelope
	Proxy     struct{ State, Message string }
	Routing   *struct{ Strategy, Etag string }
	Accounts  []proxyAccount
	Account   *proxyAccount
	Truncated bool
}
type proxyAccount struct {
	ID, Provider, Label, Status, Renewal string
	Disabled                             bool
	RequiresSignIn                       bool       `json:"requires_sign_in"`
	Expires                              *time.Time `json:"access_expires_at"`
	Priority                             *int
	Weight                               *float64
	Cooldowns                            []struct {
		Model  *string
		Until  *time.Time
		Reason string
	}
	Requests *struct {
		Success, Failed *int
		Recent          []struct {
			At              time.Time
			Success, Failed *float64
		}
	}
	Quota []struct {
		Window   string
		Used     *float64   `json:"used_ratio"`
		Resets   *time.Time `json:"resets_at"`
		Observed *time.Time `json:"observed_at"`
	}
}
type signInResponse struct {
	envelope
	Session, Account, State string
	Expires                 *time.Time `json:"expires_at"`
}
type reauthReceipt struct {
	Session, URL string
	RedirectHint string     `json:"redirect_hint"`
	Expires      *time.Time `json:"expires_at"`
}

func proxyActions() []plugin.ActionSpec {
	account := field("account", "Account ID", plugin.Text)
	strategy := choice("strategy", "Strategy")
	for _, key := range []string{"round-robin", "fill-first", "weighted-round-robin"} {
		strategy.Options = append(strategy.Options, plugin.Option{Value: key, Label: strategies[key]})
	}
	start := action("proxy_reauth_start", "Sign in again", false, account)
	start.Result = "view"
	return []plugin.ActionSpec{
		action("proxy_set_strategy", "Change routing strategy", false, strategy, field("etag", "Routing version", plugin.Text)),
		action("proxy_set_account_routing", "Change account routing", false, account, field("priority", "Priority", plugin.Number), field("weight", "Weight", plugin.Number)),
		action("proxy_set_account_enabled", "Enable account", false, account, field("enabled", "Enabled", plugin.Bool)),
		action("proxy_refresh_account", "Renew access", false, account), action("proxy_reset_cooldown", "Clear cooldown", false, account),
		start, action("proxy_reauth_finish", "Finish sign-in", false, field("session", "Sign-in session", plugin.Text), field("redirect_url", "Full redirect address", plugin.Secret)),
	}
}
func detailLink(view, key, value string) *plugin.Link {
	return &plugin.Link{View: view, Params: map[string]string{key: value}}
}

func (p *Fleet) proxyView(ctx context.Context, v *plugin.View, name string, params url.Values) error {
	var d proxyResponse
	switch name {
	case "proxy":
		v.Title = "Proxy"
		if err := p.get(ctx, "proxy", p.readToken, &d); err != nil {
			return err
		}
		p.proxyOverview(v, d)
	case "account":
		v.Title = "Account"
		id := params.Get("id")
		if len(params["id"]) != 1 || !proxyID.MatchString(id) {
			v.Blocks = []plugin.Block{notice("info", "Open an account from Proxy.")}
			return nil
		}
		if err := p.get(ctx, "proxy/accounts/"+id, p.readToken, &d); err != nil {
			return err
		}
		if d.Proxy.State != "ok" {
			v.Blocks = []plugin.Block{p.proxyNotice(d)}
			return nil
		}
		if d.Account == nil || d.Account.ID != id {
			return errResponse
		}
		p.accountView(v, d)
	case "sign-in":
		v.Title = "Sign-in"
		session := params.Get("session")
		v.RefreshSeconds = 0
		if len(params["session"]) != 1 || !proxyID.MatchString(session) {
			v.Blocks = []plugin.Block{notice("info", "Start sign-in from a proxy account.")}
			return nil
		}
		var d signInResponse
		if err := p.get(ctx, "proxy/sign-in/"+session, p.readToken, &d); err != nil {
			return err
		}
		if d.Session != session {
			return errResponse
		}
		p.signInView(v, d)
	}
	return nil
}
func (p *Fleet) proxyNotice(d proxyResponse) plugin.Block {
	text, tone := p.label(d.Proxy.State, proxyStates), "muted"
	if proxyStates[d.Proxy.State] != "" {
		tone = "warn"
	}
	if text == "" {
		text = "Proxy state not reported"
	}
	if msg := p.clean(d.Proxy.Message, 400); msg != "" {
		text = join(text, msg)
	}
	return notice(tone, text)
}
func (a proxyAccount) needsSignIn() bool { return a.RequiresSignIn || a.Status == "needs_sign_in" }
func countStat(label string, n int, known, warn bool) plugin.Item {
	item := plugin.Item{Label: label, Value: "Not reported"}
	if known {
		item.Value = strconv.Itoa(n)
		if warn && n > 0 {
			item.Tone = "warn"
		}
	}
	return item
}
func (p *Fleet) proxyOverview(v *plugin.View, d proxyResponse) {
	if d.Proxy.State != "ok" {
		v.Blocks = []plugin.Block{p.proxyNotice(d)}
	} else {
		strategy := "Not reported"
		var usable, cooling, signIn int
		list := plugin.Block{Type: "list", Title: "Accounts", Empty: "No accounts"}
		if d.Accounts == nil {
			list.Empty = "Accounts not reported"
		}
		for _, a := range d.Accounts {
			weightedOut := d.Routing != nil && d.Routing.Strategy == "weighted-round-robin" && a.Weight != nil && *a.Weight == 0
			if a.Status == "active" && !a.Disabled && !a.needsSignIn() && !weightedOut {
				usable++
			}
			if a.Status == "cooling_down" {
				cooling++
			}
			if a.needsSignIn() {
				signIn++
			}
			list.Items = append(list.Items, p.accountItem(a, d.Generated))
		}
		if d.Routing != nil {
			strategy = p.label(d.Routing.Strategy, strategies)
			defaults := plugin.Config{}
			if strategies[d.Routing.Strategy] != "" {
				defaults["strategy"] = d.Routing.Strategy
			}
			p.offer(v, "proxy_set_strategy", "", "Change how the proxy chooses an account for new requests?", plugin.Config{"etag": d.Routing.Etag}, defaults)
		}
		known := d.Accounts != nil
		v.Blocks = []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "Strategy", Value: strategy}, countStat("Usable", usable, known, false), countStat("Cooling down", cooling, known, true), countStat("Need sign-in", signIn, known, true)}}, list}
		if d.Truncated {
			v.Blocks = append(v.Blocks, notice("info", "The hub returned only the first 200 accounts."))
		}
	}
	if p.panelURL != "" {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Title: "Management panel", Items: []plugin.Item{{Title: "Open the proxy's own panel for anything not covered here", Link: &plugin.Link{Href: p.panelURL}}}})
	}
}
func quantity[T int | float64](v *T) string {
	if v == nil {
		return "Not reported"
	}
	return fmt.Sprint(*v)
}
func timestamp(t *time.Time) string {
	if t == nil {
		return "Not reported"
	}
	return t.UTC().Format(time.RFC3339)
}
func remaining(d time.Duration) string {
	switch {
	case d <= 0:
		return "0 min"
	case d < time.Minute:
		return "under 1 min"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return plural(int(d/(24*time.Hour)), "day", "days")
	}
}
func (a proxyAccount) totals() (ok, failed string) {
	if a.Requests == nil {
		return "Not reported", "Not reported"
	}
	return quantity(a.Requests.Success), quantity(a.Requests.Failed)
}
func (p *Fleet) accountItem(a proxyAccount, now time.Time) plugin.Item {
	state := p.enum(a.Status, accountStates)
	ok, failed := a.totals()
	item := plugin.Item{Title: p.word(a.Label), Subtitle: join(p.word(a.Provider), state.Text), Tone: state.Tone,
		Meta: join("Priority "+quantity(a.Priority), "weight "+quantity(a.Weight), ok+" ok, "+failed+" failed")}
	if proxyID.MatchString(a.ID) && p.word(a.ID) == a.ID {
		item.Link = detailLink("account", "id", a.ID)
	}
	if a.Status == "cooling_down" {
		var until time.Time
		for _, c := range a.Cooldowns {
			if c.Until != nil && c.Until.After(until) {
				until = *c.Until
			}
		}
		word := "Cooling down"
		if until.After(now) {
			word += ", " + remaining(until.Sub(now)) + " left"
		}
		item.Badges = append(item.Badges, badge(word, "warn"))
	}
	if a.needsSignIn() {
		item.Badges = append(item.Badges, badge("Needs sign-in", "warn"))
	}
	if a.Disabled || a.Status == "disabled" {
		item.Badges = append(item.Badges, badge("Disabled", "muted"))
	}
	if a.Renewal == "failed" {
		item.Badges = append(item.Badges, badge("Renewal failed", "warn"))
	}
	var highest *float64
	for _, q := range a.Quota {
		if q.Used != nil && (highest == nil || *q.Used > *highest) {
			highest = q.Used
		}
	}
	if highest != nil {
		tone := "info"
		if *highest >= 0.8 {
			tone = "warn"
		}
		item.Badges = append(item.Badges, badge(fmt.Sprintf("Quota %.0f%% used", *highest*100), tone))
	}
	return item
}
func (p *Fleet) accountView(v *plugin.View, d proxyResponse) {
	a := d.Account
	v.Title = p.word(a.Label)
	state := p.enum(a.Status, accountStates)
	expiry := "Not reported"
	if a.Expires != nil {
		expiry = "Expired " + age(d.Generated, *a.Expires)
		if a.Expires.After(d.Generated) {
			expiry = "In " + remaining(a.Expires.Sub(d.Generated))
		}
	}
	ok, failed := a.totals()
	renewal := map[string]plugin.Badge{"ok": badge("OK", "ok"), "renewing": badge("Renewing", "info"), "failed": badge("Failed", "warn")}[a.Renewal]
	if renewal.Text == "" {
		renewal = badge("Unknown", "muted")
	}
	v.Blocks = []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "Status", Value: state.Text, Tone: state.Tone}, {Label: "Priority", Value: quantity(a.Priority)}, {Label: "Weight", Value: quantity(a.Weight)}, {Label: "Access expires", Value: expiry}, {Label: "Access renewal", Value: renewal.Text, Tone: renewal.Tone}, {Label: "Requests ok", Value: ok}, {Label: "Requests failed", Value: failed}}}}
	if a.Quota == nil {
		v.Blocks = append(v.Blocks, notice("info", "This provider doesn't report quota."))
	} else {
		rows := table("Quota", "No quota windows", col("window", "Window"), plugin.Column{Key: "used", Label: "Used", Format: "percent", Align: "right"}, plugin.Column{Key: "resets", Label: "Resets", Format: "time"}, col("observed", "Observed"))
		for _, q := range a.Quota {
			observed := "Not reported"
			if q.Observed != nil {
				observed = age(d.Generated, *q.Observed)
			}
			var used any
			if q.Used != nil {
				used = *q.Used
			}
			rows.Rows = append(rows.Rows, map[string]any{"window": p.word(q.Window), "used": used, "resets": timestamp(q.Resets), "observed": observed})
		}
		v.Blocks = append(v.Blocks, rows)
	}
	cooldowns := table("Cooldowns", "No cooldowns", col("model", "Model"), plugin.Column{Key: "until", Label: "Until", Format: "time"}, col("reason", "Reason"))
	if a.Cooldowns == nil {
		cooldowns.Empty = "Cooldowns not reported"
	}
	for _, c := range a.Cooldowns {
		model := "Whole account"
		if c.Model != nil {
			model = p.word(*c.Model)
		}
		cooldowns.Rows = append(cooldowns.Rows, map[string]any{"model": model, "until": timestamp(c.Until), "reason": p.label(c.Reason, map[string]string{"rate_limited": "Rate limited"})})
	}
	chart := plugin.Block{Type: "chart", Title: "Requests, last 200 minutes", Unit: "number", Empty: "No recent requests", Series: []plugin.Series{{Name: "Succeeded", Tone: "ok"}, {Name: "Failed", Tone: "crit"}}}
	if a.Requests == nil || a.Requests.Recent == nil {
		chart.Empty = "Recent requests not reported"
	} else {
		for _, r := range a.Requests.Recent {
			at := float64(r.At.Unix())
			chart.Series[0].Points = append(chart.Series[0].Points, plugin.Point{&at, r.Success})
			chart.Series[1].Points = append(chart.Series[1].Points, plugin.Point{&at, r.Failed})
		}
	}
	v.Blocks = append(v.Blocks, cooldowns, chart)
	label := p.clean(a.Label, 120)
	account := plugin.Config{"account": a.ID}
	defaults := plugin.Config{}
	if a.Priority != nil {
		defaults["priority"] = *a.Priority
	}
	if a.Weight != nil {
		defaults["weight"] = *a.Weight
	}
	p.offer(v, "proxy_set_account_routing", "", "Change the priority and weight used to route requests to "+label+"?", account, defaults)
	toggle := "Disable account"
	confirm := "Disable " + label + "? Routing skips a disabled account."
	if a.Disabled {
		toggle = "Enable account"
		confirm = "Enable " + label + "? Routing skips a disabled account. Enabling it makes it eligible for requests."
	}
	p.offer(v, "proxy_set_account_enabled", toggle, confirm, plugin.Config{"account": a.ID, "enabled": a.Disabled}, nil)
	p.offer(v, "proxy_refresh_account", "", "Ask the proxy to renew access for "+label+"?", account, nil)
	if len(a.Cooldowns) > 0 {
		p.offer(v, "proxy_reset_cooldown", "", "Clear the proxy's cooldown for "+label+"? This clears only the proxy's own cooldown, not the provider's limit.", account, nil)
	}
	p.offer(v, "proxy_reauth_start", "", "Open a new sign-in session for "+label+"?", account, nil)
}

const redirectAddressHelp = "Paste the whole address the browser landed on, starting with http://localhost."

func signInCopy(state string) (text, tone string) {
	switch state {
	case "waiting":
		return "Waiting for the address from the sign-in page.", "info"
	case "working":
		return "Checking the sign-in.", "info"
	case "ok":
		return "Signed in. The account is usable again.", "ok"
	case "error":
		return "The sign-in failed. Start again with Sign in again.", "crit"
	case "expired":
		return "This sign-in expired after 5 minutes. Start again with Sign in again.", "warn"
	default:
		return "The sign-in state is unknown.", "muted"
	}
}

func signInError(code string) string {
	switch code {
	case "not_found", "conflict":
		return "This sign-in is no longer available. Start again with Sign in again."
	case "forbidden":
		return "The hub refused access to this sign-in. Check the Fleet plugin settings."
	case "busy", "rate_limited", "upstream_failed":
		return "The hub could not check the sign-in. Check the hub and try again."
	}
	text, _ := signInCopy("error")
	return text
}

func (p *Fleet) signInView(v *plugin.View, d signInResponse) {
	states := map[string]string{"waiting": "Waiting", "working": "Working", "ok": "Signed in", "error": "Sign-in failed", "expired": "Expired"}
	word := states[d.State]
	if word == "" {
		word = "Unknown"
	}
	v.Blocks = []plugin.Block{{Type: "stats", Items: []plugin.Item{{Label: "State", Value: word}, {Label: "Expires", Value: timestamp(d.Expires)}}}}
	text, tone := signInCopy(d.State)
	if d.State == "waiting" {
		p.offer(v, "proxy_reauth_finish", "", "Finish this sign-in session? The proxy uses the redirect address to renew access to the account.", plugin.Config{"session": d.Session}, nil)
	}
	if d.State == "waiting" || d.State == "working" {
		v.RefreshSeconds = 5
	}
	v.Blocks = append(v.Blocks, notice(tone, text))
	if proxyID.MatchString(d.Account) && p.word(d.Account) == d.Account {
		v.Blocks = append(v.Blocks, plugin.Block{Type: "list", Items: []plugin.Item{{Title: "Open account", Link: detailLink("account", "id", d.Account)}}})
	}
}
func (p *Fleet) signInStarted(response receiptResponse, label string) (plugin.View, error) {
	r := response.Receipt.Reauth
	if r == nil || !proxyID.MatchString(r.Session) || p.word(r.Session) != r.Session || r.Expires == nil || r.RedirectHint == "" || !p.safeLink(r.URL) {
		return plugin.View{}, errResponse
	}
	steps := "Open the sign-in page on any device and sign in. The browser then lands on an address on " + p.word(r.RedirectHint) + " that fails to load. Copy that whole address and paste it on the sign-in page here."
	return plugin.View{Title: "Sign in again: " + label, UpdatedAt: response.Generated, Blocks: []plugin.Block{notice("info", steps), {Type: "list", Items: []plugin.Item{
		{Title: "Open sign-in page", Link: &plugin.Link{Href: r.URL}}, {Title: "Continue sign-in here", Link: detailLink("sign-in", "session", r.Session)},
	}}, {Type: "stats", Items: []plugin.Item{{Label: "Expires", Value: timestamp(r.Expires)}}}}}, nil
}
func (p *Fleet) safeLink(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || raw != p.clean(raw, 8192) {
		return false
	}
	parts := []string{u.Path, u.Fragment}
	for entry := range strings.SplitSeq(u.RawQuery, "&") {
		key, value, _ := strings.Cut(entry, "=")
		for _, encoded := range []string{key, value} {
			decoded, err := url.QueryUnescape(encoded)
			if err != nil {
				return false
			}
			parts = append(parts, decoded)
		}
	}
	for _, token := range []string{p.readToken, p.operatorToken} {
		if token == "" {
			continue
		}
		for _, part := range parts {
			if strings.Contains(part, token) {
				return false
			}
		}
	}
	return true
}
