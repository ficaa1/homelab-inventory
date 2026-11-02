# API

The packaged API starts in setup mode when neither environment nor stored
`API_TOKEN` exists. Every configured token must be at least
32 characters, have no surrounding whitespace or CR/LF, and differ from the others.
Send credentials as `Authorization: Bearer <token>`; never in URLs.

`API_TOKEN` authorizes dashboard/admin reads and writes, plus agent routes.
`AGENT_TOKEN` authorizes only inventory posts and release manifest/artifact GETs.
The updater does not need `/agent/status`; that route requires `API_TOKEN`.
If `AGENT_TOKEN` is unset, agents use `API_TOKEN` and hold an admin credential;
the API logs a warning. `OPERATOR_TOKEN` authorizes only release offers and
restart/allowlisted execution; unset disables those routes. Neither API nor agent tokens
can authorize operator routes. Wrong/missing operator credentials return 403;
other authentication failures return 401. GET routes also accept HEAD, including
the agent download routes. JSON errors use `{"error":"..."}`. Only a successful first claim returns the API/agent tokens; code redemption
returns only the agent token. Both responses use Cache-Control: no-store.

`-allow-unauthenticated` is for loopback development only; startup refuses any
non-loopback listen address with that flag. Browser sessions can authorize the
same dashboard/admin routes as the API token, but cannot authorize agent,
operator or setup routes. A supplied Authorization header decides authority,
even when the browser also carries a session cookie.

| Route | Purpose | Authentication |
| --- | --- | --- |
| `GET /setup/status` | Claim state and remaining window | Public |
| `POST /setup/claim` | Generate/persist initial credentials | Public, only within claim window |
| `POST /setup/enroll` | Create single-use enrollment code and Linux command | API token or browser session |
| `GET /setup/enroll` | List code hints, expiry and redemption hostname | API token or browser session |
| `POST /agent/enroll` | Exchange code for agent token | Enrollment code, rate limited |
| `GET /install.sh` | Installer with API origin prefilled | Public |
| `GET /agent/bin/SHA256SUMS` | Packaged agent checksums | Public |
| `GET /agent/bin/{linux-amd64,linux-arm64}/{discover,agentupdate}` | Packaged agent binaries | Public |
| `GET /healthz` | Health check | Public |
| `GET /auth/status` | Browser sign-in status (no-store) | Public |
| `GET /auth/login?next=<path>` | Begin provider sign-in | Public; 404 when disabled |
| `GET /auth/callback` | Complete provider sign-in | Public; signed state, rate limited |
| `POST /auth/logout` | Clear browser session, 204 | CSRF header when a session cookie is present |
| `GET /settings/auth` | Sign-in settings, masked secret and sources | API token or session |
| `PUT /settings/auth` | Merge and validate sign-in settings, invalidate sessions | API token or session + CSRF |
| `POST /settings/auth/test` | Test temporary discovery without saving | API token or session + CSRF |
| `GET /capabilities` | Operator/restart configuration and enabled plugin tabs | API token |
| `GET /plugins` | Built-in plugin status, schemas and masked settings | API token |
| `PUT /plugins/{id}` | Enable, disable or merge plugin settings | API token |
| `GET /uploads/targets` | Configured destinations across enabled upload plugins | API token |
| `GET /uploads/files` | Folder files with name, size and modified time | API token or session |
| `GET /uploads/files/{name}` | Download one folder file | API token or session |
| `GET /uploads/preview/{name}` | Bounded text or raster image preview | API token or session |
| `POST /plugins/{id}/upload/{target}` | Stream a raw file to a configured destination | API token |
| `POST /plugins/{id}/test` | Test temporary merged settings without saving | API token |
| `GET /plugins/{id}/views/{name}` | Validated declarative plugin view | API token |
| `GET /plugins/{id}/images/{ref}` | Private plugin image | API token |
| `POST /inventory` | Submit inventory | Agent or API token |
| `GET /agent/manifest` | Read update manifest | Agent or API token |
| `GET /agent/artifacts/{sequence}/{name}` | Download update artifact | Agent or API token |
| `GET /agent/status` | Read agent status | API token |
| `POST /agent/offers` | Submit operator offer | Operator token |
| `GET /overview` | Fleet overview | API token |
| `GET /hosts` | List hosts | API token |
| `GET /hosts/{name}` | Read host | API token |
| `GET /hosts/{name}/packages` | List host packages | API token |
| `GET /hosts/{name}/packages/changes` | Package changes | API token |
| `GET /packages` | Search packages | API token |
| `GET /hosts/{name}/history` | Host inventory history | API token |
| `GET /hosts/{name}/drift` | Host drift | API token |
| `GET /hosts/{name}/trend` | Host trends | API token |
| `GET /hosts/{name}/deps` | Host dependencies | API token |
| `GET /hosts/{name}/compose` | Host Compose summary | API token |
| `GET /hosts/{name}/compose/diff` | Compose changes | API token |
| `GET /hosts/{name}/compose/file` | Read Compose file | API token |
| `GET /hosts/{name}/firewall-plan` | Suggested firewall plan | API token |
| `GET /images` | Image summary | API token |
| `GET /ports` | Port map | API token |
| `GET /alerts` | Alert report | API token |
| `GET /services` | List launcher services | API token |
| `POST /services/accept` | Accept discovered services | API token |
| `PUT /services/{key}` | Update launcher service | API token |
| `DELETE /services/{key}` | Delete launcher service | API token |
| `GET /icons/{slug}` | Service icon | API token |
| `GET /notify` | Notification status | API token |
| `GET /settings` | Read settings | API token |
| `PUT /settings` | Update settings | API token |
| `POST /settings/reset` | Reset settings | API token |
| `POST /notify/test` | Send test notification | API token |
| `POST /digest/send` | Deliver a composed digest | API token or session + CSRF |
| `GET /silences` | List silences | API token |
| `POST /silences` | Create silence | API token |
| `DELETE /silences/{id}` | Delete silence | API token |
| `POST /actions/preview` | Preview restart | API token |
| `POST /actions/execute` | Execute restart | Operator token |
| `POST /exec` | Run one allowlisted Docker command | Same authority as restart execution |
| `GET /actions/log` | Read action log | API token |
| `GET /actions/stacks` | List restartable stacks | API token |
| `GET /storage` | Storage summary | API token |
| `GET /storage/runs` | Inventory run storage | API token |
| `POST /storage/runs/prune` | Prune runs | API token |
| `DELETE /storage/runs/{host}` | Delete host runs | API token |
| `GET /` | Dashboard shell | Public |
| `GET /manifest.webmanifest` | Web manifest | Public |
| `GET /sw.js` | Service worker | Public |
| `GET /icon.svg` | Dashboard icon | Public |

## First-run setup and enrollment

While unclaimed or expired, all routes return 503 `{"error":"setup required"}`
except `/`, dashboard static assets, `/healthz`, `/setup/*`, `/install.sh`,
`/agent/bin/*` and the public `/auth/*` routes. While unclaimed, auth status says
`enabled:false`, login returns 404, and callback redirects with `disabled`. Development open mode is separate. Setup enrollment accepts API bearer authority or a browser session (with the CSRF header). `/agent/enroll` is usable only after claiming.

`GET /setup/status` returns one of:

```json
{"state":"unclaimed","window_seconds_left":512,"window_seconds_total":600,"api_url":"http://192.0.2.10:8080","http":true}
{"state":"expired"}
{"state":"claimed"}
```

`POST /setup/claim` requires `Content-Type: application/json` and `{}`. One caller
gets 200 `{"api_token":"<64 hex>","agent_token":"<64 hex>"}`; simultaneous/later
claims get 409 `{"error":"already claimed"}`. After 10 minutes without a claim,
it returns 410 `{"error":"setup window expired"}`; restart to reopen the window.
Tokens use crypto/rand and private atomic storage. Only the socket address of
the successful claimant is logged.

`POST /setup/enroll` also requires JSON `{}`. It returns:

```json
{"id":"9f2c41d07a5be318","code":"K7Q-4MD-2XP","expires_at":"2026-10-29T12:30:00Z","api_url":"http://192.0.2.10:8080","http":true,"commands":{"linux":"curl -fsSL 'http://192.0.2.10:8080/install.sh' | sudo sh -s -- --enroll 'K7Q-4MD-2XP' --allow-http"}}
```

Codes use nine unambiguous characters (no 0/O/1/I/L), last 15 minutes, are
single-use, and are stored as SHA256 hashes. At most 20 unused codes may be
outstanding (429 `{"error":"too many open codes"}` at the limit). `id` is random, not derived from the code. Codes survive restarts. An API-only manual
configuration provisions a separate stored agent token on first enrollment.
`GET /setup/enroll` returns `{"outstanding":[{"id":"9f2c41d07a5be318","code_hint":"K7Q-…","expires_at":"…","used_by":null}]}`;
a redeemed entry records its hostname. Expired entries are omitted; up to 100
recent redeemed entries are retained until expiry. Hashes and full codes are
never included in the list.

`POST /agent/enroll` takes `{"code":"K7Q-4MD-2XP","hostname":"host-a"}` and returns
`{"agent_token":"…","api_url":"…"}`. Malformed, unknown, used and expired codes
all get 403 `{"error":"invalid enrollment code"}`. All attempts count against
10/minute per socket peer IP, independent of source port or forwarding headers;
excess requests get 429 and Retry-After: 60. Limits reset on API restart.

`PUBLIC_URL` overrides the origin; otherwise request Host and TLS determine it.
Only TRUSTED_PROXY=1 accepts X-Forwarded-Proto/Host. Hosts are strictly validated
and generated shell values are quoted. Installer/binary 404 responses explain
missing packaged files and manual setup. When packaged binaries are missing, the
Linux command instead prints setup guidance and exits unsuccessfully. SHA256SUMS paths are
`linux-amd64/discover`, `linux-amd64/agentupdate` and arm64 equivalents.

## Capabilities and operator requests

`GET /capabilities` uses the API token and returns, for example:

```json
{"operator_enabled":true,"agent_offers_enabled":true,"actions_enabled":true,"actions_host":"host-a","plugins":[]}
```

Offers require configured operator authority; status can still return 503 when no
release is available. Actions require an enabled restart manager and operator
authority; `actions_host` is the sole target. The dashboard reads these at load,
hides rollout controls when offers are disabled, and shows restart controls only
on the matching enabled host. A capability flag does not grant authorization.

The release-offer and restart-execution POST routes require `Content-Type: application/json`, one bounded
JSON object, and `confirm:true`. Unknown fields or invalid JSON return 400;
non-JSON Content-Type returns 415.

`POST /agent/offers` accepts `{"action":"promote","sequence":1,"confirm":true}`.
Actions are `promote`, `hold`, `auto`, or `update`; only `update` takes a nonempty
exact `host`. Others omit `host` or send an empty string. Stale sequence or signer
hold returns 409; missing release/journal/persistence returns 503. See
[self-update](self-update.md) for precedence and signer controls.

`POST /actions/preview` uses the API token and accepts
`{"kind":"stack_restart","target":"app","host":"host-a"}` (or
`container_restart` with an exact container name). It returns containers, impact,
operations and `container_set_hash`. Execution uses operator bearer, the same
fields plus `confirm:true` and that hash. Both POST routes accept optional `host`:
when supplied it must be a string exactly equal to `ACTIONS_HOST`, otherwise 400
before resolution or execution. Omitting it retains configured-host behavior.
`GET /actions/stacks?host=X` returns `[]` for any non-target host; absent host
lists the configured host. It never selects another Docker daemon.

Missing target inventory returns 503. Execution with a disabled manager returns
503 after authorization. Changed container membership returns 409 with a fresh
`preview`, requiring another confirmation. Execution returns an audit entry:
200 for `result:"ok"`, 500 for `failed`/`partial`. Other validation failures return
400. Inspect [restart limitations](restarts.md) before enabling the Docker socket.

The dashboard prompts for the operator token and keeps it only in sessionStorage
under `homelab-inventory.operator-token`. A 403 clears it. Offer 403s show
“Promotion is disabled — set OPERATOR_TOKEN or use the signer CLI”; restart
403/404/503 responses disable controls for that page session.

## Allowlisted execution and composed digests

`POST /exec` is enabled with restart execution: `ACTIONS_ENABLED=1`,
`ACTIONS_HOST` and `OPERATOR_TOKEN`. It uses the same authorization as
`POST /actions/execute`. There is no caller-selected host or shell.
The request is one JSON object (at most 16 KiB), with `command` and optional
`compose_file`, `service`, `container`, `old_image`, `new_image`:

```json
{"command":"compose-set-image","compose_file":"/srv/app/compose.yml","old_image":"example/app:1","new_image":"example/app:2"}
```

Commands are `compose-set-image`, `compose-pull`, `compose-up` (`up -d`),
`compose-restart`, `container-restart` and `image-prune` (dangling images only).
Compose commands require an exact file from `ACTIONS_COMPOSE_FILES`; `service`
is optional and must be a safe name. A Compose restart derives the configured
project name from that file. Container restarts retain the latest target-host
inventory and live exact-name checks. Image references are validated, must differ,
and the old reference must occur exactly once. Replacement is atomic and preserves
file permissions (and the owner, when running as root); mount a writable parent directory to permit atomic rename.
An upgrade uses set-image, pull, then up as separate requests.

Execution shares the restart lock and `actions-audit.jsonl`: intent must persist
before execution, followed by its outcome. The response has `command`, `stdout`,
`stderr`, `exit_code` and optional `error`. Success is 200; rejected commands are
400; a nonzero command exit is 500; disabled execution or an unavailable audit is
503. Inspect a `running` audit row before retrying an interrupted request.

`POST /digest/send` has the same authorization as `POST /notify/test` and accepts
`{"title":"Daily report","body":"**Everything is healthy.**"}` (at most 1 MiB).
Body must contain non-whitespace text; an omitted or empty title becomes
`homelab digest`. Both fields are sent verbatim, with no template rendering,
through the digest sender, falling back to the alert sender. Notifications must
be enabled and the notification endpoint configured. Success returns
`{"sent":true}`; invalid JSON/body is 400, disabled/unconfigured sending is 503,
and delivery failure is 502. Every send attempt records a `kind:"digest"`
delivery without advancing the scheduled digest clock.

## Plugins

All plugin routes use normal API authentication. Agent and operator tokens are
refused; unclaimed setup mode returns 503. See [plugins](plugins.md) for the Go
contract, configuration layering and declarative block formats.

`GET /plugins` returns an array of objects containing `id`, `name`, `description`,
`version`, `enabled`, `state`, `message`, `checked_at`, `tab`, `settings_schema` and
`settings`. State is `disabled`, `unconfigured`, `ok`, `error` or `unavailable`.
Secret settings return only `{"set":true}` or `{"set":false}`. The list is empty
when the build contains no plugins and there are no retained plugin entries.

`PUT /plugins/{id}` accepts `{"enabled":true,"settings":{"endpoint":"https://example.org"}}`.
Both fields are optional. Present setting values replace stored values; `null`
clears a key, including its environment fallback; absent keys keep stored values.
Validation and Configure failures return 422
`{"error":"invalid plugin settings","fields":{"endpoint":"must be an http(s) URL"}}`
and save nothing. Success returns one updated list item. Disabling calls Close,
removes the capability tab and clears plugin alerts.

`POST /plugins/{id}/test` accepts an optional body
`{"settings":{"endpoint":"https://example.org"}}`. It applies the same merge
without saving and returns `{"ok":true,"message":"Connected","checks":[]}`.
It has a ten-second context deadline and permits one concurrent test per plugin;
a concurrent request returns 429. Invalid settings return 422.

`GET /plugins/{id}/views/{name}` forwards query parameters and returns at most
1 MiB of validated View JSON. Unknown/disabled plugins or views return 404;
plugin failures or invalid views return 502 `{"error":"..."}`.
`GET /plugins/{id}/images/{ref}` requires an enabled ImageSource and a valid ref,
returns only `image/*`, caps bytes at 4 MiB, and sets
`Cache-Control: private, max-age=3600` and `X-Content-Type-Options: nosniff`.

`GET /capabilities` includes `plugins:[{"id":"sample","name":"Sample","tab":{...}}]`
for enabled plugins with tabs. The MCP bridge retains two generic GET tools:
`plugins_list` reads `/plugins` and removes settings and their schemas before
returning the list; `plugin_view` takes `plugin`, `view` and optional string-valued
`params`, reading the corresponding view route. Neither can change settings.

### Service JSON routes

Plugin JSON routes use `/plugins/{id}/api/{path}` with normal API authentication
(or a browser session, with CSRF for writes). They share the plugin lifecycle lock.
Disabled or unknown plugins return 404; unavailable configured services return an
error rather than invented zero totals. These additional read routes return data
objects, separate from declarative views:

| GET route | Response |
| --- | --- |
| `/plugins/media/api/tautulli/activity` | Stream counts, bandwidth and `sessions` with user, title and playback/transcode fields |
| `/plugins/media/api/tautulli/history?hours=24` | `total_plays`, `total_duration`, `distinct_users`, `truncated`, `plays` (maximum 200) |
| `/plugins/media/api/tautulli/libraries` | Array of section IDs, names, types, counts and `total_bytes` |
| `/plugins/media/api/sonarr/summary` | `status`, `series_total`, `series_monitored`, `missing`, `missing_soon`, `recent_adds`, `queue` |
| `/plugins/media/api/radarr/summary` | `status`, `movies_total`, `missing`, `missing_soon`, `recent_adds`, `queue` |
| `/plugins/cloudflare/api/zone` | `id`, `name`, `status`, `plan` |
| `/plugins/cloudflare/api/analytics?hours=24` | Zone/window, requests, bytes, threats, cached/uncached, uniques, `cache_ratio`, hourly buckets |
| `/plugins/network/api/summary` | `generatedAt`, VPN inventory, metrics, route/DNS counts and optional download-client health |
| `/plugins/tdarr/api/shrink-candidates?limit=25` | Array of eligible file paths, sizes, codecs, classes and estimated savings |

History and analytics accept `hours` from 1 to 168, defaulting to 24 for absent
or invalid values. Cloudflare uses UTC date bounds for hourly analytics.
Candidate `limit` must be positive and is capped at 500. The existing
`GET /plugins/tdarr/api/candidates` keeps its page-oriented object response.

`POST /plugins/tdarr/api/shrink` accepts
`{"paths":["/worker/movies/Example/movie.mkv"],"withPc":false}`.
It accepts one bounded JSON object (64 KiB), refuses unknown fields, empty lists,
more than 16 paths and configured protected paths. The configured authenticated
sidecar must resolve symlinks and expand folders through `/validate` before
`/shrink` receives anything. Resolved paths must be unique media files within
configured Tdarr roots, and protections are checked again, including mapped
source roots. `withPc` opts into the secondary worker.

The call is serialized with other Tdarr operations, has a six-minute deadline,
and relays the sidecar's JSON and HTTP status. Refusals are 400, protected paths
403, unavailable validation/configuration 503, and an uncertain sidecar submission
502. Inspect the sidecar before retrying an uncertain result. Reviewed
`batch/preview` and `batch/execute` routes remain available.

## MCP bridge

The stdio bridge uses `MCP_API_URL`. `MCP_API_TOKEN` or a mode-0600
`MCP_API_TOKEN_FILE` supplies the API bearer; these take precedence over the
existing `API_TOKEN`/`API_TOKEN_FILE` variables. Set a separate `OPERATOR_TOKEN`
or mode-0600 `OPERATOR_TOKEN_FILE` for `exec`; the API bearer alone cannot
execute Docker commands. The bridge refuses redirects and never retries writes.
An API error is returned as an MCP result with `isError:true`, including when a
required plugin is disabled.

Inventory tools and `plugins_list`/`plugin_view` remain available. The service
and mutation tools map as follows:

| Tool | HTTP endpoint | Options |
| --- | --- | --- |
| `exec` | POST `/exec` | `command`, `compose_file`, `service`, `container`, `old_image`, `new_image` |
| `send_digest` | POST `/digest/send` | `title`, `body` |
| `tdarr_shrink` | POST `/plugins/tdarr/api/shrink` | `paths` (string array), `withPc` (boolean) |
| `tdarr_candidates` | GET `/plugins/tdarr/api/shrink-candidates` | `limit` |
| `cloudflare_zone` | GET `/plugins/cloudflare/api/zone` | — |
| `cloudflare_analytics` | GET `/plugins/cloudflare/api/analytics` | `hours` |
| `tautulli_activity` | GET `/plugins/media/api/tautulli/activity` | — |
| `tautulli_history` | GET `/plugins/media/api/tautulli/history` | `hours` |
| `tautulli_libraries` | GET `/plugins/media/api/tautulli/libraries` | — |
| `sonarr_summary` | GET `/plugins/media/api/sonarr/summary` | — |
| `radarr_summary` | GET `/plugins/media/api/radarr/summary` | — |
| `network` | GET `/plugins/network/api/summary` | — |

`exec`, `send_digest` and `tdarr_shrink` are the bridge's deliberate POSTs.
Their options are JSON body fields; read options become query parameters.

## Browser sign-in

`GET /auth/status` is public with `Cache-Control: no-store`. It returns
`enabled`, `provider` (configured name or issuer host), and `signed_in`.
A valid browser session adds `email` and `expires_at` (UTC RFC3339); otherwise
both are omitted. `enabled` requires complete, enabled configuration and a
claimed instance. It does not describe bearer-token enforcement.

Login redirects to discovery's authorization endpoint with scopes
`openid email profile`, state, nonce and S256 PKCE. The signed `hi_oidc` cookie
lasts ten minutes, is HttpOnly, SameSite=Lax and scoped to `/auth/`. `next` must
start with a single `/`; external URLs and ambiguous paths fall back to `/`.
A successful callback sets `hi_session` (HttpOnly, SameSite=Lax, Path=/,
Secure when the public URL uses HTTPS) and redirects to `next`. A failed
callback redirects to `/#/login?error=<code>`: `state`, `denied`, `provider`,
`unverified`, `not_allowed` or `disabled`. Callback attempts are limited to
20/minute per socket peer IP (not forwarding headers); excess requests return
429 with Retry-After: 60. The short-lived state cookie is cleared on callback.

`POST /auth/logout` clears the session and returns 204. When a session cookie
is present it requires `X-Requested-By: homelab-inventory`. All unsafe methods
(other than GET/HEAD) authenticated by cookie require the same header; if
Origin is supplied it must equal the configured public URL's origin. Failure
returns 403 `{"error":"missing CSRF header"}`. Bearer-authenticated requests
are exempt. A session never authorizes inventory POSTs, release downloads,
the setup claim, release offers or restart execution. The public packaged
installer downloads remain public.

`GET /settings/auth` returns `enabled`, `state`, `message`, `issuer`,
`client_id`, `client_secret:{"set":true|false}`, `allowed`, `public_url`,
`provider_name`, `session_hours`, `require_verified`, derived `redirect_uri`
and `sources`. Each source is `stored`, `env` or `default`. State is
`unconfigured` when fields are missing, `disabled` when configured but off,
`ok` when enabled and configured, or `error` after a failed test of the saved
configuration. Provider name falls back to the issuer host.

PUT accepts a partial object of those editable fields. Values replace, null
clears (suppressing environment seeds), and omitted fields keep their values.
Clearing session hours or verified-email policy restores its default; clearing
enabled stores false. Secrets never need re-entry. Invalid fields or enabling
without issuer, client ID, secret, public URL or an allowed address/domain
returns 422 `{"error":"invalid sign-in settings","fields":{"allowed":"add at least one address or @domain"}}`
and saves nothing. Validation does no network I/O. Every successful PUT, even
an unchanged save, increments the stored generation and signs everyone out;
200 returns the GET shape. Notification/retention reset preserves auth settings.

`POST /settings/auth/test` accepts no body, `{}`, or
`{"settings":{"issuer":"https://accounts.example.org"}}`. It merges settings
without saving, fetches discovery within ten seconds, verifies the exact
issuer and validates authorization/token/userinfo URLs. It returns 200
`{"ok":true,"message":"Discovery and endpoints are valid","checks":[{"name":"Discovery","ok":true,"message":"Discovery and endpoints are valid"}]}`
(or `ok:false` with a safe error); bad settings return 422. Only one test runs
at a time; a second returns 429. This checks discovery, not actual credentials
or a user's access: complete sign-in to check those.

## Upload requests

`GET /uploads/targets` returns a JSON array sorted by plugin ID and then destination
order. Each entry has `plugin`, `plugin_name`, `id`, `label`, optional `help` and
`accept` (lower-case extensions with a dot), and `max_bytes`. An empty `accept`
allows any extension. Only enabled, configured destinations are listed.

`GET /uploads/files` lists up to 500 regular files in the folder destination,
newest first, as `{name,size,modified}` entries. Downloads and previews accept
only a plain filename: dotfiles, symlinks and subdirectories are refused.
`GET /uploads/preview/{name}` allows Markdown, HTML, text/log/CSV/JSON/YAML/conf
(up to 2 MiB), and JPEG/PNG/GIF/WebP/AVIF (up to 25 MiB). Other kinds return
415 and oversized previews return 413; downloads remain available. Preview
responses are no-store, nosniff and sandboxed.

`POST /plugins/{id}/upload/{target}` takes the file as the raw body, with a required
`Content-Length` and `X-Upload-Name: <percent-encoded UTF-8 filename>` (browser
`encodeURIComponent`). There is no multipart envelope on this API route. Names
must be a single component, at most 200 bytes, with no control characters, slashes,
backslashes or leading dot. Success is `200 {"ok":true,"name":"…","message":"…"}`;
`name` is the actual stored name when known.

Missing name is 400, invalid name 422, missing length 411, excessive length 413,
unaccepted extension 415, unknown/disabled plugin or target 404, and destination
failure 502. Size is limited by the destination and a core maximum of 2 GiB, with
a bounded streaming reader. Only this route extends read/write deadlines to 30
minutes; ordinary API limits remain unchanged. Reverse proxies may need matching
body-size and timeout settings.

Before delivery, the API appends an `intent` to `<data>/upload-audit.jsonl` (0600),
then an `outcome` with time, actor, plugin, target, original name, consumed byte
count, streaming SHA-256 and result/error. The intent has the declared length and
an empty digest; a failed outcome's digest covers only bytes consumed. File
contents and credentials are never written to the audit. If outcome persistence
fails, the destination may already have accepted the file; inspect it before
retrying. There is no automatic retry or audit retention policy.


## Dusk devices and administration

The optional [Dusk protocol](dusk/protocol-v1.md) uses independent hashed device
credentials under `/tv/v1`. These never authorize ordinary dashboard, agent,
operator, inventory or upload routes. The API token does not authorize TV routes.
`POST /tv/v1/pair` is public, single-use-code protected and rate limited. All TV
writes require JSON and their endpoint's device scope; disabling Dusk returns 503.

The following administrator routes accept API bearer or browser session authority.
Browser writes require the existing `X-Requested-By: homelab-inventory` CSRF header.
All setup responses are no-store.

| Route | Purpose |
| --- | --- |
| GET `/dusk/admin` | Profiles, active devices, policies and history selections |
| POST `/dusk/profiles` | `{name}` creates; `{id,name}` renames |
| DELETE `/dusk/profiles/{id}` | Delete profile and revoke its devices/codes |
| POST `/dusk/codes` | `{profile_id,scopes}` → `{code,expires_at}` |
| DELETE `/dusk/devices/{id}` | Immediately revoke a device |
| GET `/dusk/options` | Configured upstream requester and movie/TV choices |
| PUT `/dusk/policies/{id}` | Save selected requester/approval/movie/TV policy |
| GET `/dusk/enrichment` | Optional Tautulli account choices |
| PUT `/dusk/enrichment/{id}` | `{user_id}` selects account; zero clears |
| POST `/dusk/import/{id}` | `{csv}` imports bounded household Letterboxd CSV |

Request policy fields are `label`, `approval` (`manual` or `automatic`),
`requester_id`, `movie` and `tv`. Each selection contains `server_id`, `profile_id`
and `root`; an unconfigured kind has zero profile and empty root. Upstream keys
are neither accepted nor returned. Names and root folders are administrator-only.
TV capabilities expose only policy label and approval.

Private state lives under `<data>/plugins/dusk/core/`: `devices.json` contains
credential/code hashes, `policy.json` policies, selected history users and durable
operation receipts, and `personal.json` local feedback/imports. Files are private
and atomically replaced. Never publish these files with protocol examples.
