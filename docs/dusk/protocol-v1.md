# Dusk protocol version 1

Dusk is an optional built-in integration, disabled by default. Configure Media
connections once, then enable Dusk in Admin → Plugins. Device credentials only
permit the scopes selected when an administrator creates a pairing code.

All routes append to the configured base URL, including any proxy path prefix.
Use HTTPS; clients may explicitly opt into trusted LAN HTTP. Clients never follow
credential-bearing redirects or retry writes automatically. JSON errors contain
`{"error":"safe message"}`. Responses are private and must not be cached publicly.

`POST /tv/v1/pair` accepts `{"code":"ABCD-EFGH","device_name":"Living room TV"}`.
Codes ignore case and grouping, expire after a short window and redeem once.
201 returns `api_version:1`, `device_id`, `token` and `capabilities`. Only this
response includes the device secret; servers retain its hash. Invalid or expired
codes return 400, throttled attempts 429 and disabled integration 503.

All other routes require `Authorization: Bearer <device secret>`. Revocation is
immediate. Credentials cannot authorize inventory, administrator, uploads, agent
or operator routes. Profile identity is server-owned; TV callers cannot select it.

`GET /tv/v1/capabilities` returns the shape in [the example](v1/capabilities.json).
Feature flags describe configured and permitted behavior, independent of temporary
upstream health. Missing flags mean disabled. Clients must reject unsupported
major versions. Request policies, when present, expose only label and approval
(`manual` or `automatic`), never upstream credentials.

| Route under `/tv/v1` | Shape / required permission |
| --- | --- |
| GET `/media` | `checked_at`, downloads source states/titles/truncated, plex status/counts, upcoming; media |
| GET `/browse` | `titles`, `hidden`; media |
| GET `/discover` | [discovery example](v1/discovery.json); discover |
| GET `/search?q=…` | `titles`, `hidden`; search, query at most 80 characters |
| GET `/title/{movie\|tv}/{id}` | `title`, `requests_enabled`, optional request_policy; search |
| GET `/similar/{movie\|tv}/{id}` | discovery shape; discover |
| POST `/request` | `{kind,id,season,operation,confirmed:true}` → `{state}`; requests |
| POST `/preference` | `{kind,id,hidden}` → `{state:"ok"}`; preferences |
| POST `/discover/refresh` | `{}` → discovery shape; refresh |
| GET `/taste/picks?kind=movie\|tv` | `picks`, `judged`; taste |
| GET `/taste/familiar?kind=movie\|tv` | `titles`, `judged`; taste |
| POST `/taste/verdict` | `{kind,id,verdict,title,year}` → `{state:"ok"}`; taste |

Title IDs are positive integers below one billion. Lists and strings are bounded.
Unavailable/unconfigured source sections use null data or explicit source states;
successfully fetched empty lists use `[]`. Health failures do not remove capability
flags. Verdicts are loved, liked, meh, disliked, seen, pass or empty (clear).
Preferences, feedback and request receipts are household-profile scoped.

Request outcomes retain duplicate and ambiguous-result protection across restart.
An unknown outcome must be inspected upstream before retrying; clients never
replay timed-out requests. Ordinary Media dashboard reads never submit requests.

Portable recommendations use content metadata and explicit household feedback.
No external datasets or trained models are required or bundled. Optional imports
accept user-provided Letterboxd CSV; they never fetch arbitrary URLs.
