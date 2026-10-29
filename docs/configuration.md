# Configuration

API flags: `-addr` defaults to `:8080`, `-dir` to `data`.
`-notify-level`, `-notify-interval` and `-digest-interval` override their environment
seeds. Saved dashboard settings override notification and retention seeds; reset
settings in Admin to restore them. Other environment changes require a restart.

| API variable | Default | Meaning |
| --- | --- | --- |
| `API_TOKEN` | stored or setup | Dashboard/admin and MCP credential, also accepted on agent routes. At least 32 characters. Overrides the stored API token; when neither exists, a 10-minute claim window opens. |
| `AGENT_TOKEN` | unset | Agent-only inventory posts and release downloads. At least 32 characters when set; if unset, agents need API_TOKEN and startup warns that agents hold an admin credential. |
| `OPERATOR_TOKEN` | unset | Separate bearer for offers and restart execution; unset disables those routes. At least 32 characters, distinct from API_TOKEN and AGENT_TOKEN, no surrounding whitespace or CR/LF. Invalid values refuse startup. |
| `ACTIONS_ENABLED` | off | Only `1` enables execution; requires `ACTIONS_HOST` and operator token. |
| `ACTIONS_HOST` | empty | Exact inventory hostname of the Docker daemon reached by the API. |
| `ACTIONS_COMPOSE_FILES` | empty | Comma-separated exact `project=/absolute/compose.yml` mappings; invalid/duplicate entries refuse startup. See [restarts](restarts.md). |
| `PUBLIC_URL` | request origin | Absolute http(s) origin, optional port, no path/query/credentials. Used in enrollment commands and served installer; takes precedence over request headers. |
| `TRUSTED_PROXY` | off | Only `1` trusts X-Forwarded-Proto and X-Forwarded-Host. Enable only behind a proxy that replaces those headers and blocks direct access. Otherwise request TLS and Host determine the origin. |
| `AGENT_BIN_DIR` | `/usr/share/homelab-inventory/agent` | Public installer binary tree: SHA256SUMS plus linux-amd64/{discover,agentupdate} and linux-arm64/{discover,agentupdate}. Missing files return 404 with manual setup guidance. |
| `AGENT_RELEASE_DIR` | empty | Public signed release tree; empty disables updates. Set `/agent-releases` only with its read-only mount. |
| `AGENT_CANARY_HOSTS` | empty | Comma-separated exact inventory hostnames; empty means promote-only. |
| `ICONS_FETCH` | off | Only `1` permits outbound launcher-icon downloads; otherwise letter fallbacks. |
| `NOTIFY_LEVEL` | critical | Minimum severity: critical, warning or info. |
| `NOTIFY_INTERVAL` | 5m | Transition evaluation interval; zero disables it. |
| `DIGEST_INTERVAL` | 168h | Summary interval; zero disables it. |
| `APPRISE_URL` | empty | Notification endpoint; empty disables delivery. |
| `DIGEST_APPRISE_URL` | empty | Digest endpoint; falls back to `APPRISE_URL`. |
| `RUN_KEEP_ALL_DAYS` | 7 | Keep every run through this age. |
| `RUN_HOURLY_DAYS` | 30 | Keep hourly samples through this age. |
| `RUN_DAILY_DAYS` | 365 | Keep daily samples through this age; zero means forever. |
| `RUN_COMPRESS_AFTER_DAYS` | 2 | Compress retained runs older than this age. |

All three configured tokens must differ and have no surrounding whitespace or CR/LF.

Retention values are nonnegative integers and must form a valid ordered policy.
Invalid notification durations fall back to defaults. `API_PORT` belongs to
Compose, not the binary: it defaults to published port 8080; the container still
listens on 8080. The optional manual `.env` is private; uncomment Compose `env_file` to load it.
Generated API/agent tokens and hashed enrollment codes live in `<data>/state-auth.json`
at 0600, replaced atomically. Environment tokens override stored values individually.
The 10-minute window is fixed in production; tests inject a clock/window internally.
For a manual API-only installation, the first Add a host request provisions a
separate stored agent token, so enrollment never hands out admin authority.

| Agent, signer or bridge variable | Default | Consumer / meaning |
| --- | --- | --- |
| `API_URL` | empty | `agentupdate` fallback for `-api-url`. |
| `AGENT_TOKEN` | empty | `agentupdate` environment credential; falls back to API_TOKEN only if unset. A configured token file takes precedence. Prefer private files. |
| `API_TOKEN` | empty | MCP bridge admin credential (takes precedence over API_TOKEN_FILE); agentupdate fallback when no AGENT_TOKEN is configured. |
| `ALLOW_HTTP` | off | `1` explicitly permits agent plain HTTP beyond loopback. Installer persists this in config; updater also accepts `-allow-http`. |
| `AGENT_PLATFORMS` | `linux/amd64,linux/arm64` | Makefile, signer and release script; also supports `freebsd/amd64`. Duplicates/unknown platforms fail. Updaters follow the signed manifest list. |
| `SIGNER_HOST` | empty | Release script runs locally unless an explicit SSH target is supplied. |
| `SIGNER_PORT` | 22 | Release script SSH port. |
| `SIGNER_BIN` | `./bin/agentsign` | Release script signer executable; use an absolute trusted remote path with SSH. |
| `LOCAL_SIGNER_BIN` | `./bin/agentsign` | Local platform-contract parser during SSH publication. |
| `SIGNER_KEY` | `$HOME/.config/homelab-inventory/release.key` | Release script private signer-owned mode-0600 key, outside the public tree. |
| `SIGNER_DIR` | `$HOME/inventory-releases` | Release script public signed tree. |
| `SIGNER_INPUT` | `bin` | Release script artifact input directory. |
| `MCP_API_URL` | required | MCP bridge API base URL. |
| `API_TOKEN_FILE` | empty | MCP bridge API_TOKEN file; must be a private regular file (0600). Unreadable or group/world-readable files refuse startup. |

Discover uses flags. The local installer accepts `ALLOW_HTTP=1` or `--allow-http`
for LAN HTTP; HTTPS is required by default beyond loopback.
The installer writes mode-0600 `/var/lib/homelab-agent/config.json` with `api_url`,
`token_file`, `dir`, `discover`, `public`, `signed` and `allow_http`; `-config` replaces
runner flag configuration except an explicit HTTP opt-in, which also applies with `-config`. `-configure -config FILE` writes it and
`-check-config` validates direct URL/token-file configuration without posting.
The token remains in its separate private file. See [install](install.md) and
[self-update](self-update.md) for flags and enrollment. Standard shell `PATH`,
`HOME` and build-tool variables retain their ordinary meanings.
