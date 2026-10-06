# sites

A small CLI for an owner-managed Ubuntu 26.04 LTS AMD64 VPS. Development runs on
macOS Apple Silicon. The version remains **0.1.0** while stabilizing.

Caddy serves applications directly with automatic HTTPS. No Nginx or Cloudflare
is required. PHP, Node, Composer, MySQL, Redis, and RoadRunner are shared system
installations. Every application gets a dedicated managed Ubuntu user.
Nuxt always runs through its Node server; the project controls SSR itself.

## Local development

Use the Go version in `go.mod`:

```sh
go build -o bin/sites ./cmd/sites
./bin/sites version
./bin/sites setup --dry-run --ssh-port 22

work=$(mktemp -d)
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --config-only --name mango-web --dir /srv/apps/mango-web \
  --type nuxt --domain web.example.com
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --config-only --name mango-api --dir /srv/apps/mango-api \
  --type laravel --domain api.example.com --web-driver octane \
  --octane-workers 2 --queue-workers 2 --scheduler
./bin/sites --config "$work/config.toml" config validate
./bin/sites --config "$work/config.toml" list
./bin/sites --state-dir "$work/state" ports
./bin/sites --config "$work/config.toml" --state-dir "$work/state" doctor
./bin/sites --config "$work/config.toml" --state-dir "$work/state" \
  --templates-dir ./templates deploy mango-web --dry-run
```

`--config-only` registers metadata and reserves ports without host operations.
Host previews execute no commands, download nothing, and write no files.
An enable/deploy preview requires existing port reservations; after editing TOML,
run `ports --allocate`. A preview does not establish that packages, permissions,
application dependencies, or DNS will work on the VPS.

## Initial VPS setup

Start with Ubuntu 26.04 LTS AMD64. Establish SSH key access and a sudo login before
setup. Point each domain's A record at the VPS; only publish AAAA records if IPv6
is working. Allow the same SSH/web ports in the provider's firewall.

Extract the new workflow artifact, preserving its executable and `templates/`:

```sh
sha256sum -c sites-linux-amd64.tar.gz.sha256
mkdir sites-distribution
tar -xzf sites-linux-amd64.tar.gz -C sites-distribution
cd sites-distribution
./sites setup --dry-run
sudo ./sites setup
sudo install -m 755 sites /usr/local/bin/sites
```

Setup installs PHP 8.5 CLI/FPM and extensions (bcmath, curl, gd, imagick, intl,
mbstring, mysql, redis, xml, zip), Composer, MySQL, Redis, Node 24/npm, Caddy,
Git, build tools, ACL tools, UFW, Fail2ban, unattended-upgrades, ncdu, and the
image tools from the server checklist, including shared SVGO 4.1.0.
Caddy uses its official stable repository;
Node uses the signed NodeSource 24 repository. Composer comes from Ubuntu.
RoadRunner defaults to release `2025.1.15`, installed once under
`/opt/roadrunner/VERSION/rr`, with `/usr/local/bin/rr` pointing at it. Downloads
must match GitHub's published asset SHA256. Previous version directories remain.
Use `--roadrunner-version YEAR.MAJOR.PATCH` for a deliberate shared upgrade.
An existing regular `/usr/local/bin/rr` must be relocated before setup; existing
symlinks are replaced.

Setup preserves existing Caddy configuration and adds a `sites-*.caddy` import.
It copies missing templates into `/etc/sites/templates`, preserving local edits.
It creates a missing empty config, enables services, and reloads FPM/Caddy. It
sets PHP CLI to 8.5, FPM execution time to 60s, post size to 128M, upload size to
100M, and leaves CLI execution time unlimited. It enables the SSH Fail2ban jail.

Before enabling UFW, setup allows discovered SSH ports, TCP 80/443, and UDP 443.
For socket-activated SSH, port forwarding, or a nonstandard SSH configuration,
pass the actual reachable port with `--ssh-port PORT`. Use `--no-firewall` to
leave UFW unchanged. Setup does not change SSH authentication or create an admin
login. It does not attach Ubuntu Pro, reboot, or install personal shell plugins.
Redis and image utilities are included by default; `--no-redis` and `--no-images`
skip them. Initial setup targets a clean host; rerunning it can upgrade installed
packages and reload shared services. Runtime upgrades are manual owner actions.

## Add and deploy Laravel

Clone the project into its own directory under `/srv/apps`. This example uses a
public HTTPS repository; replace the URL and domain with yours:

```sh
sudo git clone https://github.com/OWNER/PROJECT.git /srv/apps/mango-api
sudo sites register --name mango-api --dir /srv/apps/mango-api \
  --type laravel --domain api.example.com --web-driver octane \
  --octane-workers 2 --queue-workers 2 --scheduler
sudo sites database mango-api --show
sudo cp /srv/apps/mango-api/.env.example /srv/apps/mango-api/.env
sudoedit /srv/apps/mango-api/.env
sudo sites deploy mango-api --no-pull
```

Registration creates `sites-mango-api`, gives it ownership of the clone, reserves
only required ports, and creates a dedicated MySQL database and localhost user.
Only that database receives privileges, without `GRANT OPTION`. Credentials are
saved as root-readable files under `/var/lib/sites/credentials/` and
`/var/lib/sites/databases/`; they are never stored in TOML or printed unless
`database APP --show` is used. Keep the state directory backed up: it records
ownership and the password needed for retrying a partial database creation.
The generated password includes 256 random bits and mixed character classes.

Prepare `.env` with production application settings, generated database values,
and any mail/storage/Nightwatch secrets. Deployment verifies that managed DB
values match. Use `--no-database` during registration to use an existing database
instead. The tool refuses to adopt an existing MySQL database/account or an
existing Ubuntu user without its own ownership record. No database import,
password reset, or database deletion is performed.

For PHP-FPM, register with `--web-driver fpm` (the default). It gets an `ondemand`
pool with five children and a Unix socket accessible to Caddy. No TCP port is
reserved. App users are system accounts with login disabled and private homes
under `/var/lib/sites-users/`. A `--user` override must still be a new dedicated
account. No manual user creation is needed. Caddy receives ACL access to public
assets; `.env` is mode 600 and the project root is private to its user plus Caddy
traversal access. Public upload directories receive inherited read ACLs.

## Add and deploy Nuxt

The same process works for Medfolio SSR and Mango's `ssr: false` SPA:

```sh
sudo git clone https://github.com/OWNER/NUXT-PROJECT.git /srv/apps/mango-web
sudo sites register --name mango-web --dir /srv/apps/mango-web \
  --type nuxt --domain web.example.com
# Prepare .env if the project requires it.
sudo sites deploy mango-web --no-pull
```

Nuxt gets one HTTP port, a systemd service running
`node --env-file-if-exists=.env .output/server/index.mjs`, and a Caddy reverse proxy.
The service binds to loopback with `NITRO_HOST` and `NITRO_PORT`. Static generation
is outside this version; there is no SSR/static mode flag or separate static
service template.

## Standard deployment commands

`deploy APP...` deploys sequentially; `deploy --all` selects all registered apps.
Flags also work after an app name. Deployments reject dirty Git working trees,
run `git pull --ff-only` unless `--no-pull` is specified, and then execute:

- Laravel: `composer install --no-dev --optimize-autoloader --no-interaction
  --prefer-dist`, then `composer check-platform-reqs --no-dev`.
- Laravel initialization: generate `APP_KEY` only when absent, clear cached
  configuration, and create `public/storage` only when absent. Existing keys
  and uploads remain intact.
- Nuxt, and Laravel projects with `package.json`: `npm ci --include=dev`, then
  `npm run build`. Build tools require dev dependencies even in production.
- Laravel: `php8.5 artisan migrate --force --no-interaction`, then
  `php8.5 artisan optimize:clear --no-interaction` and
  `php8.5 artisan optimize --no-interaction`. Application caches are cleared
  after migrations so the first deployment can create the database cache table.
- Render/validate configuration, start services, verify sockets/listeners,
  reload Caddy, and perform the optional HTTP health check.

Composer and npm lockfiles must be committed. Commands run as the app user,
including Git. A private repository needs a read-only deploy key/credentials
available to that account's home; the manager does not copy your personal SSH
keys. Use `--no-pull` for the initial clone or an intentionally prepared checkout.
Projects must already declare Octane/RoadRunner or other selected Composer
packages; the manager installs locked dependencies without editing requirements.
Nuxt's `.env` is read by Node at runtime as well as by Nuxt at build time.

Deployment is **in place with downtime**: managed routing/services are disabled
before pulling or replacing code. A failed deployment stops immediately and
leaves the app disabled, or reports a failed health check after startup. Code and
schema rollback are not automatic. Private logs/history record the commit,
timestamps, and outcome under `/var/lib/sites/deployments/`. No `deploy.sh`, shell
hooks, or interactive menu are executed. Laravel always runs with production
`APP_ENV` and debug disabled through the managed environment.
Octane also receives `OCTANE_HTTPS=true` for direct HTTPS through Caddy.

## Manage services

```sh
sudo sites enable mango-api
sudo sites status mango-api
sudo sites status
sudo sites restart mango-api
sudo sites restart mango-api web
sudo sites restart mango-api queue
sudo sites logs mango-api queue --follow
sudo sites disable mango-api
sudo sites remove mango-api
```

`enable` reconciles generated files and component counts. Caddy and FPM config
are validated before reload. Ordinary configuration failures restore prior files
and services; a failed restoration returns an error and retains recovery state.
Reconciliation and backend switching can interrupt requests; no zero-downtime
switch is promised. `restart APP` performs reconciliation; selecting a component
restarts only that component. FPM `web` restart reloads the shared FPM master.
Its journal is shared across pools. Status returns systemd's nonzero result for
inactive/failed services. Queue services allow 120s to stop and use a 60s worker
timeout; set Laravel's queue `retry_after` above this timeout.

`disable` retains users/databases/ports. `remove` stops services, verifies that
ports are free and the owned user has no remaining processes, removes managed
configuration and the Ubuntu account, then unregisters and releases ports.
It preserves the clone, `.env`, uploads, home directory, MySQL accounts/databases,
and credentials. Draining FPM requests can delay removal; retry once they finish.
Retained project/home files become root-owned before account deletion, so a
reused Linux UID cannot read their secrets. Registration restores ownership.
An interrupted removal may leave conservative orphan reservations; errors are
reported and `doctor` detects them. Accounts/files with changed identities or
unmanaged content are refused.

Repeated `--alias DOMAIN` adds permanent HTTPS redirects to the main domain;
`--serving-domain DOMAIN` serves the same app on additional names. Direct HTTPS
uses public ACME certificates with working DNS and reachable ports. Wildcard
metadata may be stored with `--config-only`, but enabling it returns an error:
DNS challenge plugins and credentials are not implemented.

Optional Laravel flags: `--nightwatch`, `--inertia-ssr`, `--queue-workers N`,
`--scheduler`. Nightwatch needs its package and token in `.env`. Inertia needs a
built SSR bundle that explicitly uses `process.env.SSR_PORT` for its port; the
manager also supplies `INERTIA_SSR_URL`. Startup fails if it does not listen on
the reserved port. Inertia's stock server may bind all interfaces: keep UFW
restricted to SSH/web and configure loopback in the application where supported.
RoadRunner HTTP/RPC and Nuxt bind explicitly to loopback.

## Configuration and state

Defaults: `/etc/sites/config.toml`, `/var/lib/sites/`, `/etc/sites/templates/`,
and `/srv/apps/`. Override them with `--config`, `--state-dir`, `--templates-dir`,
`--apps-dir`. Host commands still require root on Ubuntu 26.04 AMD64; alternate
paths do not bypass platform checks. See [examples/config.toml](examples/config.toml).
TOML uses maintained [go-toml v2](https://github.com/pelletier/go-toml). Unknown
keys, duplicates, shared runtime users, overlapping project trees, and invalid
component combinations are rejected.

**Migration from the original milestone:** remove `[apps.nuxt] mode = ...` and
`deploy_file` entries. Each Nuxt app now needs a `nuxt-http` reservation, including
formerly static apps. Change pre-existing runtime user names to unused dedicated
accounts if necessary, then run `ports --allocate`. The `--nuxt-mode` and
`--deploy-file` flags no longer exist. Original release `v0.1.0` assets are older;
use the workflow artifact for this implementation until a new release is published.

| Component | Reserved endpoint |
| --- | --- |
| Octane | `octane-http`, `roadrunner-rpc` |
| Nuxt | `nuxt-http` |
| Inertia SSR | `inertia-ssr` |
| Nightwatch | `nightwatch-ingest` |
| FPM, queue, scheduler | No TCP ports |

Registration supports `--port ENDPOINT=PORT` to import free assignments. Automatic
range defaults to 10000–19999. Assignments remain stable across deployment,
disabling, and range changes. Locks and atomic writes protect portable state.
A host-operation lock serializes setup/deploy/lifecycle commands. Lock acquisition
waits at most 10s; locks release on process exit. Leave permanent lock files in place.
All generated files carry an ownership marker and paths use a `sites-` prefix.
Multiple files and host commands cannot be one atomic transaction; interrupted
operations retain state/resources for inspection and retry. Registration can
persist before a later permission/database step fails; use `database APP` or
`enable APP` to finish preparation, rather than duplicate-registering it.

`doctor` remains a portable configuration/registry/port check. It does not identify
which process owns a port, so a running managed listener is reported as occupied.
Use `status`, `logs`, and the optional deployment health check for a running app.

## Checks and distribution

```sh
gofmt -w cmd internal
test -z "$(gofmt -l cmd internal)"
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags '-s -w' -o bin/sites-linux-amd64 ./cmd/sites
```

GitHub Actions runs formatting/vet/tests, builds the CGO-free Linux AMD64 binary,
packages it with `templates/`, the example config, and README, generates SHA256,
and uploads the archive/checksum. It runs on pushes, pull requests, and manual
runs, using the Go version in `go.mod` and official supported actions.
A separate disposable Ubuntu job runs setup (without enabling UFW), deploys
sample Laravel/FPM and both SSR/SPA Nuxt apps, checks local HTTPS and MySQL, and
exercises restart/disable/remove. It uses localhost certificates and never
connects to the production server. The same destructive fixture script is
`scripts/host-smoke.sh`; outside Actions it requires `SITES_HOST_TEST=1` explicitly.

Local tests cover private credentials, partial SQL retry, refusal to adopt accounts,
configuration rollback, safe removal, failed deployments, and previews alongside
registry/locking/listener tests. Real host integration is exercised by the Ubuntu
workflow; it cannot run natively on macOS. Review its result before using a real VPS.
