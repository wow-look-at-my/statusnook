# statusnook

The org's fork of [goksan/Statusnook](https://github.com/goksan/Statusnook).
Almost the entire application is one file, `main.go` (~19,800 lines): handlers,
templates as Go string constants, SQL, and the background loops.

## Build and CI

- Build: `go build .` -- CGO is required (`mattn/go-sqlite3`).
- Offline config check: `./statusnook -validate-config path/to/config.yaml`.
- CI (`.github/workflows/ci.yml`) builds the container image on every branch
  and pushes `ghcr.io/wow-look-at-my/statusnook:latest` from `main`.

**CI is not go-toolchain**, which the org otherwise requires for Go repos: it
caps files at 750 lines and requires 80% coverage, and this fork carries a
19,800-line `main.go` with no test suite upstream. Splitting the file is a real
piece of work and nobody should silently weaken the gate to dodge it -- so the
image build is the gate here, and this paragraph is the visible record that
the usual one is not met. `go vet ./...` and `go test ./...` both pass.

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
- The three template caches and the `meta*` globals are read from HTTP
  handlers and written from background goroutines. Concurrent map access is a
  Go runtime fatal error, not a panic: keep the mutexes.
- `rwDB` is a single connection opened IMMEDIATE. Never hold its transaction
  across a network call -- an SMTP send once blocked every writer, monitor
  logging included, for the OS connect timeout.
- The eleven `.gz` files under `static/` have no uncompressed counterpart.
  Serving them only to gzip clients 404s everyone else.
