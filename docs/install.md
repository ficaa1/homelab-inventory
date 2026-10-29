# Install

## The server

On a Linux server (amd64 or arm64), as root:

```sh
curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh | sh
```

1. Open the address it prints **within 10 minutes**. The first visitor becomes
   the admin, so keep the port on your own network until then.
2. Save the API token it shows. It is shown once.
3. Optionally set up notifications in the wizard.

Missed the 10 minutes? Restart the service (the installer prints the command)
and open the page again.

Uses Docker Compose if it works on the server, otherwise installs a single
binary with a systemd service. Data lives in `/var/lib/homelab-inventory`.

## Add hosts

The wizard, and later **Admin → Add a host**, gives you a command to paste on
each Linux host. It downloads the agent, checks it, and sends the first report
straight away. After that the host reports every 15 minutes.

Check an agent with:

```sh
systemctl status homelab-discover.timer
journalctl -u homelab-discover.service -n 20
```

If you use plain HTTP, the command contains `--allow-http`. Only do that on a
network you trust: anyone on it could read the token.

## Upgrade or remove

```sh
curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh -o get.sh
sudo sh get.sh                          # upgrade to the latest release
sudo sh get.sh --version v0.1.0         # or a specific one
sudo sh get.sh --uninstall              # remove, keep data
sudo sh get.sh --uninstall --purge      # remove, delete data
```

Back up the data folder before upgrading. Purge asks you to type `DELETE`.

## Installer options

| Option | Default |
| --- | --- |
| `--port N` | 8080 |
| `--dir DIR` | `/opt/homelab-inventory` (installer files) |
| `--data DIR` | `/var/lib/homelab-inventory` (your data) |
| `--version vX.Y.Z` | latest |
| `--docker` / `--no-docker` | auto-detect |
| `--from DIR` | use release files from DIR, download nothing |
| `--prefix DIR` | test install under DIR, no root, no service |
| `--uninstall`, `--purge`, `--yes` | see above; `--yes` skips the purge prompt |

Repeat a custom `--dir` every time you run it. Changing `--data` does not move
existing data; copy it yourself.

## Details

**Docker install.** Runs `ghcr.io/ficaa1/homelab-inventory:<version>` as Compose
project `homelab-inventory`, with `compose.yml` in `--dir` and data mounted at
`/data` (owned by UID 10001). No Docker socket is mounted. Docker itself must
already be installed.

**Binary install.** Checks the download against the release `SHA256SUMS`, then
installs `/usr/local/bin/homelab-inventory`, a `homelab-inventory` service and
system user, and the agent files under `/usr/share/homelab-inventory/agent`. The
service can only write to its data folder. Needs systemd, `curl`, `tar` and
`sha256sum`.

**Mirrors and offline installs.** `INSTALL_BASE_URL` replaces the GitHub download
location (a folder holding `SHA256SUMS` and the archives); `INSTALL_IMAGE`
replaces the Docker image name. To build the release files yourself (Go 1.25):

```sh
umask 022
make release-artifacts VERSION=v0.1.0
sh scripts/get.sh --from "$PWD/dist" --prefix /tmp/inventory-test --no-docker
```

**Manual Compose.** From a checkout, `cd deploy/api && docker compose up -d`,
then open `http://<server>:8080` within 10 minutes and claim it as above.
`API_PORT` changes the published port. `docker compose down -v` deletes your data.

**Preset tokens instead of the wizard.** Copy `deploy/api/.env.example` to
`deploy/api/.env` (mode 0600), set `API_TOKEN` and `AGENT_TOKEN` to different
random values of 32+ characters (`openssl rand -hex 32`), uncomment `env_file` in
`deploy/api/docker-compose.yml`, and start it. There is no claim step. What each
token can do is in [Configuration](configuration.md).

**Agent without the wizard.** Build with `make && make dist`, then copy
`scripts/install-agent.sh`, `bin/discover-linux-amd64` and
`bin/agentupdate-linux-amd64` (or the `arm64` files) to one folder on the host.
Put the agent token in a mode-0600 file and run there:

```sh
sudo sh ./install-agent.sh --api-url https://inventory.example.org \
  --binary-dir "$PWD" --token-file /root/inventory-token
```

Leave out `--token-file` to type the token instead, or use `--enroll CODE` with a
code from Admin → Add a host. Rerun it to change settings;
`--uninstall` removes it. Files go to `/var/lib/homelab-agent` and
`/usr/local/bin`. The agent runs as root so it can see everything; run as
another user it simply collects less.

**One report, no service.** With `discover` and `agentupdate` in `/usr/local/bin`:

```sh
/usr/local/bin/agentupdate -post-only -discover /usr/local/bin/discover \
  -dir "$HOME/.local/state/inventory-agent" \
  -api-url https://inventory.example.org -token-file "$HOME/inventory-token"
```

**TrueNAS SCALE and hosts without systemd.** Put `discover` and `agentupdate` in a
folder on a dataset (system folders are replaced on upgrade), configure once, and
schedule the last command every 15 minutes with the appliance's cron jobs:

```sh
mkdir -p /mnt/example/agent/state && chmod 700 /mnt/example/agent/state
/mnt/example/agent/agentupdate -configure \
  -config /mnt/example/agent/config.json -dir /mnt/example/agent/state \
  -discover /mnt/example/agent/discover \
  -api-url https://inventory.example.org \
  -token-file /mnt/example/agent/token
/mnt/example/agent/agentupdate -config /mnt/example/agent/config.json
```

**Checksums.** They catch a broken download, not a compromised server. Read
`get.sh` before running it as root if you prefer.
