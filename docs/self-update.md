# Signed agent updates

Plain posting is the default. Agents use `AGENT_TOKEN` for posts and release downloads;
`API_TOKEN` also works, but gives the host admin authority. `/agent/status` requires
`API_TOKEN` and is not used by the updater. HTTPS is required beyond loopback unless
installer `--allow-http`, updater `-allow-http`, or `ALLOW_HTTP=1` explicitly permits
plaintext LAN traffic. The installer saves that choice in `allow_http`. Signed updates replace only `discover`. The pinned
`agentupdate` reporting supervisor is upgraded manually with the local installer.
It verifies Ed25519 signatures, artifact size/hash and a read-only self-test before
running a release. Inventories never authorize promotion. Keep the signing private
key on a trusted signer machine, outside the release tree and API container.

## Create a release

On the signer machine, from the checkout:

```sh
export SIGNER_KEY="$HOME/.config/homelab-inventory/release.key"
export SIGNER_DIR="$HOME/inventory-releases"
export AGENT_PLATFORMS=linux/amd64,linux/arm64
make keygen > release-public.key
make release VERSION=v0.1.0
```

`keygen` refuses to overwrite an existing key. Keys must be signer-owned regular
files with mode 0600; the signer need not be root. Preserve a secure offline backup.
`SIGNER_INPUT` defaults to `bin`; `SIGNER_BIN` defaults to `./bin/agentsign`.
`AGENT_PLATFORMS` is parsed by the signer for Makefile builds and publication;
its signed list travels in the manifest and is validated by every updater.
Linux amd64/arm64 are the defaults. `freebsd/amd64` is an optional thin-inventory
build; the local installer supports Linux only. Do not remove a fleet platform
until those hosts stop using that release channel.

Copy only the public release tree to an API-readable location and mount it
read-only as `/agent-releases`; set `AGENT_RELEASE_DIR=/agent-releases`.
The tree contains numbered artifact directories, `latest.json`, `public.key` and
`control.json`. Control metadata contains only sequence and mode, no secrets, and
is written mode 0644 like the public manifests. Never mount `SIGNER_KEY` in the API or serve it over HTTP.
If the signer publishes to a local directory shared with the API, API writes must
still be prevented with a read-only mount. For a separate signer, transfer the
complete tree atomically while held, then expose it. Preserve old sequences and
never reset `latest.json` to an older release.

For optional SSH execution, preinstall a trusted native `agentsign` on the signer,
set `SIGNER_HOST=operator@192.0.2.10`, `SIGNER_PORT=22`, and set absolute remote
`SIGNER_BIN`, `SIGNER_KEY` and `SIGNER_DIR` paths. `LOCAL_SIGNER_BIN` selects the
local signer used to parse the build contract (default `./bin/agentsign`). The
release script stages discover artifacts over SSH and signs there; it never
copies the private key back. No automatic sudo is used. SSH paths and version
strings accept only conservative shell-safe characters.

## Enroll or upgrade a plain host

Transfer the public key through a trusted channel and compare its fingerprint
out of band. Copy `latest.json` as `manifest.json` and that release's discover artifacts to the host.
Copy the self-contained installer and matching `make dist` discover/agentupdate
artifacts to one directory as described in [install](install.md). A first signed installation can use:

```sh
sudo sh ./install-agent.sh --binary-dir "$PWD" --api-url https://inventory.example.org \
  --token-file /root/inventory-token --signed \
  --public-key /root/release-public.key --manifest /root/release/manifest.json \
  --artifacts /root/release
```

An existing plain installation can opt in **without reinstalling its schedule**:

```sh
sudo cp /root/release-public.key /var/lib/homelab-agent/public.key
sudo chmod 600 /var/lib/homelab-agent/public.key
sudo /var/lib/homelab-agent/agentupdate -dir /var/lib/homelab-agent \
  -public /var/lib/homelab-agent/public.key \
  -initialize /root/release/manifest.json -input /root/release
sudo /var/lib/homelab-agent/agentupdate -configure \
  -config /var/lib/homelab-agent/config.json -dir /var/lib/homelab-agent \
  -public /var/lib/homelab-agent/public.key -discover /usr/local/bin/discover \
  -api-url https://inventory.example.org \
  -token-file /var/lib/homelab-agent/token -signed
sudo /usr/local/bin/homelab-discover
```

Stop the timer and wait for a running service before manual enrollment; resume it
afterward. Initialization refuses existing replay state. Installer reruns with
`--signed --public-key ...` retain it and require the same pinned key.

## Canary, promote and hold

Set `AGENT_CANARY_HOSTS=host-a,host-b` in the API environment to name explicit
inventory hostnames. An empty value means promote-only. A new release starts in
`auto`: only configured canaries and explicitly offered hosts receive it. Hostnames
are routing hints, not security identities; an agent credential can download
signed artifacts but cannot sign or authorize rollout changes.

Inspect `GET /agent/status`, agent status in Hosts, and the resulting inventories.
A trial must report successfully without error diagnostics before being committed.
Promotion is always an explicit operator decision; uploaded status is display data.
On the signer:

```sh
make promote  # offer current release to every host
make hold     # stop new offers; does not undo an installed release
make auto     # restore configured canary selection
```

These controls need no API operator token. The signer hold overrides all browser
choices. Browser changes require a distinct `OPERATOR_TOKEN` (at least 32
characters), use `Authorization: Bearer <operator token>` on `POST /agent/offers`,
and include JSON `{"action":"promote","sequence":1,"confirm":true}`. Other actions
are `auto`, `hold`, and `update` (with an exact `host`). Unset operator token disables
that route. The dashboard discovers this via `GET /capabilities` with the API
token, then prompts separately for the operator credential on an action. A 403
clears the saved session token. A new sequence clears old browser decisions. Browser decisions can
supersede signer `auto`/`promote`; use signer `hold` for an unconditional stop.

## Rollback and key rotation

If a trial cannot collect/post successfully, or a pending trial is interrupted,
the next supervisor run restores its previous bundle. Established releases keep
posting useful partial inventory. For a later discovered problem, hold releases
and run locally on the affected host:

```sh
sudo /var/lib/homelab-agent/agentupdate -dir /var/lib/homelab-agent -recover
```

A previous signed release must exist. The high-water sequence never decreases;
the rejected sequence cannot be replayed. To redistribute an older executable,
publish its bytes as a **new higher sequence**, with its actual build version and
revision. The signer CLI uses the checkout's revision; use a matching checkout
when publishing an older build. Back up private agent state before manual repair;
never delete replay state to force a downgrade.

For planned key rotation: signer `hold`, stop every agent schedule and wait for
running reports, back up the public tree, create a new private key with a different
path, then run `SIGNER_KEY=/private/new.key scripts/release-agent.sh resign`.
This re-signs the held current manifest without changing hashes or sequence.
Distribute the new public key to each host through the trusted manual channel,
resume schedules, verify reports, then `auto` or `promote`. Old releases remain
signed with the old key; only the current envelope is rotated. If the signing key
was compromised, first stop schedules and offers; evaluate already installed
executables before resuming. There is no online key enrollment or revocation service.
