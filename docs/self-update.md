# Signed agent updates

Off by default. When on, agents install new versions of `discover` that you
signed, first on a few test hosts, then everywhere once you promote them.

## 1. Make a key and a release

On a machine you trust (not the API server's container), from a checkout:

```sh
export SIGNER_KEY="$HOME/.config/homelab-inventory/release.key"
export SIGNER_DIR="$HOME/inventory-releases"
make keygen > release-public.key
make release VERSION=v0.1.0
```

Back up the private key somewhere offline. It never goes near the API.

## 2. Serve it

Copy `SIGNER_DIR` to the API server, mount it read-only at `/agent-releases`,
and set:

```text
AGENT_RELEASE_DIR=/agent-releases
AGENT_CANARY_HOSTS=host-a,host-b
```

The canary hosts get each new release first. Leave it empty to roll out only
when you promote.

## 3. Switch hosts to signed updates

Give each host `release-public.key` through a channel you trust, then install the
agent with `--signed` (the other files are as in [Install](install.md#details)):

```sh
sudo sh ./install-agent.sh --binary-dir "$PWD" --api-url https://inventory.example.org \
  --token-file /root/inventory-token --signed \
  --public-key /root/release-public.key --manifest /root/release/manifest.json \
  --artifacts /root/release
```

`manifest.json` is the release's `latest.json`; `/root/release` holds that
release's files.

## 4. Roll out

Check the canaries in **Hosts**. Then, on the signer:

```sh
make promote   # offer the release to every host
make hold      # stop offering it (installed hosts keep it)
make auto      # back to canaries only
```

You can also promote from the dashboard with the `OPERATOR_TOKEN`. `make hold`
always wins.

## If a release is bad

A host that cannot report with a new release goes back to the previous one by
itself. To go back by hand, hold the release and run on the host:

```sh
sudo /var/lib/homelab-agent/agentupdate -dir /var/lib/homelab-agent -recover
```

To ship an older version again, publish it as a new release. Release numbers
only go up.

## Details

**What is checked.** Each release is signed with Ed25519; hosts check the
signature, file sizes and hashes, and run a read-only self-test before using it.
Only `discover` updates itself. `agentupdate`, which does the checking, is
upgraded by rerunning the installer. Reports never decide a rollout: an agent
token can download releases but cannot sign or promote them.

**Platforms.** `AGENT_PLATFORMS` (default `linux/amd64,linux/arm64`, plus
`freebsd/amd64` if you want it) sets what each release contains.

**Signing over SSH.** Set `SIGNER_HOST=operator@192.0.2.10`, `SIGNER_PORT`, and
absolute remote `SIGNER_BIN`, `SIGNER_KEY` and `SIGNER_DIR`. The build is copied
to the signer and signed there; the key never leaves it.

**Existing plain host.** Without reinstalling, stop its timer, then:

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

**Dashboard offers.** `POST /agent/offers` with `Authorization: Bearer <operator
token>` and `{"action":"promote","sequence":1,"confirm":true}`. Other actions:
`auto`, `hold`, and `update` with a `host`. A new release clears earlier choices.

**New key.** `make hold`, stop every agent's timer, back up the release folder,
make a new key at a different path, and run
`SIGNER_KEY=/private/new.key scripts/release-agent.sh resign`. Give hosts the new
public key, restart their timers, check they report, then `make auto` or
`make promote`. If the old key leaked, check what is already installed before
you resume.

**Never** reset `latest.json` to an older release, delete a host's update state
to force a downgrade, or mount the private key into the API.
