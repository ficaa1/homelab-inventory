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

When the server has a signed release configured, enter the target's exact hostname
and check signed-enrollment readiness before copying the command. It includes
`--updates slots` and converts an existing public installation with the same one
paste. Existing hosts are not converted by a server upgrade. A configured canary
gets the candidate; other hosts get the last signer-promoted baseline. Before the
first promotion, only canaries can enroll. Hold pauses signed enrollment too.
Follow [the signing and rollout runbook](self-update.md) to install a native
signer, publish, migrate canaries, verify them, promote and migrate the rest.

The installer and launcher are trusted on first use. Use a trusted HTTPS origin
and compare the release-key fingerprint in Admin with the signer through an
independent channel. A checksum served beside a binary does not authenticate the
server. The installer pins the release key for later updates. An enrollment code
is short-lived; the agent token stays in a private file. If installation is
interrupted, rerun the same command without deleting migration/replay state;
redemption retry uses a persisted nonce. Obtain a fresh code if its retry window
has expired.

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
`--uninstall` removes its schedule and credentials while retaining signed replay
state. Files go to `/var/lib/homelab-agent` and
`/usr/local/bin`. The agent runs as root so it can see everything; run as
another user it simply collects less.

**One report, no service.** With `discover` and `agentupdate` in `/usr/local/bin`:

```sh
/usr/local/bin/agentupdate -post-only -discover /usr/local/bin/discover \
  -dir "$HOME/.local/state/inventory-agent" \
  -api-url https://inventory.example.org -token-file "$HOME/inventory-token"
```

## TrueNAS SCALE and LXC

For a systemd LXC, use the generated Linux root command inside the guest. If it
has no SSH service, open its console from the hypervisor (for example, `pct enter
VMID`) and paste there. The guest downloads its own artifacts. Do not pass the
token through `pct exec` stdin. Verify both privileged and unprivileged canaries;
the timer does not require systemd namespace hardening that those guests lack.

For TrueNAS SCALE choose **TrueNAS / cron** in Add a host. Supply an absolute
path on a mounted, executable pool dataset, for example
`/mnt/example/homelab-agent`. Use a new product directory when replacing an
unrelated private agent. Paste the generated root command in the appliance
console. Its option shape is:

```sh
curl -fsSL https://inventory.example.org/install.sh | sh -s -- \
  --enroll CODE --updates slots --scheduler truenas-cron \
  --state-dir /mnt/example/homelab-agent
```

`CODE` stands for the fresh code from the UI. To adopt a predecessor schedule,
choose its explicit numeric job ID or exact description in the UI, which adds
`--replace-schedule ID`. Ambiguous descriptions and duplicate matching jobs
refuse; do not guess a job or disable unrelated schedules manually.

The installer validates the effective dataset mount, rejects symlink/noexec
paths, and puts the launcher, runtime pair, private token, pinned key and state
on that dataset. It creates one root TrueNAS configuration-DB cron job, identity
`homelab-inventory-agent`, running the absolute `agentboot -config ...` command
every 15 minutes. It does not rely on changes under `/usr` or `/etc` surviving an
appliance upgrade. The launcher rechecks the dataset mount before touching state,
so an unavailable pool does not turn an empty mountpoint into a fresh install.

The selected predecessor's configuration is snapshotted and its identity checked
again before cutover. Only after an accepted new report does the installer disable
that exact predecessor and activate the new job. On cutover failure it restores
the saved schedule. Keep the old directory, key and replay state: a different
product's high-water mark must not be imported into this product's trust domain.
Interrupted migration retains its journal for the next installer run.

Check Hosts for signed mode, the expected updater and sequence, fresh accepted
inventory and committed operation. Verify two more scheduled reports and check
that only the intended cron job is active. Validate persistence across an actual
TrueNAS boot-environment upgrade before relying on fleet rollout. Unit fixtures
do not establish appliance or LXC behavior.

**Checksums.** They catch a broken download, not a compromised server. Read
`get.sh` before running it as root if you prefer.

For local signed installation or migration, `--updates slots` installs the
launcher and both signed runtime components. Supply trusted `--binary-dir`,
`--public-key`, `--manifest` (the release's `N/runtime.json`), `--artifacts` and
`--token-file`, or use the generated download/enrollment command above. Rerunning
the installer upgrades the launcher while preserving the floor and runtime.
Legacy signed conversion validates the existing key and replay state and refuses
a baseline below its floor, including a rejected higher sequence. Publish a higher
repair release rather than deleting state or forcing a downgrade. See
[runtime slots](self-update.md#runtime-slots).
