// Package catalog turns the port map into a home page: the list of things you
// actually run, with names you recognise and links that work.
//
// # Where the names come from
//
// Two sources, and the order matters.
//
// The table in services.go is a *seed*, not the source of truth. It exists so
// that a fresh install is not sixty anonymous tiles — it recognises the
// services most homelabs run and guesses a name, a category and a link. It is
// hand-maintained, which means it will always be somewhat out of date, and
// nothing depends on it being right.
//
// The Book is the source of truth. It lives in the store, it is edited from the
// dashboard, and a curated entry always beats the table. Adding a service you
// run means naming it once on the page — not editing Go and redeploying.
//
// This inversion is deliberate. A hand-maintained table that only its author
// can change is a maintenance burden disguised as a feature; the same table
// used as a first guess, with a human's answer stored over the top, is just a
// convenience that ages harmlessly.
//
// # Lifecycle
//
// The catalog is a reconciliation between what is listening right now and what
// the Book already knows:
//
//   - present, not yet curated -> "new". Surfaced as a prompt to name it, and
//     rendered meanwhile with whatever the seed table guessed.
//   - present and curated -> "active". The ordinary case.
//   - curated but not present -> "missing". Kept, and never deleted.
//
// **Nothing is ever removed automatically.** A service leaves the inventory
// when it is genuinely gone, and equally when a host missed one push, when
// Docker was mid-restart during collection, or when a collector errored. A
// catalog that pruned itself would quietly forget a real service because of a
// transient failure, and nobody would know it had happened. So disappearance is
// a question — kept, marked missing, with the date it was last seen — and
// deleting is a decision a person makes.
//
// Same division of labour as internal/portmap: structured fields out,
// formatting left to whatever displays them.
package catalog

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ficaa1/homelab-inventory/internal/icons"
	"github.com/ficaa1/homelab-inventory/internal/inventory"
)

// Status is where an entry sits in the lifecycle above.
type Status string

const (
	// StatusNew is listening now and never confirmed by a human.
	StatusNew Status = "new"
	// StatusActive is listening now and curated.
	StatusActive Status = "active"
	// StatusMissing was curated once and is not listening now. Kept, because a
	// missed push looks exactly like a decommissioned service.
	StatusMissing Status = "missing"
)

// Service is one thing you can click on.
type Service struct {
	// Key identifies the entry, and is what the curation endpoints take. It is
	// "host:port" — stable in practice, since a homelab service keeps its port
	// far more reliably than it keeps its container name.
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Category Category `json:"category"`
	Note     string   `json:"note,omitempty"`
	URL      string   `json:"url,omitempty"`
	// AltURLs are other ways to reach the same service — a Tailscale address
	// where the host has one.
	AltURLs []string `json:"alt_urls,omitempty"`

	Host    string              `json:"host"`
	Port    int                 `json:"port"`
	Owner   string              `json:"owner,omitempty"`
	Project string              `json:"compose_project,omitempty"`
	Kind    inventory.OwnerKind `json:"kind,omitempty"`
	Exposed bool                `json:"exposed"`

	// IconSlug names the icon to show, resolved from the display name or from
	// a human's override. The UI asks the server for it by slug; it is never a
	// URL, which is what keeps the icon endpoint from being a general-purpose
	// fetcher.
	IconSlug string `json:"icon_slug,omitempty"`

	Status Status `json:"status"`
	// Curated records that a human has confirmed this entry, which is what
	// stops it being offered as new again on the next poll.
	Curated bool `json:"curated"`
	// Recognised means the seed table had a guess. False is not a problem — it
	// just means the name came from the process rather than from the table.
	Recognised bool `json:"recognised"`
	// Suppressed entries are the infrastructure sockets kept off the home page
	// by default: ssh, databases, exporters. They are listed rather than
	// dropped, so any of them can be promoted to a tile from the UI.
	Suppressed bool `json:"suppressed,omitempty"`

	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
}

// Group is one rendered section.
type Group struct {
	Category Category  `json:"category"`
	Services []Service `json:"services"`
}

// Catalog is the whole home page, plus the two things that want a decision.
type Catalog struct {
	GeneratedAt time.Time `json:"generated_at"`
	Groups      []Group   `json:"groups"`
	// New is everything listening that nobody has confirmed yet. The UI turns
	// this into "3 new services — name them".
	New []Service `json:"new,omitempty"`
	// Missing is everything curated that has stopped listening. The UI turns
	// this into "still gone? forget it, or leave it".
	Missing []Service `json:"missing,omitempty"`
	// Suppressed is the infrastructure deliberately kept off the home page, so
	// it can be found and promoted rather than being invisible.
	Suppressed []Service `json:"suppressed,omitempty"`
}

// Stored is one curated entry as it is persisted.
//
// Deliberately small: it holds a human's decisions and the lifecycle dates, and
// nothing derived. The owner, the kind, whether it is exposed — all re-read
// from the inventory on every build, so the Book cannot go stale about facts.
// It only remembers opinions.
type Stored struct {
	Name     string   `json:"name,omitempty"`
	Category Category `json:"category,omitempty"`
	Note     string   `json:"note,omitempty"`
	URL      string   `json:"url,omitempty"`
	Hidden   bool     `json:"hidden,omitempty"`
	// Icon overrides the guessed slug. "-" means "no icon, use the initials",
	// which is a real preference and has to be distinguishable from "not set".
	Icon string `json:"icon,omitempty"`
	// Curated is set once a human has confirmed or edited the entry.
	Curated bool `json:"curated,omitempty"`
	// Owner is recorded only so a missing entry can still be described on the
	// page after it has stopped appearing in any inventory.
	Owner     string    `json:"owner,omitempty"`
	FirstSeen time.Time `json:"first_seen,omitempty"`
	LastSeen  time.Time `json:"last_seen,omitempty"`
}

// Book is the curated catalog, persisted between runs.
type Book struct {
	Entries   map[string]Stored `json:"entries"`
	UpdatedAt time.Time         `json:"updated_at,omitempty"`
}

// Key builds an entry key from a host and port.
func Key(host string, port int) string { return fmt.Sprintf("%s:%d", host, port) }

// SplitKey is the inverse, for validating a key that arrived over HTTP.
func SplitKey(key string) (host string, port int, ok bool) {
	i := strings.LastIndexByte(key, ':')
	if i <= 0 || i == len(key)-1 {
		return "", 0, false
	}
	n, err := fmt.Sscanf(key[i+1:], "%d", &port)
	if n != 1 || err != nil || port < 1 || port > 65535 {
		return "", 0, false
	}
	return key[:i], port, true
}

// staleAfter bounds how often a build rewrites the Book purely to refresh
// last-seen timestamps. Without it, every load of the home page would be a
// disk write; with it the dates stay accurate to within one push interval,
// which is all "last seen 3 days ago" needs.
const staleAfter = 15 * time.Minute

// Build reconciles what is listening against what the Book knows.
//
// It returns the page, the Book as it should now be stored, and whether that
// differs meaningfully from what was passed in — so a caller can persist only
// when something happened rather than on every page load.
func Build(invs []*inventory.Inventory, book Book, now time.Time) (*Catalog, Book, bool) {
	next := Book{Entries: map[string]Stored{}, UpdatedAt: book.UpdatedAt}
	for k, v := range book.Entries {
		next.Entries[k] = v
	}

	c := &Catalog{GeneratedAt: now.UTC(), Groups: []Group{}}
	byCat := map[Category][]Service{}
	seenKeys := map[string]bool{}
	changed := false

	for _, inv := range invs {
		if inv == nil || inv.Network == nil {
			continue
		}
		host := inv.Host.Hostname
		platform := detectPlatform(inv)
		addr := primaryAddress(inv)
		alt := tailscaleAddress(inv)

		// Classify the whole host first, because whether a socket deserves a
		// tile depends on its siblings: Plex answers on 32400, 32401 and 32600
		// and Portainer on 8000 and 9443, and only one of each is the one you
		// click. The extras are suppressed rather than dropped, so they stay in
		// the "not shown" list and can be promoted like anything else.
		var found []classified
		seenPorts := map[int]bool{}
		for _, l := range inv.Network.Listeners {
			if l.Protocol != "tcp" || l.Ephemeral || seenPorts[l.Port] {
				continue
			}
			seenPorts[l.Port] = true
			e, suppressed := classify(host, platform, l)
			found = append(found, classified{entry: e, suppressed: suppressed})
		}
		markDuplicates(found)

		for _, f := range found {
			e, suppressedByDefault := f.entry, f.suppressed
			key := Key(host, e.Port)
			seenKeys[key] = true
			e.Key = key
			e.Status = StatusNew

			stored, known := next.Entries[key]
			if known {
				// A curated opinion always beats the seed table's guess.
				if stored.Name != "" {
					e.Name = stored.Name
					e.Recognised = true
				}
				if stored.Category != "" {
					e.Category = stored.Category
				}
				if stored.Note != "" {
					e.Note = stored.Note
				}
				if stored.Icon != "" {
					e.IconSlug = stored.Icon
				}
				e.Curated = stored.Curated
				if stored.Curated {
					e.Status = StatusActive
				}
			}

			if e.IconSlug == "" {
				e.IconSlug = icons.SlugFor(e.Name)
			}
			if e.IconSlug == "-" {
				e.IconSlug = ""
			}

			e.URL = buildURL(addr, e.Port, e.scheme, e.path)
			if alt != "" {
				e.AltURLs = []string{buildURL(alt, e.Port, e.scheme, e.path)}
			}
			if known && stored.URL != "" {
				e.URL = stored.URL
				e.AltURLs = nil
			}

			// Suppressed is this package's default; Hidden is the human's
			// decision. A curated entry overrides the default in both
			// directions — that is how an exporter gets a tile if you want one,
			// and how a service you do not care about loses its tile without
			// being forgotten.
			hidden := suppressedByDefault
			if known && stored.Curated {
				hidden = stored.Hidden
			}
			e.Suppressed = hidden

			upd := stored
			upd.Owner = e.Owner
			if upd.FirstSeen.IsZero() {
				upd.FirstSeen = now
				changed = true
			}
			if now.Sub(upd.LastSeen) > staleAfter {
				upd.LastSeen = now
				changed = true
			}
			if !known {
				changed = true
			}
			next.Entries[key] = upd
			e.FirstSeen, e.LastSeen = upd.FirstSeen, now

			switch {
			case hidden:
				c.Suppressed = append(c.Suppressed, e.Service)
			case !e.Curated:
				c.New = append(c.New, e.Service)
				byCat[e.Category] = append(byCat[e.Category], e.Service)
			default:
				byCat[e.Category] = append(byCat[e.Category], e.Service)
			}
		}
	}

	// Anything curated that stopped listening. Never deleted here — see the
	// package comment: a transient collection failure must not silently erase a
	// service from the page.
	for key, stored := range next.Entries {
		if seenKeys[key] || !stored.Curated {
			continue
		}
		host, port, ok := SplitKey(key)
		if !ok {
			continue
		}
		c.Missing = append(c.Missing, Service{
			Key: key, Name: stored.Name, Category: stored.Category, Note: stored.Note,
			Host: host, Port: port, Owner: stored.Owner, URL: stored.URL,
			Status: StatusMissing, Curated: true, Recognised: stored.Name != "",
			FirstSeen: stored.FirstSeen, LastSeen: stored.LastSeen,
		})
	}

	for _, cat := range CategoryOrder {
		list := byCat[cat]
		if len(list) == 0 {
			continue
		}
		sortServices(list)
		c.Groups = append(c.Groups, Group{Category: cat, Services: list})
	}
	sortServices(c.New)
	sortServices(c.Suppressed)
	sort.Slice(c.Missing, func(i, j int) bool { return c.Missing[i].LastSeen.After(c.Missing[j].LastSeen) })

	if changed {
		next.UpdatedAt = now
	}
	return c, next, changed
}

func sortServices(list []Service) {
	sort.Slice(list, func(i, j int) bool {
		a, b := strings.ToLower(list[i].Name), strings.ToLower(list[j].Name)
		if a != b {
			return a < b
		}
		if list[i].Host != list[j].Host {
			return list[i].Host < list[j].Host
		}
		return list[i].Port < list[j].Port
	})
}

// Curate applies one human decision to the Book.
//
// Additive by design: a key that has never been seen can be curated in advance,
// and a key whose service is currently missing stays curated. The only thing
// that removes an entry is Forget.
func (b *Book) Curate(key string, s Stored, now time.Time) error {
	if _, _, ok := SplitKey(key); !ok {
		return fmt.Errorf("catalog: %q is not a valid service key (want host:port)", key)
	}
	if err := validateStored(s); err != nil {
		return err
	}
	if b.Entries == nil {
		b.Entries = map[string]Stored{}
	}
	prev := b.Entries[key]
	s.Curated = true
	s.Owner = prev.Owner
	s.FirstSeen = prev.FirstSeen
	if s.FirstSeen.IsZero() {
		s.FirstSeen = now
	}
	s.LastSeen = prev.LastSeen
	b.Entries[key] = s
	b.UpdatedAt = now
	return nil
}

// Forget removes an entry entirely, which is the only way anything leaves the
// Book. If the service is still listening it reappears as new on the next
// build, which is correct: forgetting is not hiding.
func (b *Book) Forget(key string, now time.Time) {
	if b.Entries == nil {
		return
	}
	delete(b.Entries, key)
	b.UpdatedAt = now
}

// AcceptAll marks every currently-listening, uncurated entry as curated with
// whatever the seed table guessed.
//
// This exists for the first run, where the alternative is naming thirty-three
// tiles by hand before the page is usable. Accepting a guess is still a
// decision — it just stops being one decision per service.
func (b *Book) AcceptAll(c *Catalog, now time.Time) int {
	if b.Entries == nil {
		b.Entries = map[string]Stored{}
	}
	n := 0
	for _, s := range c.New {
		prev := b.Entries[s.Key]
		prev.Name = s.Name
		prev.Category = s.Category
		if prev.Note == "" {
			prev.Note = s.Note
		}
		prev.Curated = true
		if prev.FirstSeen.IsZero() {
			prev.FirstSeen = now
		}
		b.Entries[s.Key] = prev
		n++
	}
	if n > 0 {
		b.UpdatedAt = now
	}
	return n
}

func validateStored(s Stored) error {
	if s.Icon != "" && s.Icon != "-" && icons.Normalise(s.Icon) != s.Icon {
		return fmt.Errorf("catalog: an icon must be a plain slug like %q, not a URL", "sonarr")
	}
	if len(s.Name) > 64 || len(s.Note) > 200 || len(s.URL) > 512 {
		return fmt.Errorf("catalog: name, note or URL is too long")
	}
	if s.Category != "" && !validCategory(s.Category) {
		return fmt.Errorf("catalog: %q is not a category", s.Category)
	}
	if s.URL == "" {
		return nil
	}
	// These values land in an href on a page served from the dashboard's own
	// origin, so a javascript: URL here would be stored XSS. Allowlist the two
	// schemes a link may use, rather than blocking the ones that look dangerous.
	u, err := url.Parse(s.URL)
	if err != nil {
		return fmt.Errorf("catalog: URL is not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("catalog: a URL must use http or https, not %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("catalog: URL has no host")
	}
	return nil
}

func validCategory(c Category) bool {
	for _, k := range CategoryOrder {
		if k == c {
			return true
		}
	}
	return false
}

// entry carries the presentation hints classify works out but that callers
// never need to see.
type entry struct {
	Service
	scheme string
	path   string
	// canonical is the port this service's tile should point at when it answers
	// on several. Zero means "no opinion", and the lowest port wins.
	canonical int
}

// classified is one socket plus whether it is infrastructure by default.
type classified struct {
	entry
	suppressed bool
}

// markDuplicates suppresses every socket of a service except the one worth
// clicking.
//
// Plex answers on 32400, 32401 and 32600; Portainer's UI is on 9443 while 8000
// is its edge agent; Tdarr's server and its UI are adjacent ports. Three Plex
// tiles is not a home page. The seed table names the canonical port where it
// knows one, and otherwise the lowest wins — which is right often enough and,
// more importantly, is stable, so the tile does not move between runs.
//
// The losers are suppressed rather than discarded: they show up in the "not
// shown" list and can be promoted, because occasionally the second port really
// is the one you want.
func markDuplicates(found []classified) {
	best := map[string]int{} // display name -> index of the winner so far
	for i, f := range found {
		if f.suppressed || f.Name == "" {
			continue
		}
		j, seen := best[f.Name]
		if !seen {
			best[f.Name] = i
			continue
		}
		if betterPort(f.entry, found[j].entry) {
			found[j].suppressed = true
			best[f.Name] = i
		} else {
			found[i].suppressed = true
		}
	}
}

func betterPort(a, prev entry) bool {
	if a.canonical != 0 || prev.canonical != 0 {
		aWins := a.Port == a.canonical
		pWins := prev.Port == prev.canonical
		if aWins != pWins {
			return aWins
		}
	}
	return a.Port < prev.Port
}

// classify names a listening socket from the seed table, and says whether it is
// infrastructure that should stay off the home page by default.
//
// It never refuses: everything listening gets an entry, because something
// invisible cannot be promoted. The second return is a default, not a verdict —
// a curated entry overrides it either way.
func classify(host, platform string, l inventory.Listener) (entry, bool) {
	owner, project := "", ""
	var kind inventory.OwnerKind
	if l.Owner != nil {
		owner, project, kind = l.Owner.Name, l.Owner.ComposeProject, l.Owner.Kind
	}
	if owner == "" {
		owner = l.Process
	}

	base := Service{
		Host: host, Port: l.Port, Owner: owner,
		Project: project, Kind: kind, Exposed: l.Exposed,
		Category: CatOther,
	}
	base.Name = prettify(owner)
	if base.Name == "" {
		base.Name = fmt.Sprintf("port %d", l.Port)
	}

	suppress := hiddenPorts[l.Port] || hiddenOwners[normalise(owner)]

	// A socket held by a VPN sidecar belongs to whatever is behind it.
	if namespaceHosts[normalise(owner)] {
		if k, ok := byPort[l.Port]; ok {
			return applyKnown(base, k), k.NoUI
		}
		base.Name = fmt.Sprintf("port %d (behind %s)", l.Port, owner)
		return entry{Service: base}, false
	}

	// What the host *is* can settle a name the process cannot: nginx on a
	// TrueNAS box is the TrueNAS UI, while nginx anywhere else is anyone's
	// guess. Checked before the global table so the specific answer wins.
	if owners, ok := platformOwners[platform]; ok {
		if k, ok := owners[normalise(owner)]; ok {
			return applyKnown(base, k), k.NoUI
		}
	}
	if k, ok := lookupName(owner, l); ok {
		return applyKnown(base, k), k.NoUI
	}
	if k, ok := byPort[l.Port]; ok && owner == "" {
		return applyKnown(base, k), k.NoUI
	}
	return entry{Service: base}, suppress
}

func applyKnown(base Service, k known) entry {
	base.Name = k.Display
	base.Category = k.Category
	base.Note = k.Note
	base.Recognised = true
	return entry{Service: base, scheme: k.Scheme, path: k.Path, canonical: k.Port}
}

// lookupName matches the owner, then the compose service, exactly and then by
// prefix — so "photoprism-photoprism-1" finds "photoprism" without needing its
// own line.
func lookupName(owner string, l inventory.Listener) (known, bool) {
	candidates := []string{owner}
	if l.Owner != nil {
		candidates = append(candidates, l.Owner.ComposeService, l.Owner.ComposeProject)
	}
	for _, c := range candidates {
		if n := normalise(c); n != "" {
			if k, ok := byName[n]; ok {
				return k, true
			}
		}
	}
	for _, c := range candidates {
		n := normalise(c)
		if n == "" {
			continue
		}
		for key, k := range byName {
			if strings.HasPrefix(n, key) || strings.HasPrefix(key, n) {
				return k, true
			}
		}
	}
	return known{}, false
}

// normalise strips the decoration compose and systemd add to a name:
// "photoprism-photoprism-1" and "AdGuardHome.service" both reduce to something
// the table can match.
func normalise(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".service")
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == '-' {
			if _, err := fmt.Sscanf(s[i+1:], "%d", new(int)); err == nil {
				s = s[:i]
			}
			break
		}
		if s[i] < '0' || s[i] > '9' {
			break
		}
	}
	return s
}

func prettify(s string) string {
	s = strings.TrimSuffix(s, ".service")
	s = strings.ReplaceAll(s, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func buildURL(addr string, port int, scheme, path string) string {
	if addr == "" {
		return ""
	}
	if scheme == "" {
		scheme = "http"
		if port == 443 || port == 8443 {
			scheme = "https"
		}
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, addr, port, path)
}

// detectPlatform identifies an appliance from what it runs, not from its
// os-release or its hostname.
//
// Both of the platforms that matter here lie about themselves in os-release:
// TrueNAS SCALE reports "Debian GNU/Linux 11 (bullseye)" and Proxmox reports
// "Debian GNU/Linux 12 (bookworm)", because both genuinely are Debian. So the
// marker is a daemon only that platform runs — middlewared is TrueNAS's entire
// control plane — and a hostname is never consulted, which keeps this
// meaningful on someone else's fleet.
func detectPlatform(inv *inventory.Inventory) string {
	if inv.Proxmox != nil && inv.Proxmox.Available {
		return "proxmox"
	}
	if inv.Systemd != nil {
		for _, u := range inv.Systemd.Units {
			if normalise(u.Name) == "middlewared" {
				return "truenas"
			}
		}
	}
	return ""
}

// primaryAddress picks the address a browser should use.
//
// A physical interface wins outright. Only if there is none does a bridge
// count, and then only one that is not Docker's: host-b's address lives on
// `vmbr0`, which is a bridge, while host-a has seventeen Docker bridges holding
// 172.x and 192.0.2.x addresses that would each look equally plausible to a
// naive "first private IPv4" rule.
func primaryAddress(inv *inventory.Inventory) string {
	var bridge string
	for _, i := range inv.Host.Interfaces {
		for _, a := range i.Addresses {
			ip := parseIPv4(a)
			if ip == "" {
				continue
			}
			switch i.Kind {
			case inventory.IfKindPhysical:
				return ip
			case inventory.IfKindBridge:
				if bridge == "" && !isDockerBridge(i.Name) {
					bridge = ip
				}
			}
		}
	}
	return bridge
}

// tailscaleAddress returns the host's tailnet address if it has one. This
// dashboard is read from a phone off the LAN more often than on it, so the
// alternative route is worth carrying.
func tailscaleAddress(inv *inventory.Inventory) string {
	for _, i := range inv.Host.Interfaces {
		if !strings.HasPrefix(i.Name, "tailscale") {
			continue
		}
		for _, a := range i.Addresses {
			if ip := parseIPv4(a); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func isDockerBridge(name string) bool {
	return strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "docker")
}

func parseIPv4(cidr string) string {
	s := cidr
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return ""
	}
	return ip.String()
}
