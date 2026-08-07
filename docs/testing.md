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

Production code carries a handful of variables so a test can point it
somewhere local. Each is restored by the test that changes it.

| Seam | Where | What it unlocks |
|---|---|---|
| `githubAPIBaseURL` | `app.go` | the config webhook, the repo and config-path checks, the update check |
| `slackAPIBaseURL` | `app.go` | the OAuth install callback |
| `postmarkAPIBaseURL` | `app.go` | the suppression list a subscribe consults |
| `notificationLoopInterval` | `notifications.go` | the queue, without waiting out a ten-second tick |
| `dbDSN(immediate)` | `db.go` | a second pool on the same file, for the faulty driver |
| `SSL_CERT_FILE` (set in `TestMain`) | `smtpfake_test.go` | every SMTP send |
| `sqlite3-faulty` driver | `faultydriver_test.go` | every branch after a query |

`SSL_CERT_FILE` is set in `TestMain` because `crypto/x509` caches the system
pool on first use. `net/smtp` upgrades to TLS whenever the server offers
STARTTLS and verifies the certificate, and statusnook's own auth refuses to
send credentials over anything else -- so the fake SMTP server has to present
a certificate the process trusts.

Both background loops do their work in a named function -- `drainNotificationQueue`
and `monitorScheduler.checkDueMonitors` -- rather than an anonymous closure per
tick, so a test runs one pass directly instead of arming a fault and hoping the
goroutine reaches it inside the window. A fresh `monitorScheduler` carries no
last-checked stamps, so every monitor is due on the pass it is handed.

## The sweeps

Rather than asserting one case, these replay a whole surface.

- **`sweepFaults`** arms the faulty driver to fail statement _n_, replays the
  request for _n_ = 1, 2, 3... and requires a 5xx as long as the fault actually
  fired. A handler that logs an error and renders anyway fails it. That is how
  `getEditService` was found rendering an edit form built from a failed lookup,
  and how the setup gate was found answering 200 with an empty body.
  `TestHandlersSurfaceAFailureAtEveryStatement` runs it over every route that
  needs no setup; the rest have a test each, because getting past their gate
  takes something -- a signature, a Postmark channel, GitHub sync on, or a
  token that is spent the first time it works.
- **A route that ends the session goes last, and a POST guard follows every
  swept route.** Logging out, logging in, accepting an invitation and following
  a cross-auth link each replace or delete the session the sweep runs under,
  and every later POST is then answered 401 or 403 before it runs a statement --
  so the sweep sees no fault fire, concludes there is nothing deeper to fail,
  and reports success having swept nothing. `requireStillAdmin` is what makes
  that loud.
- **`TestFormsAnswerHostileInput`** replaces each field of each form in turn
  with a value nobody would type. The requirement is only that something comes
  back: a panicking handler drops the connection, which from outside is
  indistinguishable from the instance being gone.
- **`TestHandlersFailCleanlyWhenTheDatabaseIsGone`** closes the pool and hits
  everything, which covers the first `Begin` of every handler at once.

## What is not covered, and why

Coverage is 80.6%; go-toolchain requires 80%. 1,304 of 6,726 statements are
uncovered, and they are two different problems.

The first needs something this process cannot have:

| Uncovered | Statements | Needs |
|---|---|---|
| `main` | 104 | listeners, TLS, signal handling, shutdown |
| `postSettings`' TLS branch, `postSetupDomain`, `lookupDomain`, `randomNS`, `monitorUnconfirmedDomainLoop`, `attemptCertificateAcquisition` | 297 | DNS answers from the root servers down and a live ACME server |
| `postUpdate` | 67 | downloading a release binary over the running one and restarting into it |
| `retentionLoop` | 8 | a ticker that only fires once a day |

476 statements, 7.1% of the module -- most of the 20% the gate allows, before
a single handler branch. The rest is an even spread of a few statements per
handler, so the margin over the gate is thin: a handler that loses its tests
takes CI red, and the way back is another sweep, not a lowered gate.
