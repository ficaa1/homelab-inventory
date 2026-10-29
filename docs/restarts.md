# Restart actions

Restarts are off by default. Inventory collection remains read-only. Preview and
stack reads use the API token; execution requires the separate
operator token used for agent offers. `GET /capabilities` tells the dashboard
whether actions are enabled and returns the configured `actions_host`.

To enable restarts, set these in the API's private environment:

```text
ACTIONS_ENABLED=1
ACTIONS_HOST=host-a
OPERATOR_TOKEN=<a separate random credential of at least 32 characters>
ACTIONS_COMPOSE_FILES=app=/srv/app/compose.yml,db=/srv/db/compose.yml
```

`ACTIONS_HOST` must match the inventory hostname of the Docker daemon reached by
the API. Mount its Docker socket using the commented Compose line. **Docker socket
access gives the API container host-level Docker authority.** A socket marked
read-only still allows Docker API mutations; it does not make the socket safe.
Use this only for a trusted API on a trusted host with a protected operator token.
No arbitrary remote host or Docker endpoint is accepted from a browser request.

The image runs as UID/GID 10001 by default. For socket access, add its actual
numeric group with Compose `group_add: ["<socket GID>"]` (inspect it locally with
`stat -c %g /var/run/docker.sock`), and ensure allowed compose files are readable.
An explicit Compose `user: "0:0"` override also works, with broader privilege.
Existing bind-mounted data directories must be writable by UID 10001; a fresh
named volume inherits the image directory ownership.

The compose map is optional and matches exact project names and absolute file
paths. No prefix, wildcard, Portainer ID or inventory-supplied path grants access.
Mount each explicitly permitted compose file and any required project files in
the API at the same absolute paths, preferably read-only. Compose restart uses
`docker compose -f FILE -p PROJECT restart`; it does not pull images or recreate
services. Test your project with the same Docker CLI/Compose environment first.

Stacks without a configured file restart their known containers one at a time,
providers before dependents, with deterministic ordering for cycles. A failure
stops the sequence and records failed/skipped results. An allowlisted compose
restart has one audit result named `stack:<project>`; its CLI can partially restart
a project before returning an error. Container restarts always
require an exact safe name in the latest configured host inventory. Missing or
wrong-host inventory fails closed. Only `ACTIONS_HOST` inventories are considered.
Inventories are untrusted agent input: a shared agent credential can impersonate
hostnames and supply container/stack metadata. Before each container restart,
a fixed Docker inspect command must resolve the target to exactly `/NAME`;
ID prefixes that resolve to a different name fail closed. Restart uses the
slash-prefixed name, preventing a later ID-prefix fallback. The confirm/hash
flow remains required, but cannot authenticate an inventory's source. There is no general shell/exec HTTP endpoint.

Preview: `POST /actions/preview` with API token authentication and JSON
`{"kind":"stack_restart","target":"app"}` (or `container_restart` plus an exact
container name). The preview returns containers, dependency impact, operations
and `container_set_hash`. Both preview and execute accept optional `host`; when supplied
it must exactly equal `ACTIONS_HOST`, otherwise 400 before any target is resolved
or command runs. Omit it for configured-host behavior.
`GET /actions/stacks?host=X` returns `[]` for a different host; absent host keeps
configured-host behavior. The dashboard shows controls only on the enabled target.
Execution: `POST /actions/execute`, with operator bearer
authentication, the same fields, `confirm:true` and that hash. A changed inventory
set returns 409 with a fresh preview and requires another confirmation. Preview
is information only; it grants no execution authority. A restart can interrupt
clients or restart the API itself; an interrupted audit row stays `running`, so
inspect Docker before retrying. Membership confirmation does not detect every
metadata/configuration change between preview and execution.

`GET /actions/log` uses the API token and exposes safe audit outcomes, never
credentials. Disable actions, remove the socket mount and recreate the API
container to withdraw execution access. A missing operator token disables operator
routes; enabling actions without `ACTIONS_HOST` and `OPERATOR_TOKEN` refuses startup.
