# Configuration

Most settings live in the dashboard under **Admin**: notifications, storage
retention, plugins and sign-in. What you save there wins over the environment;
**Reset to environment** undoes it. The variables below go in the API's
environment (the Compose `.env` file, or the systemd unit) and need a restart.

## Tokens

| Token | Lets you |
| --- | --- |
| `API_TOKEN` | Use the dashboard, the API and the MCP bridge. Created by the setup wizard. |
| `AGENT_TOKEN` | Only send reports and download agent updates. Also created by the wizard. |
| `OPERATOR_TOKEN` | Only promote agent updates and restart containers. Optional; unset turns both off. |

Each must be at least 32 characters, and all three must be different. Without an
`AGENT_TOKEN`, agents use the `API_TOKEN` and the log warns you about it.

## Common settings

| Variable | Default | Does |
| --- | --- | --- |
| `APPRISE_URL` | off | Where notifications go |
| `NOTIFY_LEVEL` | `critical` | Lowest severity that notifies: `critical`, `warning` or `info` |
| `DIGEST_INTERVAL` | `168h` | How often a summary is sent; `0` turns it off |
| `DIGEST_APPRISE_URL` | `APPRISE_URL` | A separate destination for the summary |
| `PUBLIC_URL` | the browser's address | The address put in Add host commands, e.g. `https://inventory.example.org` |
| `TRUSTED_PROXY` | off | `1` to trust `X-Forwarded-*` headers from your reverse proxy |
| `ICONS_FETCH` | off | `1` to download logos for launcher tiles |

Restarts and agent updates have their own pages: [restarts](restarts.md),
[self-update](self-update.md).

## Browser sign-in (OIDC)

Lets people sign in with an account instead of pasting the API token.

1. Open **Admin → Sign-in**.
2. Fill in **Public URL**, the address you open the dashboard at.
3. Copy the **Redirect URI** it shows, and register it with your provider.
4. Enter the provider's **issuer URL**, **client ID** and **client secret**.
5. Under **Allowed accounts**, list email addresses, or `@example.org` for a domain.
6. Press **Test**, switch on **Allow sign-in**, and **Save**.

| Provider | Issuer | Notes |
| --- | --- | --- |
| Google | `https://accounts.google.com` | Create a "Web application" OAuth client |
| Authentik | from the app's discovery URL | Usually ends in the application's path |
| Authelia | your Authelia address | Confidential client, PKCE S256, scopes `openid email profile` |
| Keycloak | `https://id.example.org/realms/home` | Confidential client with Standard flow |

The account's email must be verified unless you untick that option. Saving
sign-in settings signs everyone out.

## Details

**API flags.** `-addr` (default `:8080`), `-dir` (default `data`),
`-notify-level`, `-notify-interval`, `-digest-interval`.

**More API variables.**

| Variable | Default | Does |
| --- | --- | --- |
| `NOTIFY_INTERVAL` | `5m` | How often alerts are checked for changes; `0` turns it off |
| `RUN_KEEP_ALL_DAYS` | 7 | Keep every report this long |
| `RUN_HOURLY_DAYS` | 30 | Then one an hour, this long |
| `RUN_DAILY_DAYS` | 365 | Then one a day, this long; `0` keeps them forever |
| `RUN_COMPRESS_AFTER_DAYS` | 2 | Compress reports older than this |
| `AGENT_BIN_DIR` | `/usr/share/homelab-inventory/agent` | Where the Add host command gets agent files |
| `ACTIONS_ENABLED`, `ACTIONS_HOST`, `ACTIONS_COMPOSE_FILES` | off | See [restarts](restarts.md) |
| `AGENT_RELEASE_DIR`, `AGENT_CANARY_HOSTS` | off | See [self-update](self-update.md) |
| `API_PORT` | 8080 | Compose only: the published port |

**Sign-in from the environment.** `OIDC_ENABLED`, `OIDC_ISSUER`,
`OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_ALLOWED` (comma-separated),
`OIDC_PUBLIC_URL`, `OIDC_PROVIDER_NAME` (button label, defaults to the issuer's
host) and `OIDC_SESSION_HOURS` (default 168, up to 720). Values saved in Admin win.

How sign-in works: discovery, authorization code with PKCE, and the email from
the userinfo endpoint (ID tokens are not parsed). The issuer must match exactly,
including a trailing slash, and must use HTTPS except on loopback. Sessions
survive restarts. A signed-in browser can do what the API token does in the
dashboard, but cannot post reports, promote updates or restart containers.

**Agent, signer and MCP bridge variables.**

| Variable | Used by | Does |
| --- | --- | --- |
| `API_URL` | agentupdate | Default for `-api-url` |
| `AGENT_TOKEN` | agentupdate | Token, if no token file is configured |
| `ALLOW_HTTP` | agent installer, agentupdate | `1` allows plain HTTP beyond loopback |
| `AGENT_PLATFORMS` | make, signer | Default `linux/amd64,linux/arm64`; `freebsd/amd64` also works |
| `SIGNER_HOST`, `SIGNER_PORT` | release script | Sign over SSH on another machine |
| `SIGNER_BIN`, `LOCAL_SIGNER_BIN` | release script | Signer path (default `./bin/agentsign`) |
| `SIGNER_KEY` | release script | Private key, default `~/.config/homelab-inventory/release.key` |
| `SIGNER_DIR` | release script | Public release folder, default `~/inventory-releases` |
| `SIGNER_INPUT` | release script | Build folder, default `bin` |
| `MCP_API_URL` | MCP bridge | API address (required) |
| `API_TOKEN`, `API_TOKEN_FILE` | MCP bridge | Token, or a mode-0600 file containing it |

**Files.** Tokens made by the wizard are kept in `<data>/state-auth.json`; the
sign-in key in `<data>/state-session-key`. Both are private; include them in
backups and keep those backups safe. The agent's settings are in
`/var/lib/homelab-agent/config.json`, its token in a separate file next to it.
