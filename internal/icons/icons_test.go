package icons

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type memState struct{ data map[string][]byte }

func (m *memState) ReadState(name string, v any) error {
	b, ok := m.data[name]
	if !ok {
		return nil
	}
	return json.Unmarshal(b, v)
}

func (m *memState) WriteState(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[name] = b
	return nil
}

// Normalise is the only thing between a URL path parameter and an address this
// server will fetch. If it lets anything through, the icon endpoint becomes a
// general-purpose fetcher pointed at the internal network.
func TestNormaliseCannotEscapeIntoAURL(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"../../etc/passwd",
		"http://169.254.169.254/latest/meta-data/",
		"plex/../../../root",
		"a?b=c#d",
		"a%2f..%2fb",
		"../..",
		"//evil.example.com/x",
		"plex\n.svg",
	} {
		got := Normalise(in)
		for _, bad := range []string{"/", ".", ":", "?", "#", "%", "\\", "\n", " "} {
			if strings.Contains(got, bad) {
				t.Errorf("Normalise(%q) = %q, which still contains %q", in, got, bad)
			}
		}
	}
	if got := Normalise("AdGuard Home"); got != "adguard-home" {
		t.Errorf("Normalise = %q, want adguard-home", got)
	}
	if got := Normalise("  Calibre--Web  "); got != "calibre-web" {
		t.Errorf("Normalise = %q, want calibre-web", got)
	}
	if Normalise("!!!") != "" {
		t.Error("a slug of pure punctuation should come out empty, not partial")
	}
}

// Serving a fetched SVG from our own origin is the risky part. The <img> tag
// and the CSP are the first two defences; this is the third.
func TestRefusesAnSVGContainingScript(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`,
		`<svg><a href="javascript:alert(1)">x</a></svg>`,
		`<svg><foreignObject><body onload="alert(1)"/></foreignObject></svg>`,
		`<svg><iframe src="x"></iframe></svg>`,
		`not an svg at all`,
	} {
		if err := checkSVG([]byte(body)); err == nil {
			t.Errorf("accepted a hostile or non-SVG body: %q", body)
		}
	}
	if err := checkSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0"/></svg>`)); err != nil {
		t.Errorf("rejected a plain SVG: %v", err)
	}
}

// Fetching is off by default because it tells a third party which services run
// here. With it off, the cache must be inert rather than quietly reaching out.
func TestFetchingIsOffByDefault(t *testing.T) {
	t.Parallel()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	}))
	defer srv.Close()

	c := NewCache(&memState{}, false)
	c.Client = srv.Client()
	if _, _, ok := c.Get(context.Background(), "plex"); ok {
		t.Error("returned an icon with fetching disabled")
	}
	if hits != 0 {
		t.Errorf("made %d outbound requests with fetching disabled", hits)
	}
}

func TestSlugForKnownServices(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"Sonarr":         "sonarr",
		"Proxmox VE":     "proxmox",
		"TrueNAS":        "truenas",
		"AdGuard Home":   "adguard-home",
		"Home Assistant": "home-assistant",
		"Uptime Kuma":    "uptime-kuma",
		"Calibre-Web":    "calibre-web",
		"Homelab API":    "",
	} {
		if got := SlugFor(name); got != want {
			t.Errorf("SlugFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInitials(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"Plex":           "Pl",
		"Home Assistant": "HA",
		"qBittorrent":    "QB",
		"copyparty":      "Co",
		"X":              "X",
		"":               "?",
	} {
		if got := Initials(name); got != want {
			t.Errorf("Initials(%q) = %q, want %q", name, got, want)
		}
	}
}
