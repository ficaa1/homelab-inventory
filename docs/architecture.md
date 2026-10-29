# Architecture

Discover collects locally through independent collectors. External commands pass through a narrow read-only allowlist; collection redacts sensitive fields before serialization. Collectors report unavailable or failed sections separately from successful empty results. Correlation joins facts from collectors after collection.

The API accepts snapshots, keeps history, and derives fleet views, alerts, drift and trends. Rules are shared library code; command entry points handle I/O. Notifications record state so alerts send on transitions, while optional digests are periodic summaries. The dashboard is a single embedded file. The MCP bridge exposes read-only API views over stdio.

Self-update and restart actions are separate opt-in capabilities; see [self-update](self-update.md) and [restarts](restarts.md).
