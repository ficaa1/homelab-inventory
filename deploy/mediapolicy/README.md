# Media-policy service

`mediapolicy` runs the Archive policy and an optional seeding mover in a separate
process. The dashboard's Archive plugin controls it over authenticated HTTP;
Tdarr reads its library identities and title leases before planning work.
Build it with `make` (`bin/mediapolicy`) or `make mediapolicy-linux`
(`bin/mediapolicy-linux-amd64`). It uses only the Go standard library.

The archive policy replaces opted-in 2160p library files with smaller releases,
checks quality and playback activity, imports through Sonarr/Radarr in copy
mode, and verifies each replacement. Original files go to the application's
recycle bin for recovery. Automatic grabs run within the configured window;
verification and ready imports can continue outside it. Operator approvals,
queue controls, restore and purge use reviewed plans rather than automatic
retries. Only titles with the `archive` tag participate; `4k` requests a 2160p
replacement. The service can create these tags on an operator's request.

The seeding mover relocates completed, manually managed qBittorrent downloads
from the download disk to the library storage. It links existing imported data
where possible, checks exclusive file ownership, rechecks the torrent, and
removes source files only after destination verification. Moves and library
folder changes require a reviewed plan and confirmation. Interrupted moves
halt for inspection rather than replaying a write.

## Requirements

- Sonarr v4 and Radarr with API keys, explicit indexer IDs, and matching path
  mappings. Write admission checks the exact `reviewedVersions` in the policy;
  update those only after reviewing the installed versions' API behavior.
- Tautulli with an API key when archive or seeding is enabled. Its Plex library
  identities, playback history and current streams provide safety checks.
- qBittorrent when seeding is enabled. The mover checks qBittorrent 5.2.3 and
  libtorrent 2.0.14.0; unsupported versions or missing fields refuse a move.
  Automatic torrent management and incomplete-file extensions must be off for
  the selected download. Archive-only deployments use Arr's configured
  download client and do not need qBittorrent credentials here.
- `ffprobe` on PATH for automatic archive imports (included in the image).
- Read-only inspection mounts covering download and library app paths. Archive
  import requires a seven-day recycle bin on the same filesystem as the
  library, copy import, and disabled completed-download removal. Unknown
  queue, history, playback, file identity or storage state blocks writes.
- Optional writable mounts for seeding, subtitle carry-over, restore and purge.
  The service user needs the relevant filesystem permissions. Seeding's library
  and destination hardlinks must use one writable mount; separate bind mounts
  cannot be linked even if they point at the same filesystem.

## Configure and run

Copy `policy.example.json` to `policy.json` and `env.example` to `.env`. Set a
random `MEDIA_POLICY_TOKEN` of at least 32 characters and fill only the service
credentials you use. Keep `.env` private (`chmod 600 .env`); the policy file
contains settings rather than secrets. Both workers start disabled.

From this directory, create `state/` writable by the container's numeric UID
and GID, then run:

```sh
docker compose -f compose.example.yaml up -d --build
```

The example publishes HTTP on loopback port 8792. Use a private Docker network
or an authenticated TLS reverse proxy for a remote dashboard. Set Archive's
`policy_url` and `policy_token` and Tdarr's media-policy settings to this
service's reachable base URL and the same token. `MEDIA_POLICY_TOKEN` is the
service's environment variable; it is separate from dashboard API credentials.
All routes, including `/healthz`, require `Authorization: Bearer <token>`.
`GET /status` includes `service`, `checked`, `archive`, and non-null `leases`.

Without `-apply`, library and torrent writes are refused at the HTTP boundary
and filesystem actions produce previews. Local state and audit records are
still written, and release searches can contact an indexer. Use
`-no-release-search` to prohibit those requests as well. Begin with a dry-run:

```sh
bin/mediapolicy -config deploy/mediapolicy/policy.json -state-dir ./media-policy-state -once -no-release-search
```

Set the relevant worker's enabled setting and review its report. To allow
writes, add `-apply` to the command or Compose `command` list and recreate the
container. Request bodies cannot enable writes in a dry-run process. The
seeding mover still requires a preview hash and explicit confirmation.

## Policy settings

Missing fields inherit defaults; unknown JSON fields are rejected. Restart the
service after editing the policy. Dashboard scoring overrides are stored in
state and can be reset to the file's settings.

| Setting | Default | Meaning |
| --- | --- | --- |
| `archiveEnabled`, `seeding.enabled` | `false` | Enable each worker explicitly. |
| `archive.timezone` | `UTC` | IANA timezone for the schedule and notification days. |
| `archive.start`, `archive.end` | `01:00`, `06:00` | Start inclusive, end exclusive; overnight windows are supported. |
| `archive.downloadMinFreeGB` | `20` | Decimal GB the download disk must retain after a grab; `0` disables the floor. |
| `archive.requireFreeleech` | `false` | When true, both automatic and manual releases need an explicit freeleech flag. |
| `archive.downloadRoots` | `[/data/downloads/completed]` | Approved app-visible downloads. |
| `archive.libraryRoots` | `[/data/media/movies, /data/media/series]` | Approved app-visible media. |
| `archive.mounts` | none | App-to-local read-only inspection mappings. |
| `archive.sidecarMounts`, `archive.restoreMounts` | none | Explicit writable views; empty disables those filesystem actions. |
| `archive.quarantineDir` | empty | Existing app-path directory outside library roots for reviewed replacement files. |
| `seeding.downloadRoot` | `/data/downloads/completed` | Completed download subtree eligible for moves. |
| `seeding.downloadMount` | app root `/data/downloads`, no local root | Writable disk view; also bounds any library folders moved off that disk. |
| `seeding.nasMount` | app root `/data/media`, no local root | Single writable library/destination view. |
| `seeding.seedRoot` | `/data/media/.seeding` | Fresh destination directory per torrent hash. |
| `seeding.nasMinFreeGB` | `500` | Decimal GB the destination storage must retain after a move. |
| `seeding.libraryRoots` | movie/series roots above | Destination library roots keyed by `radarr` and `sonarr`. |

Set `archive.indexerIDs` separately for each application; the example's `1`
values are placeholders. `archive.plexKeys` optionally supplies stable Plex
identity overrides. Defaults require 10 seeders, 30 idle days, less than half
the original bytes, and a score of 60. Selection has configurable size bands,
codec factors and empty trusted/denied group lists. Lookups are cached for 24
hours and limited to five per minute. Notifications are optional, transition
based, batched daily, and held from 21:00 through 09:00 in the policy timezone.

When enabling seeding, uncomment both writable mounts and keep inspection
mappings disjoint. Create the configured destination parent. For subtitle
carry-over, map only the needed library roots in `sidecarMounts`. For restore
and purge, `restoreMounts` must cover library files, recycle bins and quarantine.
A read-only inspection mount never grants filesystem write permission.

## State and recovery

`-state-dir` (default `./media-policy-state`, `/state` in Compose) contains:

- `state.json`: jobs, recovery evidence, title leases, selection overrides,
  queues, deduplication and recent results.
- `audit.jsonl`: durable action intents and outcomes.
- `subtitles-*`: staged subtitle files needed for carry-over or restoration.
- The process lock: only one service or restore command may open the directory.

Back up the entire directory while the service is stopped. Preserve application
recycle bins separately; state backups do not contain original media. Never
start with an empty state directory to bypass a failed job. After a crash,
inspect the job, audit, Arr history/queue and qBittorrent before retrying.
Persistence failures disable further writes until restart; unresolved jobs
retain title exclusion. Failed imports stay available for operator recovery.

To restore an original, stop the service first and back up state. Preserve its
recorded recycle files before the application's seven-day cleanup. Preview a
restore with the same config, state and writable app-to-local mapping:

```sh
bin/mediapolicy -config policy.json -state-dir ./state -restore \
  -restore-job '<job-id>' -restore-map /data/media=/data/media -rescan=false
```

Supply every required mapping, including the recycle bin. If replacement files
occupy the original paths, use `-quarantine` with their comma-separated app paths
and `-quarantine-dir` with an existing directory on that filesystem. Review the
exact files, identities and destinations in the plan. Repeat with `-apply` only
when the plan is correct. Rescanning is enabled by default and requires the
appropriate Arr URL/key; omit `-rescan=false` when it should run. Originals are
published without overwriting existing files. Replacements move aside rather
than being deleted. The restore command retains the job for reconciliation.

The dashboard also offers restore/purge previews. Their applied requests require
the current plan hash, job ID and displayed confirmation phrase. Purge removes
only the reviewed recycle evidence; it cannot act through inspection mounts.
Use clear only for a failed pre-import job after checking that the library is
unchanged and the queue is clear. Never hand-edit jobs or remove media to make
an error disappear.
