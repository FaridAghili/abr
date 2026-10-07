# Using Abr

Keep `templates/` beside the executable during initial setup. Setup installs PHP
8.5/extensions (including Imagick SVG support and Excimer), Node 24, latest compatible
npm, npm-check-updates, Caddy, Composer, shared RoadRunner, MySQL 8.4, Redis and image
optimization tools. It configures key-only SSH, local databases, UFW, Fail2ban and
security updates. Root key login remains allowed. Use `--admin-user USER` when the
SSH account differs from the sudo user; `--ssh-port PORT` preserves an additional
port. `--no-firewall`, `--no-redis`, `--no-images` skip those features. Setup neither
attaches Ubuntu Pro nor reboots; failed setup can leave package changes in place.

For private repositories, use one GitHub account SSH key for the VPS:

```sh
sudo abr git setup
# Add the printed public key to GitHub account Settings → SSH and GPG keys.
sudo abr clone git@github.com:OWNER/PROJECT.git /srv/apps/api
sudo abr register --name api --dir /srv/apps/api --type laravel \
  --domain api.example.com --alias www.api.example.com --web-driver octane \
  --queue-workers 2 --scheduler
sudo install -m 600 /srv/apps/api/.env.example /srv/apps/api/.env
sudo abr database api --show
sudoedit /srv/apps/api/.env
sudo abr deploy api --no-pull
sudo abr status api
```

Copy the displayed database credentials into `.env` and set your app secrets.
Later, `sudo abr deploy api` pulls with `git pull --ff-only`. Apps share the GitHub
key's permissions; private Composer/npm dependencies may need separate credentials.
Use `git setup --key /absolute/private-key` to import an existing unencrypted key.

Laravel deployment runs **npm ci → npm run build → Composer install → migrations →
optimization → services**. It generates a missing APP_KEY and storage link. FPM is
the default driver. Optional flags: `--scheduler`, `--queue-workers N`, `--nightwatch`,
`--inertia-ssr`, `--octane-workers N`, `--health-check URL`; `--no-database` keeps DB
management external. Install the corresponding Laravel packages in your project;
Inertia's server bundle must honor `SSR_PORT`. Octane uses the shared RoadRunner;
remove any app-local `rr` binary. Deployments have downtime and no automatic rollback.

```sh
sudo abr clone git@github.com:OWNER/WEB.git /srv/apps/web
sudo abr register --name web --dir /srv/apps/web --type nuxt --domain example.com
# Prepare .env if needed; commit package-lock.json (and composer.lock for Laravel).
sudo abr deploy web --no-pull
sudo abr restart api web
sudo abr logs api queue@1
sudo abr ports
sudo abr disable api
sudo abr enable api
```

`--alias DOMAIN` redirects to the main domain; `--serving-domain DOMAIN` serves the
same app. Both are repeatable. Independent subdomain apps are supported; wildcards
are not. Edit `/etc/abr/config.toml` to change settings, then run `abr ports
--allocate` and `abr enable APP`. `abr remove APP` removes managed services/user
and reservations while preserving the project, secrets, home and database.

Database backups and imports are available in the menu and CLI:

```sh
sudo abr database backup api --output-dir /var/lib/abr/backups
sudo abr database backup api portal --output-dir /var/lib/abr/backups
sudo abr database backup --all --output-dir /var/lib/abr/backups
sudo abr disable api
sudo abr database import api /absolute/backup.sql --yes
sudo abr enable api
```

Selections are registered app names; `--all` includes every registered managed
DB, excluding MySQL system databases and external DBs. Each dump is a separate,
root-only SQL file in a new/private root-owned directory (0700). Backups stream
with an InnoDB consistent snapshot and include routines, triggers and events;
avoid schema changes during export. Nontransactional tables and multiple DBs are
not one consistent snapshot. Copy backups off the VPS. Imports use the app's scoped
account, disable client shell/file commands, and can overwrite data. Stop all
writers first. Failure may leave partial changes; import does not clear extra
existing tables or restart services. Dumps with foreign DEFINERs may need review.

Runtime templates stay editable under `/etc/abr/templates`; setup preserves
existing templates. Review/copy updated distribution templates when upgrading.
Shared settings apply on `abr setup`; app templates on `abr enable/deploy APP`.
Defaults suit a modest shared host: MySQL 256 MiB buffer pool/100 connections,
Redis 256 MiB with **no eviction** and AOF every second, FPM five ondemand children
per app/recycling every 500 requests, shared OPcache 128 MiB. Measure RAM/workload
and adjust these limits; Redis rejects writes at its limit and crashes can lose
about one second of writes. PHP error display/version headers are disabled.

Caddy strips Server/X-Powered-By headers, compresses dynamic responses with
[zstd/gzip](https://caddyserver.com/docs/caddyfile/directives/encode), and serves
[precompressed Brotli](https://caddyserver.com/docs/caddyfile/directives/file_server)
for built assets generated during deploy. Versioned Vite/Nuxt assets get immutable
browser caching; HTML/API/SSR responses keep the application's cache policy.
The packaged welcome page is replaced with a generic 404. Identifying text in
application bodies must be removed in the application itself.

For portable local development, use temporary paths and `--config-only`:

```sh
task_dir=$(mktemp -d)
go run ./cmd/abr register --config "$task_dir/config.toml" \
  --state-dir "$task_dir/state" --config-only --name demo \
  --dir /srv/apps/demo --type laravel --domain demo.example.com
go run ./cmd/abr list --config "$task_dir/config.toml"
go run ./cmd/abr --config "$task_dir/config.toml" --state-dir "$task_dir/state" \
  --templates-dir ./templates deploy demo --dry-run
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/abr-linux-amd64 ./cmd/abr
```

[config.example.toml](../config.example.toml) documents the configuration. `abr help`
lists commands; `abr doctor` checks portable config/registry/port availability.
CI checks formatting/vet/tests, packages binary/templates/example/checksum, and
runs actual setup/deployment/backup/restore tests on a disposable Ubuntu host.
`scripts/host-smoke.sh` changes an entire host: never run it on production.
