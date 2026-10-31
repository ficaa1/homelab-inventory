# Plugins

Plugins add tabs to the dashboard. Four ship with it: **Media**, **Uploads**, **Network** and **Dusk**. They are already installed and start switched off.

## Turn one on

1. Open **Admin → Plugins**.
2. Flip the switch next to the plugin.
3. Fill in the form that opens.
4. Press **Test**, then **Save and enable**.

The plugin's tab appears in the top bar. Flip the switch off to remove it again;
its settings are kept.

## Media

Shows what is playing, downloading and coming up, from the services you use.
Fill in any of them; each needs a URL and an API key.

| Service | Where to find the API key |
| --- | --- |
| Sonarr | Settings → General → Security |
| Radarr | Settings → General → Security |
| Tautulli | Settings → Web Interface → API |
| Overseerr | Settings → General |

Media discovery and dashboard views are read-only. Explicit Dusk requests can submit to Overseerr only with device permission and an administrator-selected request policy.

## Uploads

Adds an upload button to every page. Set up any of these destinations:

| Destination | What to enter | Takes |
| --- | --- | --- |
| Folder | A folder path (`/uploads` in Docker) | Any file |
| Paperless-ngx | URL and an API token | `.pdf .png .jpg .jpeg .tif .tiff .webp .gif .txt` |
| Calibre-Web | URL, username and password | `.epub .pdf .mobi .azw3 .cbz .fb2` |

Files are limited to 100 MiB unless you change the limit. The folder never
overwrites: a second `a.pdf` is saved as `a (1).pdf`.

**Docker:** the installer already mounts an `uploads/` folder next to its
`compose.yml`. Enter `/uploads`. For a manual Compose install, create it first:

```sh
mkdir -p deploy/api/uploads
sudo chown 10001:10001 deploy/api/uploads
```

**Binary install:** pick a folder such as `/srv/inventory-uploads`, make it
writable by the `homelab-inventory` user, and allow the service to write there:

```sh
sudo systemctl edit homelab-inventory   # add: [Service] ReadWritePaths=/srv/inventory-uploads
sudo systemctl restart homelab-inventory
```

## Network

Shows network traffic, probes and busy containers from a Prometheus you already
run. Enter its URL (and a bearer token if it needs one).

It uses whichever of these exporters Prometheus scrapes:

| Exporter | Adds |
| --- | --- |
| node_exporter | Traffic, errors and drops per host |
| blackbox_exporter | Probe status and latency |
| cAdvisor | The ten busiest containers |

Run node_exporter with host networking (`network_mode: host`). On a Docker bridge
it only sees its own container's traffic.

## Details

**Settings.** Saved in `<data>/state-plugins.json` (private; back it up with the
data folder). You can also seed a setting from the environment as
`PLUGIN_<ID>_<KEY>`, for example `PLUGIN_NETWORK_PROMETHEUS_URL`. A value saved in
Admin wins. Secrets only ever show as set or not set.

**Alerts.** Plugins raise alerts like any host does, polled every five minutes.
If a plugin fails three polls in a row it raises `plugin.unreachable`.

| Alert | When |
| --- | --- |
| `media.sonarr.health`, `media.radarr.health` | Sonarr or Radarr reports a health problem (info, or warning for errors) |
| `media.queue.stuck` | A download has had a warning for over an hour |
| `network.probe.down` | A blackbox probe has failed for 10 minutes |

**Media dashboard.** Every request is a GET. Usernames, IPs, devices, paths and tracker
URLs are never shown; a stream says "a user". Posters come through the API, so the
browser never contacts another server. Results are cached for 20 seconds.
Upcoming shows 7 days by default (1 to 30).

**Uploads.** Files stream straight to the destination and are never stored in
the API's data folder. A Paperless "queued" result means Paperless accepted the
file, not that it finished processing it. **Recent** lists the last 100 uploads
(name, size, destination, result). The API also keeps an audit log; see
[API](api.md).

**Network.** It only runs fixed, built-in queries, and never changes Prometheus.
History covers 1, 6 (default) or 24 hours. Interface names matching
**Exclude interfaces** (loopback, bridges, VPNs by default) are left out. Host,
probe and container names come from your metric labels, so anyone who can see
the dashboard sees them.

Writing your own plugin: see [Writing plugins](writing-plugins.md).

## Dusk

Pairs household televisions with Media without copying any upstream API keys.
Dusk starts disabled. It adds setup controls under **Admin → Plugins → Dusk**,
not a new dashboard tab.

1. Configure any Media sources you use. Sonarr and Radarr add downloads/library;
   Tautulli adds playback; Overseerr adds metadata, search and recommendations.
2. Enable Dusk, then add a household profile. Each profile keeps its own hidden
   titles, taste feedback, import, request policy and optional viewing history.
3. Optionally select a Tautulli account to enrich that household's recommendations
   and familiar titles. No account is selected automatically.
4. To allow requests, enable Dusk requests and save a policy for that household.
   Choose the Overseerr requester, manual/automatic approval, and separate movie
   and TV server, quality profile and root folder. You can configure only one
   media kind. The selected requester's upstream approval permissions must agree
   with your policy; Dusk does not change upstream account permissions.
5. Create a pairing code, choosing the profile and permissions. Read access is the
   default; requests, preferences, refresh and taste are separate opt-ins. Enter
   the server address and code on the TV within 15 minutes.

Revoke a device in the same panel to disconnect it immediately. Deleting a
household revokes its devices and outstanding codes. Turning Dusk off blocks all
TV access while preserving configuration. Turning Media off removes its features;
a temporary upstream outage leaves features configured and reports unavailable.

**Discovery and Taste.** The portable default uses Overseerr's metadata feed and
similar-title recommendations, reranked with local likes and selected viewing
history. Explicit feedback overrides inferred history. Familiar shows selected
recent history or available titles. Optional Letterboxd CSV imports accept pasted
`Name`, `Year`, `Rating`, `Watched`, `Watchlist` columns (only Name required), up to
64 KiB and 500 rows. Standard watched/ratings exports work; for a watchlist export
add a `Watchlist` column with `true` values. URL columns are ignored. Names match
through bounded Media search, three per discovery read, with exact year matching
when supplied. Reimport to retry unmatched names after a source outage. Imports
and feedback stay local; nothing writes to Letterboxd or Plex. No datasets or
trained models are bundled or needed.

**Requests.** Only an explicit authorized TV action submits a request. Ordinary
Media reads remain read-only. Durable profile-scoped receipts prevent duplicate
and ambiguous requests across devices and restarts. An `unknown` outcome is never
replayed: inspect it upstream before taking any other action. There is no automatic
receipt deletion or reset button. Back up the private data directory.

The [versioned TV protocol](dusk/protocol-v1.md) includes synthetic examples.

Dusk request destination choices use Overseerr's server list and read quality
profiles/root folders directly from the matching Media Sonarr/Radarr connection.
Configure those Media connections with the same URL (including base path) and API
key as the selected Overseerr destination. Only exact normalized URL and key
matches appear; credentials from Overseerr never authorize new destinations.
The integration never invokes Overseerr's connection-test POST. Requests use
`X-API-User` to act as the selected requester and verify current request and
approval permissions before submitting.

### Migration integrations

Archive, Cloudflare, Security canaries and Tdarr are optional built-ins. Fresh
installations leave them disabled. Admin backup previews name the integrations
that legacy configuration will enable; restored configuration is applied only
when the operator confirms replacement.

| Integration | Active functionality | External state |
| --- | --- | --- |
| Archive | Policy status, paginated library/seeding, cached-release approvals, policy tags/scoring, queue controls, reviewed restore/purge | Existing media-policy service owns workers, leases, jobs and media |
| Media | Per-title and per-season download/import progress, including import-history joins | Existing Arr services |
| Network | Optional Prometheus metrics/probes, route-provider routes, inventory VPN facts, seeding-only qBittorrent reads, native bounded speed measurements/history | Existing exporters, route provider and qBittorrent |
| Cloudflare | Zone identity/plan, paginated DNS and complete UTC day/week hourly analytics | Cloudflare zone (read permissions only) |
| Security | Fake bearer/path canaries, durable deduplicated trip history and transition alerts | Local plugin cache; no credential values in trip history |
| Tdarr | Status, compression candidates, exact library-file preview and guarded batch execution | Existing Tdarr, media-policy, Arr, Tautulli and shrink sidecar |
| Uploads | Mounted folder (including a Copyparty-served folder), Calibre-Web, Paperless API token or legacy username/password login | Existing destination files and service databases |

Plugin action IDs and fields are compiled into the binary. The browser renders
plain forms, asks for explicit confirmation and never retries a submission.
External changes require the separate operator credential; local Taste feedback
uses normal administrator authentication. Plugins cannot accept arbitrary HTTP
paths or commands. Unknown write outcomes require upstream inspection.

Tdarr requires explicit Arr and Tdarr media roots and an optional protected-title
identity list. It checks fresh library/lease state, exact Arr identities, eligible
candidates, idle Plex and Tdarr, and the existing host-side `/validate` guard.
Each reviewed preview expires after two minutes and is consumed once. A durable
submission receipt is written before `/shrink`; matching receipts prevent replay
after a restart. An uncertain or failed submission must be reconciled by an
operator before manually removing its `submission-<plan-hash>.receipt`. The
public source supplies no private library paths or protected title defaults.

Network speed measurement uses owner-configured latency/upload and range-download
endpoints. It runs only after an operator action, cancels on plugin shutdown,
retains cooldown/backoff across restarts, and keeps bounded payload, concurrency
and time limits. It can consume several gigabytes of traffic. No background
measurement or measurement during restore is started.
