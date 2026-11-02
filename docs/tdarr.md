# Tdarr companion

The optional dashboard Tdarr plugin reads Tdarr, media-policy, Arr and Tautulli,
then previews exact files before submitting a confirmed batch. It cannot run host
commands. This companion supplies its authenticated `/validate` and `/shrink`
endpoints and the host CLI behind them. You can also use the CLI without the
dashboard. It replaces library files through Tdarr; keep backups.

Use a dedicated Tdarr instance for these batches. The CLI parks GPU workers on
**all nodes**, sweeps **every file** to “Not required” (including standalone health
checks), queues only the named files, and refuses to arm if anything else is
queued. Disable automatic scanning/requeueing and keep all other worker types at
zero. The helper lock serializes the CLI and poller, but cannot lock out Tdarr's
UI or other API clients. Do not change the queue during a batch. Folder expansion
is capped at 16 media files; paths, symlink targets and protected hard links are
checked before queue changes. Configured protection failures refuse the batch.

## Install

Run on the Linux host with the media mounted locally. Requires Bash, Python 3.10+
(stdlib only), curl, jq, util-linux `flock`, sudo, systemd and cron. Tdarr nodes
need working NVIDIA NVENC and matching server/node versions for this Flow.
No dashboard or Python build dependency is added.

1. In Tdarr's classic Plugins area, install
   `deploy/tdarr/Tdarr_Plugin_hlb1_FFMPEG_Lossless_Audio_Drop_Or_Transcode.js`
   into the server's `Tdarr/Plugins/Local/` directory (usually
   `/app/server/Tdarr/Plugins/Local/` in the container), mode 0644, readable by
   Tdarr. Refresh Local plugins. Keep the filename: the Flow references its ID.
2. Import `deploy/tdarr/Shrink.json` through Tdarr's Flows import control. Install
   the Community classic plugin
   `Tdarr_Plugin_vdka_Tiered_NVENC_CQV_BASED_CONFIGURABLE` if absent. Select the
   imported **Shrink** Flow and enable Flows on both movie and series libraries.
   Copy the resulting Flow ID into `TDARR_FLOW_ID`. Library folders must exactly
   match `TDARR_MOVIE_ROOT` and `TDARR_SERIES_ROOT`.
   The Flow drops redundant lossless audio or converts it to EAC3 640k, skips HDR
   video encoding, encodes non-HEVC with NVENC, re-encodes HEVC above 30 Mbps,
   then runs a quick health check before replacement. Errors require review.
3. Install the host files as root:

   ```sh
   useradd --system --no-create-home --shell /usr/sbin/nologin tdarr-shrink
   install -o root -g root -m 0755 deploy/tdarr/tdarr-shrink /usr/local/bin/
   install -o root -g root -m 0755 deploy/tdarr/tdarr-shrinkd.py /usr/local/bin/
   install -o root -g root -m 0755 deploy/tdarr/tdarr-notify.sh /usr/local/bin/
   install -o root -g root -m 0644 deploy/tdarr/tdarr-shrinkd.service /etc/systemd/system/
   install -o root -g tdarr-shrink -m 0640 deploy/tdarr/tdarr-shrink.env.example /etc/default/tdarr-shrink
   install -d -o root -g root -m 0700 /var/lib/tdarr-shrink
   (umask 027; python3 -c 'import secrets; print(secrets.token_hex(32))' > /etc/tdarr-shrink.token)
   chown root:tdarr-shrink /etc/tdarr-shrink.token
   chmod 0640 /etc/tdarr-shrink.token
   ```

4. Edit the env file for your mounts, node name, Flow ID and Apprise HTTP API
   notification URL, for example `https://notify.example.org/notify/tdarr`.
   Use `visudo -f /etc/sudoers.d/tdarr-shrink` to install this line, mode 0440:

   ```sudoers
   tdarr-shrink ALL=(root) NOPASSWD: /usr/local/bin/tdarr-shrink
   ```

   The CLI, env file, token, parent directories and sudoers file must not be
   writable by the service user. Sudo runs only this fixed CLI; it revalidates
   all paths, including flags and expanded files. The service's `User=` and
   `Group=` default to `tdarr-shrink`. To use another account, change them with
   `systemctl edit tdarr-shrinkd` and update sudoers and file group ownership.
   Systemd does not expand env variables in `User=`.
5. Enable the service with `systemctl daemon-reload` and
   `systemctl enable --now tdarr-shrinkd`. Add the following root crontab entry:

   ```cron
   * * * * * /usr/local/bin/tdarr-notify.sh
   ```

   Run the poller once to seed idle state. It parks workers on drain and posts
   one batch-done notification, warning if files need review. Failed deliveries
   retry next minute; HTTP 204 is treated as an unconfigured Apprise key.
   Delivery can repeat if a process exits after sending but before saving state.

## Settings

All runtime settings use `/etc/default/tdarr-shrink`. Use literal `KEY=value`
lines, quote JSON and spaces, and do not use shell expansions, `export` or inline
commands. Restart the sidecar after edits; CLI and poller read it each run.
The example supplies a sample mapping and cache note; the code defaults below
apply when a value is omitted. Empty required values refuse execution.

| Setting | Default and meaning |
| --- | --- |
| `TDARR_API` | `http://127.0.0.1:8266`; Tdarr server API base URL |
| `TDARR_API_KEY` | Empty; optional Tdarr `x-api-key` |
| `TDARR_FLOW_ID` | None; required imported Flow ID, checked against both libraries |
| `TDARR_MOVIE_ROOT` | `/media/movies`; Tdarr movie library folder |
| `TDARR_SERIES_ROOT` | `/media/series`; Tdarr series library folder |
| `TDARR_PATH_MAP` | `{}`; required JSON object mapping container/Arr roots to existing host directories; include both library roots |
| `TDARR_NODE_NAME` | None; required primary GPU node name |
| `TDARR_PC_NODE` | Empty; optional secondary GPU node name |
| `TDARR_CACHE_NOTE` | Empty; advisory printed when requesting the secondary node |
| `TDARR_ALLOWED_PREFIXES` | `["/media/movies","/media/series"]`; JSON array of sidecar input roots |
| `TDARR_SHRINKD_BIND` | `127.0.0.1`; listener address |
| `TDARR_SHRINKD_PORT` | `8791`; listener port |
| `TDARR_TOKEN_FILE` | `/etc/tdarr-shrink.token`; bearer token file |
| `RADARR_URL` | `http://127.0.0.1:7878`; used only with configured protections |
| `RADARR_API_KEY` | Empty; Radarr key, takes precedence over config XML |
| `RADARR_CONFIG` | Empty; optional host path to Radarr `config.xml`, reading `ApiKey` |
| `TDARR_PROTECTED_RADARR_IDS` | Empty; comma-separated positive Radarr movie IDs; refuses folders, symlink aliases and hard links |
| `APPRISE_NOTIFY` | Empty; Apprise HTTP API notification URL; unset leaves notification pending after parking |
| `TDARR_STATE_DIR` | `/var/lib/tdarr-shrink`; root-owned lock and busy/idle state |
| `TDARR_LOG_FILE` | `/var/log/tdarr-shrink.log`; poller transition/delivery log |

Map Arr roots separately when they differ from Tdarr roots. For example,
`"/data/movies":"/srv/media/movies"` maps an Arr movie path to the same host
files as `"/media/movies":"/srv/media/movies"`. Input and resolved files must
remain inside these roots. Add host or Arr roots to `TDARR_ALLOWED_PREFIXES` if
HTTP callers need them; the CLI always applies its filesystem checks too. Keep
movie and series roots distinct and non-overlapping, with no ambiguous aliases.

## Dashboard and secondary node

Set the dashboard plugin's `sidecar_url` to `http://127.0.0.1:8791` and
`sidecar_key` to the token file's contents. `tdarr_url` / `tdarr_key` match
`TDARR_API` / `TDARR_API_KEY`. Its `tdarr_movie_root` / `tdarr_series_root` match
the companion's library roots; `radarr_root` / `sonarr_root` must be in the path
map. Configure media-policy, Arr and Tautulli separately for dashboard preview
and playback checks. Mirror companion protected IDs in the dashboard's
`protected_titles` (for example `[{"kind":"radarr","id":12}]`); CLI protection
is independent of dashboard settings.

A container's loopback is not the host's loopback. If the dashboard runs in a
container, bind the sidecar to a dedicated bridge gateway reachable by that
container and use that address in `sidecar_url`. Restrict access with the host
firewall; bearer auth over plain HTTP needs a trusted network. Do not expose the
sidecar publicly.

`sudo /usr/local/bin/tdarr-shrink --validate-only /media/movies/Example`
checks and expands files without contacting Tdarr or changing its queue.
`sudo /usr/local/bin/tdarr-shrink /media/movies/Example` queues them.
Add `--with-pc` (dashboard `withPc: true`) to arm the configured secondary node.
An unset, offline or unregistered secondary is a noted skip. Both nodes need
translated access to the same library **and shared transcode cache**; this
companion does not mount storage or configure path translators. Keep secondary
worker limits at zero until explicitly armed. `secondary_label` only changes
the dashboard label.

## Checks

- Refused batch: check `journalctl -u tdarr-shrinkd`, token/URL pairing, allowed
  prefixes, the host path map and mount permissions. Configured protected IDs
  require reachable Radarr and readable protected folders even for series-only
  batches. Preview must expand to the exact same paths as execution.
- No worker start: check primary node name, imported Flow ID, enabled Flows,
  library folders, schedule blocks, node pause state and NVENC availability.
  `queue is empty after arming` means Tdarr still sees a closed queue gate.
  Standalone health-check workers should remain zero; the Flow does its own check.
- No notification: check root cron, `/var/log/tdarr-shrink.log`, state in
  `/var/lib/tdarr-shrink/state` and the Apprise URL's configured recipients.
  Pending queued files prevent drain. Review warnings count all retained errors
  in the dedicated Tdarr instance, not just the latest batch.
- After a timeout or uncertain response, inspect Tdarr's queue, workers and
  library files before retrying. The helper cannot roll back a running encode.

Offline verification: `bash -n deploy/tdarr/tdarr-shrink deploy/tdarr/tdarr-notify.sh`
and `(cd deploy/tdarr && python3 -m unittest)`. Run shellcheck when installed.
