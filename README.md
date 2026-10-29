# homelab-inventory

![homelab-inventory: overview, launcher, alerts, disk forecast, network chart, media and uploads, running on a demo fleet](docs/images/demo.gif)

A dashboard for your homelab. A small agent on each Linux host reads what is
running (containers, disks, ZFS, Proxmox, ports, packages) and reports it every
15 minutes. The dashboard shows it all in one place, warns you before things
break, and links to every service it finds.

The agent only reads. It never changes anything on your hosts.

## Install

On a Linux server, as root:

```sh
curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh | sh
```

Then:

1. Open the address it prints **within 10 minutes**. The first person to open it
   becomes the admin, so keep it on your own network until you have.
2. Save the API token it shows you. It is shown once.
3. Copy the **Add host** command and run it on each Linux host.

It uses Docker if you have it, otherwise a single binary with a systemd service.
More options, upgrades and removal: [Install](docs/install.md).

## What you get

- **Overview** of every host, with what needs attention.
- **Alerts** for full disks, failed units, unhealthy containers, ZFS and backup
  problems. Notifications (through [Apprise](https://github.com/caronc/apprise))
  only fire when something changes. [All alerts](docs/alerts.md)
- **Forecasts** of when a disk will fill up.
- **Launcher** tiles for every web service it finds.
- **History**: what changed on a host, and package versions.
- **Plugins** for media (Sonarr, Radarr, Tautulli, Overseerr), uploads (folder,
  Paperless-ngx, Calibre-Web) and network traffic from Prometheus.
  [Plugins](docs/plugins.md)
- Optional: [sign-in](docs/configuration.md#browser-sign-in-oidc) with Google,
  Authentik, Authelia or Keycloak, [signed agent updates](docs/self-update.md),
  [container restarts](docs/restarts.md), and an MCP server for AI assistants.

![Fleet overview with synthetic hosts](docs/images/home.png)

## Good to know

- Linux hosts (amd64, arm64). Proxmox, TrueNAS and ZFS are picked up when present.
- Use HTTPS (through your own reverse proxy) beyond a trusted network.
- Docs: [install](docs/install.md), [configuration](docs/configuration.md),
  [plugins](docs/plugins.md), [alerts](docs/alerts.md), [API](docs/api.md),
  [security](SECURITY.md), [contributing](CONTRIBUTING.md).
- Status: v0.1, early. Licensed under [AGPL-3.0](LICENSE).
