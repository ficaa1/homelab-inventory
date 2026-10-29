package catalog

import (
	"strings"
	"testing"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

func listener(port int, process, owner string, kind inventory.OwnerKind) inventory.Listener {
	l := inventory.Listener{Protocol: "tcp", Port: port, Process: process, Address: "0.0.0.0", Exposed: true}
	if owner != "" {
		l.Owner = &inventory.ListenerOwner{Name: owner, Kind: kind}
	}
	return l
}

func hostInv(name string, ifname string, addr string, ls ...inventory.Listener) *inventory.Inventory {
	return &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host: inventory.Host{
			Hostname: name,
			Interfaces: []inventory.Interface{
				{Name: ifname, Kind: inventory.IfKindPhysical, Addresses: []string{addr}},
			},
		},
		Network: &inventory.Network{Listeners: ls},
	}
}

// build is the common case: an empty Book, everything therefore new.
func build(invs ...*inventory.Inventory) *Catalog {
	c, _, _ := Build(invs, Book{}, time.Now())
	return c
}

// curated builds once, accepts every guess, and builds again — the state the
// page is in after the first "looks right".
func curated(invs ...*inventory.Inventory) (*Catalog, Book) {
	now := time.Now()
	c, book, _ := Build(invs, Book{}, now)
	book.AcceptAll(c, now)
	c2, book2, _ := Build(invs, book, now)
	return c2, book2
}

func find(c *Catalog, name string) (Service, bool) {
	for _, g := range c.Groups {
		for _, s := range g.Services {
			if s.Name == name {
				return s, true
			}
		}
	}
	return Service{}, false
}

func findIn(list []Service, name string) (Service, bool) {
	for _, s := range list {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

func TestBuildNamesAndLinksRealServices(t *testing.T) {
	t.Parallel()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(32400, "Plex Media Server", "plex", inventory.OwnerContainer),
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished),
		listener(2283, "docker-proxy", "immich_server", inventory.OwnerPublished),
		listener(18000, "docker-proxy", "paperless-ngx-webserver", inventory.OwnerPublished),
		listener(18001, "docker-proxy", "adguardhome", inventory.OwnerPublished),
		listener(18002, "docker-proxy", "owncloud_server", inventory.OwnerPublished),
	)
	c := build(inv)

	plex, ok := find(c, "Plex")
	if !ok {
		t.Fatal("no Plex tile")
	}
	// Plex's UI is at /web, which no amount of socket inspection reveals.
	if plex.URL != "http://192.0.2.10:32400/web" {
		t.Errorf("Plex URL = %q", plex.URL)
	}
	if immich, ok := find(c, "Immich"); !ok || immich.Category != CatPhotos {
		t.Errorf("Immich missing or miscategorised: %+v", immich)
	}
	if sonarr, ok := find(c, "Sonarr"); !ok || sonarr.Category != CatMedia {
		t.Errorf("Sonarr missing or miscategorised: %+v", sonarr)
	}
	for _, name := range []string{"Paperless-ngx", "AdGuard Home", "ownCloud"} {
		if _, ok := find(c, name); !ok {
			t.Errorf("missing product %s on a neutral host and custom port", name)
		}
	}

}

// The port map correctly attributes a VPN sidecar's sockets to the sidecar,
// because that is who holds them. A launcher wants the other answer.
func TestServicesBehindAVPNSidecarAreNamed(t *testing.T) {
	t.Parallel()
	c := build(hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8080, "docker-proxy", "gluetun", inventory.OwnerPublished),
		listener(9696, "docker-proxy", "gluetun", inventory.OwnerPublished),
	))
	if s, ok := find(c, "qBittorrent"); !ok || s.Port != 8080 {
		t.Errorf("qBittorrent not resolved from behind Gluetun: %+v", s)
	}
	if s, ok := find(c, "Prowlarr"); !ok || s.Port != 9696 {
		t.Errorf("Prowlarr not resolved from behind Gluetun: %+v", s)
	}
}

// TrueNAS and Proxmox both report themselves as plain Debian, so the platform
// has to be inferred from what they run.
func TestPlatformIsDetectedFromWhatRunsNotFromOSRelease(t *testing.T) {
	t.Parallel()
	tn := hostInv("host-c", "ens18", "192.0.2.12/24",
		listener(443, "nginx", "nginx.service", inventory.OwnerUnit))
	tn.Host.OS.Pretty = "Debian GNU/Linux 11 (bullseye)"
	tn.Systemd = &inventory.Systemd{
		Available: true,
		Units:     []inventory.Unit{{Name: "middlewared.service", Active: "active"}},
	}

	s, ok := find(build(tn), "TrueNAS")
	if !ok {
		t.Fatal("nginx on a TrueNAS box was not recognised as its UI")
	}
	if s.URL != "https://192.0.2.12:443" {
		t.Errorf("TrueNAS URL = %q, want the https UI", s.URL)
	}

	plain := hostInv("web", "eth0", "192.0.2.23/24",
		listener(443, "nginx", "nginx.service", inventory.OwnerUnit))
	if _, ok := find(build(plain), "TrueNAS"); ok {
		t.Error("plain nginx was labelled TrueNAS")
	}
}

// host-a has seventeen Docker bridges holding private addresses that a naive
// "first private IPv4" rule would happily pick.
func TestPrimaryAddressIgnoresDockerBridges(t *testing.T) {
	t.Parallel()
	inv := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host: inventory.Host{
			Hostname: "host-a",
			Interfaces: []inventory.Interface{
				{Name: "docker0", Kind: inventory.IfKindBridge, Addresses: []string{"172.17.0.1/16"}},
				{Name: "br-0ae78aacb13d", Kind: inventory.IfKindBridge, Addresses: []string{"192.0.2.28/20"}},
				{Name: "ens18", Kind: inventory.IfKindPhysical, Addresses: []string{"192.0.2.10/24"}},
				{Name: "tailscale0", Kind: inventory.IfKindTunnel, Addresses: []string{"192.0.2.27/32"}},
			},
		},
		Network: &inventory.Network{Listeners: []inventory.Listener{
			listener(3000, "grafana", "grafana", inventory.OwnerContainer),
		}},
	}
	s, ok := find(build(inv), "Grafana")
	if !ok {
		t.Fatal("no Grafana tile")
	}
	if s.URL != "http://192.0.2.10:3000" {
		t.Errorf("URL = %q, want the physical interface's address", s.URL)
	}
	if len(s.AltURLs) != 1 || !strings.Contains(s.AltURLs[0], "192.0.2.27") {
		t.Errorf("AltURLs = %v, want the tailnet address", s.AltURLs)
	}

	// host-b's address lives on vmbr0, which is a bridge but not Docker's.
	pve := &inventory.Inventory{
		SchemaVersion: inventory.SchemaVersion,
		Host: inventory.Host{Hostname: "host-b", Interfaces: []inventory.Interface{
			{Name: "vmbr0", Kind: inventory.IfKindBridge, Addresses: []string{"192.0.2.11/24"}},
		}},
		Network: &inventory.Network{Listeners: []inventory.Listener{
			listener(8006, "pveproxy worker", "pveproxy.service", inventory.OwnerUnit),
		}},
	}
	if s, ok := find(build(pve), "Proxmox VE"); !ok || s.URL != "https://192.0.2.11:8006" {
		t.Errorf("host-b tile = %+v, want https on vmbr0's address", s)
	}
}

// Infrastructure is suppressed by default, but listed — something invisible
// cannot be promoted.
func TestInfrastructureIsSuppressedButListed(t *testing.T) {
	t.Parallel()
	c := build(hostInv("host-c", "ens18", "192.0.2.12/24",
		listener(22, "sshd", "ssh.service", inventory.OwnerUnit),
		listener(5432, "postgres", "postgresql@15-main.service", inventory.OwnerUnit),
		// A systemd unit name arrives with its suffix; an exact lowercase
		// match against "pvedaemon" would miss "pvedaemon.service".
		listener(85, "pvedaemon worker", "pvedaemon.service", inventory.OwnerUnit),
	))
	for _, g := range c.Groups {
		if len(g.Services) > 0 {
			t.Errorf("category %s got tiles for pure infrastructure: %+v", g.Category, g.Services)
		}
	}
	if len(c.Suppressed) != 3 {
		t.Fatalf("Suppressed = %d entries, want 3 — they must be listed, not dropped", len(c.Suppressed))
	}
	if len(c.New) != 0 {
		t.Errorf("suppressed infrastructure was offered as new: %+v", c.New)
	}
}

// ---------------------------------------------------------------- lifecycle

// The whole point of the Book: a name given on the page beats the seed table,
// and survives a rebuild.
func TestCuratedNameBeatsTheSeedTable(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished))

	var book Book
	if err := book.Curate("host-a:8989", Stored{
		Name: "TV shows", Category: CatHome, URL: "https://sonarr.example.com",
	}, now); err != nil {
		t.Fatal(err)
	}

	c, _, _ := Build([]*inventory.Inventory{inv}, book, now)
	s, ok := find(c, "TV shows")
	if !ok {
		t.Fatalf("the curated name did not win; groups = %+v", c.Groups)
	}
	if s.Category != CatHome {
		t.Errorf("Category = %q, want the curated one", s.Category)
	}
	if s.URL != "https://sonarr.example.com" {
		t.Errorf("URL = %q, want the curated one", s.URL)
	}
	if len(s.AltURLs) != 0 {
		t.Errorf("AltURLs = %v; an explicit URL should replace the derived ones", s.AltURLs)
	}
	if s.Status != StatusActive || !s.Curated {
		t.Errorf("status = %q curated = %v, want active/true", s.Status, s.Curated)
	}
	if len(c.New) != 0 {
		t.Errorf("a curated service was still offered as new: %+v", c.New)
	}
}

// A service nobody has confirmed is offered for naming — once. After that it
// must stop nagging.
func TestNewServicesArePromptedThenStopBeingPrompted(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished),
		listener(7878, "docker-proxy", "radarr", inventory.OwnerPublished))

	c, book, changed := Build([]*inventory.Inventory{inv}, Book{}, now)
	if !changed {
		t.Error("first sight of two services was not worth persisting")
	}
	if len(c.New) != 2 {
		t.Fatalf("New = %d, want 2", len(c.New))
	}
	// They still render while unconfirmed — a prompt is not a blank page.
	if _, ok := find(c, "Sonarr"); !ok {
		t.Error("a new service was not rendered while awaiting a name")
	}

	if n := book.AcceptAll(c, now); n != 2 {
		t.Fatalf("AcceptAll accepted %d, want 2", n)
	}
	c2, _, _ := Build([]*inventory.Inventory{inv}, book, now)
	if len(c2.New) != 0 {
		t.Errorf("still prompting after accept: %+v", c2.New)
	}
	if _, ok := find(c2, "Sonarr"); !ok {
		t.Error("accepting the guess lost the tile")
	}
}

// The rule that matters most: a service that stops listening is a question,
// never an action. A missed push and a decommissioned service look identical.
func TestDisappearedServiceIsKeptAndFlaggedNotDeleted(t *testing.T) {
	t.Parallel()
	now := time.Now()
	both := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished),
		listener(7878, "docker-proxy", "radarr", inventory.OwnerPublished))
	_, book := curated(both)

	// The next collection catches Radarr mid-restart, so it is simply absent.
	onlyOne := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished))
	c, book2, _ := Build([]*inventory.Inventory{onlyOne}, book, now.Add(time.Hour))

	if len(c.Missing) != 1 || c.Missing[0].Name != "Radarr" {
		t.Fatalf("Missing = %+v, want just Radarr", c.Missing)
	}
	if _, ok := book2.Entries["host-a:7878"]; !ok {
		t.Fatal("the entry was deleted; a transient absence must never remove one")
	}
	if _, ok := find(c, "Radarr"); ok {
		t.Error("a missing service still holds a tile in the main grid")
	}

	// It comes back. It must return to normal, not reappear as new.
	c3, _, _ := Build([]*inventory.Inventory{both}, book2, now.Add(2*time.Hour))
	if len(c3.Missing) != 0 {
		t.Errorf("Missing = %+v after it returned", c3.Missing)
	}
	if len(c3.New) != 0 {
		t.Errorf("a returning service was offered as new again: %+v", c3.New)
	}
	s, ok := find(c3, "Radarr")
	if !ok || s.Status != StatusActive {
		t.Errorf("Radarr = %+v, want an active tile", s)
	}
}

// Forget is the only thing that removes an entry, and it is not the same as
// hiding: something still listening comes straight back as new.
func TestForgetIsTheOnlyDeletion(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished))
	_, book := curated(inv)

	book.Forget("host-a:8989", now)
	if len(book.Entries) != 0 {
		t.Fatalf("Forget left %d entries", len(book.Entries))
	}
	c, _, _ := Build([]*inventory.Inventory{inv}, book, now)
	if len(c.New) != 1 {
		t.Errorf("forgetting something still listening should re-offer it as new, got %+v", c.New)
	}
}

// Hiding keeps the entry and the decision; the service stays known, it just
// stops occupying the home page.
func TestHidingKeepsTheEntry(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished))

	var book Book
	if err := book.Curate("host-a:8989", Stored{Name: "Sonarr", Category: CatMedia, Hidden: true}, now); err != nil {
		t.Fatal(err)
	}
	c, _, _ := Build([]*inventory.Inventory{inv}, book, now)
	if _, ok := find(c, "Sonarr"); ok {
		t.Error("a hidden service still has a tile")
	}
	if _, ok := findIn(c.Suppressed, "Sonarr"); !ok {
		t.Error("a hidden service vanished entirely; it must stay promotable")
	}
	if len(c.New) != 0 {
		t.Errorf("a hidden service was offered as new: %+v", c.New)
	}
}

// Curating an infrastructure socket promotes it: the suppression list is a
// default, not a verdict.
func TestSuppressedInfrastructureCanBePromoted(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-e", "eth0", "192.0.2.30/24",
		listener(5432, "postgres", "postgresql@15-main.service", inventory.OwnerUnit))

	var book Book
	if err := book.Curate("host-e:5432", Stored{
		Name: "Paperless database", Category: CatInfra, Hidden: false,
	}, now); err != nil {
		t.Fatal(err)
	}
	c, _, _ := Build([]*inventory.Inventory{inv}, book, now)
	if _, ok := find(c, "Paperless database"); !ok {
		t.Errorf("a promoted infrastructure socket did not get a tile; groups = %+v", c.Groups)
	}
}

// Steady state must not write to disk on every page load.
func TestSteadyStateDoesNotRewriteTheBook(t *testing.T) {
	t.Parallel()
	now := time.Now()
	inv := hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(8989, "docker-proxy", "sonarr", inventory.OwnerPublished))

	_, book, changed := Build([]*inventory.Inventory{inv}, Book{}, now)
	if !changed {
		t.Fatal("the first build should persist")
	}
	// A poll a minute later changes nothing.
	_, _, changed = Build([]*inventory.Inventory{inv}, book, now.Add(time.Minute))
	if changed {
		t.Error("a steady-state poll wanted to write; the home page polls every 60s")
	}
	// Much later, the last-seen date is worth refreshing.
	_, _, changed = Build([]*inventory.Inventory{inv}, book, now.Add(time.Hour))
	if !changed {
		t.Error("last-seen never gets refreshed; 'missing since' would be wrong")
	}
}

// These values land in an href on a page served from the dashboard's own
// origin, so a javascript: URL here would be stored XSS.
func TestCurateRejectsDangerousURLs(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, bad := range []string{
		"javascript:alert(document.cookie)",
		"JavaScript:alert(1)",
		"data:text/html,<script>alert(1)</script>",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
	} {
		var b Book
		if err := b.Curate("host-a:1", Stored{URL: bad}, now); err == nil {
			t.Errorf("Curate accepted %q", bad)
		}
	}
	var b Book
	if err := b.Curate("host-a:8989", Stored{URL: "https://sonarr.example.com/path"}, now); err != nil {
		t.Errorf("Curate rejected a good URL: %v", err)
	}
	if err := b.Curate("nonsense", Stored{Name: "x"}, now); err == nil {
		t.Error("Curate accepted a malformed key")
	}
	if err := b.Curate("host-a:8989", Stored{Category: "Nonsense"}, now); err == nil {
		t.Error("Curate accepted an unknown category")
	}
}

func TestSplitKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key  string
		host string
		port int
		ok   bool
	}{
		{"host-a:8989", "host-a", 8989, true},
		{"host-e:8000", "host-e", 8000, true},
		{"host-a", "", 0, false},
		{"host-a:", "", 0, false},
		{":80", "", 0, false},
		{"host-a:0", "", 0, false},
		{"host-a:70000", "", 0, false},
		{"host-a:abc", "", 0, false},
	} {
		host, port, ok := SplitKey(tc.key)
		if ok != tc.ok || (ok && (host != tc.host || port != tc.port)) {
			t.Errorf("SplitKey(%q) = %q, %d, %v; want %q, %d, %v",
				tc.key, host, port, ok, tc.host, tc.port, tc.ok)
		}
	}
}

// Three Plex tiles is not a home page. The extras are suppressed rather than
// dropped, because occasionally the second port really is the one you want.
func TestDuplicatePortsOfOneServiceAreSuppressedNotDropped(t *testing.T) {
	t.Parallel()
	c := build(hostInv("host-a", "ens18", "192.0.2.10/24",
		listener(32400, "Plex Media Server", "plex", inventory.OwnerContainer),
		listener(32401, "Plex Media Server", "plex", inventory.OwnerContainer),
		listener(32600, "Plex Media Server", "plex", inventory.OwnerContainer),
		// Portainer's UI is on 9443; 8000 is its edge agent, and the seed table
		// names 9443 as canonical so the lower port must not win.
		listener(8000, "docker-proxy", "portainer", inventory.OwnerPublished),
		listener(9443, "docker-proxy", "portainer", inventory.OwnerPublished),
	))

	var plex, portainer []Service
	for _, g := range c.Groups {
		for _, s := range g.Services {
			switch s.Name {
			case "Plex":
				plex = append(plex, s)
			case "Portainer":
				portainer = append(portainer, s)
			}
		}
	}
	if len(plex) != 1 || plex[0].Port != 32400 {
		t.Errorf("Plex tiles = %+v, want exactly one on 32400", plex)
	}
	if len(portainer) != 1 || portainer[0].Port != 9443 {
		t.Errorf("Portainer tiles = %+v, want exactly one on 9443 (the UI, not the agent)", portainer)
	}
	// The four losers are still listed, so any of them can be promoted.
	if len(c.Suppressed) != 3 {
		t.Errorf("Suppressed = %d, want the 3 duplicate ports kept and promotable", len(c.Suppressed))
	}
	for _, s := range c.Suppressed {
		if s.Key == "" {
			t.Error("a suppressed duplicate has no key, so it cannot be promoted")
		}
	}
}
