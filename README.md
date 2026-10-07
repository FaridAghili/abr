![Abr logo](assets/abr-logo.png)

# Abr

**Abr (ابر)** means cloud in Persian. A Go CLI and interactive menu for a clean
**Ubuntu 26.04 LTS AMD64** VPS.
Laravel uses PHP-FPM or shared RoadRunner/Octane; Nuxt uses Node for SSR or SPA.
Caddy serves direct HTTPS. Users, databases, services and ports are managed per app.
Version **1.0.0**; development supports macOS ARM64.

## Start with a fresh VPS

1. Allow your SSH port, TCP 80/443 and UDP 443 in your provider firewall, and point
   your app's DNS at the VPS.

2. On your computer (macOS/Linux shell), replace the placeholders and install
   your SSH public key for your existing root/sudo account. If you need a key,
   first run `ssh-keygen -t ed25519`.

   ```sh
   VPS_HOST=YOUR_VPS_IP
   VPS_USER=YOUR_SSH_USER
   VPS_PORT=22
   cat ~/.ssh/id_ed25519.pub | ssh -p "$VPS_PORT" "$VPS_USER@$VPS_HOST" \
     'umask 077; mkdir -p ~/.ssh; cat >> ~/.ssh/authorized_keys'
   ssh -p "$VPS_PORT" -o PasswordAuthentication=no \
     -o KbdInteractiveAuthentication=no "$VPS_USER@$VPS_HOST"
   ```

   **Test key login in a second terminal and keep that session open during setup.**
   Setup requires working key access and disables password login.

3. On the VPS, update Ubuntu. If `/var/run/reboot-required` exists, run
   `sudo reboot`, then reconnect before continuing.

   ```sh
   sudo apt-get update
   sudo apt-get full-upgrade -y
   ```

4. Download **abr-linux-amd64** from the
   [v1.0.0 release](https://github.com/FaridAghili/abr/releases/tag/v1.0.0).
   Release assets are published only after the disposable Ubuntu host test passes.
   The release also includes a bundle with standalone editable templates and the
   generic example config, plus SHA256 checksums. Upload the binary using the
   variables from step 2.

   ```sh
   scp -P "$VPS_PORT" abr-linux-amd64 "$VPS_USER@$VPS_HOST:"
   ```

5. On the VPS, install the binary, then run setup:

   ```sh
   cd ~
   sudo install -m 755 abr-linux-amd64 /usr/local/bin/abr
   abr setup --dry-run --admin-user "$(id -un)"
   sudo abr setup --admin-user "$(id -un)"
   sudo abr
   ```

Setup asks for a VPS name, such as `my-vps`. It sets Ubuntu's persistent hostname,
updates its local `/etc/hosts` mapping, and preserves the name across cloud-init
boots. For scripts, pass `--hostname my-vps`. The GitHub key comment and suggested
GitHub title use this name; renaming the VPS preserves the existing key.

Only the binary is needed on the VPS; templates and the generic example config are
embedded. Setup installs PHP 8.5/extensions (including Imagick SVG support and
Excimer), Node 24, latest compatible npm, npm-check-updates, Caddy, Composer,
shared RoadRunner, MySQL 8.4, Redis and image optimization tools. It also installs
Zsh as root's default login shell, Oh My Zsh under `/root/.oh-my-zsh`, and enables
`git`, `zsh-autosuggestions` and `zsh-syntax-highlighting` (loaded last). This
configures root even when invoked through sudo; `--admin-user` only selects the
SSH administrator. Existing `.zshrc` settings are preserved, with an initial
backup at `/root/.zshrc.pre-abr`. The managed shell block uses the editable
`zshrc.tmpl` template. Reconnect after setup to use the new shell. Setup configures
key-only SSH, local databases, UFW, Fail2ban and
security updates. Root key login remains allowed. Use `--admin-user USER` when the
SSH account differs from the sudo user; `--ssh-port PORT` preserves an additional
port. `--no-firewall`, `--no-redis`, `--no-images` skip those features. Setup neither
attaches Ubuntu Pro nor reboots; failed setup can leave package changes in place.
Shared npm tools are installed under `/opt/abr/node-tools` by Ubuntu's `_apt`
account with package scripts disabled. Root publishes the completed tree and
links its commands into `/usr/local/bin`; installers retain no write access.
Setup removes staging caches and retires only a recorded previous installation
when none of its public tool links still use it. After installing Node tools,
setup runs `ncu -g` and installs any available global upgrades. Ordinary
`npm -g` and `ncu -g` use the shared installation by default.

For an existing VPS, choose **Server & credentials → Update server**, or run:

```sh
sudo abr update
sudo abr update --dry-run
```

Update refreshes apt indexes, runs `full-upgrade`, `autoremove` and `autoclean`,
self-updates Composer to its stable release, then runs `ncu -g` and installs all
suggested upgrades for the shared global tools, usually npm and SVGO. Globals
without an ncu suggestion retain their exact versions. Other global prefixes
and app dependencies are left alone. Package downloads and installs
still run as `_apt` with scripts disabled. Project dependency lockfiles and app
code are handled by Deploy. APT upgrades Zsh; update also runs Oh My Zsh's
unattended updater and fast-forward pulls for both shell plugins. Plugin
conflicts stop the update without resetting local edits. Run setup first.
Updates stop on failure; completed apt changes are not rolled back, and the
server is not rebooted.

## Deploy apps

Run `sudo abr` for the interactive menu. Forms show one field at a time, with a
short explanation and example. Use Enter to continue and Shift+Tab to revisit a
field. Advanced registration settings are optional. **Server & credentials**
contains VPS setup and updates, shared GitHub/Composer access and the TablePlus admin login; **Tools** contains checks
and bulk operations. **Set up VPS** asks for a server name (advanced settings
are optional), installs the tools, automatically creates/reuses and displays the
GitHub public key, asks for Composer credentials (or lets you skip), and ends
with the TablePlus MySQL login and password. Press Enter after copying the key
to continue the same setup workflow.

**Clone application** asks for the repository and app name, detects Laravel
or Nuxt, and continues directly to registration using that name. Detection uses
root `artisan` / `nuxt.config.*` files, with Composer and npm dependencies as a
fallback; unknown or ambiguous projects ask for the framework. Enter the domain
(**Prefer www** is the TUI default), then choose Laravel's web server, queue
workers and MySQL. Scheduler, other components and extra domains are optional
advanced settings. Nuxt registration skips MySQL and every Laravel service
(PHP-FPM, Octane, queue workers, scheduler, Nightwatch and Inertia SSR).
After registration, if `.env.example` exists, Abr copies it to `.env` if
absent, fills managed Laravel database values, and offers to open it in nano as the app
user. Save with Ctrl+O, Enter, and exit with Ctrl+X; deployment then starts
automatically using the cloned checkout. You can also skip the editor and deploy.
Without `.env.example`, environment preparation and editing are skipped and
deployment starts directly; any existing `.env` is left untouched.
The workflow stops on errors; preview mode does not advance into steps that need
newly created files. Config-only registration saves settings without deployment.
Destructive imports and full app deletion require confirmation.

For private repositories, use one GitHub account SSH key for the VPS:

```sh
sudo abr git setup
# Add the printed public key to GitHub account Settings → SSH and GPG keys.
sudo abr clone git@github.com:OWNER/PROJECT.git api
sudo abr register --name api --type laravel \
  --domain api.example.com --web-driver octane \
  --queue-workers 2 --scheduler
sudo abr env api
sudoedit /srv/apps/api/.env
sudo abr deploy api --no-pull
sudo abr status api
```

`abr env APP` preserves existing `.env` settings and updates managed MySQL values
without printing secrets. For a new Laravel `.env`, it also sets production mode,
disables debug and uses the registered HTTPS domain as `APP_URL`. Existing keys
and app settings are kept. Projects without `.env.example` skip this command
without creating or changing `.env`. Nuxt deployments use only npm, the Node
service and Caddy; they do not create databases or run Laravel services.
Set your app secrets before deploying.
Managed MySQL uses `DB_HOST=127.0.0.1` and port 3306. `DB_DATABASE` is the app
name with hyphens replaced by underscores. `DB_USERNAME` uses the same name up
to 32 characters; longer usernames are shortened with a hash suffix. Abr adds no
prefix to database names or usernames.
Later, `sudo abr deploy api` fetches and fast-forwards to the checked upstream commit. Apps share the GitHub
key's permissions; private Composer/npm dependencies may need separate credentials.
Use `git setup --key /absolute/private-key` to import an existing unencrypted key.
Imported keys must be regular files without symlinks and no larger than 64 KiB.

Save a private Composer account once for all managed apps and future deployments:

```sh
sudo abr composer auth --host nova.laravel.com
# Enter your Nova account email and license key at the hidden token prompt.
sudo abr deploy api --no-pull
```

The interactive menu offers **Server & credentials → Composer credentials**. Nova uses
your account email as the HTTP Basic username and your license key as its password
([Nova installation documentation](https://nova.laravel.com/docs/v5/installation)).
For scripts, use `--username USER --password-stdin` and pipe the token from a
secret manager or private file; no token flag is accepted. Rerun the command to
replace a repository's credentials; other saved repositories are retained.
Abr stores the shared `auth.json` under `/var/lib/abr/composer`, owned by root.
During Composer commands, app users get read access through ACLs and a shared
`COMPOSER_HOME`; each app has its own writable cache under its private home.
All managed apps share these accounts' package access. Removal revokes that app's
access and preserves the saved credentials. Project-local `auth.json` can override
shared credentials; keep it out of source control. Without saved shared credentials,
Composer uses the app user's own configuration.

Before stopping an app, deployment checks Git access and fast-forward eligibility,
Laravel `.env` and managed database access, incoming manifests and lock files,
PHP/Composer platform requirements, and npm lock consistency/runtime requirements.
Composer and npm checks run without scripts/plugins in a private temporary directory;
Composer downloads packages into the app’s cache before downtime, checking private
repository access when an archive is needed. Project scripts, builds and migrations
can still fail during deployment.

Laravel deployment then runs **npm ci → npm run build → Composer install → migrations →
optimization → services**. It generates a missing APP_KEY and storage link. FPM is
the default driver. Optional flags: `--scheduler`, `--queue-workers N`, `--nightwatch`,
`--inertia-ssr`, `--octane-workers N`, `--health-check URL`; `--no-database` keeps DB
management external. Install the corresponding Laravel packages in your project;
Inertia's server bundle must honor `SSR_PORT`. Octane uses the shared RoadRunner;
remove any app-local `rr` binary. Deployments have downtime and no automatic rollback.

Use **App → More actions → Edit settings** to change domains, workers, components
or the deployment health check. Forms start with current values. Saving settings
reserves any new ports and keeps existing services running; deploy to apply them.
Disabling the database retains its data and credentials.

```sh
sudo abr edit api --queue-workers 3 --scheduler=false
sudo abr edit api --domain api.example.com --canonical-host non-www
sudo abr edit api --health-check https://api.example.com/up
sudo abr deploy api
```

Only supplied flags change settings. `--alias` and `--serving-domain` replace their
lists (repeat the flag for multiple entries; pass an empty value to clear).
`--health-check ''` clears the check. `--config-only` saves portable settings locally.

The `sudo abr` entry point is needed for users, permissions, databases and system
services. Composer, npm, Artisan, Git updates and asset compression run as the
dedicated app user with a clean environment and `no_new_privs`; their scripts
cannot elevate through setuid programs. Do not manually run `sudo composer
install`, `sudo npm ci`, or `sudo php artisan` inside a project. Package scripts
still have the app user's access to project files, secrets and the shared Git key;
review dependencies and commit lockfiles. This is not a sandbox for hostile apps.
Initial clones use `_apt` and a temporary SSH identity; root publishes the
checkout before registration assigns it to the dedicated app user.

```sh
sudo abr clone git@github.com:OWNER/WEB.git web
sudo abr register --name web --type nuxt --domain example.com
# Prepare .env if needed; commit package-lock.json (and composer.lock for Laravel).
sudo abr deploy web --no-pull
sudo abr restart api web
sudo abr logs api queue@1
sudo abr ports
sudo abr disable api
sudo abr enable api
```

Cloning and registration use `/srv/apps/NAME` automatically. Enter the same app
name for both; the TUI does not ask for a project path.

`--alias DOMAIN` redirects to the main domain; `--serving-domain DOMAIN` serves the
same app. Both are repeatable. Independent subdomain apps are supported; wildcards
are not. Edit `/etc/abr/config.toml` to change settings, then run `abr ports
--allocate` and `abr enable APP`. `abr remove APP` removes managed services/user
and reservations while preserving the project, secrets, home and database.

To permanently delete an app and its data, choose **Remove application → Fully
delete app and data** in the TUI, or run:

```sh
sudo abr remove api --purge --dry-run
sudo abr remove api --purge --yes
```

Full removal deletes the registered project directory (including uploads and
`.env`), the app's home and private Ubuntu account/group, recorded managed MySQL
database and recorded local MySQL accounts, credentials, deployment history, service configs,
configuration entry and port reservations. Shared runtimes, shared Composer/Git
credentials, self-managed databases and separately exported backups are kept.
Back up anything you need first; full removal cannot be undone.

### Canonical host per app

The registration menu offers **As entered**, **Prefer www**, and **Prefer non-www**.
The CLI supports the same choice for Laravel and Nuxt:

```sh
sudo abr register --name website --type laravel \
  --domain example.com --canonical-host www
```

`www` serves `www.example.com` and redirects `example.com` to it. `non-www` serves
`example.com` and redirects `www.example.com` to it. Both accept either spelling
in `--domain`. The default, `as-entered`, serves exactly the entered hostname and
adds no www alias. A separate app at `api.example.com` keeps its own hostname;
choices apply only to the selected app's exact www/non-www pair.

The resulting canonical hostname is saved as `domain` and the other hostname as
an `aliases` entry. Other redirect aliases and additional serving domains keep
their existing roles. A conflicting `--serving-domain` for the selected pair is
rejected. Point both hostnames' DNS at the VPS; set Laravel's `APP_URL` to the
canonical HTTPS URL. Redirects use **308**, preserving method, path and query.

For an existing app, edit its `domain` and `aliases` in `/etc/abr/config.toml`, then
run `sudo abr config validate` and `sudo abr enable APP`.

## TablePlus admin access

Setup creates a password-protected `root` account for loopback TCP connections,
with access to all databases and permission to administer accounts. Apps use
their own database users. Ubuntu's `root@localhost` socket login remains available
to Abr and `sudo mysql`; MySQL listens on `127.0.0.1` only.

Open **Server & credentials → MySQL admin · TablePlus**, or run:

```sh
sudo abr database --admin --show
```

In TablePlus, create a **MySQL** connection and enable **SSH**:

| Field | Value |
| --- | --- |
| Database host | `127.0.0.1` |
| Database port | `3306` |
| Database user | `root` |
| Database password | The password shown by the command |
| Database | Leave blank to browse all databases |
| SSH host | Your VPS IP address or SSH hostname |
| SSH port | Your existing SSH port, usually `22` |
| SSH user and key | Your existing administrator SSH login |

The generated password is saved in root-only `/var/lib/abr/mysql-admin.json`.
Repeated setup reuses it; app removal, including full removal, retains this
server account. Passwords are displayed only when explicitly requested. Setup
enables MySQL `skip_name_resolve` to distinguish this TCP account from socket
root; existing hostname-based grants or an unrecorded loopback root account
require manual review before setup changes accounts.

## Database backups and imports

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

## Templates and runtime settings

Runtime templates stay editable under `/etc/abr/templates`; setup copies missing
files from the binary and preserves your template edits when rerun. The
[source templates](templates/) define the initial defaults.
`--templates-dir` selects another installed template directory. Shared settings
apply on `abr setup`; app templates on `abr enable APP` or `abr deploy APP`.
Templates are trusted root configuration; validate edits on a disposable Ubuntu
host. App templates use Go text/template with validated values and escaped paths.
Host configuration, state, templates and project parents must be root-owned and
not writable by other users. Config/template/state files must also have trusted
owners and permissions; symlinks are rejected. App users own only their own
project and home. Systemd app services prevent privilege escalation, have no
capabilities, protect system directories and home directories, and use private
temporary directories. Apps can still write their projects and upload storage.
The shared FPM service also enforces `no_new_privs` for its pool workers; applying
that drop-in during setup restarts FPM.
Generated files carry ownership markers. Caddy/FPM configurations are validated
before reload and restored on ordinary configuration failures. Shared
SSH/MySQL/Redis/PHP/update templates are copied as native configuration.

Nuxt uses one service regardless of SSR settings and loads `.env` with Node's
`--env-file-if-exists`; Laravel reads `.env` itself. Inertia's bundle must honor
`SSR_PORT`, and Laravel receives `INERTIA_SSR_URL`. Nightwatch receives its ingest
endpoint. Octane finds shared RoadRunner through PATH. Queue timeout/shutdown
grace are 60s/120s; customize these and other limits in the templates.

Defaults suit a modest shared host: MySQL 256 MiB buffer pool/100 connections,
Redis 256 MiB with **no eviction** and AOF every second, FPM five ondemand children
per app/recycling every 500 requests, shared OPcache 128 MiB. Measure RAM/workload
and adjust these limits; Redis rejects writes at its limit and crashes can lose
about one second of writes. PHP error display/version headers are disabled.

Caddy redirects HTTP to HTTPS for every main domain, additional serving domain,
and redirect alias using method-preserving 308 responses. HTTPS responses,
including aliases, static files and errors, set six-month HSTS
(`Strict-Transport-Security: max-age=15768000`). Each managed host gets its own
policy; it does not force unrelated subdomains onto HTTPS or opt into preload.
The templates set `X-Frame-Options: SAMEORIGIN` and
`X-Content-Type-Options: nosniff`, following the
[Laravel deployment guide](https://laravel.com/docs/13.x/deployment).
They also set `Referrer-Policy: strict-origin-when-cross-origin` on HTTPS responses.
These headers are applied when responses are written, overriding upstream values.

Caddy strips Server/Via/X-Powered-By headers, compresses dynamic responses with
[zstd/gzip](https://caddyserver.com/docs/caddyfile/directives/encode), and serves
[precompressed Brotli](https://caddyserver.com/docs/caddyfile/directives/file_server)
for built assets generated during deploy. Versioned Vite/Nuxt assets get immutable
browser caching; HTML/API/SSR responses keep the application's cache policy.
Missing files in `/build/assets/` (Laravel) and `/_nuxt/` (Nuxt) return a direct
Caddy 404 without calling application workers. Only successful versioned asset
responses receive the immutable cache policy.
The packaged welcome page is replaced with a generic 404. Identifying text in
application bodies must be removed in the application itself.
Default site templates also return 404 for `.env`, `.env.*`, and `.git` paths.

Managed database names use the app name with hyphens replaced by underscores and
no added prefix: `example-api` gets `example_api`. Long names are preserved;
database names are independent of the dedicated runtime/MySQL account name.
New managed MySQL accounts receive grants scoped to their exact database,
including when MySQL's `partial_revokes` setting is enabled. Repeated database
commands verify recorded accounts and preserve passwords, grants and contents.

Artisan, Composer and npm command output stays private because project scripts
and exception messages can contain SQL bindings and database passwords.
Deployment history identifies failed commands. Composer download failures suggest
`abr composer auth` for private-package credentials; inspect application logs privately
when investigating failures. Secret-file reads reject symlinks/special files and
are limited to 1 MiB. Deployment logs and backups have no automatic retention;
monitor disk space and archive them deliberately.

## Development

Source templates stay in `templates/`; Go embeds them and `config.example.toml`
when building the executable. Rebuild to bundle changes to these defaults.

From the repository root on macOS/Linux, use temporary paths and `--config-only`:

```sh
task_dir=$(mktemp -d)
go run ./cmd/abr register --config "$task_dir/config.toml" \
  --state-dir "$task_dir/state" --config-only --name demo \
  --type laravel --domain demo.example.com
go run ./cmd/abr list --config "$task_dir/config.toml"
go run ./cmd/abr --config "$task_dir/config.toml" --state-dir "$task_dir/state" \
  --templates-dir ./templates deploy demo --dry-run
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/abr-linux-amd64 ./cmd/abr
```

[config.example.toml](config.example.toml) documents the configuration;
`abr config example` prints the embedded copy without changing live settings.
`abr help` lists commands; `abr doctor` checks portable config/registry/port availability.
CI uses one Ubuntu runner to check formatting/vet/race tests, scan reachable Go
dependency vulnerabilities, build the standalone Linux AMD64 binary,
and run actual setup/deployment/backup/restore tests from an isolated binary.
It runs on main pushes, version tag pushes, pull requests, and manual requests,
cancels superseded runs, and caches Go dependencies and builds. The binary is
uploaded directly only after the host test passes. Version tags must match the
binary's version. Tag runs also package the executable, standalone templates and
generic example config, then publish a GitHub release with SHA256 checksums after
all checks pass. Main pushes and pull requests do not publish releases.
`scripts/host-smoke.sh` changes an entire host: never run it on production.
