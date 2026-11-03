# VPN health and recovery

**Network → VPN** shows one card per detected Docker VPN gateway, labelled with
its container name and host. Each card shows that gateway's tunnel handshake,
exit address, received/sent bytes and dependent containers. Expand a card on
Network's overview, or open VPN for the full panel. Unknown facts remain
unavailable; zero traffic is shown as zero.

## Discovery and live checks

Discovery stays read-only. On Linux, the agent joins Docker metadata with tunnel
interfaces and counters from `/proc/<pid>/net/dev`. A candidate must either have a
running `wg*` or `tun*` interface, or have `NET_ADMIN` with at least one configured
namespace consumer. It identifies namespace
consumers from Docker's `container:<name-or-id>` network mode and compares their
network namespaces. It does not execute programs inside containers or read their
environment. A gateway with `NET_ADMIN` and configured namespace consumers can
still be reported when it is stopped or its proc files cannot be read. Every
gateway on the host is reported and checked.

Live API checks require the [restart configuration](restarts.md): enabled
actions, an exact `ACTIONS_HOST`, the operator credential and Docker socket
access. Only that host is checked locally. Other hosts retain inventory facts.
Enable the Network plugin in Admin to see its VPN panel; core VPN alerts do not
depend on that plugin being enabled.

The API uses fixed allowlisted commands to inspect container state, read
WireGuard handshake/counters, and check the exit address from inside the gateway.
WireGuard keys are discarded before command output is retained. Commands have
bounded time and output; no shell or arbitrary browser-supplied URL is accepted.

The exit check uses `curl`, falling back to `wget` when curl is absent. The
server-side `VPN_CHECK_URL` defaults to `https://ipinfo.io/json`. Set it to an
HTTP(S) endpoint returning a plain IP address or JSON with `ip` and optional
`country`, `city`, `org`. Each exit check makes one request from inside the
tunnel. The selected provider sees the VPN exit IP and request; use your own endpoint if that matters. A tunnel
**server** container normally shows its host's public address as its exit; this
is expected, since its outbound traffic does not use a remote VPN exit.
**The dashboard page makes no external requests** for VPN data.

WireGuard observations refresh about once a minute. When the handshake is
older than three minutes, exit checks run about once a minute, including after a
successful check. Panel-triggered checks and checks without WireGuard evidence
share a five-minute cache. Failed exit checks are retried at least 30 seconds
apart. Each gateway has its own cadence and failure history. These checks
supplement the usual inventory schedule. Panel refreshes request checks in the
background and read the last published observations without waiting for Docker.

## Read the panel

| Status | Meaning |
| --- | --- |
| OK | A recent handshake or successful exit check verifies connectivity. |
| Down | The gateway is stopped, or repeated exit checks confirm a failure. |
| Cut off | A running dependent is not using the gateway's current network: it started before the gateway, or the agent saw it in a different network namespace. The reason names the container. |
| Unknown | There is not enough evidence to diagnose connectivity. |

The reason and status age explain the verdict. A **Live** label identifies API
checks; the collection age remains visible for inventory facts. A failed page
refresh keeps the last answer marked stale.

Each dependent shows its container state and start time when available:
**Attached**, **Cut off**, or **Attachment unknown**. Configuring a shared network
namespace alone does not prove that the running process is attached to the
current gateway. Restarting just the gateway can leave consumers cut off.

The panel also shows the last reconnect's actor, result and reason, verified exit
address when available, and automatic recovery state, last run and cooldown.
A stale handshake alone is a warning, because an idle tunnel may still work.
Core alerts are `vpn.down` and `vpn.cutoff` (critical), and
`vpn.handshake.stale` (warning after three minutes). Normal alert delivery remains
transition-only.

## Reconnect manually

Press **Reconnect VPN**, review the gateway and affected dependent containers,
then confirm. The dashboard asks for the separate operator token, using the same
flow as other restart actions. Reconnect is available only for the configured
actions host and any discovered gateway. Only that gateway's dependents are
included. If the container set changes, review the new preview and confirm again.

Recovery restarts the gateway first, waits for healthy tunnel readiness, then
restarts dependent containers in name order so they attach to its current
namespace. It attempts dependent recovery even if tunnel readiness times out.
The final result includes exit verification and container-state checks.
Read **Admin → Recent actions** for the durable outcome. `partial`, `failed`, or
an interrupted response requires inspection; the browser never replays execute
automatically.

For API clients, preview with the API credential:

```json
{"kind":"vpn_reconnect","target":"vpn-demo","host":"host-a"}
```

Send this to `POST /actions/preview`. Execute at `POST /actions/execute` with the
operator credential, the same fields, `"confirm":true` and the preview's
`container_set_hash`. `GET /actions/log` includes reconnect history.

## Automatic recovery

In **Admin → VPN recovery**, enable automatic reconnect and set how long a
confirmed failure must persist. It is **off by default**; the delay defaults to
**10 minutes**, with an allowed range of **1–1440 minutes**. Saved settings take
effect without a restart:

```json
{"vpn_autofix":true,"vpn_autofix_after_minutes":10}
```

Use `PUT /settings` with normal administrator authentication to save these values.
An omitted field keeps its previous value. Explicit `false` disables recovery
without changing the delay. Resetting settings restores off/10.

To leave one gateway out (for example a reverse-proxy tunnel you would rather
restart yourself), untick it under **Gateways it may reconnect**. That saves
`vpn_autofix_skip`, a list of container names (at most 32); its card then reads
**Off for this gateway**. Manual **Reconnect VPN** still works for it, and its
health alerts are unchanged. `[]` puts every gateway back.

Only persistent **Down** or **Cutoff** states trigger automatic recovery.
Unknown evidence and an old handshake alone do not. Recovery shares the normal
action lock and audit, records actor `auto`, and reserves a **one-hour cooldown**
per gateway before executing. At most one reconnect runs at a time across all
gateways. If another action is busy, the automatic attempt is skipped without
consuming that gateway's cooldown window. Each gateway's failure timer and last
automatic attempt survive API restarts; gateways removed from inventory lose
their stored state. The old single-gateway state file (or an incompatible file)
is accepted by starting fresh, so it never blocks startup. Success and
failure reports respect notification on/off and mute settings, but bypass the
alert severity floor because they report an action outcome. Disabling actions
or automatic recovery stops future automatic attempts.

Immediately before each automatic reconnect, the API makes one direct request
to the same `VPN_CHECK_URL` from the dashboard process, outside the tunnel. This
request has a five-second timeout and a bounded body. If it also fails, recovery
is skipped with the reason “internet unreachable from the dashboard host;
automatic reconnect skipped”. The cooldown is not consumed, and no skip
notification is sent; the normal `vpn.down` alert remains responsible for the
failure notification. Manual Reconnect does not make this direct request.

Privacy: this is a second request to the same URL, from the dashboard host's own
address, only just before an automatic reconnect.

## Limitations

- Live probes and recovery operate only on `ACTIONS_HOST`; there is no cross-host
  Docker execution.
- Inventory `vpns` is `null` when Docker was not collected or was unavailable,
  and `[]` when Docker collection succeeded but no gateway was found. Legacy
  `vpn` mirrors the first gateway, or remains omitted when none exist. The API
  prefers `vpns` and falls back to `vpn` for older agents.
- The agent has no path to execute VPN probes or recovery. Discovery remains
  read-only; privileged operations belong to the API executor and its separate
  operator authorization. Enabling automatic recovery does not grant the agent
  execution privileges.
- A gateway's “exit on fatal” option is unnecessary for this recovery flow. Docker
  restart policies can restart the gateway while leaving dependent containers
  cut off from its new network namespace. The cutoff alert detects this state;
  reconnect recovers the gateway and its dependents together.
