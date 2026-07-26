# Deployment

Statusnook reads its settings from the database (populated by the setup wizard
and the admin UI) and from the environment. **Whatever the environment sets, the
environment owns**: those values are re-applied on every start and the matching
UI controls become read-only. That is what makes a container reproducible - the
compose file is the source of truth, not a state the operator clicked into
existence once.

## Docker Compose

The repository ships a working [`docker-compose.yml`](../docker-compose.yml) and
[`.env.example`](../.env.example).

```
cp .env.example .env
$EDITOR .env
docker compose up -d
```

The container comes up with the admin account already created, the
configuration pulled from your repository, and monitors running. There is no
setup wizard to walk through.

- **Data**: everything Statusnook persists (database, the key that decrypts
  config secrets, managed certificates) lives in `/app/statusnook-data`. The
  compose file mounts a named volume there. That is the only thing to back up.
- **User**: the image runs as uid/gid `10001`, not root. With a *named volume*
  this needs no thought. With a **bind mount** (`/volume1/docker/statusnook` on
  a Synology, for example) the host directory must be writable by that uid, or
  set `user: "1000:1000"` in the service and `chown` the directory to match.
- **Health**: `GET /healthz` returns 200 when the process can read its
  database. The image's `HEALTHCHECK` uses it.
- **Updates**: `docker compose pull && docker compose up -d`. In-place
  self-update is disabled under Docker - the image owns the binary.

### TLS

By default the container serves plain HTTP on 8000, for a reverse proxy
(Caddy, nginx, Traefik, your NAS's built-in proxy) to terminate TLS in front of
it. When you do that, set `STATUSNOOK_TRUST_PROXY=true` so Statusnook believes
`X-Forwarded-Proto` and marks session cookies `Secure`.

To let Statusnook manage its own certificates instead, set
`STATUSNOOK_TLS=auto` and `STATUSNOOK_DOMAIN=status.example.com`, and publish
ports 80 and 443. The domain must resolve to the host from the public internet;
Let's Encrypt has to reach it.

> Do **not** set `STATUSNOOK_TRUST_PROXY=true` unless a proxy you control sits
> in front of Statusnook. It makes Statusnook trust client-supplied headers for
> the scheme and the client address.

## Configuration from a private GitHub repository

Point Statusnook at a repository holding a single YAML file - the monitors,
services, mail groups and notification channels it should watch. See
[configuration.md](configuration.md) for that file's format.

```yaml
STATUSNOOK_GITHUB_REPO: your-org/your-statusnook-config
STATUSNOOK_GITHUB_CONFIG_PATH: statusnook.yaml
STATUSNOOK_GITHUB_BRANCH: ""        # empty: the default branch
STATUSNOOK_GITHUB_TOKEN_FILE: /run/secrets/statusnook_github_token
STATUSNOOK_GITHUB_POLL_INTERVAL: 60s
```

Statusnook polls the file and applies it whenever its contents change. **No
inbound connectivity is required**, which is the point for a NAS behind NAT:
GitHub never has to reach your network. A webhook is optional and only makes
the update immediate.

While the repository is configured this way:

- the config settings page shows the repository, branch and path as
  read-only - change the environment to point somewhere else
- the config editor is read-only; the repository is the source of truth
- `applyConfig` runs on each new revision, so a broken config leaves the last
  good one running and records the errors on the settings page

### The token

Create a **fine-grained** personal access token
(<https://github.com/settings/personal-access-tokens/new>):

- **Resource owner**: the account or org that owns the config repository
- **Repository access**: *Only select repositories* -> that one repository
- **Permissions**: *Repository permissions* -> **Contents: Read-only**. Nothing
  else. No account permissions.

That is the entire access Statusnook needs. It never writes to the repository.

Pass it as `STATUSNOOK_GITHUB_TOKEN`, or as `STATUSNOOK_GITHUB_TOKEN_FILE`
pointing at a file (Docker/Kubernetes secret mounts) to keep it out of the
process environment. The token is held in memory only: it is never written to
the database and never rendered into a page, so a database backup does not leak
repository access.

Fine-grained tokens expire (one year maximum). When the token expires, syncs
start failing and the log says so - the last applied config keeps running.

### Webhook (optional)

If GitHub *can* reach the instance, set `STATUSNOOK_GITHUB_WEBHOOK_SECRET` (or
`..._FILE`) and add a webhook in the repository:

- Payload URL: `https://<your-domain>/github-config-webhook`
- Content type: `application/json`
- Secret: the same value
- Events: just pushes

Deliveries are rejected unless the HMAC signature matches, and a push to a
branch other than the watched one is ignored. Without a secret configured the
endpoint answers 404.

## Environment variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `STATUSNOOK_PORT` | `80` (`8000` in the image) | HTTP port. `-port` wins over it. Plain `PORT` is honoured when this is unset. |
| `STATUSNOOK_DATA_DIR` | `statusnook-data` | Where the database, secret key and certificates live. |
| `STATUSNOOK_NAME` | - | Status page name, re-applied on every start. |
| `STATUSNOOK_DOMAIN` | - | Public hostname for notification links, and the certificate subject when TLS is `auto`. |
| `STATUSNOOK_TLS` | - | `auto` (managed certificates) or `off`. Unset leaves the stored setting alone. |
| `STATUSNOOK_TRUST_PROXY` | `false` | Trust `X-Forwarded-Proto` / `X-Forwarded-For`. Only behind your own proxy. |
| `STATUSNOOK_ADMIN_USERNAME` | - | Admin account to create. Must be set together with the password. |
| `STATUSNOOK_ADMIN_PASSWORD` | - | Its password, minimum 8 characters. `..._FILE` supported. |
| `STATUSNOOK_GITHUB_REPO` | - | `owner/name` or a repository URL. |
| `STATUSNOOK_GITHUB_BRANCH` | default branch | Branch to read the config from. |
| `STATUSNOOK_GITHUB_CONFIG_PATH` | `statusnook.yaml` | Path to the config file in the repository. |
| `STATUSNOOK_GITHUB_TOKEN` | - | Read-only PAT. `..._FILE` supported. |
| `STATUSNOOK_GITHUB_POLL_INTERVAL` | `60s` | How often to poll. `0` disables polling. Minimum `10s`. |
| `STATUSNOOK_GITHUB_WEBHOOK_SECRET` | - | Enables the webhook endpoint. `..._FILE` supported. |
| `STATUSNOOK_GITHUB_API_URL` | `https://api.github.com` | For GitHub Enterprise Server. |
| `STATUSNOOK_DISABLE_SELF_UPDATE` | `true` under Docker | Blocks in-place binary replacement. |

Every secret-shaped variable also accepts a `..._FILE` form naming a file to
read it from. Setting both forms of the same variable is an error.

An invalid value is fatal at startup, with the variable named. A container that
silently ignores half its configuration is worse than one that refuses to
start.

### Admin account

While `STATUSNOOK_ADMIN_USERNAME`/`STATUSNOOK_ADMIN_PASSWORD` are set, they are
authoritative: the account is created if missing, and its password is reset on
start if it no longer matches (which also drops that user's sessions). Manage
the password in the environment, not in the UI - a password changed in the UI
goes back to the environment's value on the next restart. This is also the
recovery path for a lost admin password.

Setting them completes the setup wizard, so the instance is reachable
immediately. Additional users can still be invited from the admin UI.

## Standalone (systemd)

```
curl -fsSL https://get.statusnook.com | sudo bash            # ports 80 + 443
curl -fsSL https://get.statusnook.com | sudo bash -s -- -port 8000
```

The systemd unit runs from `/home/statusnook`. The environment variables above
work here too, via a `[Service]` `Environment=` line or an `EnvironmentFile=`.

TLS material moved into the data directory (`statusnook-data/certmagic`,
`statusnook-data/self-signed-*.pem`) so one directory holds all state. An
existing install's `certmagic/` directory and self-signed certificate files are
moved there automatically on first start.
