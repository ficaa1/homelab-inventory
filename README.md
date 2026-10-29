# homelab-inventory

A read-only inventory agent, API, and single-file dashboard for a Linux homelab. It collects hosts, Docker, Proxmox, ZFS, ports and storage, and presents alerts and history.

Features include packages, drift and trends, transition-only notifications via Apprise, a service launcher, optional signed agent self-update, optional restart actions, and an MCP bridge for assistants.

## Quick start

1. Clone and start the API; no `.env` is needed:

   ```sh
   git clone https://github.com/ficaa1/homelab-inventory
   cd homelab-inventory/deploy/api
   docker compose up -d
   ```

2. Open `http://localhost:8080` (or your server's address) within **10 minutes**.
   Claim the instance and save the API token shown by the setup wizard. Restrict
   access during setup: the first visitor becomes the administrator.
3. Paste the wizard's Add host command on each Linux host. It installs the agent
   and sends the first report. Use Admin → Add a host for later additions.

Use HTTPS for untrusted networks. HTTP installer commands explicitly opt in to
plaintext credential transport. Signed updates and restart actions remain opt-in.

See [installation](docs/install.md), [configuration](docs/configuration.md), [self-update](docs/self-update.md), and [restart actions](docs/restarts.md).

| Program | Purpose |
| --- | --- |
| discover | Collect a host inventory |
| agentupdate | Post inventories; optionally supervise signed discover updates |
| agentsign | Sign agent releases |
| api | Ingest inventories and serve the dashboard and API |
| mcpbridge | Expose inventory reads over MCP |
| portmap | Summarize listening ports |
| alerts | Evaluate inventory alerts |

Discover uses allowlisted read-only commands and redacts sensitive data at collection. A null section means unavailable or not collected; an empty section means collected with no results. Alert notifications fire on transitions.

Linux agents are supported. A FreeBSD build exists with thinner collection. Proxmox, TrueNAS and ZFS are detected when present.

Status: v0.1, early. Licensed under [AGPL-3.0](LICENSE).

![Fleet overview with synthetic hosts](docs/images/home.png)

![Host details and containers with synthetic data](docs/images/host.png)

![Port map with synthetic hosts](docs/images/ports.png)
