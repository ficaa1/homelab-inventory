# Security

Use GitHub private vulnerability reporting (Security → Report a vulnerability) on this repository.

After setup the API requires `API_TOKEN`, the dashboard/admin and MCP credential.
`AGENT_TOKEN` only authorizes inventory posts and release manifest/artifact
reads. `API_TOKEN` also works on those routes; if no agent token is configured,
startup warns that agents then hold an admin credential. `OPERATOR_TOKEN` only
authorizes release offers and restart execution; unset disables those routes.
Every configured token must be at least 32 characters and all three must differ.
Protect credentials at rest and use HTTPS. Agents refuse HTTP beyond loopback
unless explicitly permitted with `--allow-http` or `ALLOW_HTTP=1`. Open development
mode only binds to a literal loopback address.

Agent inventories are untrusted input. Shared agent credentials do not establish
host identity: a compromised agent can impersonate another host, falsify facts,
and consume storage, but cannot use its agent token for admin or operator routes.
Restart resolution only considers `ACTIONS_HOST` inventories and configured exact
Compose paths, then checks container names against live Docker before execution.
Inventories and the confirm/hash flow do not authenticate the reporting host.
Docker socket access grants broad host control even when the socket mount is
read-only; enable it only on a trusted API host, with protected operator authority.
The container runs non-root by default; socket access needs the socket group or
an explicit root override. Restart actions are off by default.

Keep signing keys outside the API and public release tree; distribute public keys
through a trusted channel. Redaction happens at collection but cannot recognize
every secret or sensitive topology detail. Treat inventories, logs and backups
as sensitive. Restrict network access and preserve private filesystem permissions.

First-run setup uses a **first-visitor claim window** lasting 10 minutes after
process start. Until claimed, inventory/admin routes refuse requests. Anyone
who can reach the API during that window can become its administrator. Restrict
access to a trusted network or SSH tunnel until claiming finishes; do not expose
an unclaimed instance to the Internet. Expiry requires a process restart. A stored
or environment API token disables claiming. Credentials are shown only in the
successful claim response, stored at 0600, and never logged. Protect volume backups.

Enrollment codes are bearer credentials: nine unambiguous random characters,
SHA256-hashed at rest, single-use, expire after 15 minutes, and limited to 20
outstanding codes. Redemption hands out the shared AGENT_TOKEN, not per-host
identity. Anyone redeeming a stolen code gains agent authority until that token
is rotated. Protect command history and do not share live codes. Redemption has
uniform 403 failures and a limit of 10 attempts/minute per socket peer IP (429
with Retry-After). Proxy clients share the proxy's limit; forwarded client-IP
headers are ignored. The bounded in-memory rate-limit table resets at restart;
code use/expiry persists. This is a local abuse bound, not distributed DDoS protection.

The served installer runs code with root authority and fetches binaries plus
SHA256SUMS from the same API origin. Verification detects corruption, not an
attacker controlling that origin or plaintext transport. A network attacker
can replace an HTTP installer and checksums, steal enrollment codes/tokens, and
execute code as root. Prefer HTTPS; the generated HTTP command deliberately
includes --allow-http for trusted-LAN use. Inspect the script before execution.
Set PUBLIC_URL to a trusted HTTPS origin behind a proxy. Set TRUSTED_PROXY=1 only
when the proxy overwrites forwarded headers and direct API access is blocked;
strict Host validation prevents shell injection but does not authenticate DNS
or select which otherwise valid hostname a visitor should trust.
