# How this is tested

Upstream shipped no tests. What is here now drives the real router against a
real database, and reaches the parts that talk to the network through seams
rather than mocks of the app's own code.

## The harness

`apphttp_test.go` boots an instance the way `main` does -- `initDB`, the
migration runner, `newRouter`, a real session cookie -- against a temporary
working directory. Two entry points:

- `newTestApp(t)` is a first boot: `schema.sql`'s `setup=domain`, no account.
- `withTestApp(t)` is past setup, with an admin logged in.

Both swap the `db` and `rwDB` globals and put them back, and both clear the
login throttle first: it is a package-level map keyed by client IP and every
test shares 127.0.0.1, so a test that spends its ten attempts would lock out
every test after it.

## Seams

Four variables exist so a test can point production code somewhere local. Each
is restored by the test that changes it.

| Seam | Where | What it unlocks |
|---|---|---|
| `githubAPIBaseURL` | `app.go` | the config webhook, the repo and config-path checks, the update check |
| `notificationLoopInterval` | `notifications.go` | the queue, without waiting out a ten-second tick |
| `SSL_CERT_FILE` (set in `TestMain`) | `smtpfake_test.go` | every SMTP send |
| `sqlite3-faulty` driver | `faultydriver_test.go` | every branch after a query |

`SSL_CERT_FILE` is set in `TestMain` because `crypto/x509` caches the system
pool on first use. `net/smtp` upgrades to TLS whenever the server offers
STARTTLS and verifies the certificate, and statusnook's own auth refuses to
send credentials over anything else -- so the fake SMTP server has to present
a certificate the process trusts.

## The sweeps

Three tests replay every route rather than asserting one case:

- **`TestHandlersSurfaceAFailureAtEveryStatement`** arms the faulty driver to
  fail statement _n_, replays the request for _n_ = 1, 2, 3... and requires a
  5xx as long as the fault actually fired. A handler that logs an error and
  renders anyway fails it. That is how `getEditService` was found rendering an
  edit form built from a failed lookup, and how the setup gate was found
  answering 200 with an empty body.
- **`TestFormsAnswerHostileInput`** replaces each field of each form in turn
  with a value nobody would type. The requirement is only that something comes
  back: a panicking handler drops the connection, which from outside is
  indistinguishable from the instance being gone.
- **`TestHandlersFailCleanlyWhenTheDatabaseIsGone`** closes the pool and hits
  everything, which covers the first `Begin` of every handler at once.

## What is not covered, and why

Coverage is 69.6%; go-toolchain requires 80%. The gap is not spread evenly --
it is five things that need something this process cannot have:

| Uncovered | Statements | Needs |
|---|---|---|
| `main` | 104 | listeners, TLS, signal handling, shutdown |
| `postSettings` domain branch, `postSetupDomain`, `lookupDomain`, `monitorUnconfirmedDomainLoop`, `attemptCertificateAcquisition` | ~330 | DNS to the root servers and a live ACME server |
| `postUpdate` | 73 | downloading a release binary over the running one and restarting into it |
| `slackOAuth2Callback` | 64 | slack.com's OAuth endpoint (no seam yet) |
| `postSubscribeEmail`'s suppression sync | ~85 | api.postmarkapp.com (no seam yet) |

Roughly 650 statements, near 10% of the module. The Slack and Postmark halves
could be reached the same way GitHub was, with a base-URL seam; the ACME,
DNS and self-update paths could not, short of running against real services.
