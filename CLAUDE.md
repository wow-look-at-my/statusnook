# Statusnook

Status page and endpoint monitor. One Go binary, SQLite, server-rendered HTML
with htmx. No frontend build step.

## Build and test

- `go-toolchain --cgo` runs everything: tidy, vet, file-length check, tests,
  build. **CGO is required** (`github.com/mattn/go-sqlite3`); without it the
  package does not compile.
- Files are capped at 750 lines.
- Run it locally: `go-toolchain --cgo` then `./statusnook -port 8000`
  (dev builds listen on 8000 and set `metaSSL=false`).
- Tests use the real routing table via `newRouter()` and a temporary database
  (`useTestDBs`, `newTestServer`). Coverage is enforced at 80% by the toolchain
  and currently sits well below that; every new test helps.

## Templates

All markup lives in `templates/`, embedded into the binary - never in Go string
literals (a test enforces this):

- `root.html` - the page layout every page template extends.
- `<handler>.html` - one page per handler, loaded with `parseTmpl("x.html")`.
- `*_email.html`, `*_slack.json` - notification bodies (`parseEmailTmpl`,
  `parseTextTmpl`).
- `fragment_*.html` - the out-of-band bits htmx swaps in. Render them through
  the helpers in template.go (`alertOOB`, `bannerOOB`, `fieldAlertOOB`,
  `inlineErrorOOB`, `saveErrorsOOB`, `renderFragment`), which escape the
  message rather than concatenating it into markup.

## Layout

One area per file, named after it: `monitor_*`, `alert_*`, `notification_*`,
`mail_group*`, `service.go`, `subscribe*`, `setup_*`, `settings*`, `config_*`.
Handlers, their SQL helpers and their markup sit together.

- `main.go` - flags, env bootstrap, router, background loops, servers.
- `env.go` - every `STATUSNOOK_*` variable. Invalid values are fatal.
- `bootstrap.go` - reconciles the database with the environment on each start.
- `github.go` - GitHub contents API client, sync, poll loop.
- `config_github.go` - push webhook (HMAC verified, branch filtered).
- `config_apply*.go` - `applyConfig` as ordered steps on a `configApplier`.
- `config_generate.go` - the reverse: database to YAML.
- `db.go` - data dir, DSN, schema bootstrap, migrations from `migrations/`.
- `context.go` - middleware, page context, session cookie construction.
- `monitor_loop.go`, `notification_loop.go`, `health.go` - background loops.

## Invariants

- Env-provided settings are re-applied on every start and their UI controls go
  read-only. The environment owns what it sets.
- The GitHub token from the environment stays in memory: never written to the
  database, never rendered into a page.
- Session cookies are `Secure` only when the request actually arrived over TLS
  (`requestIsHTTPS`, which consults `X-Forwarded-Proto` only when
  `STATUSNOOK_TRUST_PROXY=true`). Marking them `Secure` on a plain-HTTP LAN
  deployment locks the operator out.
- Sessions expire after `sessionLifetime`; `validateSession` enforces it.
- Every write goes through `rwDB` (single connection, `_txlock=immediate`);
  reads use `db`.
- All state lives under `dataDir()`: database, secret key, certificates.
- Mail goes through `sendMail` (smtp.go), never `net/smtp.SendMail`: the
  standard function dials without a timeout and used to be able to wedge the
  notification loop permanently.
- Monitor frequency/timeout/attempts come from the allow-lists in
  monitor_option.go; the radio groups in the forms must match them.

## Docs

- `docs/deployment.md` - Docker/Compose, TLS, the private-repo config flow, the
  token's exact permissions, every environment variable.
- `docs/configuration.md` - the YAML config format and secret encryption.
