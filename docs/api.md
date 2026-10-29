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
restart execution; unset disables those routes. Neither API nor agent tokens
can authorize operator routes. Wrong/missing operator credentials return 403;
other authentication failures return 401. GET routes also accept HEAD, including
the agent download routes. JSON errors use `{"error":"..."}`. Only a successful first claim returns the API/agent tokens; code redemption
returns only the agent token. Both responses use Cache-Control: no-store.

`-allow-unauthenticated` is for loopback development only; startup refuses any
non-loopback listen address with that flag. `/auth/status` is always
`{"enabled":false}` (no browser login provider); it does not describe bearer enforcement.

| Route | Purpose | Authentication |
| --- | --- | --- |
| `GET /setup/status` | Claim state and remaining window | Public |
| `POST /setup/claim` | Generate/persist initial credentials | Public, only within claim window |
| `POST /setup/enroll` | Create single-use enrollment code and Linux command | API token only |
| `GET /setup/enroll` | List code hints, expiry and redemption hostname | API token only |
| `POST /agent/enroll` | Exchange code for agent token | Enrollment code, rate limited |
| `GET /install.sh` | Installer with API origin prefilled | Public |
| `GET /agent/bin/SHA256SUMS` | Packaged agent checksums | Public |
| `GET /agent/bin/{linux-amd64,linux-arm64}/{discover,agentupdate}` | Packaged agent binaries | Public |
| `GET /healthz` | Health check | Public |
| `GET /auth/status` | Authentication status | Public |
| `GET /capabilities` | Operator/restart configuration and enabled plugin tabs | API token |
| `GET /plugins` | Built-in plugin status, schemas and masked settings | API token |
| `PUT /plugins/{id}` | Enable, disable or merge plugin settings | API token |
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
| `GET /silences` | List silences | API token |
| `POST /silences` | Create silence | API token |
| `DELETE /silences/{id}` | Delete silence | API token |
| `POST /actions/preview` | Preview restart | API token |
| `POST /actions/execute` | Execute restart | Operator token |
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
except `/`, dashboard static assets, `/healthz`, `/setup/*`, `/install.sh` and
`/agent/bin/*`. Development open mode is separate. Setup enrollment still
requires API bearer authority. `/agent/enroll` is usable only after claiming.

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

Both operator POST routes require `Content-Type: application/json`, one bounded
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
for enabled plugins with tabs. The MCP bridge adds two GET tools:
`plugins_list` reads `/plugins` and removes settings and their schemas before
returning the list; `plugin_view` takes `plugin`, `view` and optional string-valued
`params`, reading the corresponding view route. Neither can change settings.
