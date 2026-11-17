# Vulnerabilities

The built-in `cve` plugin scans stored host packages and sampled container package
sets against distro advisories through [OSV.dev](https://osv.dev/). Enable
**Vulnerabilities** in **Admin → Plugins**. Its views list CVEs, source packages,
and coverage for each host and sampled image. Scans run on activation, at the
configured interval, and when requested from the page. The schedule survives
restarts: after a successful scan the next one is due an interval later. A
failed or interrupted scan runs again at startup. Page reads use local cache.

## Sources and network disclosure

OSV receives distro ecosystems, source package names and installed source
versions. Binary names are used only when no source name was collected; binary
versions are used when no source version was collected. Hostnames, image names,
container names, credentials and full inventories are never sent to these feeds.
OSV decides which versions are affected; the plugin does not guess matches from
names or compare installed versions against generic upstream releases.

[CISA KEV](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) adds known
exploitation, ransomware use and remediation dates. The public feed is downloaded
at most once per day. [FIRST EPSS](https://www.first.org/epss/) receives matched
CVE IDs in groups of at most 100, at most daily, and returns exploitation
probability and percentile. Either source can be disabled with an empty URL.
Failures retain previous enrichment and appear under source status. Cached data
can therefore be older than the latest scan.

Only the plugin's background scan and connection Test contact upstream services.
Page requests and alert polls never contact the network. All requests use the
plugin host's configured-authority allowlist, ten-second request timeout and
8 MiB response cap. Query batches contain at most 100 packages and run four at a
time; advisory details use ten workers and retry timeouts, 429 and 5xx responses
with backoff. Query pagination follows OSV continuation tokens.

A batch that fails is split in half and retried until the failing package is
alone. A package OSV still cannot answer is left out of that scan and listed
under coverage as skipped, so one slow package does not discard the rest. The
Ubuntu `linux` kernel source is the usual case: OSV takes more than ten seconds
to return its first page. More than 10 skipped packages (or 2% of queries) fails
the scan instead, since that points at an OSV outage.

## Settings

| Key | Default | Meaning |
| --- | --- | --- |
| `osv_url` | `https://api.osv.dev` | Required OSV API base URL |
| `kev_url` | `https://www.cisa.gov/sites/default/files/feeds/known_exploited_vulnerabilities.json` | CISA feed URL; empty disables KEV |
| `epss_url` | `https://api.first.org/data/v1/epss` | FIRST API URL; empty disables EPSS |
| `interval_hours` | `12` | Scan interval, from 1 to 168 hours |
| `include_images` | `true` | Scan sampled container package sets as well as hosts |
| `alert_kev` | `true` | One alert per unsuppressed known exploited CVE |
| `alert_critical` | `false` | Alert on critical CVEs with a published distro fix |

URLs must use HTTP or HTTPS without credentials, query parameters or fragments.
Settings validation performs no network I/O. Changing settings restarts the worker.
Changing a feed URL or **include images** starts a scan right away; other settings
keep the schedule.

## Matching, severity and fixes

Supported ecosystems are Debian major releases, Ubuntu releases (even-year
April releases use OSV's `:LTS` suffix), Alpine major/minor releases, Rocky Linux
major releases and AlmaLinux major releases. Collections with non-`ok` status,
unknown distros and disabled image scans remain visible as unsupported coverage.
Hosts with no software collection remain visible too. RPM package collection is
currently unsupported by the agent; Rocky and AlmaLinux mapping is available
when package facts exist, without implying RPM database collection support.

The projection contains only package facts from the software store. It combines
identical ecosystem/name/version queries across sets while retaining the installed
binaries and their host/image occurrences. Dpkg source names/source versions and
apk origins therefore match the distro's patched source package, including epochs,
distro revisions and backports. Debian, apk and RPM version ordering determine
whether a distro's published fixed version is newer than the installed source.
A fix indicates an advisory version, not that a repository upgrade is available.

Records group by their upstream or alias CVE ID, falling back to the OSV ID, so
one CVE can combine Debian, Ubuntu and Alpine records. Multi-CVE notices (Ubuntu
`USN-`, Debian `DSA-`, `DLA-` and `DTSA-`) are ignored: the per-CVE `DEBIAN-CVE-`
and `UBUNTU-CVE-` records cover the same CVEs, and a notice listing hundreds of
CVEs cannot be attributed to one of them.

Severity prefers the distro's own rating, as Trivy and Grype do for OS packages:
Ubuntu priority (negligible counts as low) or Debian urgency (unimportant counts
as low). The highest rating across a CVE's distro records wins. Without one, the
highest CVSS v3.0/v3.1 base score sets it (9, 7 and 4 for critical, high and
medium; positive scores below 4 are low), scored with FIRST's formula. The CVSS
score is shown either way. Debian's "not yet assigned" is no rating. CVSS v4
vectors are kept with score `0` (unscored); severity then stays unknown.

Priority adds 1,000 for KEV, 400/300/200/100 for critical/high/medium/low,
rounded EPSS percentile multiplied by 99, and 50 when any occurrence is fixable.
The overview ranks source-package fixes by the sum of their fixable CVE priorities.
Package fix versions show the highest published version needed for their fixable
CVEs; a CVE with an occurrence lacking a fix is not counted as fully cleared.

## Local state and alerts

The private plugin cache stores trimmed advisory records, enrichment, the last
complete result and suppressions. Details are limited to 4,000 characters,
references to 20, and affected entries to the packages this scan matched (one
Ubuntu kernel CVE lists every kernel flavour of every release). A record that
later matches another package is fetched again. Records live in 256 gzip shards
under `plugins/cve/advisories/`, so a lab matching tens of thousands of
advisories adds a few hundred files to the data directory and its backups, not
one file per advisory. Records no installed package matches any more are pruned
after each successful scan. Fetched records are saved every 1,000 advisories and when a scan stops, so a
large first scan resumes after an interruption. Unchanged OSV modification
timestamps reuse cached details. A failed match/detail scan preserves the previous
complete result and reports its error. Restarted pages can display the last result
before a new scan completes. Back up the plugin cache with the API data directory.

Suppression states are `accepted`, `not_affected` and `false_positive`. Each needs
a reason of at most 500 characters and records the authenticated actor and time.
An optional `YYYY-MM-DD` expiry applies through that UTC date. Expired entries
stop applying on reads, without waiting for another scan. Suppressed CVEs are
excluded from totals, package/host counts and alerts; the vulnerability filter can
show suppressed records or all records. Deleting a suppression restores the CVE.

KEV alerts are critical when a fix exists for any occurrence, otherwise warning.
Optional critical-severity alerts require a fix and are warning. At most 50 alerts
are returned; the core handles stable keys and transition-only notifications.

EOL flags use a static table covering Debian 10–13 (including LTS), Ubuntu
20.04–26.04 (standard support) and Alpine 3.17–3.24. Future support dates are
scheduled dates; update the table when vendor schedules change. No support-feed
request is needed.

## Coverage limits

Only distro packages are covered. Language packages inside images, static
binaries and the Proxmox kernel are not covered. A container set samples one
running container's writable root for an image ID; containers sharing that image
may differ. Coverage follows the last stored package collection, not a live host
inspection. A successful empty result means no matches for those collected facts;
unsupported or failed coverage does not establish that a host has no CVEs.
