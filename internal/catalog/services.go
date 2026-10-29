package catalog

// This file is the one hand-maintained table in the project, and it is
// deliberately data rather than code: adding a service is a line here, not a
// change to any logic.
//
// Everything else derives what it knows from the machines. This cannot: no
// amount of inspecting a socket tells you that `gerbil` is the thing you think
// of as "the reverse proxy", or that Plex's UI is at /web rather than /. A
// launcher that says "gerbil" and "immich_server" is an inventory, not a home
// page, so the friendly names are written down.

// Category groups services the way a person thinks about them, not the way
// they are deployed. Order here is the order they render in.
type Category string

const (
	CatMedia      Category = "Media"
	CatPhotos     Category = "Photos"
	CatFiles      Category = "Files & documents"
	CatHome       Category = "Home"
	CatNetwork    Category = "Network"
	CatMonitoring Category = "Monitoring"
	CatInfra      Category = "Infrastructure"
	CatOther      Category = "Other"
)

// CategoryOrder is the default launcher category order.
var CategoryOrder = []Category{
	CatMedia, CatPhotos, CatFiles, CatHome, CatNetwork, CatMonitoring, CatInfra, CatOther,
}

// known describes one recognisable service.
type known struct {
	Display  string
	Category Category
	// Path is appended to the URL when the UI does not live at the root.
	Path string
	// Scheme overrides the default (http, or https on a port known to be TLS).
	Scheme string
	// Port is the canonical port for this service, when it has one. It only
	// matters for de-duplication: Plex answers on 32400, 32401 and 32600, and
	// Portainer on both 8000 and 9443, but each is one tile and this says
	// which port that tile should point at.
	Port int
	// NoUI marks something that listens but has no web interface worth a tile:
	// exporters, sidecars, agents. They stay visible in the ports view, which
	// is the place for "what holds this socket"; a launcher tile that leads
	// nowhere is just clutter.
	NoUI bool
	// Note is a one-line description shown under the tile.
	Note string
}

// byName matches a container name, compose service, or systemd unit, lowercased
// and with a trailing "-1"/".service" already stripped. Keys are matched exactly
// first, then as a prefix, so "photoprism-photoprism-1" finds "photoprism".
var byName = map[string]known{
	// ---------------------------------------------------------------- media
	"plex":            {Port: 32400, Display: "Plex", Category: CatMedia, Path: "/web", Note: "Media server"},
	"tautulli":        {Display: "Tautulli", Category: CatMedia, Note: "Plex statistics"},
	"sonarr":          {Display: "Sonarr", Category: CatMedia, Note: "TV"},
	"radarr":          {Display: "Radarr", Category: CatMedia, Note: "Films"},
	"bazarr":          {Display: "Bazarr", Category: CatMedia, Note: "Subtitles"},
	"prowlarr":        {Display: "Prowlarr", Category: CatMedia, Note: "Indexers"},
	"overseerr":       {Display: "Overseerr", Category: CatMedia, Note: "Requests"},
	"tdarr":           {Port: 8265, Display: "Tdarr", Category: CatMedia, Note: "Transcoding"},
	"qbittorrent":     {Display: "qBittorrent", Category: CatMedia, Note: "Torrents"},
	"deluge":          {Display: "Deluge", Category: CatMedia, Note: "Torrents"},
	"jackett":         {Display: "Jackett", Category: CatMedia, Note: "Indexers"},
	"calibre":         {Port: 8084, Display: "Calibre", Category: CatMedia, Note: "Ebook library"},
	"calibre-web":     {Display: "Calibre-Web", Category: CatMedia, Note: "Ebook reader"},
	"muse":            {Display: "Muse", Category: CatMedia, NoUI: true, Note: "Discord music bot"},
	"isponsorblocktv": {Display: "iSponsorBlockTV", Category: CatMedia, NoUI: true},

	// --------------------------------------------------------------- photos
	"immich_server": {Display: "Immich", Category: CatPhotos, Note: "Photos"},
	"immich-server": {Display: "Immich", Category: CatPhotos, Note: "Photos"},
	"photoprism":    {Display: "PhotoPrism", Category: CatPhotos, Note: "Photos"},

	// ---------------------------------------------------------------- files
	"owncloud_server":         {Display: "ownCloud", Category: CatFiles, Note: "File sync"},
	"paperless-webserver":     {Display: "Paperless-ngx", Category: CatFiles, Note: "Document archive"},
	"paperless-ngx":           {Display: "Paperless-ngx", Category: CatFiles, Note: "Document archive"},
	"paperless-ngx-webserver": {Display: "Paperless-ngx", Category: CatFiles, Note: "Document archive"},
	"paperless":               {Display: "Paperless-ngx", Category: CatFiles, Note: "Document archive"},
	"copyparty":               {Display: "copyparty", Category: CatFiles, Note: "File drop"},
	"hoarder":                 {Display: "Hoarder", Category: CatFiles, Note: "Bookmarks"},
	"karakeep":                {Display: "Karakeep", Category: CatFiles, Note: "Bookmarks"},
	"nextcloud":               {Display: "Nextcloud", Category: CatFiles, Note: "File sync"},

	// ----------------------------------------------------------------- home
	"homeassistant": {Display: "Home Assistant", Category: CatHome, Note: "Home automation"},
	"mealie":        {Display: "Mealie", Category: CatHome, Note: "Recipes"},

	// -------------------------------------------------------------- network
	"adguardhome":  {Display: "AdGuard Home", Category: CatNetwork, Note: "DNS and ad blocking"},
	"pangolin":     {Port: 443, Display: "Pangolin", Category: CatNetwork, Note: "Tunnel and reverse proxy"},
	"gerbil":       {Port: 443, Display: "Pangolin", Category: CatNetwork, Note: "Tunnel and reverse proxy"},
	"traefik":      {Display: "Traefik", Category: CatNetwork, NoUI: true, Note: "Reverse proxy"},
	"gluetun":      {Display: "Gluetun", Category: CatNetwork, NoUI: true, Note: "VPN sidecar for other containers"},
	"flaresolverr": {Display: "FlareSolverr", Category: CatNetwork, NoUI: true},
	"tailscaled":   {Display: "Tailscale", Category: CatNetwork, NoUI: true},

	// ----------------------------------------------------------- monitoring
	"grafana":       {Display: "Grafana", Category: CatMonitoring, Note: "Dashboards"},
	"uptime-kuma":   {Display: "Uptime Kuma", Category: CatMonitoring, Note: "Uptime checks"},
	"netdata":       {Port: 19999, Display: "Netdata", Category: CatMonitoring, Note: "Live host metrics"},
	"dozzle":        {Display: "Dozzle", Category: CatMonitoring, Note: "Container logs"},
	"speedtest":     {Display: "Speedtest", Category: CatMonitoring, Note: "Line speed history"},
	"cadvisor":      {Display: "cAdvisor", Category: CatMonitoring, NoUI: true},
	"node_exporter": {Display: "Node exporter", Category: CatMonitoring, NoUI: true},

	// --------------------------------------------------------------- infra
	"portainer":   {Port: 9443, Display: "Portainer", Category: CatInfra, Scheme: "https", Note: "Container management"},
	"pveproxy":    {Port: 8006, Display: "Proxmox VE", Category: CatInfra, Scheme: "https", Note: "Hypervisor"},
	"middlewared": {Display: "TrueNAS", Category: CatInfra, Note: "Storage appliance"},
	"apprise":     {Display: "Apprise", Category: CatInfra, NoUI: true, Note: "Notification relay"},
	"homelab-api": {Display: "Homelab API", Category: CatInfra, Note: "This dashboard"},
	"api":         {Display: "Homelab API", Category: CatInfra, Note: "This dashboard"},
	"watchtower":  {Display: "Watchtower", Category: CatInfra, NoUI: true, Note: "Automatic updates"},
	"spiceproxy":  {Display: "SPICE proxy", Category: CatInfra, NoUI: true},
	"proftpd":     {Display: "FTP", Category: CatInfra, NoUI: true, Note: "Plaintext FTP"},
}

// byPort is the fallback, and it exists for one specific reason.
//
// Containers sharing a VPN sidecar's network namespace all appear to own the
// sidecar's socket: for example `Gluetun` holds :8080, :9696, :9117, :8112
// and :8191, which are really qBittorrent, Prowlarr, Jackett, Deluge and
// FlareSolverr. The port map calls that out as a known limitation and is right
// to — "who holds this socket" is genuinely Gluetun. But a launcher wants the
// other answer, so the well-known port breaks the tie.
//
// Only consulted when the owner is a namespace host or is unattributed, never
// to override a name that matched.
var byPort = map[int]known{
	8080: {Display: "qBittorrent", Category: CatMedia, Note: "Torrents (via VPN)"},
	9696: {Display: "Prowlarr", Category: CatMedia, Note: "Indexers (via VPN)"},
	9117: {Display: "Jackett", Category: CatMedia, Note: "Indexers (via VPN)"},
	8112: {Display: "Deluge", Category: CatMedia, Note: "Torrents (via VPN)"},
	8191: {Display: "FlareSolverr", Category: CatNetwork, NoUI: true},
	6881: {Display: "BitTorrent", Category: CatMedia, NoUI: true},
}

// namespaceHosts are containers whose sockets belong to something behind them.
var namespaceHosts = map[string]bool{"gluetun": true}

// hiddenPorts are infrastructure sockets that are never a launcher tile: the
// things every machine has, which say nothing about what this server is for.
// They remain fully visible in the ports view.
var hiddenPorts = map[int]bool{
	22: true, // SSH
	53: true, 111: true, 123: true, 137: true, 138: true, 139: true,
	445: true, 2049: true, 3128: true, 5353: true, 6000: true,
	25: true, 21: true,
	// Databases and internal RPC, matched by port as well as by owner because
	// an unattributed socket still should not become a tile.
	3306: true, 5432: true, 6379: true, 27017: true,
	5357: true, 3493: true, 11211: true,
}

// hiddenOwners are processes that listen but are never something you click.
var hiddenOwners = map[string]bool{
	"sshd": true, "ssh": true, "init.scope": true, "rpcbind": true,
	"systemd-resolved": true, "ntp": true, "chronyd": true, "avahi-daemon": true,
	"smbd": true, "nmbd": true, "docker-proxy": true, "master": true,
	"systemd": true, "cron": true, "postfix": true, "nslcd": true,
	// Databases, caches and internal daemons. These are real services and they
	// matter, but a launcher tile pointing a browser at PostgreSQL is noise —
	// they stay in the ports view, which is where "what holds this socket"
	// belongs.
	"postgres": true, "postgresql": true, "redis-server": true, "redis": true,
	"mariadb": true, "mysqld": true, "mongod": true, "meilisearch": true,
	"wsdd": true, "pvedaemon": true, "pvestatd": true, "pve-firewall": true,
	"pve-ha-lrm": true, "pve-ha-crm": true, "pvescheduler": true,
	"chrome": true, "chromium": true, "gunicorn": true, "uwsgi": true,
	"docker": true, "dockerd": true, "containerd": true, "netdata-claim": true,
}

// platformOwners recognise a service by what the host *is* rather than by the
// process name, for the cases where the process name is too generic to mean
// anything on its own. TrueNAS serves its entire web UI from nginx: matching
// "nginx" globally would mislabel every other reverse proxy on the fleet, but
// "nginx on a TrueNAS box" is unambiguous.
//
// Keyed on a substring of the host's os-release pretty name, so it keys off the
// platform rather than a particular hostname.
var platformOwners = map[string]map[string]known{
	"truenas": {
		"nginx": {Display: "TrueNAS", Category: CatInfra, Scheme: "https", Port: 443, Note: "Storage appliance"},
	},
	"proxmox": {
		"pveproxy": {Display: "Proxmox VE", Category: CatInfra, Scheme: "https", Port: 8006, Note: "Hypervisor"},
	},
}
