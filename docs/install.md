# Install

Linux amd64 and arm64 agents are supported. Missing Docker, ZFS or Proxmox tools
produce partial inventories and diagnostics. `discover` only reads the host;
posting and optional executable replacement belong to `agentupdate`.

## Server installer

On a Linux amd64 or arm64 server:

```sh
curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh | sh
```

The installer needs root. For an ordinary account it exits with the exact
`curl … | sudo sh -s -- …` command, preserving flags. This works when piped,
without trying to re-execute a script from an already-consumed standard input.
Review the script before granting root authority if you prefer:

```sh
curl -fsSL https://github.com/ficaa1/homelab-inventory/releases/latest/download/get.sh -o get.sh
sudo sh ./get.sh --port 8080
```

It waits up to 30 seconds for `/healthz`, reads `/setup/status`, and prints the
server's LAN URL and the minutes left to claim it. Open the URL within that
window and save the API token shown once. The first visitor becomes the
administrator; keep the port on a trusted network during setup. After claiming,
reruns say “already set up” and preserve your stored credentials. The wizard
can configure notifications and supplies Add host commands. Restart the service
with the command printed by the installer to reopen an expired setup window.

| Option | Default / effect |
| --- | --- |
| `--port N` | 8080; published Docker port or binary listen port |
| `--dir DIR` | `/opt/homelab-inventory`; Compose file and installer metadata |
| `--data DIR` | `/var/lib/homelab-inventory`; persistent private data |
| `--version vX.Y.Z` | Latest release; also accepts prerelease tags |
| `--docker` / `--no-docker` | Auto-detect working `docker compose version`, or require a backend |
| `--from DIR` | Use local release artifacts with no downloads; selects binary mode |
| `--prefix DIR` | Stage a disposable binary installation beneath DIR without root or service changes |
| `--uninstall` | Remove server service and installed files; retain data and system account |
| `--purge` | With `--uninstall`, delete data after typing `DELETE` at the terminal |
| `--yes` | Confirm purge without an interactive prompt |
| `--help` | Show usage |

Paths must be absolute, dedicated directories using letters, digits, `/`, `_`,
`.` or `-`, with no spaces, traversal components or trailing slash. Keep data
separate from installer files. Reruns using the same `--dir` retain the recorded
backend, data directory and port unless explicitly overridden. To change the
backend, uninstall first; check ownership before reusing existing data. Copy
existing data yourself before changing `--data`; the installer does not migrate it.

### Docker or binary

With Docker Compose, the installer pulls
`ghcr.io/ficaa1/homelab-inventory:<release-tag>`, writes `compose.yml` under
`--dir`, and runs it as project `homelab-inventory` with `restart: unless-stopped`.
It bind-mounts `--data` at `/data`, mode 0700, owned by image UID/GID 10001.
No Docker socket is mounted. Docker and the Compose plugin must already be
installed; the installer does not install a container engine.

Without Compose (or with `--no-docker`), it verifies the archive against the
release `SHA256SUMS` before changing installed files. It installs:

- `/usr/local/bin/homelab-inventory` and `homelab-inventory.service`.
- Both agent architectures and their checksums under `/usr/share/homelab-inventory/agent`, so wizard enrollment works without a checkout.
- A system account `homelab-inventory` and a mode-0700 data directory it owns.

The unit limits writes to the data directory, uses a private temporary directory,
and forbids privilege escalation. Ports below 1024 receive only the bind-service
capability. Binary installs require systemd, `useradd`, `curl`, `tar` and
`sha256sum` (or BusyBox's sha256sum), plus system CA certificates for HTTPS.
Docker installs require `curl`. The shell script works with dash and BusyBox sh.

### Upgrade and uninstall

Rerun the one-liner to install the latest release, or pin a version:

```sh
sudo sh ./get.sh --version v0.1.0
```

Repeat a custom `--dir` on every invocation. Docker pulls the selected image and
recreates the container; binary installs replace executables and restart the
service. Both retain data and authentication state. Back up the data directory
before upgrading; checksums detect download corruption, not a compromised release.

```sh
sudo sh ./get.sh --uninstall
sudo sh ./get.sh --uninstall --purge
```

Uninstall keeps data by default. Purge requires the explicit typed confirmation
(or `--yes`) and permanently removes the chosen data directory. It does not
uninstall Docker, delete images, or remove the system account.

### Mirrors and offline staging

`INSTALL_BASE_URL` is the directory containing `SHA256SUMS` and release archives;
it replaces the entire GitHub download base. Use HTTPS and a matching pinned
version for a private mirror. The installer never accepts or prints tokens:
configure mirror access outside the script. `INSTALL_IMAGE` does the same for the
Docker path: it replaces the image repository (the tag is still the version).

```sh
sudo env INSTALL_BASE_URL=https://downloads.example.org/inventory/v0.1.0 \
  sh ./get.sh --version v0.1.0 --no-docker
```

For latest, the manifest must name exactly one amd64 release, and its matching
archives must stay available at the same base. On GitHub the version is resolved
from that manifest; no GitHub API call is needed. Docker uses the GHCR image even
when binary downloads use a mirror. While the repository and package are private,
anonymous downloads will not work; use a configured mirror or copied artifacts.

Build exactly the files shipped by the release workflow with Go 1.25:

```sh
umask 022
make release-artifacts VERSION=v0.1.0
sh scripts/get.sh --from "$PWD/dist" --prefix /tmp/inventory-test --no-docker
```

`--prefix` stages the binary, unit and agent tree without `useradd`, `systemctl`,
or starting a background service. It prints the command to launch the staged API
manually. Paths from `--dir` and `--data` are also placed under the prefix. The
health/setup wait applies to real service installs. Use `--from` on a real host
without `--prefix` to install copied release artifacts with no downloads. The
script still makes local HTTP health checks after starting that service.

## Manual Compose option

1. From a checkout, run Compose. No `.env` or hand-written tokens are needed:

   ```sh
   cd deploy/api
   docker compose up -d
   ```

2. Open `http://localhost:8080` (or your server's address) **within 10 minutes**.
   Claim the instance in the wizard and save the API token when it is shown once.
   You can optionally configure and test Apprise notifications. If the window
   expires, restart the API container and reopen the page. The first visitor
   can claim it: restrict access until you finish setup.

3. Copy the wizard's Add host command and paste it on each Linux host. It downloads
   the matching amd64/arm64 binaries, checks SHA256SUMS, redeems a single-use code,
   installs a timer and posts the first inventory immediately. Watch hosts appear
   in the wizard. Admin → Add a host provides more commands later.

The image includes the installer and agent binaries. Commands need `curl`,
`sha256sum`, a POSIX shell and systemd; use the manual instructions below for
appliances. HTTP commands include `--allow-http`: only run them on a trusted
network, or configure HTTPS before adding hosts. Review the served script before
running it with root authority. Checksums fetched over HTTP do not prevent an
active network attacker from replacing both the script and binaries.

`API_PORT` changes the published port; the container listens on 8080. The named
volume holds inventories and private `state-auth.json` (generated credentials and
hashed enrollment codes). It survives image replacement and API restarts. Back it
up securely; `docker compose down -v` deletes it. No Docker socket or host mounts
are required. Never publish an unclaimed instance to an untrusted network.

If `/install.sh` returns 404, its response explains that agent binaries are
missing. Use the packaged image, set `AGENT_BIN_DIR` to the expected tree, or use
manual setup below. The server installer includes those files; an unpackaged local API build does not.

## Manual setup

For preconfigured credentials, copy `deploy/api/.env.example` to
`deploy/api/.env`, chmod it 0600, and set distinct random `API_TOKEN` and
`AGENT_TOKEN` values (at least 32 characters each; `openssl rand -hex 32` can
produce one locally). Uncomment `env_file` in `deploy/api/docker-compose.yml`,
then run `docker compose up -d --build` from `deploy/api`. An environment API token
skips the claim wizard. Enter it at the normal dashboard token gate.

Environment tokens override saved ones individually. Invalid/short/colliding
credentials refuse startup. A stored API token also skips claiming after a
restart. To deliberately reset setup, first stop the API, securely back up and
remove `state-auth.json`, and remove environment API credentials. This also
invalidates stored enrollment codes. `-allow-unauthenticated` remains a separate
loopback-only development mode (`make api`); it does not run the claim flow.

Use HTTPS through your own reverse proxy for access beyond a trusted local
machine. Tokens travel in an Authorization header and must not cross plaintext
untrusted networks. There is no browser login provider.

**Token scope:** keep `API_TOKEN` for the dashboard/admin and MCP. Give agents
`AGENT_TOKEN`, which only permits posting inventories and downloading signed
releases. If unset, agents must use `API_TOKEN`; startup warns that they then
hold an admin credential. Agents share a credential and can impersonate hostnames;
inventories are untrusted input. Rotate the agent credential on the API and all
agents if any agent is compromised.

`OPERATOR_TOKEN` only authorizes release offers and restart execution. It is
optional; unset disables both routes. All three tokens must differ, be at least
32 characters when set, and have no surrounding whitespace or CR/LF. The dashboard
prompts separately for operator authority and keeps it for the browser session.
Never put tokens in a command argument, URL or Git.

### Install an agent with a token file

From the repository root (`cd ../..` if still in `deploy/api`), build static
binaries with Go 1.25 or newer:

```sh
umask 022
make
make dist
```

The installer is self-contained: copy just `scripts/install-agent.sh`,
`bin/discover-linux-amd64` and `bin/agentupdate-linux-amd64` to one directory on
the target (use the `arm64` filenames on an arm64 host). Preserve the artifact
names. No `deploy/agent` files, checkout, signer or private signing key is needed
there. In the checkout, `--binary-dir "$PWD/bin"` uses the `make dist` output.

Create a private mode-0600 file on the target containing only `AGENT_TOKEN`
(or `API_TOKEN` if a separate agent token is not configured). Then, in the
copied directory **on the target host**:

```sh
sudo sh ./install-agent.sh --api-url https://inventory.example.org \
  --binary-dir "$PWD" --token-file /root/inventory-token
```

Omit `--token-file` to enter it at a hidden terminal prompt. `--binary-dir DIR`
selects a different artifact directory. Repo-layout and `--binary-dir` modes use local files. `--download` fetches
binaries and SHA256SUMS from the API. `--enroll CODE` replaces `--token-file`
with a one-time code redemption; the resulting agent token is never printed. Review checksums from a trusted release before running copied executables.
Checksums served by the same origin detect corruption, not a compromised server.

HTTPS is required for agent API URLs beyond loopback. A LAN without TLS must
explicitly accept plaintext credential exposure with installer `--allow-http`
or `ALLOW_HTTP=1`. For example, add `--allow-http` for `http://192.0.2.10:8080`.
The choice is saved in `config.json` for scheduled reports and signed downloads.
Direct updater use accepts `-allow-http` or `ALLOW_HTTP=1`. This does not add TLS;
other machines on the network may see the credential.

The installer writes root-only configuration under `/var/lib/homelab-agent`,
installs `/usr/local/bin/discover` and `homelab-discover`, and starts a first report.
The systemd service and timer are embedded in the installer; the timer reports
every 15 minutes. Collection runs as root to read process owners, crontabs and platform state. An unprivileged manual invocation collects
less information. Check outcomes with:

```sh
systemctl status homelab-discover.timer
journalctl -u homelab-discover.service -n 20
sudo /usr/local/bin/homelab-discover
```

Rerun the same installer to update configuration or the pinned runner. Signed
replay state is preserved. To remove the schedule, binaries and credentials:

```sh
sudo sh ./install-agent.sh --uninstall
```

Signed releases/public key/state remain for safe future enrollment. Archive or
remove that private state only when deliberately decommissioning the host.
For a disposable local test, `--prefix /absolute/test/root` writes everything
beneath that directory, runs one report, and never invokes systemd or requires root.

## Manual one-shot posting

The shortest fallback uses the same safe poster, without installing a service.
Copy `bin/agentupdate-linux-amd64` to `/usr/local/bin/agentupdate` and
`bin/discover-linux-amd64` to `/usr/local/bin/discover` (or use arm64 artifacts):

```sh
/usr/local/bin/agentupdate -post-only -discover /usr/local/bin/discover \
  -dir "$HOME/.local/state/inventory-agent" \
  -api-url https://inventory.example.org -token-file "$HOME/inventory-token"
```

It checks the collector's exit status, caps output, validates the JSON, uses a
mode-0600 temporary inventory and bounded collection/HTTP deadlines, refuses
redirects, and logs only outcomes. It preserves partial inventories.

For a manual `curl` fallback, create a private curl configuration file containing
`header = "Authorization: Bearer <your agent token>"` in your editor (mode 0600).
Use an absolute collector path. Never use `set -x` around credentials.

```sh
umask 077
report=$(mktemp)
trap 'rm -f "$report"' EXIT HUP INT TERM
if timeout 360 /usr/local/bin/discover -compact > "$report"; then
  curl --fail --silent --show-error --connect-timeout 10 --max-time 60 \
    --config "$HOME/inventory-curl.conf" \
    -H 'Content-Type: application/json' --data-binary "@$report" \
    https://inventory.example.org/inventory > /dev/null
else
  echo 'Collection failed; nothing posted' >&2
  exit 1
fi
```

The wrapper is preferable for recurring reports: it also enforces the output
limit, validates inventory identity/schema, and locks against overlapping runs.

## TrueNAS SCALE and other non-systemd hosts

Use a persistent, executable dataset directory of your choosing; appliance
updates can replace system directories. From `make dist`, copy
`bin/discover-linux-amd64` and `bin/agentupdate-linux-amd64` there, renaming them
to `discover` and `agentupdate` respectively (use the arm64 artifacts if applicable).
As an administrator, create a private token file and state directory, then run:

```sh
mkdir -p /mnt/example/agent/state
chmod 700 /mnt/example/agent/state
/mnt/example/agent/agentupdate -configure \
  -config /mnt/example/agent/config.json -dir /mnt/example/agent/state \
  -discover /mnt/example/agent/discover \
  -api-url https://inventory.example.org \
  -token-file /mnt/example/agent/token
/mnt/example/agent/agentupdate -config /mnt/example/agent/config.json
```

Use your appliance's supported cron-job interface, or a crontab on an ordinary
Linux host, to run that final command every 15 minutes. For TrueNAS SCALE, choose
the intended account explicitly and verify the dataset is mounted after reboot.
No automatic sudo rules or appliance configuration changes are installed. Root
cron collects more facts; unprivileged cron keeps the same read-only invariant
but can return less information. Protect the directory and token from other users.
