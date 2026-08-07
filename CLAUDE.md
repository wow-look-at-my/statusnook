# statusnook

The org's fork of [goksan/Statusnook](https://github.com/goksan/Statusnook).
A single Go binary: HTTP handlers, the monitor and notification loops, and an
embedded SQLite database.

## Build and CI

- Build: `go build .` -- CGO is required (`mattn/go-sqlite3`).
- Offline config check: `./statusnook -validate-config path/to/config.yaml`.
- Regenerate SQL: `go generate ./...` (sqlc is a `tool` dependency, so this
  needs nothing on `PATH` beyond Go).
- CI is `wow-look-at-my/go-toolchain@v1` with `cgo: true`, `autorelease: false`
  and the `generate:` approval hash. It publishes nothing; whatever runs an
  instance is built and deployed separately, and the config that drives one
  lives in wow-look-at-my/status as plain YAML any statusnook reads.
- `id-token: write` is required even with autorelease off -- go-toolchain
  fetches secrets from secret-server over OIDC on every run.
- Editing the `//go:generate` line in `generate.go` changes its approval hash
  and fails the build until `generate:` in `.github/workflows/ci.yml` is set
  to the hash the failure prints.

**CI is red on coverage, and the reason is real.** go-toolchain requires 80%;
this fork is at 5.2%, because upstream shipped no tests at all. Do not weaken
the gate to dodge it -- this paragraph is the visible record that it is unmet.
The 750-line file cap and every other gate pass.

## Where things live

- `main.go` -- process entry, flags, routing.
- Handlers and loops by area: `alerts*.go`, `app*.go`, `config*.go`,
  `monitors*.go`, `notifications*.go`, `services.go`, `users*.go`,
  `setup.go`, `domain.go`, `db.go`, `render.go`.
- `configapply*.go` -- the config-file apply path, one file per section.
- `templates/*.html` + `templates.go` -- every page template, `//go:embed`ed
  one constant per file.
- `sql/*.sql` + `sqlc.yaml` -> `internal/sqlcgen/` -- typed query code. The
  `*db.go` wrappers adapt it to the app's own types. Every application query
  lives here; the only hand-written SQL left is `db.go` (schema bootstrap,
  migration runner, `pragma_table_info` introspection) and `validate.go`, which
  are DDL and dynamic identifiers sqlc cannot express.
- `schema.sql` -- the schema a **fresh** install gets.
- `migrations/*.sql` -- applied in filename order to an **existing** install.

## Invariants

- A schema change goes in **both** `schema.sql` and a migration. A fresh
  install records every migration as skipped and only ever runs `schema.sql`,
  so a migration-only change never reaches new installs. sqlc reads
  `schema.sql`, so the migration alone will not make a new column compile.
- `applyConfig` writes as it validates, deletes included. Any caller must
  discard the transaction when it returns messages -- `configWebhook` did not,
  and a single bad monitor in a pushed config deleted everything the file no
  longer mentioned.
- The three template caches are read from HTTP handlers and written from the
  monitor and notification goroutines. Concurrent map access is a Go runtime
  fatal error, not a panic -- no recover, no shutdown -- so keep the mutexes.
- The `meta*` globals are read by `getPageCtx` on nearly every request and
  written by admin handlers and by `monitorUnconfirmedDomainLoop`. They are
  atomics (`metastate.go`) for that reason; `metaConfigFileEnabled` in
  particular gates every mutating admin handler.
- `rwDB` is a single connection opened IMMEDIATE. Never hold its transaction
  across a network call -- an SMTP send once blocked every writer, monitor
  logging included, for the OS connect timeout.
- The eleven `.gz` files under `static/` have no uncompressed counterpart.
  Serving them only to gzip clients 404s everyone else.
- Two sqlc/sqlite footguns, each with a test that fails on it: a multi-byte
  character anywhere in a `sql/*.sql` file truncates every query after it
  (`char(N)` instead), and a query mixing `?N` with bare `?` asks sqlite for
  more parameters than sqlc passes (name every parameter in that query).
