# Plugins

Plugins add tabs to the dashboard. Three ship with it: **Media**, **Uploads** and
**Network**. They are already installed and start switched off.

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

Read-only: it never grabs, requests, changes or deletes anything.

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

**Media.** Every request is a GET. Usernames, IPs, devices, paths and tracker
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
