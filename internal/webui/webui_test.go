package webui

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func body(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /: status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// No CDN, no font fetch, no build step. This is what keeps the FROM scratch
// image possible — there is no filesystem in it to put assets on — and, more
// usefully, means the dashboard still works on a homelab that cannot reach the
// internet, which is exactly the situation you are in when something is broken
// enough to go looking.
//
// The check targets things that *load*, rather than any occurrence of a scheme.
// The launcher renders links to your own services — href="http://192.0.2.10:8989"
// — at runtime from fetched data, so "the page must never contain the string
// http://" stopped being the invariant the moment it existed. What must stay
// true is that nothing is fetched from another origin to render the page.
func TestSelfContained(t *testing.T) {
	t.Parallel()
	page := body(t)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	policy := rec.Header().Get("Content-Security-Policy")
	for _, directive := range []string{"connect-src 'self'", "img-src 'self' data:", "frame-ancestors 'none'", "default-src 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Errorf("CSP missing %s", directive)
		}
	}
	for _, tag := range []string{"script", "style"} {
		blocks := regexp.MustCompile("(?s)<"+tag+">(.*?)</"+tag+">").FindAllStringSubmatch(page, -1)
		if len(blocks) != 1 {
			t.Fatalf("expected one %s block", tag)
		}
		sum := sha256.Sum256([]byte(blocks[0][1]))
		if !strings.Contains(policy, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'") {
			t.Errorf("CSP does not authorize %s", tag)
		}
	}

	for _, bad := range []string{
		`src="http`, `src='http`, `src=http`,
		`<script src`, `<link rel="stylesheet"`, `<link rel='stylesheet'`,
		"@import", "url(http", "//cdn", "fonts.googleapis", "unpkg.com", "jsdelivr",
		`fetch("http`, `fetch('http`,
	} {
		if strings.Contains(page, bad) {
			t.Errorf("dashboard loads something external (%q); it must be self-contained", bad)
		}
	}
	// Every fetch the page makes must be same-origin, i.e. a rooted path.
	if !strings.Contains(page, `fetch(path, {`) {
		t.Error("the single same-origin fetch helper is gone; check nothing fetches cross-origin")
	}
}

// The ports view used to render a warning pill reading "yes" under a column
// headed "Exposed" and never say what it was claiming, which made it look like
// an unexplained scold. The definition is the feature; assert it is still
// there and still says the thing that matters.
func TestExposedIsExplained(t *testing.T) {
	t.Parallel()
	page := body(t)
	for _, want := range []string{
		"wildcard address",
		"0.0.0.0",
		"loopback",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the definition of \"exposed\" no longer mentions %q", want)
		}
	}
	// The kernel ephemeral range is the other thing a reader cannot be
	// expected to infer from a colour. Asserted on the concept rather than one
	// phrasing, so trimming the prose does not fail this for the wrong reason —
	// what must not happen is the explanation disappearing.
	for _, want := range []string{"kernel's dynamic", "claimable"} {
		if !strings.Contains(page, want) {
			t.Errorf("the kernel ephemeral range explanation no longer mentions %q", want)
		}
	}
}

// Every one of these is fetched by the page; a rename on the server side that
// misses the dashboard should fail here rather than in a browser.
func TestFetchesTheEndpointsItNeeds(t *testing.T) {
	t.Parallel()
	page := body(t)
	for _, path := range []string{`api("/overview")`, `api("/alerts")`, `api("/hosts")`, `api("/ports")`,
		`"/history"`, `api("/settings")`, `"/notify/test"`,
		`api("/actions/stacks?host="`, `"/packages"`, `"/changes?limit=10"`, `api("/actions/log?limit=20")`, `"/actions/preview"`, `"/actions/execute"`,
		`api("/agent/status")`, `('/agent/offers'`, `"/storage/runs/prune"`, `"/firewall-plan"`} {
		if !strings.Contains(page, path) {
			t.Errorf("the dashboard no longer calls %s", path)
		}
	}
}

func TestHandlerServesOnlyTheRoot(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/nope", "/index.html", "/../etc/passwd"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s: status = %d, want 404", path, rec.Code)
		}
	}
}

// The dashboard is one file with inline script, so an id is effectively a
// global variable. Reusing one is silent: $("home") returns whichever element
// the parser saw first, and writing to it destroys the other. That happened —
// the launcher's container collided with the header's home button and rendered
// itself into the page header, deleting it.
func TestElementIDsAreUnique(t *testing.T) {
	t.Parallel()
	page := body(t)

	// Only attributes in the static markup, not the ids the script builds into
	// strings, which is why this looks for id=" at a tag position.
	re := regexp.MustCompile(`<[a-zA-Z][^>]*\sid="([^"]+)"`)
	seen := map[string]int{}
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		seen[m[1]]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %q appears %d times in the static markup; ids are globals here", id, n)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only found %d ids, the regex is probably not matching", len(seen))
	}
}

// Installable and openable offline. The paths matter: a service worker may
// only control pages at or below its own path, so it has to be served from the
// root rather than tucked into a subdirectory.
func TestPWAAssetsAreServed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ path, contentType string }{
		{"/manifest.webmanifest", "application/manifest+json"},
		{"/sw.js", "text/javascript"},
		{"/icon.svg", "image/svg+xml"},
	} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200", tc.path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.contentType) {
			t.Errorf("GET %s: Content-Type = %q, want %q", tc.path, ct, tc.contentType)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s: empty body", tc.path)
		}
	}
}

// The worker must never cache an API response. A stale alert list is worse
// than no alert list: it would show a green page for a fleet that is on fire.
func TestServiceWorkerCachesOnlyTheShell(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/sw.js", nil))
	sw := rec.Body.String()

	if !strings.Contains(sw, "SHELL.includes(url.pathname)") {
		t.Error("the worker no longer restricts caching to the shell")
	}
	for _, endpoint := range []string{"/alerts", "/hosts", "/ports", "/services", "/archive", "/overview"} {
		if strings.Contains(sw, "'"+endpoint+"'") || strings.Contains(sw, `"`+endpoint+`"`) {
			t.Errorf("the worker references the API endpoint %q; responses must never be cached", endpoint)
		}
	}
	// Network first, or a deploy leaves everyone on the old shell.
	if !strings.Contains(sw, "fetch(req)") {
		t.Error("the worker is not network-first")
	}
}

// The manifest has to be same-origin and self-contained like everything else.
func TestManifestReferencesNothingExternal(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/manifest.webmanifest", nil))
	body := rec.Body.String()
	for _, bad := range []string{"http://", "https://"} {
		if strings.Contains(body, bad) {
			t.Errorf("manifest references something external (%q)", bad)
		}
	}
	if !strings.Contains(body, `"display": "standalone"`) {
		t.Error("the manifest does not ask to open as an app")
	}
}

// The admin panel's whole job is to explain why notifications are quiet, so
// the page must state the arithmetic rather than only echoing the settings.
// A page that renders the floor and not its consequence would look correct
// and reproduce the confusion this was built to end.
func TestAdminExplainsWhyNotificationsAreQuiet(t *testing.T) {
	t.Parallel()
	page := body(t)

	// The verdict must be derived from the coverage counts, not hardcoded.
	for _, want := range []string{"at_or_above_floor", "coverage", "total_alerts"} {
		if !strings.Contains(page, want) {
			t.Errorf("the admin panel does not read %q, so it cannot explain the silence", want)
		}
	}
	// Transitions, not state, is the thing that surprises people most.
	if !strings.Contains(page, "change") {
		t.Error("the panel never says notifications fire on changes rather than on state")
	}
	// A delivery that apprise accepted is not a delivery that arrived, and
	// conflating them is how a wrong key looks like a working notifier.
	if !strings.Contains(page, "apprise accepted") {
		t.Error("the panel presents an accepted send as a delivered one")
	}
}

// The apprise URL is settable from this page, so the page must never render
// one back into the DOM.
func TestAdminNeverRendersTheAppriseURL(t *testing.T) {
	t.Parallel()
	page := body(t)
	if strings.Contains(page, "s.apprise_url") || strings.Contains(page, "settings.apprise_url") {
		t.Error("the page reads an apprise_url field; the API deliberately never returns one")
	}
	if !strings.Contains(page, "apprise_hint") {
		t.Error("the page does not use the redacted hint, which is the only form it should show")
	}
}

// Icons are an inline sprite referenced by id, so a typo in an icon name
// renders nothing and fails silently. Every literal reference, in the markup or
// through ic("name") in the script, must resolve to a <symbol> on the page.
func TestEveryIconResolves(t *testing.T) {
	t.Parallel()
	page := body(t)
	have := map[string]bool{}
	for _, m := range regexp.MustCompile(`<symbol id="i-([a-z0-9-]+)"`).FindAllStringSubmatch(page, -1) {
		have[m[1]] = true
	}
	if len(have) < 10 {
		t.Fatalf("found %d icon symbols; the inline sprite is missing", len(have))
	}
	used := regexp.MustCompile(`href="#i-([a-z0-9-]+)"|\bic\("([a-z0-9-]+)"`).FindAllStringSubmatch(page, -1)
	if len(used) == 0 {
		t.Fatal("no icon references found")
	}
	// Host sections and overview cards take their icon as the second argument.
	for _, m := range regexp.MustCompile(`\b(?:sect|opsCard)\(["'][^"']+["'],\s*["']([a-z0-9-]+)["']`).FindAllStringSubmatch(page, -1) {
		if !have[m[1]] {
			t.Errorf("section icon %q has no symbol", m[1])
		}
	}
	for _, m := range used {
		name := m[1] + m[2]
		if !have[name] {
			t.Errorf("icon %q is referenced but has no <symbol id=\"i-%s\">", name, name)
		}
	}
}

// The phone layout puts the navigation in a fixed bar at the bottom of the
// screen, where an iPhone's home indicator sits. Without viewport-fit=cover
// and the safe-area inset the bar's labels end up under it.
func TestPhoneNavRespectsSafeArea(t *testing.T) {
	t.Parallel()
	page := body(t)
	for _, want := range []string{"viewport-fit=cover", "env(safe-area-inset-bottom", "@media (max-width: 640px)"} {
		if !strings.Contains(page, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

// Live overview data must stay behind the API, never an embedded demo report.
func TestHomeHasNoEmbeddedOverviewData(t *testing.T) {
	t.Parallel()
	page := body(t)
	for _, field := range []string{"generated_at", "hosts_reporting", "units_failed", "bandwidth_kbps"} {
		if regexp.MustCompile(`["']` + field + `["']\s*:`).MatchString(page) {
			t.Errorf("production page embeds an overview payload field %q", field)
		}
	}
}

// Motion is decoration: a reader who asked the OS for less of it gets none,
// in the stylesheet and in the script's Web Animations alike.
func TestMotionHonoursReducedMotion(t *testing.T) {
	t.Parallel()
	page := body(t)
	css := regexp.MustCompile(`@media \(prefers-reduced-motion: reduce\) \{\s*\*, \*::before, \*::after \{ animation: none !important; transition: none !important; \}`)
	if !css.MatchString(page) {
		t.Error("page has no global reduced-motion override")
	}
	if !strings.Contains(page, `matchMedia("(prefers-reduced-motion: reduce)")`) {
		t.Error("script animations do not check prefers-reduced-motion")
	}
}

// Removed asset paths must continue to return 404.
func TestRemovedAssetsAreNotServed(t *testing.T) {
	for _, path := range []string{"/vendor/", "/vendor/marked.min.js", "/vendor/marked-18.1.0.umd.js", "/upload/preview/file"} {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d", path, w.Code)
		}
	}
}
