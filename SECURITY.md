# Security

Report a vulnerability privately: **Security → Report a vulnerability** on this
repository.

## Keep in mind

- **Claim it quickly.** For 10 minutes after first start, whoever opens the
  dashboard becomes its admin. Keep it on your own network, or behind an SSH
  tunnel, until you have claimed it.
- **Use HTTPS** beyond a trusted network. Tokens and Add host codes travel with
  each request, and the Add host command runs as root.
- **Three tokens, three jobs.** `API_TOKEN` runs the dashboard, `AGENT_TOKEN` only
  sends reports, `OPERATOR_TOKEN` only promotes updates and restarts containers.
  Give hosts the agent token, never the API token.
- **Hosts are not verified.** Agents share one token, so a compromised host can
  pretend to be another and send false data. It cannot use the admin features.
  If one is compromised, change `AGENT_TOKEN` on the API and on every agent.
- **Restart actions mean Docker access**, and Docker access means control of that
  host. They are off by default.
- **Inventories are sensitive.** Secrets are removed when collected, but not every
  secret can be recognised, and the data maps your network. Protect the data
  folder and its backups.
- **Keep signing keys** off the API server.

## Details

**Add host codes** are single-use, expire after 15 minutes, are stored hashed,
and are limited to 20 at a time and 10 tries a minute per address. Redeeming one
hands out the shared agent token.

**Behind a reverse proxy**, set `PUBLIC_URL` to your HTTPS address. Set
`TRUSTED_PROXY=1` only if the proxy overwrites `X-Forwarded-*` headers and the
API cannot be reached directly.

**Plain HTTP** is refused by agents beyond loopback unless you pass
`--allow-http` or `ALLOW_HTTP=1`. Anyone on that network could then read tokens,
or swap the installer and its checksums.

**Development mode** (`-allow-unauthenticated`) only listens on loopback.

**Plugins** run inside the API and are trusted code. They can only reach the
addresses you configured for them, and cannot send HTML or scripts to the page.
