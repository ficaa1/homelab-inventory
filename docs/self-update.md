# Signed agent updates

Signed updates are opt-in for each host. Configuring the API makes signed
onboarding available; it does not convert existing unsigned agents. A signed
runtime contains `discover` and `agentupdate`; the installed `agentboot` verifies
and supervises both. The launcher itself is upgraded by rerunning the installer.

## 1. Install the native signer

Use a trusted checkout and Go 1.25 or later. Build the payloads and native Linux
amd64 signer from the same source and version, even when building on another OS:

```sh
umask 022
make dist signer-linux VERSION=v0.1.0
sha256sum bin/agentsign-linux-amd64
```

Transfer `bin/agentsign-linux-amd64` through your trusted administrative channel
to the signer machine. Compare its SHA-256 with the build output before installing
it. On that Linux amd64 machine, as root:

```sh
install -d -m 0755 /usr/local/libexec/homelab-inventory
install -o root -g root -m 0755 ./agentsign-linux-amd64 \
  /usr/local/libexec/homelab-inventory/agentsign
install -d -m 0700 /root/.config/homelab-inventory
install -d -m 0755 /srv/homelab-inventory/releases
```

`make signer-linux` sets `CGO_ENABLED=0`, `GOOS=linux`, `GOARCH=amd64`, and uses
exactly the payload's version flags. A build-machine-native `bin/agentsign` may
not run on the signer. Replace the installed signer when changing release source:
its compiled agent revision must match the payload. The API must never receive
the private key or a writable signing directory.

## 2. Generate the key and publish

From the trusted build checkout, configure the existing remote signer interface.
The example names below are placeholders; choose your own signer and paths:

```sh
export SIGNER_HOST=root@signer.example.org
export SIGNER_PORT=22
export SIGNER_BIN=/usr/local/libexec/homelab-inventory/agentsign
export SIGNER_KEY=/root/.config/homelab-inventory/release.key
export SIGNER_DIR=/srv/homelab-inventory/releases
export LOCAL_SIGNER_BIN="$PWD/bin/agentsign"
export SIGNER_INPUT=bin
export AGENT_PLATFORMS=linux/amd64,linux/arm64
scripts/release-agent.sh keygen > release-public.key
make release VERSION=v0.1.0
```

For signing locally, leave `SIGNER_HOST` unset and set the other paths for that
machine. `keygen` creates a signer-owned mode-0600 Ed25519 key, or validates and
reuses the existing one. It prints only the public key. Back up the private key
offline. Use a distinct key and release directory for a different product or
trust domain; never overwrite another installation's key.

`make release` builds static artifacts and signs a new increasing sequence. Its
legacy `latest.json` contains only `discover`; `N/runtime.json` describes the
compatible protocol-1 `discover`/`agentupdate` pair. `AGENT_PLATFORMS` is signed
into both manifests; Linux amd64 and arm64 are the defaults. `freebsd/amd64` can
be published for legacy agents, but the slots installer targets Linux only.

## 3. Serve the release tree read-only

Make the complete release tree available to the API host using your deployment
process. Keep old sequence directories and copy immutable release directories
before replacing `latest.json` and `control.json`. Copy `promoted.json` after a
signer promotion as well; a remote snapshot must include its referenced release.
Never copy the private key. For Compose, add to the API service:

```yaml
volumes:
  - /srv/homelab-inventory/releases:/agent-releases:ro
environment:
  AGENT_RELEASE_DIR: /agent-releases
  AGENT_CANARY_HOSTS: canary-vm,canary-lxc
```

Use actual hostnames you selected as canaries, never a default fleet list. Restart
the API after changing environment configuration. For a binary service, set the
same variables and grant read access to the release tree while retaining signer
write ownership. The API stores browser rollout decisions under its own data
directory; it never writes the release tree.

In **Admin → Add a host**, check signed-release readiness, the public-key
fingerprint and the configured canaries. The fingerprint is SHA-256 of the
decoded 32-byte Ed25519 public key, not the text file's checksum. Compare it with
a value obtained through your trusted signer channel. Configure notifications
for warning transitions and use the normal notification test before rollout.

## 4. Migrate the canaries with one paste

In Add a host, enter the exact hostname of the selected canary and copy its
fresh generated signed command. Run it in that host's root console (or use the
sudo command). The generated command includes `--updates slots`, downloads the
launcher, redeems the short-lived enrollment code and selects the signed runtime.
The long-lived agent credential is stored privately, not placed in the command.
The same command handles a fresh install or conversion of an existing public
unsigned or legacy signed installation.

This first download is **trust on first use (TOFU)**: trusting the installer,
launcher and first public key requires a trusted API origin/transport. Matching
checksums from that same server catch corruption, not a compromised server.
Use HTTPS and compare the displayed release-key fingerprint through an
independent trusted channel. `--allow-http` is an explicit trusted-network
exception and exposes enrollment credentials to observers. Once installed, the
release key is pinned and the installer checks it on reruns. The launcher itself
is checked only against the API’s SHA256SUMS, not a release signature, so rerunning
enrollment again trusts the installer and launcher supplied by that API.

A configured canary receives the current candidate. Other new hosts receive the
last signer-promoted runtime. Before the first signer promotion, non-canary
signed enrollment refuses. Hold blocks new signed enrollment as well as update
offers; it cannot be bypassed with Add a host. The API cannot choose an arbitrary
older signed baseline.

The installer stages and verifies the pair before changing the existing schedule,
retains migration intent and enrollment retry identity, and requires an accepted
first report before completing cutover. Rerun the same command after an
interruption; keep the migration state. A replacement code is an escape hatch
when the retry window expires. Do not erase state to force another attempt.

The private `.migration` directory records preparation, backups and cutover intent.
A prepared retry rechecks the key and legacy floor, then reuses its saved token.
A lost redemption response can be retried with the same persisted installation
nonce for 30 minutes; a different nonce or hostname cannot reuse the code.
The systemd timer stays active during cutover. A failed cutover restores the old
configuration; after an abrupt kill, the next timer tick runs the old schedule or
the installed launcher reconciles the saved intent. Handled termination signals
trigger rollback immediately. If TrueNAS middleware cannot restore cron, recovery
files and intent remain for the next cron invocation to retry. Floors survive
rollback. TrueNAS cron hides middleware mail output and records up to 1 MiB per
invocation in `launcher.log`, retaining one previous log, both mode 0600.
Keep backups private: they include the previous token.

Check **Hosts** for signed mode, the expected sequence and updater, a fresh
accepted inventory, and healthy committed operation. Check locally on systemd:

```sh
systemctl status homelab-discover.timer
journalctl -u homelab-discover.service -n 20
sudo /var/lib/homelab-agent/agentboot -dir /var/lib/homelab-agent -status
```

Verify at least two subsequent healthy scheduled reports. Include a representative
unprivileged LXC canary before broad rollout; success on a VM does not establish
container compatibility. For an LXC without SSH, enter its console from the
hypervisor and paste the generated root command there. Downloads and installation
run inside the guest; do not pipe credentials through container execution stdin.

## 5. Promote, then migrate the remaining hosts

After reviewing the canaries, on the configured signer checkout:

```sh
make promote
```

This verifies the retained protocol-1 envelope and every artifact, then writes
`promoted.json` with its sequence and exact signed-envelope SHA-256. That pointer
selects the immutable bootstrap baseline. Transfer the updated release metadata
if the API serves a copy. Only signer promotion changes this baseline. Browser
or operator-token promotion changes update offers, but cannot authorize a new
bootstrap baseline in the read-only release tree.

Obtain and paste one fresh command per remaining host. Future runtime releases
need only publish → check canaries → signer promote, without another host login.

```sh
make hold      # pause offers and signed enrollment; keep installed runtimes
make auto      # latest candidate to configured canaries
```

Hold, auto and publishing a new sequence retain the promoted pointer. A newly
published candidate does not become the default for ordinary new hosts. Keep all
retained releases: the signer performs no release garbage collection. Never
remove the promoted sequence or data needed by in-flight downloads/reports.

## Migration and recovery boundaries

Legacy signed conversion preserves its validated same-key replay floor, including
a higher rejected sequence. A selected baseline below that floor must refuse;
publish a higher repair release instead. Unreadable state or a mismatched key
fails closed. Never delete legacy state, slot controls, or the stable lock inode
to get past an error. Sequence comparisons do not depend on release timestamps.

A separate product's key, floor and schedule are a different trust domain. For a
TrueNAS migration use a new dataset directory and credential, and explicitly
select only the predecessor schedule to replace; preserve its files and key.
See [TrueNAS and LXC installation](install.md#truenas-scale-and-lxc).

A failing candidate falls back to the recorded known-good runtime. A quarantined
sequence stays rejected even if fallback inventories succeed. Hold the release,
inspect the safe stage/code and publish corrected code at a higher sequence.
Do not reset `latest.json` to an older release or delete a host's floor.

Legacy plain-to-signed installation with local `--signed`, `--public-key`,
`--manifest` and `--artifacts` remains available, but updates only `discover`.
Use `--updates slots` for the replaceable runtime pair. Trusted offline slots
installation can supply `--binary-dir`, `--public-key`, `--manifest` pointing to
`N/runtime.json`, `--artifacts` pointing to `N`, and `--token-file` instead of
downloading a baseline. Selecting that offline baseline is an operator trust
choice and still cannot lower an existing floor.

**Key rotation.** Once a promoted baseline exists, `agentsign -resign` refuses:
changing its envelope or shared public key would invalidate the immutable
bootstrap identity. A coordinated trust migration is required; deleting
`promoted.json` or replacing host keys is not a rotation procedure. Before any
baseline promotion, the legacy held-release rotation remains available: hold,
stop all agent schedules, back up the entire release tree and host state, create
a new key at a different path, re-sign with `scripts/release-agent.sh resign`,
provision the new public key through a trusted channel, then validate hosts before
resuming. It changes only the held current release; older retained envelopes
remain signed by the old key. Never overwrite the old private key.

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

`agent.update.unhealthy` replaces `agent.outdated`. It is critical from the
start: raised immediately on a reported failure, rollback or rejection, or after
a host has been below its eligible offer for more than 45 minutes. The API
stores the first observed lag and incident under its data directory, so
restarting it does not restart the budget. Notifications use the existing
transition, silence and severity-floor settings: repeated checks stay quiet,
with one message when it starts and one when it clears, so the default critical
floor delivers it.
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
or attest to a host's identity. Existing unsigned and legacy installations receive these reporting changes only
when a new updater is installed or they are explicitly converted to slots.

## Runtime slots

`--updates slots` installs a signed `discover` and `agentupdate` pair. The
root-owned `agentboot` launcher holds the key, token and sequence floor. Both
binaries can then update together. Upgrade the launcher by rerunning the
installer. Launcher banks and launcher self-update are not implemented.

Publish with `make release` as above. It now signs two envelopes at the same
sequence: `latest.json` is the legacy discover-only offer; `N/runtime.json` is
protocol 1 for slots. Old signed and unsigned installations keep their current
behavior. They are not converted when the API is upgraded. A release directory
made by an old signer continues to serve legacy hosts but cannot enroll or
update a slots host until a protocol-1 runtime is published. Retain old sequence
directories for in-flight reports and artifact downloads; the API verifies each
retained envelope before serving only its listed artifacts.

On a fresh Linux systemd host, supply a trusted launcher, public key and signed
baseline through your existing trusted channel:

```sh
sudo sh ./install-agent.sh --updates slots --binary-dir "$PWD/bin" \
  --api-url https://inventory.example.org --token-file /root/inventory-token \
  --public-key /root/release-public.key --manifest /root/releases/1/runtime.json \
  --artifacts /root/releases/1
```

For a disposable test use `--prefix /absolute/test-root`; it runs the first
report without systemd. The same installer supports explicit conversion with a
retained migration journal. Choose an offline baseline deliberately; the local
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
