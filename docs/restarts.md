# Restart actions

Off by default. When on, one Docker host (the one the API runs on) gets restart
buttons on its page: one per container, and one per Compose stack under **Stacks**.

**Read this first:** the API needs that host's Docker socket, which gives it full
control of Docker there, even if you mount the socket read-only. Turn this on
only if you trust the API server and keep the operator token safe.

## Turn it on

1. Add to the API's environment:

   ```text
   ACTIONS_ENABLED=1
   ACTIONS_HOST=host-a
   OPERATOR_TOKEN=<random, 32+ characters, different from your other tokens>
   ```

   `ACTIONS_HOST` is the host's name as it appears in **Hosts**.

2. In the Compose file, uncomment the Docker socket line and let the container
   use the socket's group:

   ```yaml
   group_add: ["<output of: stat -c %g /var/run/docker.sock>"]
   ```

3. Recreate the API container.

## Use it

Press the restart button on a container or stack. The dashboard shows what will restart
and what depends on it, asks you to confirm, then asks for the operator token
once per browser session. **Admin** lists recent restarts.

## Restart whole Compose projects

By default a stack restarts its containers one by one, dependencies first. To
use `docker compose restart` instead, map project names to their files:

```text
ACTIONS_COMPOSE_FILES=app=/srv/app/compose.yml,db=/srv/db/compose.yml
```

Mount each file (and anything it needs) into the API at the same path, read-only.
This restarts; it does not pull or recreate.

## Turn it off

Remove `ACTIONS_ENABLED`, remove the socket mount, and recreate the container.

## Details

- Only containers from `ACTIONS_HOST`'s latest report can be restarted, by exact
  name, and the name is checked against Docker before each restart.
- If the containers changed since you previewed, you are asked to confirm again.
- A failure stops the sequence; the log shows what restarted, failed or was skipped.
- Restarting the API's own container can cut the response short. The log then
  says `running`; check Docker before trying again.
- There is no way to run other commands.

API: `POST /actions/preview` with `{"kind":"stack_restart","target":"app"}` (or
`container_restart` and a container name) using the API token. Then
`POST /actions/execute` with the same fields, `"confirm":true` and the preview's
`container_set_hash`, using the operator token. `GET /actions/log` lists the history.
