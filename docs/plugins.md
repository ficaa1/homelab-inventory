# Plugins

Plugins are optional integrations compiled into the API. Open **Admin → Plugins**
to enable an available plugin, enter its settings, run **Test**, then save. Enabled
plugins with a tab appear in navigation. Disabling removes the tab and resolves
its cached alerts through the usual notification transitions. This backend ships
with an empty built-in list; integrations must be included in the server build.

Secret fields show only whether a value is set. Leave a field out of an update
to keep its saved value; send `null` to clear it. A failed settings validation or
Configure call returns field errors without saving anything. Test uses temporary
settings and restores the previous configuration; it does not save the form.

State lives in `<data>/state-plugins.json`, written atomically at mode 0600.
`PLUGIN_<ID>_<KEY>` supplies an environment seed when a stored value is absent
(uppercase, with plugin ID hyphens changed to underscores). Stored values win;
an explicit `null` suppresses the seed and any default. Unknown plugin entries
survive saves and appear as `unavailable` without exposing their settings.
Back up this private file along with the data directory.

## Writing a built-in

Use `internal/plugin` and add an explicit `plugin.Builtin` entry to
`builtInPlugins` in `cmd/api/plugins.go`. Registration has an ID and a constructor
`func(plugin.Host) plugin.Plugin`; there is no init registration or runtime code
loading. IDs match `[a-z][a-z0-9-]{1,31}`. The manifest returned by the instance
must use the same ID.

```go
func New(h plugin.Host) plugin.Plugin { return &Integration{host: h} }

// The instance implements:
Manifest() plugin.Manifest
Configure(plugin.Config) error
Test(context.Context) plugin.TestResult
View(context.Context, string, url.Values) (plugin.View, error)
Close() error
```

The method signatures above describe the interface; implement them as methods on
your own type. Manifest contains ID, Name, Description, Version, Settings and an
optional Tab. A Tab has Title, Icon and Views (`Name`, `Title`); the first view is
the default. View names match `[a-z][a-z0-9-]{0,31}`. Unknown icon names fall back
to the dashboard's generic icon.

Settings use `plugin.Field`: Key, Label, Help, Type, Required, Default,
Placeholder, Options and Group. Keys match `[a-z][a-z0-9_]{0,39}`. Types are
`url`, `secret`, `text`, `number`, `bool`, `select`; matching constants are
`plugin.URL`, `Secret`, `Text`, `Number`, `Bool`, `Select`. Select options have
Value and Label. Secrets cannot have defaults. URLs require http/https and a
host; numbers accept finite numeric values or a parseable number string;
booleans require JSON booleans (environment seeds use Go boolean parsing).
Required fields are enforced on enable and Test; incomplete disabled settings
can be saved. Config is a map of validated values with secrets included.

Configure runs at startup for enabled plugins, on enable, and on each save.
It must be fast and perform no network I/O. Return
`&plugin.FieldError{Key: "endpoint", Message: "explain the problem"}` for a field
error. Configure must support reconfiguration and restoration after a temporary
Test or rejected save. Close must permit later Configure to enable the instance
again. Calls on an instance are serialized; a second concurrent Test is refused.
All live operations must honor context cancellation; Test and alert polls have
a ten-second context deadline. View and image calls also have that deadline.

Use the provided Host for dependencies:

- `HTTP()` returns a ten-second client with an 8 MiB response-body cap. Only
  configured URL-field authorities (hostname and effective port) are allowed,
  checked on each request and again before dialing. Proxies are disabled.
  Redirects stay on the original host and scheme. Errors omit query strings;
  a failed request's URL is scrubbed before Go wraps the error. Do not reuse a
  failed request to recover its query parameters.
- `CacheDir()` is `<data>/plugins/<id>/`, mode 0700. Restrict plugin writes to
  that directory; built-ins run in-process and are trusted code, not a sandbox.
- `Logf` prefixes lines with `plugin <id>:` and scrubs configured secrets and
  URL queries. Never include credentials in logs, views, alerts or error messages.
- `Now()` supplies the current time.

Optional interfaces are detected with type assertions:

```go
Alerts(context.Context) ([]plugin.Alert, error)
Image(context.Context, string) (contentType string, body []byte, err error)
```

Alert has Rule, Subject, Severity (`critical`, `warning`, `info`), Summary and
Detail. Rule is local and stable. The core adds the plugin ID prefix, sets Host
to the plugin ID and Source to `plugin`, then merges cached alerts before age,
silences and notifications. Polling runs immediately and every five minutes,
with an additional refresh after settings changes. Failures retain the last
good result twice; the third replaces it with warning `plugin.unreachable`,
Subject equal to the plugin ID. A successful poll restores normal alerts.

## Declarative views

A View has Title, UpdatedAt, RefreshSeconds and Blocks. JSON uses snake_case.
The core validates the complete response before serving it and caps it at 1 MiB.
Text is plain text: strings containing angle brackets are rejected. Plugins
cannot send HTML, JavaScript, custom styles or executable dashboard components.

```json
{
  "title": "Activity",
  "updated_at": "2026-10-29T12:00:00Z",
  "refresh_seconds": 30,
  "blocks": [
    {"type":"stats","items":[{"label":"Jobs","value":"2","hint":"1 running","tone":"ok"}]},
    {"type":"list","title":"Queue","empty":"Nothing queued","items":[
      {"title":"Example job","subtitle":"Worker","meta":"12 minutes left","image":"job-42",
       "progress":{"value":0.42,"label":"42%"},"badges":[{"text":"New","tone":"info"}],
       "link":{"view":"queue","params":{"id":"42"}}}
    ]},
    {"type":"notice","tone":"info","text":"Read-only integration."}
  ]
}
```

Use the Go View, Block, Item, Column, Badge, Progress and Link structs. Block
is the union of these types:

| Type | Fields |
| --- | --- |
| `stats` | `items`: label, value, hint, tone |
| `list` | title, empty; `items`: title, subtitle, meta, tone, image, progress, badges, link |
| `table` | title, empty, columns, rows |
| `posters` | title, empty; `items`: title, subtitle, image, badge, progress, link |
| `notice` | tone, text |
| `section` | title, blocks |

There are at most 50 blocks total, 500 items or rows per block, and two block
levels (top-level blocks and children of a section). Only sections nest.
Tones are `ok`, `info`, `warn`, `crit`, `muted`, or omitted. Progress value is
between 0 and 1. Table columns have key, label, optional format (`text`, `number`,
`bytes`, `duration_s`, `time`, `percent`) and align (`left`, `right`). Rows map
column keys to scalar values.

Images use opaque refs matching `[A-Za-z0-9._-]{1,128}`, never external image
URLs. ImageSource serves them through the authenticated image route; responses
must have an `image/*` content type and fit in 4 MiB. Links either target a
manifest view with string params, or contain an http/https href. The dashboard
opens external links in a new tab with noopener/noreferrer. Bad refs, links,
tones, block types, HTML or limits produce 502 rather than unvalidated JSON.
