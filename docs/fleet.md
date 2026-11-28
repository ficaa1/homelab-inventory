# Fleet

The Fleet plugin shows the state of a fleet hub: the hosts that report to it, drift on those hosts,
skills, skill trials and usage. With an operator token it can also make changes. The plugin
talks only to the hub. It never connects to the hosts themselves.

The second half of this page is the hub API. The hub implements it, and the plugin is a client
of it.

## Settings

| Setting | Type | Needed | What it does |
| --- | --- | --- | --- |
| `hub_url` | URL | yes | The hub's base URL, without `/v1`. Example: `https://hub.example.lan:8790` |
| `read_token` | secret | yes | Bearer token for views and alerts |
| `operator_token` | secret | no | Bearer token for actions. Without it, the actions are hidden |
| `proxy_panel_url` | URL | no | A link to the proxy's own management panel, shown on the Proxy view. The plugin never calls it |

Use HTTPS where the hub has a certificate the dashboard trusts. Plain HTTP sends the tokens in
the clear, so use it only on a network you control. **Test** reports which one is in use.

## Views

| View | Shows |
| --- | --- |
| Overview | Host count, attention items, trials past their end date or ending within the hub's policy window, the model-invoked context cost per host, and a list of hosts with their status |
| Hosts | One row per host: last report, drift, broken links, refused changes, updates, free disk |
| Host | One host's items that need attention, its provider mapping, its updates and its force requests |
| Skills | Every skill with its source, mode, trial and per-host usage. Filter by source, origin or mode |
| Trials | One section per trial source with its decision table and end date |
| Skill | One skill: mode, source, hosts and usage. Actions: set its mode |
| Proxy | The model proxy behind the hub: routing strategy and one row per account with status, cooldowns and quota |
| Account | One proxy account: quota, cooldowns, recent requests. Actions: routing, enable, refresh, sign in again, reset cooldown |
| Usage | Planned. Tokens and cost per day and model, quota, and usage per host |

## Alerts

| Rule | Severity | When |
| --- | --- | --- |
| `fleet.host_stale` | warning | A host hasn't reported for longer than the hub's `stale_after_seconds`, or never reported and its profile is older than that or has an unknown age |
| `fleet.drift` | warning | A host has drift or broken links |
| `fleet.refused` | warning | A host refused a change because the file drifted |
| `fleet.trial_due` | info | A trial passed its end date and is still a trial. The summary says how many skills are undecided |
| `fleet.disk` | warning or critical | A host's free disk is below the hub's thresholds |

| `fleet.proxy_unreachable` | warning | The hub can't reach the proxy, or the proxy rejected the hub's management key |
| `fleet.proxy_sign_in` | warning | An account needs to sign in again |
| `fleet.proxy_cooling` | critical | Every enabled account of one provider is cooling down |
| `fleet.proxy_renewal` | warning | An account's access expires within 24 hours and its last renewal failed |

The subject is the host name, the source name for `fleet.trial_due`, the provider for
`fleet.proxy_cooling`, the account ID for the other proxy rules, and `proxy` for
`fleet.proxy_unreachable`. If the hub can't be
reached, the dashboard raises `plugin.unreachable` after three failed polls.

---

## Hub API, version 1

### Conventions

- **Base.** Every route is under `{hub_url}/v1/`. Requests and responses are JSON in UTF-8.
- **Auth.** Every request carries `Authorization: Bearer <token>`. The read token can call every
  `GET` route. The operator token can call every route. Report tokens are fleet-internal and can't
  call anything here.
- **Envelope.** Every successful response is an object with `api_version` (integer, `1`) and
  `generated_at` (the hub's clock), plus the route's fields.
- **Times** are RFC 3339 in UTC. **Durations** are whole seconds. **Sizes** are bytes.
  **Ratios** are fractions between 0 and 1.
- **Ages.** The plugin computes ages as `generated_at` minus the timestamp, so clock skew between
  the hub and the dashboard doesn't matter.
- **Null versus empty.** `null` means unknown or not reported. An empty array or `0` means the
  hub knows and there is nothing. A host that has never reported has `null` counts, not zeros.
- **Compatibility.** Within `/v1` the hub only adds fields and enum values. The plugin ignores
  unknown fields and shows unknown enum values as plain text with a neutral tone. A breaking
  change gets `/v2`.
- **Limits.** A response stays under 4 MiB. Lists that hit a cap set `truncated: true` on their
  parent object. Caps: 100 hosts, 2000 skills, 1000 items per host, 100 receipts.
- **No secrets.** No response contains a secret value, a token or file contents. Files are
  described by path, hash and line counts only.
- **Untrusted text.** The plugin treats every string from the hub as untrusted. It strips
  control characters and angle brackets and shortens long values before it puts them in a view.
- **Identifiers.**

  | Kind | Format | Example |
  | --- | --- | --- |
  | Host | `[a-z][a-z0-9-]{0,31}` | `host-a` |
  | Source | `[a-z0-9][a-z0-9._-]{0,63}` | `example-skills` |
  | Skill | `<source>/<name>` for third-party skills, `<name>` for own skills | `example-skills/lint-docs` |
  | Harness | `[a-z][a-z0-9-]{0,31}` | `claude` |
  | Item | Opaque, at most 512 characters, unique within a host | `claude:skills/lint-docs/SKILL.md` |

  The plugin never parses a skill or item ID. It passes them back to actions unchanged.

### Errors

Any status other than 2xx has this body:

```json
{"error": {"code": "invalid_param", "field": "url", "message": "Use an https:// URL for the source."}}
```

`message` is plain text that says how to fix the problem, and the plugin shows it as is. `field`
is set when one request field caused the error.

| Status | `code` | Meaning |
| --- | --- | --- |
| 400 | `bad_request` | The body isn't valid JSON or has unknown fields |
| 401 | `unauthorized` | Missing or wrong token |
| 403 | `forbidden` | The token's role can't call this route |
| 404 | `not_found` | Unknown route, host, source, skill, item or receipt |
| 409 | `conflict` | The state changed since the dashboard read it, such as a trial that already ended or a file hash that no longer matches |
| 409 | `not_editable` | The object can't be changed from the dashboard, such as an own skill, whose mode is set in its `SKILL.md` |
| 409 | `idempotency_mismatch` | The idempotency key was already used with a different action or parameters |
| 413 | `too_large` | The body is over 256 KiB |
| 422 | `invalid_param` | A parameter has the wrong format or value |
| 429 | `rate_limited` | Too many requests. `Retry-After` gives the seconds to wait |
| 503 | `busy` | Another action holds the repository lock. Try again |
| 502 | `upstream_failed` | Git push or another dependency failed. `message` says which |

### Shared objects

**HostSummary**

```json
{
  "name": "host-a",
  "profile_since": "2026-11-28T10:00:00Z",
  "fleet_version": "0.7.0",
  "last_seen_at": "2026-12-01T09:12:00Z",
  "synced_commit": "4f2c1a9",
  "counts": {"drift": 1, "broken_links": 0, "refused": 1, "pending": 0, "updates": 2},
  "disk": {"total_bytes": 512000000000, "free_bytes": 61000000000},
  "context_cost": [
    {"harness": "claude", "model_invoked": 14, "description_chars": 9120, "tokens_estimate": 2280}
  ]
}
```

- `profile_since` is when the hub first saw the host's profile. A host that never reported
  counts as stale only once `stale_after_seconds` have passed since then. It is `null` if unknown,
  and then a host that never reported counts as stale at once.
- `last_seen_at`, `fleet_version` and `synced_commit` are `null` if the host never reported.
- `counts` is `null` if the host never reported.
  - `drift`: files that changed since fleet wrote them.
  - `broken_links`: links whose target is missing.
  - `refused`: drifted files that also have a repository change waiting, which fleet won't apply
    over the drift. A file counts in `drift` or in `refused`, never both.
  - `pending`: changes the host hasn't applied yet.
  - `updates`: components with a newer version available.
- `disk` describes the filesystem that holds the host's home, and is `null` if not reported.
- `context_cost` has one entry per harness on the host, and is `null` if not reported.
- A host is pending when `synced_commit` differs from the hub's `repo_head`.

**Policy.** The hub owns fleet's thresholds and sends them, so the plugin hardcodes none:

```json
{
  "stale_after_seconds": 259200,
  "disk_warn_free_ratio": 0.15,
  "disk_crit_free_ratio": 0.08,
  "trial_days": 30,
  "trial_due_soon_days": 7
}
```

### `GET /v1/meta`

Used by the plugin's Test.

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "hub_version": "0.7.0",
  "repo_head": "4f2c1a9",
  "role": "read"
}
```

`role` is `read` or `operator`, depending on the token used.

### `GET /v1/overview`

Everything the Overview view and the alerts need, in one call.

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "repo_head": "4f2c1a9",
  "policy": {"stale_after_seconds": 259200, "disk_warn_free_ratio": 0.15, "disk_crit_free_ratio": 0.08, "trial_days": 30, "trial_due_soon_days": 7},
  "hosts": [ HostSummary, … ],
  "truncated": false,
  "trials": [
    {"source": "example-skills", "since": "2026-11-10", "ends": "2026-12-10", "state": "running", "undecided": 4}
  ],
  "recent": [ ReceiptSummary, … ]
}
```

- `trials[].state` is `running` (before `ends`) or `due` (past `ends`, still a trial).
- `undecided` counts the source's skills without a per-skill decision.
- `recent` holds the last 10 dashboard actions, newest first (see Receipts).

### `GET /v1/hosts`

```json
{"api_version": 1, "generated_at": "…", "repo_head": "4f2c1a9", "policy": Policy, "hosts": [ HostSummary, … ], "truncated": false}
```

Every response that has hosts also has `policy`, so a host's status comes from one response.
`truncated` is true when the host list hit its cap. The plugin then treats the response as
incomplete: views say so, and alerts fail the poll rather than resolve alerts for the missing hosts.

### `GET /v1/hosts/{name}`

One host's last report. `report` is `null` if the host never reported.

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "repo_head": "4f2c1a9",
  "policy": Policy,
  "host": HostSummary,
  "report": {
    "reported_at": "2026-12-01T09:12:00Z",
    "commit": "4f2c1a9",
    "ok_count": 212,
    "items": [
      {
        "id": "claude:skills/lint-docs/SKILL.md",
        "harness": "claude",
        "kind": "skill",
        "path": "skills/lint-docs/SKILL.md",
        "state": "refused",
        "detail": "Changed on the host since fleet wrote it; the repository has a newer version.",
        "live_sha256": "9b1e…",
        "fleet_sha256": "02ac…",
        "diff": {"lines_added": 4, "lines_removed": 1},
        "forceable": true
      }
    ],
    "providers": [
      {"harness": "claude", "provider": "proxy", "endpoint": "http://proxy.example.lan:8080"}
    ],
    "updates": [
      {"component": "proxy", "installed": "1.4.0", "latest": "1.5.0", "apply": "manual"}
    ],
    "force_requests": [
      {"id": "frc_01", "item": "claude:skills/lint-docs/SKILL.md", "live_sha256": "9b1e…", "state": "queued", "requested_at": "…", "resolved_at": null}
    ]
  }
}
```

- `items` lists only items that need attention. `ok_count` counts the rest.
- `kind`: `skill`, `instruction`, `setting` or `link`.
- `path` is relative to the harness home. It is never absolute.
- `providers[].endpoint` is a URL with no user info, query or fragment. The plugin shows only its
  scheme, host, port and path anyway.
- `state`:

  | State | Meaning |
  | --- | --- |
  | `pending` | fleet will add, update or remove this on the next sync |
  | `drift` | Changed since fleet wrote it. fleet leaves it alone |
  | `refused` | Drifted, and a repository change for it is waiting |
  | `broken_link` | The link's target is missing |
  | `remove` | Installed, but this host's profile excludes it |

- `live_sha256`, `fleet_sha256` and `diff` are set for `drift` and `refused` items, and are
  `null` otherwise. `diff` counts lines only.
- `forceable` is true when `force_file` can apply to the item.
- `updates[].apply`: `auto` (fleet applies it on sync), `manual` (a fleet command on the host) or
  `report` (another tool applies it).
- `force_requests[].state`: `queued`, `applied`, `stale` (the live hash changed before the host
  picked it up, so nothing was overwritten) or `failed`. Requests older than 30 days are dropped.

### `GET /v1/skills`

Optional query: `source=<source>`, `origin=own|third_party`, `mode=model|user|exclude`. Each
filters on the hub, and they combine.

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "usage_window_days": 30,
  "sources": [
    {
      "name": "example-skills",
      "origin": "third_party",
      "url": "https://git.example.com/example/skills",
      "defined_in": "policy",
      "trial": {"since": "2026-11-10", "ends": "2026-12-10", "state": "running"},
      "hosts": null,
      "removable": true,
      "counts": {"skills": 12, "model": 5, "user": 4, "exclude": 3}
    }
  ],
  "own_counts": {"skills": 20, "model": 14, "user": 6, "exclude": 0},
  "skills": [
    {
      "id": "example-skills/lint-docs",
      "name": "lint-docs",
      "source": "example-skills",
      "mode": "model",
      "mode_from": "decision",
      "decided_at": "2026-11-20",
      "editable": true,
      "description_chars": 412,
      "hosts": [{"host": "host-a", "installed": true}, {"host": "host-b", "installed": false}],
      "usage": {"threads": 5, "by_host": [{"host": "host-a", "threads": 5}], "last_used_at": "2026-11-30T18:02:00Z"}
    }
  ],
  "truncated": false
}
```

- `origin`: `own` (in the fleet repository) or `third_party`. Own skills have `source: null`.
- `defined_in`: `config` (hand-written) or `policy` (written by the hub).
- `trial` is `null` for sources that aren't on trial.
- `hosts` is `null` when the source installs on every host.
- `counts` and `own_counts` count every skill, whatever the filter. `sources` is never filtered.
- `mode`: `model` (model-invoked), `user` (user-only) or `exclude` (not installed).
- `mode_from`: `source` (the source's lists), `decision` (a per-skill decision, with
  `decided_at`) or `frontmatter` (own skills).
- `editable` is false for own skills.
- `usage` counts threads that loaded the skill in the last `usage_window_days`. It is `null` when
  the hub has no usage data, which is different from zero threads.

### `GET /v1/trials`

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "rules": {"trial_days": 30, "keep_model_min_threads": 3, "keep_user_min_threads": 1},
  "trials": [
    {
      "source": "example-skills",
      "since": "2026-11-10",
      "ends": "2026-12-10",
      "state": "running",
      "hosts": null,
      "skills": [
        {"id": "example-skills/lint-docs", "name": "lint-docs", "mode": "model", "decided": true, "threads": 5, "by_host": [{"host": "host-a", "threads": 5}], "suggestion": "model"},
        {"id": "example-skills/draw-graphs", "name": "draw-graphs", "mode": "user", "decided": false, "threads": 0, "by_host": [], "suggestion": "exclude"}
      ]
    }
  ]
}
```

- `threads` counts since `since`. `suggestion` follows `rules`: at least `keep_model_min_threads`
  suggests `model`, at least `keep_user_min_threads` suggests `user`, and fewer suggests
  `exclude`.
- The hub never acts on a suggestion. Only an action does.

### `GET /v1/usage` (planned)

Not implemented yet, and the plugin doesn't call it until it is. The shape is reserved so prompt
cache metrics fit without a new version.

Query: `since=<n>d` (1 to 90, default 7), `bucket=day`.

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "since": "2026-11-24T00:00:00Z",
  "bucket": "day",
  "totals": Tokens,
  "days": [{"date": "2026-11-24", "by_model": [{"model": "model-a", "tokens": Tokens}]}],
  "by_model": [{"model": "model-a", "tokens": Tokens}],
  "by_project": [{"project": "project-a", "tokens": Tokens}],
  "by_host": [{"host": "host-a", "tokens": Tokens}],
  "quota": [
    {"provider": "provider-a", "account": "…@example.com", "window": "5h", "used_ratio": 0.42, "resets_at": "…", "observed_at": "…"}
  ],
  "cache": null
}
```

**Tokens**

```json
{
  "requests": 120,
  "input": 81000, "output": 22000, "reasoning": 4000,
  "cache_read": 640000, "cache_write_5m": 30000, "cache_write_1h": 12000,
  "cost_usd": 3.41
}
```

`cache_write_5m` and `cache_write_1h` are kept apart from the start, so the write-TTL share can
be computed later. Any field is `null` when its source doesn't report it.

**Cache (reserved).** `null` until it's built. Planned shape:

```json
{
  "hit_ratio": 0.86,
  "write_share_5m": 0.71,
  "write_share_1h": 0.29,
  "expiries_after_idle": {"count": 9, "idle_gap_p50_seconds": 420},
  "by_model": [{"model": "model-a", "hit_ratio": 0.88}]
}
```

`hit_ratio` is `cache_read / (input + cache_read + cache_write_5m + cache_write_1h)`.
`expiries_after_idle` counts requests that wrote a cache prefix again after an idle gap longer
than that prefix's TTL.

### Proxy

The hub fronts one model proxy (a CLIProxyAPI-compatible server) through its management API.
The plugin never talks to the proxy, and the hub keeps the proxy's management key to itself.
The hub re-shapes every answer: account names, emails and tokens never pass through.

#### `GET /v1/proxy`

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:15:00Z",
  "proxy": {"state": "ok", "message": null, "version": "1.4.0"},
  "routing": {"strategy": "round-robin", "etag": "\"r-41\""},
  "accounts": [ Account, … ],
  "truncated": false
}
```

- `proxy.state`:
  - `ok`;
  - `unreachable`: the hub can't connect;
  - `key_rejected`: the proxy refused the management key once, and the hub stopped calling it
    until the key changes;
  - `not_configured`.

  When the state isn't `ok`, `routing` and `accounts` are `null` and `message` says what to fix.
- `routing.strategy`: `round-robin`, `fill-first` or `weighted-round-robin`. `etag` is opaque.
  `proxy_set_strategy` sends it back, so a change made meanwhile elsewhere returns `409`.
- Caps: 200 accounts.

**Account**

```json
{
  "id": "acc_3f9a1c",
  "provider": "provider-a",
  "label": "…@example.com",
  "status": "cooling_down",
  "disabled": false,
  "requires_sign_in": false,
  "renewal": "ok",
  "access_expires_at": "2026-12-01T21:00:00Z",
  "priority": 0,
  "weight": 1,
  "cooldowns": [{"model": "model-a", "until": "2026-12-01T09:40:00Z", "reason": "rate_limited"}],
  "requests": {
    "success": 1204, "failed": 31,
    "recent": [{"at": "2026-12-01T06:00:00Z", "success": 12, "failed": 0}]
  },
  "quota": [
    {"window": "primary", "used_ratio": 0.42, "resets_at": "2026-12-01T12:00:00Z", "observed_at": "2026-12-01T09:10:00Z"}
  ]
}
```

- `id` is opaque and stable for the account. Actions take it. It never contains an email or
  file name.
- `label` is masked: at most the email's domain.
- `status`: `active`, `cooling_down`, `needs_sign_in`, `disabled` or `error`.
- `renewal`: `ok`, `renewing`, `failed` or `unknown`. `access_expires_at` may be `null`.
- `cooldowns` is empty when there are none. `model` is `null` for a cooldown on the whole account.
- `requests.recent` has one entry per 10 minutes over the last 200 minutes, oldest first.
- `quota` is `null` when the provider doesn't report quota. Each entry has `observed_at`,
  because quota is only learned from responses.

#### `GET /v1/proxy/accounts/{id}`

`{"api_version": 1, "generated_at": "…", "proxy": {…}, "routing": {…}, "account": Account}`.
Returns `404` for an unknown ID.

#### `GET /v1/proxy/sign-in/{session}`

The state of a sign-in that `proxy_reauth_start` began.

```json
{"api_version": 1, "generated_at": "…", "session": "ras_7b2e", "account": "acc_3f9a1c", "state": "waiting", "message": null, "expires_at": "2026-12-01T09:20:00Z"}
```

`state`: `waiting` (for `proxy_reauth_finish`), `working`, `ok`, `error` (with `message`) or
`expired`. Sessions expire 5 minutes after they start.

### Actions

`POST /v1/actions/{action}` with the operator token.

```http
POST /v1/actions/end_trial
Authorization: Bearer <operator token>
Idempotency-Key: 0b6f6f0e-2a7c-4f0e-9a51-7f1c2d3e4a5b
Content-Type: application/json

{"params": {"source": "example-skills", "undecided": "user"}}
```

- **One change per action.** Each action is one change to the fleet repository, which the hub
  commits and pushes, except `force_file`, which queues a request for one host. Hosts apply
  repository changes on their next sync.
- **Idempotency.** `Idempotency-Key` is required: 16 to 64 characters from `[A-Za-z0-9-]`. The
  hub keeps receipts for 30 days.
  - The same key with the same action and params returns the stored receipt with
    `"replayed": true`, and nothing is applied twice.
  - The same key with anything else returns `409 idempotency_mismatch`.
  - The plugin stores the key in a receipt before it sends, and reuses it only to reconcile an
    attempt whose outcome it doesn't know.
- **Deadline.** The hub answers within 8 seconds. If the push isn't done by then, it answers
  `202` with `state: "working"`, and the receipt route reports the outcome.
- **Locking.** Actions are serialized on the repository. A request that can't get the lock within
  5 seconds gets `503 busy`, and nothing is applied.
- **Validation.** The hub validates the result before it commits, so an invalid config never
  reaches the repository. A validation failure returns `422` and changes nothing.

#### Requests

| Action | `params` | Conflicts (`409`) |
| --- | --- | --- |
| `add_trial_source` | `name`, `url` (https), exactly one of `glob` or `manifest`, `include` (array of skill names, may be empty for all), `hosts` (array, or `null` for every host), `model_invoked` (bool: `true` = every skill model-invoked, `false` = every skill user-only) | the name is taken |
| `set_skill_mode` | `skill` (skill ID), `mode` (`model`, `user` or `exclude`) | `not_editable` for own skills |
| `end_trial` | `source`, `undecided` (`user`, `exclude` or `suggestion`) | the source isn't on trial |
| `remove_source` | `source` | `not_editable` when `removable` is false |
| `force_file` | `host`, `item` (item ID), `live_sha256` (the hash the dashboard showed) | the item isn't `forceable`, or its live hash changed |
| `proxy_set_strategy` | `strategy`, `etag` | the routing changed since `etag` |
| `proxy_set_account_routing` | `account`, `priority` (integer), `weight` (0 to 1000000; 0 leaves the account out under weighted routing) | |
| `proxy_set_account_enabled` | `account`, `enabled` (bool) | |
| `proxy_refresh_account` | `account` | |
| `proxy_reset_cooldown` | `account` | the account has no cooldown |
| `proxy_reauth_start` | `account` | the account's provider can't sign in again from here |
| `proxy_reauth_finish` | `session`, `redirect_url` | the session isn't `waiting` or has expired |

- `end_trial` keeps every per-skill decision. Skills without one get `undecided`. With
  `suggestion`, each gets its row's suggestion from `GET /v1/trials`.
- `remove_source` uninstalls the source's skills on every host at their next sync. Hand-edited
  copies (drift) are left in place and reported.
- `force_file` never writes on the hub. The host overwrites that one file at its next sync, and
  only if its live hash still equals `live_sha256`.

#### Receipt

`200` (done, or replayed) and `202` (working) return:

```json
{
  "api_version": 1,
  "generated_at": "2026-12-01T09:16:02Z",
  "replayed": false,
  "receipt": {
    "id": "rcp_01J9Z6",
    "action": "end_trial",
    "params": {"source": "example-skills", "undecided": "user"},
    "state": "done",
    "summary": "Ended the trial for example-skills: 3 model-invoked, 1 user-only, 2 excluded.",
    "commit": "7d3e0b2",
    "created_at": "2026-12-01T09:16:00Z",
    "finished_at": "2026-12-01T09:16:02Z",
    "error": null,
    "hosts": [
      {"host": "host-a", "state": "pending", "last_seen_at": "2026-12-01T09:12:00Z"},
      {"host": "host-b", "state": "pending", "last_seen_at": "2026-11-30T21:40:00Z"}
    ],
    "force": null
  }
}
```

- `state`: `working`, `done` or `failed`. A `failed` receipt has `error` (the error object above)
  and changed nothing.
- `summary` is plain text for the dashboard's toast and result view.
- `hosts[].state`: `pending` until the host reports a `synced_commit` that contains `commit`,
  then `applied`.
- For `force_file`, `commit` is `null`, `hosts` names only the target host, and `force` is
  `{"id": "frc_01", "state": "queued"}`. Its state then moves as in `force_requests`.
- Proxy actions have `commit: null` and `hosts: []`. They apply to the proxy at once, except
  `proxy_refresh_account` and `proxy_reauth_finish`, which answer `202` with `working`. Their
  outcome shows on the account.
- `proxy_reauth_start` returns `reauth` in the receipt:
  `{"session": "ras_7b2e", "url": "https://…", "redirect_hint": "localhost:1455", "expires_at": "…"}`.
  Open `url` on any device and sign in. The browser then lands on an address on
  `redirect_hint` that fails to load. Copy that whole address into `proxy_reauth_finish`.
- `proxy_reset_cooldown` clears only the proxy's own cooldown, not the provider's limit.
- `redirect_url` is the one secret parameter. The hub never stores, logs or returns it, and
  receipts show it as `null`. Every other parameter is safe to store and show.

`ReceiptSummary`, used in `recent`, has `id`, `action`, `state`, `summary`, `created_at` and
`pending_hosts` (a count).

#### `GET /v1/receipts/{id}`

Returns the same body as the action, with `replayed: false`. Read token.

#### `GET /v1/receipts?limit=20`

`{"api_version": 1, "generated_at": "…", "receipts": [ ReceiptSummary, … ]}`, newest first, up
to 100.
