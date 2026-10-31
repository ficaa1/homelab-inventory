package webui

import (
	"regexp"
	"strings"
	"testing"
)

// Walk every request expression, including paths composed with a host/key.
// Unknown dynamic expressions fail closed so adding a new fetch requires review.
func TestRequestsUseRetainedRoutes(t *testing.T) {
	page := coreBody(t)
	script := strings.Split(strings.Split(page, "<script>")[1], "</script>")[0]
	pageHelpers := 0
	calls := regexp.MustCompile(`\b(api|write|operatorWrite|actionPost|fetch)\(|\bxhr\.open\("POST", `)
	literals := regexp.MustCompile(`"([^"\n]*)"|'([^'\n]*)'`)
	encode := regexp.MustCompile(`encodeURIComponent\([^)]*\)`)
	routes := regexp.MustCompile(`^/(dusk/(admin|enrichment|options)|healthz|capabilities|overview|alerts|hosts|ports|services|services/accept|services/\{value\}|icons/\{value\}|agent/status|plugins|plugins/\{value\}|plugins/\{value\}/test|plugins/\{value\}/(views|images|upload)/\{value\}|uploads/targets|auth/(status|logout)|settings/auth|settings/auth/test|agent/offers|images|silences|silences/\{value\}|settings|settings/reset|notify/test|setup/(status|claim|enroll)|storage/runs|storage/runs/prune|storage/runs/\{value\}|actions/(stacks|log|preview|execute)|hosts/\{value\}(/(history|drift|trend|deps|compose|compose/diff|compose/file|firewall-plan|packages|packages/changes))?)$`)
	checked, helpers := 0, 0
	for _, loc := range calls.FindAllStringIndex(script, -1) {
		// Definitions do not make requests.
		if strings.HasSuffix(script[:loc[0]], "function ") {
			continue
		}
		rest := script[loc[1]:]
		depth, quote, end := 0, byte(0), 0
		for i := 0; i < len(rest); i++ {
			c := rest[i]
			if quote != 0 {
				if c == '\\' {
					i++
				} else if c == quote {
					quote = 0
				}
				continue
			}
			if c == '"' || c == '\'' {
				quote = c
				continue
			}
			if c == '(' {
				depth++
			}
			if c == ')' {
				if depth == 0 {
					end = i
					break
				}
				depth--
			}
			if c == ',' && depth == 0 {
				end = i
				break
			}
		}
		expr := strings.TrimSpace(rest[:end])
		if expr == "target" {
			if !strings.Contains(script, `var paths = {export: '/admin/backup/export', preview: '/admin/backup/preview', restore: '/admin/backup/restore'}`) {
				t.Fatal("backup request paths changed without review")
			}
			checked++
			continue
		}
		if expr == "path" {
			helpers++
			continue
		}
		// Plugin pages reach only their own plugin's API, through the kit.
		if strings.Contains(expr, "pagePath(id, path)") {
			if !strings.Contains(script, `function pagePath(id, path) { return "/plugins/" + encodeURIComponent(id) + "/api/" + path; }`) {
				t.Fatal("plugin page API path changed without review")
			}
			pageHelpers++
			continue
		}
		if strings.HasPrefix(expr, "base +") {
			if !strings.Contains(script, `var base = "/hosts/" + encodeURIComponent(name) + "/packages";`) {
				t.Fatal("package request base changed")
			}
			expr = strings.Replace(expr, "base", `"/hosts/{value}/packages"`, 1)
		}
		expr = encode.ReplaceAllString(expr, `"{value}"`)
		// Query parameters cannot change the endpoint path. Drop their expression
		// before checking dynamic path fragments (compose has an optional at=).
		for _, m := range literals.FindAllStringIndex(expr, -1) {
			if q := strings.Index(expr[m[0]:m[1]], "?"); q >= 0 {
				expr = expr[:m[0]+q] + expr[m[0]:m[0]+1]
				break
			}
		}
		// Only a locally built query string may sit outside quoted path fragments.
		residue := strings.TrimSpace(literals.ReplaceAllString(expr, ""))
		residue = strings.TrimSpace(strings.ReplaceAll(residue, "+", ""))
		if residue != "" && residue != "q" {
			t.Errorf("unreviewed request expression: %s", expr)
			continue
		}
		path := ""
		for _, m := range literals.FindAllStringSubmatch(expr, -1) {
			path += m[1] + m[2]
		}
		path = strings.Split(path, "?")[0]
		if !routes.MatchString(path) {
			t.Errorf("request uses a route outside the retained API: %s (%s)", path, expr)
		}
		checked++
	}
	if checked < 40 || helpers != 5 || pageHelpers != 3 {
		t.Errorf("request scan missed calls: checked=%d helper calls=%d", checked, helpers)
	}
	t.Logf("checked %d request paths and %d shared-helper calls", checked, helpers)
}

// coreBody is the served page without the built-in plugin pages, which are
// checked by TestPluginPagesUseTheKit.
func coreBody(t *testing.T) string {
	t.Helper()
	page := strings.Replace(body(t), "<script>"+pageAssets(".js")+"\n", "<script>", 1)
	return strings.Replace(page, pageAssets(".css")+"\n", "", 1)
}

// Pages are core code but reach the network only through the kit, so the
// token handling and the 401 gate stay in one place.
func TestPluginPagesUseTheKit(t *testing.T) {
	js := pageAssets(".js")
	for _, banned := range []string{"fetch(", "XMLHttpRequest", "sessionStorage", "localStorage.getItem(\"homelab", "eval(", "new Function"} {
		if strings.Contains(js, banned) {
			t.Errorf("plugin page uses %s; use window.HI", banned)
		}
	}
}

func TestDashboardScopeAndBranding(t *testing.T) {
	page := coreBody(t)
	for _, feature := range []string{"panel-archive", "panel-network", "arcPoll", "loadUploadCaps", "netTimer", "tdarrTimer", "/seasongate", "/digest/send", `"/network`, `"/upload"`, `"/upload?`, "/taste", "/archive", "/tdarr"} {
		if strings.Contains(page, feature) {
			t.Errorf("removed feature remains: %s", feature)
		}
	}
	for _, want := range []string{`<title>homelab-inventory</title>`, `data-panel="ports"`, `/capabilities`, `homelab-inventory.operator-token`, `Promotion is disabled — set OPERATOR_TOKEN or use the signer CLI`, `Restarts are disabled`} {
		if !strings.Contains(page, want) {
			t.Errorf("missing dashboard contract: %s", want)
		}
	}
}

// Literal ids include both shell markup and markup created by retained drawers.
// This catches dangling listeners even when their removed feature is never opened.
func TestLiteralDOMReferencesResolve(t *testing.T) {
	page := body(t)
	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`id=["']([a-zA-Z0-9-]+)`).FindAllStringSubmatch(page, -1) {
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`\$\(["']([a-zA-Z0-9-]+)["']\)`).FindAllStringSubmatch(page, -1) {
		if !ids[m[1]] {
			t.Errorf("dangling DOM reference: %s", m[1])
		}
	}
}
