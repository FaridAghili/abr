# sites

A small CLI and terminal menu for an owner-managed Ubuntu 26.04 LTS AMD64 VPS.
Developed on macOS Apple Silicon. Version stays **0.1.0** during stabilization.
Caddy serves directly with automatic HTTPS. Laravel supports PHP-FPM or shared
RoadRunner/Octane; Nuxt runs its Node server with the project's existing SSR setting.
Each app gets a dedicated Ubuntu user. Ports and managed databases are automatic.

## Prepare the VPS

Start with a clean Ubuntu 26.04 AMD64 host. Attach Ubuntu Pro if desired, run your
initial apt update/upgrade, and install your SSH public key for the existing root
or sudo account. **Test a second SSH session with that key before setup.** Keep
that session open. Allow SSH and TCP 80/443 plus UDP 443 in the provider firewall.
Point application DNS records directly at the VPS; publish AAAA only with working IPv6.

Download the archive and checksum from the latest successful main-branch
[GitHub Actions run](https://github.com/FaridAghili/sites-manager/actions).
The original `v0.1.0` release assets predate these host-management features.
Extract the workflow artifact ZIP first, then:

```sh
sha256sum -c sites-linux-amd64.tar.gz.sha256
mkdir sites-distribution
tar -xzf sites-linux-amd64.tar.gz -C sites-distribution
cd sites-distribution
./sites setup --dry-run
sudo ./sites setup
sudo install -m 755 sites /usr/local/bin/sites
```

If the SSH account differs from the sudo user, use `setup --admin-user ubuntu`
(or `root`). Setup verifies an authorized key before installing packages, then
validates effective key-only SSH settings before reload. Root can log in with a
key; passwords, keyboard-interactive authentication, and empty passwords are disabled.
Conflicting SSH settings fail with restoration of the managed configuration.
Setup does not create an administrator, attach Pro, reboot, or replace your SSH keys.

Setup installs and verifies:

- PHP 8.5 CLI/FPM with bcmath, curl, gd, imagick, intl, mbstring, mysql, redis,
  xml, zip, and Excimer. SVG support includes ImageMagick's extra codecs and
  librsvg, with an actual SVG-to-PNG test that preserves packaged security limits.
- Node 24 from its signed NodeSource repository, latest compatible npm,
  npm-check-updates, and SVGO. Global npm installations disable lifecycle scripts.
- Latest stable Caddy from its signed official repository, with administration
  restricted to a private Unix socket; latest stable Composer
  with official SHA256 verification, and latest stable RoadRunner with GitHub's
  asset SHA256 verification. RoadRunner is shared under `/opt/roadrunner/VERSION/`.
- Ubuntu MySQL 8.4: loopback binding, local-infile disabled, root socket
  authentication, and strong password validation. Existing anonymous/remote-root
  accounts or a test database cause an error for manual review; setup deletes no data.
- Redis bound to loopback with protected mode, image optimization utilities
  (gifsicle, jpegoptim, libavif, optipng, pngquant, webp), Git, build/ACL tools,
  Fail2ban's SSH jail, and unattended security updates.

UFW preserves discovered sshd/socket/current-connection ports and allows web
traffic, with incoming denied and outgoing allowed. `--ssh-port PORT` preserves
an additional SSH port. `--no-firewall` leaves UFW unchanged; `--no-redis` and
`--no-images` skip those optional packages. PHP-FPM gets execution/post/upload
limits of 60s/128M/100M; CLI execution remains unlimited, and PHP exposure is disabled.

Setup copies missing templates to `/etc/sites/templates`, preserving local edits,
and preserves the existing Caddyfile. Rerunning can upgrade packages and restart
shared services. `--roadrunner-version YEAR.MAJOR.PATCH` selects a specific release.
Setup is not one transaction: package/runtime changes may remain after an error.
Keep host and database backups, and retry after resolving the reported failure.

## One key for private GitHub repositories

```sh
sudo sites git setup
```

Add the printed public key once to **GitHub account → Settings → SSH and GPG keys
→ New SSH key → Authentication Key**. Reruns reuse the identity. To import an
existing unencrypted key instead, use `git setup --key /home/ubuntu/.ssh/id_ed25519`.
The root-owned shared identity is readable only by managed app users through ACLs;
Git commands select it explicitly and strictly verify GitHub's host key.
Apps share its account permissions, including repository write access where granted.
Private Composer/npm packages outside the app repository need their own credentials.

## Register and deploy

For Laravel, replace the repository, name, and domain:

```sh
sudo sites clone git@github.com:OWNER/PROJECT.git /srv/apps/api
sudo sites register --name api --dir /srv/apps/api --type laravel \
  --domain api.example.com --alias www.api.example.com --web-driver octane \
  --octane-workers 2 --queue-workers 2 --scheduler
sudo sites database api --show
sudo install -m 600 /srv/apps/api/.env.example /srv/apps/api/.env
sudoedit /srv/apps/api/.env
sudo sites deploy api --no-pull
sudo sites status api
```

Copy the generated database values into `.env`, together with production settings
and your application's secrets. Registration creates `sites-api` and a scoped
MySQL database/user without global privileges. Credentials remain root-readable
under `/var/lib/sites/`; they are revealed only with `database APP --show`.
Use `--no-database` during registration for an existing database. For PHP-FPM,
use `--web-driver fpm` (default); it uses a Unix socket and needs no TCP port.

Nuxt works the same for SSR and `ssr: false`:

```sh
sudo sites clone git@github.com:OWNER/NUXT-PROJECT.git /srv/apps/web
sudo sites register --name web --dir /srv/apps/web --type nuxt --domain example.com
# Prepare the project's .env if required.
sudo sites deploy web --no-pull
```

After the first deployment, `sudo sites deploy api` pulls with `git pull --ff-only`.
`deploy APP...` runs sequentially; `deploy --all` selects every app. Dirty working
trees are rejected. Commit dependency lockfiles. Commands run as the app user:

1. For Nuxt and Laravel with `package.json`: `npm ci --include=dev`, then `npm run build`.
2. For Laravel: `composer install --no-dev --optimize-autoloader --no-interaction
   --prefer-dist` and `composer check-platform-reqs --no-dev`.
3. Initialize a missing Laravel APP_KEY and storage link, clear cached config,
   run `artisan migrate --force`, then `artisan optimize:clear` and `artisan optimize`.
4. Render and validate host configuration, start selected services, verify
   sockets/listeners, reload Caddy, and run the configured optional HTTP health check.

Laravel's frontend build must work before `vendor/` exists on a fresh clone.
Composer/npm packages and scripts must already declare the needed dependencies.
No custom `deploy.sh` is used. Laravel runs with production environment and debug off.
Deployments are **in place with downtime**; failures are reported and can leave
an app disabled. Code/schema rollback is not automatic. Private deployment
logs and results are stored under `/var/lib/sites/deployments/`.

Additional Laravel components: `--nightwatch` needs its package/token;
`--inertia-ssr` needs an SSR bundle built by the project's build command that
honors `process.env.SSR_PORT`. The manager supplies `INERTIA_SSR_URL`.
Queue timeout is 60s; set Laravel's `retry_after` above that. The scheduler uses
a systemd timer. No manual worker/service startup is required for selected components.

| Component | Stable reserved ports |
| --- | --- |
| Octane | HTTP and RoadRunner RPC |
| Nuxt | HTTP |
| Inertia SSR | SSR |
| Nightwatch | Ingest |
| FPM, queue, scheduler | None |

RoadRunner and Nuxt bind to loopback. Inertia's server must honor its assigned
port; configure loopback in the application where supported and keep UFW restricted.
Assignments survive deploy/disable and are released on removal. The default
allocation range is 10000–19999; `register --port ENDPOINT=PORT` imports free ports.

## Manage applications

```sh
sudo sites                  # Interactive menu; also: sites tui
sudo sites list
sudo sites ports
sudo sites status
sudo sites restart api      # Or: restart api web / queue / scheduler / nightwatch / inertia-ssr
sudo sites logs api queue --follow
sudo sites disable api
sudo sites enable api
sudo sites remove api
```

The menu offers setup, shared Git identity, clone, registration, deploy, services,
database, and portable checks. Arrows navigate; `/` searches; Tab moves between
form fields; Space toggles; Esc goes back. Confirmations default to Cancel.

Repeated `--alias DOMAIN` redirects to the main domain; `--serving-domain DOMAIN`
serves the same app without redirect. Unrelated apex/subdomain apps are independent.
Wildcard domains are unsupported. Change aliases/components/health checks later by
editing TOML, validating, allocating any new ports, and reconciling:

```sh
sudoedit /etc/sites/config.toml
sudo sites config validate
sudo sites ports --allocate
sudo sites enable api
```

Keep app names/users/directories fixed after registration; those identify owned
resources. `disable` retains users/databases/ports. `remove` deletes only recorded
managed host files and the app user, revokes shared-key access, and releases ports.
It preserves projects, .env, uploads, homes, MySQL databases/users, and credentials;
retained files become root-owned to prevent access through UID reuse.
Back up both application data and `/var/lib/sites`.

Defaults are `/etc/sites/config.toml`, `/var/lib/sites`, `/etc/sites/templates`,
and `/srv/apps`. Override with `--config`, `--state-dir`, `--templates-dir`, and
`--apps-dir`. TOML uses maintained go-toml v2 and rejects unknown keys, duplicates,
overlapping projects, shared users, and invalid components. See
[examples/config.toml](examples/config.toml). `doctor` checks portable config/state
and port availability; a running managed port is occupied, so use `status`/`logs`
to assess live services. Keep permanent lock files; process exit releases locks.

## Development and distribution

Use the Go version in `go.mod`. Host commands require root on Ubuntu 26.04 AMD64;
macOS can run portable commands and host previews without touching system paths:

```sh
go build -o bin/sites ./cmd/sites
work=$(mktemp -d)
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --config-only --name demo --dir /srv/apps/demo --type nuxt --domain demo.example.com
./bin/sites --config "$work/config.toml" config validate
./bin/sites --config "$work/config.toml" --state-dir "$work/state" list
./bin/sites --state-dir "$work/state" ports
./bin/sites --config "$work/config.toml" --state-dir "$work/state" \
  --templates-dir ./templates deploy demo --dry-run

gofmt -w cmd internal
test -z "$(gofmt -l cmd internal)"
go vet ./...
go test ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags '-s -w' -o bin/sites-linux-amd64 ./cmd/sites
```

GitHub Actions runs on push, PR, and manual dispatch with official actions and
Go from `go.mod`. It checks formatting/vet/tests, packages the Linux AMD64 CGO-free
executable, standalone `templates/`, example TOML and README, generates SHA256,
and uploads archive/checksum. Keep templates beside the executable when running
initial setup. A separate disposable Ubuntu job tests actual setup, SSH key login,
MySQL, Laravel FPM/Octane, Nuxt SSR/SPA, and lifecycle operations. It never contacts
production. Review that job before using a VPS. `scripts/host-smoke.sh` changes an
entire host and requires Actions or explicit `SITES_HOST_TEST=1` on a disposable host.
