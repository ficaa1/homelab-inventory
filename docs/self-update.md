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
Legacy signed installs update only `discover`; their `agentupdate` is upgraded
by rerunning the installer. Slots installs update both runtime components
together, as described below. Reports never decide a rollout: an agent
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

## Update health in Hosts

Hosts and alerts use the same update-health projection. Running, offered and
rejected release numbers are separate; the updater version identifies the
installed posting/checking binary. Legacy installs require an installer rerun
to upgrade it; slots installs replace it in the signed pair. A missing status field is never treated as proof of an unsigned install.

| State | Meaning |
| --- | --- |
| Signed updates not set up | The server has no `AGENT_RELEASE_DIR`. This is normal and does not alert. |
| Unsigned — updates off | The poster explicitly reports unsigned mode. Adding a server release directory does not enroll it. |
| Update status not reported | A legacy or ambiguous agent has not reported update capability. |
| Held | Release offers are paused. Installed releases keep running. |
| Waiting for canary/promotion | This host is outside the release's eligible offer set. |
| Updating | The candidate is being trialled or awaits accepted inventory delivery. |
| Current | The host reports successful operation at its eligible release. |
| Behind | The host runs below its eligible offer. |
| Failed | An update operation failed. The host page shows the first safe reason; older updaters show “reason not reported by this updater”. |
| Rolled back | The updater restored the previous release after a collector failure. |
| Blocked: release N was rejected | Release N failed a real trial and is quarantined on this host. Publish a higher signed sequence to repair it; do not delete state or lower the sequence. |

Preparation failures (download, verification, storage, permissions and self-test)
are retried without reserving a release number. The activation transaction
reserves the anti-replay floor and fallback together. A locally healthy candidate
whose inventory POST fails remains a resumable pending transaction, rather than
being rejected. Only a candidate crash, timeout, invalid output or new error
diagnostic causes quarantine. Diagnostics already present in a fresh collection
from the committed release do not fail the trial; only collector/code identities
are retained, never diagnostic message text.

The updater preserves the first failure until successful recovery. A successful
old-release inventory or a no-offer check does not erase a newer release's failure.
Old state with running sequence 5 and high-water sequence 6 migrates atomically
in place to rejected sequence 6. Its safe reason is “rejected by an earlier
updater”; its code is `unknown`. Later checks stay blocked without increasing the
counter. A higher signed release can still install. The existing floor is never
lowered. New state also pins the signed manifest's exact digest, so different
bytes at an already reserved sequence are refused.

`agent.update.unhealthy` replaces `agent.outdated`. It warns immediately on a
reported failure, rollback or rejection, or after a host has been below its
eligible offer for more than 45 minutes. The API stores the first observed lag
and incident under its data directory, so restarting it does not restart the
budget. The same unresolved incident escalates to critical after 24 hours.
Notifications use the existing transition, silence and severity-floor settings:
repeated checks stay quiet, with one escalation and a recovery transition.
A critical notification floor means only a long-stuck update reaches delivery.
A fresh installation of notification state still seeds existing conditions
silently, as for other alerts.

An incident clears only on a fresh server receipt showing successful operation
at or above the eligible offer, or withdrawal of the offer. A hold labels the
incident paused and keeps it active. An unreadable release directory or offer
journal keeps the last known host state and raises one fleet warning,
`agent.release.unavailable`, for host `api`. An unconfigured release directory
never raises that warning.

## Independent status endpoint

`POST /agent/status` accepts the agent bearer credential and `application/json`,
with a maximum body of 8192 bytes. It stores only the latest document per host,
with a server-generated receipt time, under the private data directory. For
example:

```json
{"host":"host-a","update":{"mode":"signed","updater":"v1.0.0","sequence":5,"target":6,"result":"failed","failures":1,"stage":"download","code":"network_unreachable"}}
```

The updater posts on every run, including when discovery cannot produce an
inventory, the state cannot be loaded, or the lock is busy. It also includes
`agent_update` in inventory for compatibility, preserving discover's fields and
validating the exact merged bytes. The API selects the newer channel by server
receipt time, never the host's collection clock. Receipts and the health ledger
are written atomically with mode 0600. `GET /agent/status` exposes the same host
projections and release controls to dashboard readers, including when signed
updates are not configured.

Status contains safe stage/code enums, bounded counters and sticky
`firstFailure`/`lastFailure` records. It never includes raw errors, stderr, paths,
URLs or response bodies. Shared agent-token telemetry does not authorize offers
or attest to a host's identity. Signed enrollment and conversion remain separate follow-up work. These
reporting changes reach old installations only when a new updater is installed.

## Runtime slots (fresh installations)

`--updates slots` installs a signed `discover` and `agentupdate` pair. The
root-owned `agentboot` launcher holds the key, token and sequence floor. Both
binaries can then update together. Upgrade the launcher by rerunning the
installer. Launcher banks and launcher self-update are not implemented.

Publish with `make release` as above. It now signs two envelopes at the same
sequence: `latest.json` is the legacy discover-only offer; `N/runtime.json` is
protocol 1 for slots. Old signed and unsigned installations keep their current
behavior. They are not converted when the API is upgraded. A release directory
made by an old signer continues to work for legacy hosts but cannot enroll or
update a slots host. Retain old sequence directories for in-flight reports and
artifact downloads; the API verifies each retained envelope before serving only
its listed artifacts.

On a fresh Linux systemd host, supply a trusted launcher, public key and signed
baseline through your existing trusted channel:

```sh
sudo sh ./install-agent.sh --updates slots --binary-dir "$PWD/bin" \
  --api-url https://inventory.example.org --token-file /root/inventory-token \
  --public-key /root/release-public.key --manifest /root/releases/1/runtime.json \
  --artifacts /root/releases/1
```

For a disposable test use `--prefix /absolute/test-root`; it runs the first
report without systemd. This path refuses an existing plain or legacy signed
installation. Enrollment, conversion, a promoted bootstrap selector and TrueNAS
cron are separate follow-up work. Choose the baseline deliberately; the local
installer does not bypass the operator's responsibility to select it.

Under `/var/lib/homelab-agent`:

- `agentboot`, `public.key`, `token`, `config.json`: installer-managed launcher,
  trust and credentials. The service and wrapper always invoke `agentboot`.
- `slots/a/DIGEST/` and `slots/b/DIGEST/`: immutable `discover`, `agentupdate` and
  the exact signed `runtime.json`. The baseline starts in both banks as one
  known-good identity. Each new pair has a new directory; no executable is
  overwritten while it runs.
- `control.0`, `control.1`: checksummed mirrors containing the floor, exact
  manifest digest, active and fallback identities, transaction and first failure.
  Readers choose the highest valid generation without taking `lock`.
- `report.json`: one bounded validated report, retained only during delivery.
  `lock` is a stable inode and is never unlinked to break contention.

The transaction moves through these states:

| State | Meaning |
| --- | --- |
| prepared | Both artifacts passed signature, hash, size, space and self-test checks. The candidate, floor reservation and known-good fallback are durable. Execution has not started or a shared timeout permits a bounded retry. |
| pending | Execution may have started. An interrupted pending trial falls back and quarantines its sequence. |
| report-pending | A valid trial report is durable. The launcher retries those exact bytes once per scheduled tick, with a journalled continuation count. Network and API failures leave it here and the known-good pair keeps reporting. |
| committed | The launcher witnessed a durable API receipt for the exact nonce, manifest, pair and report hash, then wrote the local commit. |
| quarantined | The candidate failed or execution was ambiguous. Only the recorded fallback runs; repair requires a higher sequence. |

A prepared transaction can resume because the pending record always precedes
each execution. A process exit or worker-written health file never commits a trial.
Preparation can stage a higher signed runtime even when both baseline banks fail.
A valid trial report proceeds unless the fallback lacks one of its new error IDs;
shared errors become the baseline, a failed fallback does not block a valid trial,
and only shared timeouts retry before quarantine on the third confirmation.
After commit, execution failures switch to a working fallback in the same tick,
rejecting code only when its identity differs; duplicate-bank failures stay visible
without rejecting the running release. The launcher independently
fetches a higher offered runtime if the worker fails, hangs, emits invalid output
or cannot plan its successor. Hold and canary policy still apply.

The worker has private pipes, no API token and no update lock. Protocol 1 checks
host, schema 5, build, signed role hashes, sequence and transaction nonce. The
launcher posts the bytes it validated without re-encoding Inventory. Unknown
fields and null sections survive. The server keeps exact accepted bytes and an
idempotent receipt (the latest trial plus a few recent reports per host); losing a POST response does not require running the candidate
again. Shared fleet credentials still provide telemetry, not host attestation or
permission to promote a release.

Each worker has a monotonic process-group deadline. On Linux the launcher kills
and reaps descendants as a subreaper. The launcher also has a 12-minute aggregate
budget, with a separate bounded status attempt. No sequence or retry decision
uses `Created` or a host calendar deadline. Health lag uses server receipt time.
Normal TLS certificate validation remains in use; clock-independent pinned TLS
is outside this step.

Downloads stream into private same-filesystem staging files. Files and parent
directories are synced before publication. Space checks reserve bytes for the
report and journal and check free inodes. The launcher prunes only its own stale
slots and partials under the lock, keeping active, fallback and pending pairs.
Under storage pressure it runs the recorded fallback without writing a selector
symlink. An unreadable floor fails closed and posts `state_corrupt`; deleting
control files is never a recovery procedure. A permanently broken disk or trust
anchor still needs operator repair.

The fault harness runs with `go test ./internal/agentboot`. It builds real
launchers and updaters, signs synthetic executable collectors, and terminates
at file-sync, rename, directory-sync and receipt boundaries. Fault injection is
compiled only with the test build tag; installed launchers have no environment
fault switches. Installer checks run with `python3 scripts/test-install-agent.py`.
These tests do not establish real LXC, arm64 or appliance behavior; verify those
platforms before fleet rollout.
