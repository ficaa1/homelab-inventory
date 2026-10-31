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

## Optional LAN server discovery

Enable **Advertise to TVs on this LAN** in Dusk settings. Set **TV reachable base
URL** to the HTTP(S) address TVs can reach (including a reverse proxy prefix), or
leave it empty to use `PUBLIC_URL`. No container address is inferred. Advertising
is off by default and stops when Dusk or LAN advertising is disabled. If the UDP
listener cannot bind, a safe diagnostic is logged and manual/paired access stays
available.

The TV sends one SSDP M-SEARCH to `239.255.255.250:1900`, TTL 1, MX 1,
ST `urn:homelab-inventory:service:dusk:1`. The enabled server replies with
`HTTP/1.1 200 OK`, matching ST, URL-derived UUID USN, `LOCATION` base URL,
`X-Dusk-Name` and `CACHE-CONTROL: max-age=60`. There are no credentials,
profiles or inventory in the response. There are no periodic announcements or
HTTP discovery probes. Responses are rate limited; clients search for two seconds,
limit replies to 4096 bytes and 16 unique addresses, and discard expired hints.
Names and addresses are untrusted LAN hints, not server authentication. Users
select and check a candidate before entering a pairing code. HTTPS validation and
explicit trusted-LAN HTTP consent still apply. Manual URL entry remains available.

Multicast normally stays on one LAN/VLAN; guest isolation, firewalls and VPNs may
block it. Permit UDP 1900 multicast inbound and UDP replies to the TV's ephemeral
port. Docker bridge port publishing alone does not deliver LAN multicast. On Linux,
operators may explicitly use host networking: copy the packaged Compose file to a
separate local file, remove `ports`, add `network_mode: host` to `api`, and run
`docker compose -f <local-file> up -d` from the same directory. The API then listens
on host port 8080; choose an unused port via the API listen configuration if needed.
Keep existing volumes and authentication configuration. Do not combine `ports`
with host networking. Native installations need no network-mode change. This is
optional; default installer networking is unchanged. Hardware/webOS multicast
support and routed networks must be checked locally; manual URL entry works when
discovery is unavailable.
