# statusnook

The org's fork of [goksan/Statusnook](https://github.com/goksan/Statusnook).
Almost the entire application is one file, `main.go` (~19,800 lines): handlers,
templates as Go string constants, SQL, and the background loops.

## Build and CI

- Build: `go build .` -- CGO is required (`mattn/go-sqlite3`).
- Offline config check: `./statusnook -validate-config path/to/config.yaml`.
- CI is `wow-look-at-my/go-toolchain@v1` with `cgo: true`. It publishes
  nothing: `autorelease: false`, and no container image is built. Whatever
  runs an instance is built and deployed separately; the config that drives
  one lives in wow-look-at-my/status and is plain YAML any statusnook reads.
- `id-token: write` is required even with autorelease off -- go-toolchain
  fetches secrets from secret-server over OIDC on every run.

**CI is red, and the two reasons are real.** go-toolchain caps files at 750
lines and requires 80% coverage; neither is configurable. This fork carries a
19,869-line `main.go` and 4.2% coverage. Getting to green means splitting the
file -- 5,380 of those lines are 134 embedded HTML templates, and
`applyConfig` (953) and `getEditMonitor` (814) each exceed the cap on their
own -- and then building a test suite upstream never had. Do not weaken the
gate to dodge it. `go build`, `go vet ./...` and `go test ./...` all pass.

## Where things live

- `main.go` -- everything except the pieces below.
- `validate.go` -- the `-validate-config` entry point.
- `retention.go` -- pruning for the tables that would otherwise grow forever.
- `loginlimit.go`, `mailheader.go` -- login throttling; header and JSON escaping.
- `schema.sql` -- the schema a **fresh** install gets.
- `migrations/*.sql` -- applied in filename order to an **existing** install.

## Invariants

- A schema change goes in **both** `schema.sql` and a migration. A fresh
  install records every migration as skipped and only ever runs `schema.sql`,
  so a migration-only change never reaches new installs.
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
