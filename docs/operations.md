# Operations guide

Start with the [README](../README.md). Use `abr help` for commands and
`abr COMMAND --help` for flags. Host operations require root on Ubuntu 26.04 AMD64;
`--dry-run` previews changes without applying them.

## Setup and updates

Test SSH key login before setup; password login is disabled. Root key login stays
allowed. `--admin-user USER` selects the SSH administrator, and `--ssh-port PORT`
preserves an extra port. Setup never reboots or attaches Ubuntu Pro.

Setup installs shared runtimes and configures root's Zsh with Oh My Zsh,
autosuggestions and syntax highlighting. Existing `.zshrc` settings are preserved.
Use `--no-firewall`, `--no-redis` or `--no-images` to skip optional features.
The VPS hostname labels the shared GitHub key; renaming preserves the key.

`sudo abr update` upgrades apt packages, Caddy, Composer, shared global npm tools
and shell plugins. It leaves app dependencies to deployment. Updates stop on
failure; completed package changes are not rolled back and no reboot occurs.
An optional `GITHUB_TOKEN`, passed with `sudo --preserve-env=GITHUB_TOKEN`, avoids
unauthenticated GitHub API rate limits. It is sent only to `api.github.com`.

## Apps and credentials

Clone by name into `/srv/apps/NAME`. Registration creates a dedicated runtime user
and, for Laravel by default, a managed MySQL database. Nuxt uses Node and Caddy
without Laravel services. CLI registration uses the domain as entered; the TUI
prefers `www` and adds a redirect. Use `--canonical-host as-entered|www|non-www`
to choose. Both hostnames need DNS when using a redirect pair.

For Laravel, `--web-driver octane`, `--queue-workers 2` and `--scheduler` enable
optional services. Shared RoadRunner uses the compatible 2025.1 series.
`--build-order composer-first` supports frontend builds that need Composer packages;
the default is `frontend-first`. Save other settings with `abr edit APP`, then deploy.

`abr env APP` prepares `.env` only when `.env.example` exists. It preserves existing
settings and fills managed MySQL values. New Laravel environments use production
mode, disabled debug and the registered HTTPS URL. Set your other secrets before
deployment. In the TUI, editing an existing `.env` triggers a `--no-pull` deployment
only when the saved contents change.

```sh
sudo abr composer auth --host nova.laravel.com
sudo abr database api --show
sudo abr database --admin --show
```

Composer prompts for the username and a hidden token. Scripts use
`--username USER --password-stdin`, with the secret supplied on stdin. Apps share GitHub and Composer
access. `git setup --key /absolute/private-key` imports an unencrypted private key.
Database `--show` explicitly displays passwords; the admin command provides a
TablePlus connection through SSH. MySQL stays on loopback.

App users have no login shell. For Laravel commands, use `abr artisan APP COMMAND`
or `abr shell APP`. Project command output stays private because errors can contain
SQL or credentials. Use application logs privately to investigate failures.

## Monitoring and logs

```sh
sudo abr logs api --type application
sudo abr logs --all --type deployment --lines 200
sudo abr disk --json
sudo abr disk --refresh
sudo abr doctor
```

Log snapshots are bounded and labeled by app/file. TUI log views support `r` to
refresh. `logs clear APP --yes` empties app/deployment file logs, keeping journals,
uploads and databases. No automatic log or backup retention is installed.

Disk reports cache app scans for one minute; `--refresh` rescans, while
`--filesystem-only` reads live capacity without scanning projects or querying SQL.
Doctor checks config, reservations and TCP listener ownership. It does not test
HTTPS or databases. Errors and unverifiable ownership return a nonzero exit code.
On macOS it checks local port availability only.

## Backups

```sh
sudo abr backup --all
sudo abr backup api portal --output-dir /var/backups/abr/apps
sudo abr backup --output /var/backups/abr/full.tar.gz
sudo abr database backup api --output-dir /var/backups/abr/sql
```

App archives include saved source and Git history, lockfiles, `.env`, Laravel
`storage/app`, managed SQL, settings and shared recovery credentials. Dependencies,
build output, caches and logs are omitted. Symlinks and Git submodules are refused.
External databases and uploads outside captured locations need separate backups.

A full-server archive also includes shared Redis persistence and Caddy certificate
storage. App archives omit both. Full backups require recorded managed MySQL
credentials for Laravel apps. Backups run online: SQL, files, Redis and separate
apps are **not one point-in-time snapshot**. Avoid schema changes during export.

Archives contain passwords and private keys. They are private and checksummed,
but **not encrypted**. Protect copies with your backup storage tooling and test
restores on a disposable server. Allow disk space for staging and compression.

### Remote destination

Choose **Server & credentials → Backup & restore** in the TUI, or:

```sh
sudo abr backup configure --host 192.0.2.10 --user backup --path /srv/backups/abr
# Install the displayed public key on the backup account.
sudo abr backup host-key
# Compare with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on that server.
sudo abr backup trust --fingerprint SHA256:CONFIRMED_FINGERPRINT
sudo abr backup test
sudo abr backup --all --transfer
```

Create the destination directory first, owned by the backup account with mode
0700. Key authentication generates a dedicated identity; `--key FILE` imports one.
Password authentication uses `--password-stdin`. Destination credentials are not
included in archives. Changed host keys are refused.

A saved destination is used even without `--transfer`; that flag requires a remote
destination. For a one-off destination use `--remote USER@HOST --remote-dir DIR`.
Apps are captured and transferred sequentially. Only a verified transfer removes
the local archive; failures retain it and stop the run. Existing remote archives
are never overwritten.

## Restore

On a **fresh server**, install Abr and establish working SSH key access.
Do not run setup first. Copy the archives onto the server, then:

```sh
sudo abr restore /var/backups/abr/full.tar.gz --dry-run
sudo abr restore /var/backups/abr/full.tar.gz --yes
```

You can restore several app archives together if their shared settings match and
no app appears twice. Full-server archives must be restored alone. Existing Abr
state is refused. Restore validates archives before provisioning, rebuilds
runtimes/dependencies and recreates users and services. Preview extracts into a
private temporary directory, so it also needs disk space.

Use `--admin-user USER` and `--ssh-port PORT` for the new server's SSH access.
Check restored apps before moving DNS; keep workers on the old server stopped
to avoid duplicate jobs. Failure may leave a partially configured target.

For a SQL-only import, back up and stop all writers first:

```sh
sudo abr disable api
sudo abr database import api /absolute/backup.sql --yes
sudo abr enable api
```

Imports can overwrite data and leave partial changes on failure. They do not
remove extra tables or restart services. Re-enable only after checking the data.

## Removal

`abr disable APP` stops services and routing while retaining data and ports.
`abr remove APP` removes managed services, the runtime user, config and reservations,
but keeps project files, uploads, the database and credentials.

`abr remove APP --purge --yes` permanently deletes the app's project and recorded
managed data. The TUI defaults to keeping data and requires confirmation for removal.
Shared server tools and separately exported backups are retained. Back up first.

## Templates and runtime settings

Config is `/etc/abr/config.toml`; state is `/var/lib/abr`; editable templates are
`/etc/abr/templates`. `abr config example` prints the generic config. Setup copies
missing templates and preserves edits. Apply app template changes with enable or
deploy; shared runtime settings apply during setup. Templates are trusted root
configuration: test edits on a disposable host.

Defaults target a modest shared host: MySQL's buffer pool is 256 MiB, Redis is
256 MiB with no eviction and AOF every second, and FPM permits five ondemand
children per app. Measure your workload before changing them.

Caddy provides direct HTTPS, compression, security headers and static-file caching.
Versioned assets can be cached for a year; stable asset names for 30 days. Change
asset URLs when content changes. Dynamic responses retain app cache policy.
Default templates block `.env` and `.git` paths.

To allow iframe embedding on selected paths:

```sh
sudo abr edit api --embed-path /banner.html --embed-origin '*'
sudo abr deploy api
```

Use `self` or a full origin such as `https://partner.example.com` to restrict sites.
Repeat flags for multiple values; use `/ads/*` for a path prefix. Clear both lists
with `--embed-path '' --embed-origin ''`. Application CSP policies still apply.
