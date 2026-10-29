# Contributing

Use Go 1.25 or later. The project uses the standard library only and builds with `CGO_ENABLED=0`. Run `umask 022; make` to vet, test and build; format edits with `gofmt` (or `make fmt`). Test changes on the relevant platform where possible; FreeBSD has a thinner collector set.

## Invariants

- Preserve credential boundaries: API_TOKEN for dashboard/admin/MCP, AGENT_TOKEN only for inventory posts and release downloads, OPERATOR_TOKEN only for privileged offers and restart execution. Keep auth comparisons constant-time; configured tokens must differ and be at least 32 characters.
- Treat inventory hostnames and metadata as untrusted. Keep API root storage names reserved in both run and software storage; only ACTIONS_HOST inventories may influence restarts, and container names need live exact Docker verification.
- Agents require HTTPS beyond loopback unless the operator explicitly enables HTTP. Unauthenticated development must bind loopback.
- Discover is read-only. Run external commands only through `internal/sysexec` with fixed, reviewed arguments and bounded output and time.
- Redact at collection, before data reaches disk or the API. Never commit real inventories, secrets or captures.
- Preserve null for unavailable or uncollected sections and empty arrays for successfully collected empty results. Report partial failures as diagnostics.
- Collectors collect facts; cross-collector joins belong in `correlate.go`. Shared rules belong in `internal/`, with thin command entry points.
- Alert notifications are transition-only. Keep stable alert keys and distinguish silence from resolution.
- Preserve schema compatibility and update the agent revision and source guard when collector or schema behavior changes.

## Adding a collector

Add a collector under `internal/collect`, register it, define its inventory schema and diagnostics, and join other sections only in correlation. If a command is needed, add the narrowest fixed command and argument validation to `internal/sysexec`; do not expose a general shell. Test parser edge cases using synthetic data, unavailable commands, timeouts, redaction, and null versus empty results. Validate on representative supported hosts when available without committing their output.

Keep changes focused, explain behavior and tradeoffs in the pull request, and report tests and platform coverage. Extend existing table tests for meaningful regressions; avoid redundant tests for simple reversible changes.
Run `make dist` and `python3 scripts/test-install-agent.py` when changing agent
installation, posting or updates. The latter uses loopback and disposable synthetic
fixtures, checks agent/admin separation, and removes its temporary state.
`go test ./internal/webui` uses Node when available to check browser contracts.
