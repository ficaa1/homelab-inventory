# Contributor guide

Read [CONTRIBUTING.md](CONTRIBUTING.md) before changing code. Preserve read-only discovery, allowlisted execution, collection-time redaction, null versus empty semantics, correlation boundaries and transition-only notifications. Use synthetic fixtures and run the relevant checks.

`internal/plugin/` owns built-in plugin lifecycle, private settings, declarative views, guarded HTTP and cached alerts; `cmd/api/plugins.go` lists built-ins explicitly. For plugin work, read [docs/plugins.md](docs/plugins.md).

`internal/auth/` owns provider discovery, browser sign-in, signed sessions and CSRF; `internal/settings/auth.go` owns their stored configuration and environment layering.
