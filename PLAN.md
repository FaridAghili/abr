# sites — current implementation and scope

Build a small Go CLI for Ubuntu 26.04 LTS AMD64, developed on macOS Apple Silicon.
Stay on version 0.1.0 while stabilizing. Use native packages and systemd; keep
portable config/registry/template logic separate from Linux operations.

## Host model

- Caddy serves directly with automatic HTTPS. No Nginx or Cloudflare dependency.
- Shared PHP 8.5, Node 24/npm, Composer, MySQL, Redis and RoadRunner installations.
- Setup includes image utilities, UFW, Fail2ban, ACL and build tools by default.
- Runtime upgrades are deliberate owner actions. RoadRunner is one root-owned
  shared executable with a versioned directory and stable symlink.
- Each project has a dedicated automatically managed Ubuntu user and private home.
- Project clones live under /srv/apps; per-project runtime versions are unnecessary.
- Nuxt always runs a Node service. SSR is controlled by the project, with no
  SSR/static manager mode or static template.

## Commands

version, config validate, list, register, ports, doctor, setup, database,
enable, disable, remove, status, restart, logs, deploy APP..., deploy --all.
No arguments prints help. An interactive menu is not implemented.

Register validates a clone, creates its runtime user, assigns required ports,
and creates a dedicated MySQL database/account for Laravel unless --no-database
is selected. A --config-only path preserves portable registration for local work.
Database credentials stay in private state files and can be printed explicitly;
the owner copies them and other application secrets into the project's .env.
Existing unrelated users/databases must not be adopted or reset.

## Configuration, state, and templates

/etc/sites/config.toml: applications and allocation range.
/etc/sites/templates/: editable standalone templates copied from the archive.
/var/lib/sites/: ports, account ownership, database credentials, generated-file
manifests, managed environment, locks, private deployment logs/results.

Laravel web.driver selects fpm or octane. Queues, scheduler, Nightwatch and
Inertia SSR are independent optional components. Names, users, domains and
project directory trees are unique. Config uses strict maintained TOML parsing.
Aliases redirect permanently; extra domains serve the same backend. Wildcard
DNS challenge provisioning remains outside the implemented scope and errors
when enabling wildcard apps.

Reserve ports only for Octane HTTP/RPC, Nuxt HTTP, Inertia SSR, and Nightwatch.
FPM uses Unix sockets; queues/scheduler have no ports. Keep saved assignments
stable, detect duplicates/listeners, lock writers, write atomically, and refuse
corrupt state. Disabling retains ports; removal releases only after services stop.

Templates cover each service, scheduler timer, per-app FPM pool, and Caddy site.
Only generated files with recorded paths/ownership markers are modified.
Validate FPM/Caddy before reload. Restore previous generated files/services on
ordinary configuration failures and report any failed restoration. Changes are
not crash-atomic across multiple files or host operations; preserve recovery state.
FPM pools share one master; use ondemand for quieter apps.

## Standard deployments

Use only standard commands; do not run deploy.sh, custom shell files, or hooks.
Lock host operations, reject dirty worktrees, pull with --ff-only, then run all
project commands as its unprivileged managed user. Git credentials must be
available to that user for private repositories; --no-pull supports initial clones.

Laravel: composer install with locked production dependencies, check platform
requirements, generate missing APP_KEY once, clear configuration caches, and
create the public storage link if absent. For projects with package.json:
npm ci --include=dev and npm run build. Then Laravel migrate --force and optimize.
Enable configured services, verify expected sockets/listeners, reload Caddy,
and run the optional health-check URL. Record commit, timestamps, result and logs.
Deploy multiple apps sequentially and fail overall if any app fails.

Deployment is in place and deliberately has downtime. Disable managed routing
and services before changing code. A failed install/build/migration leaves the
app disabled. No automatic code/database rollback is provided. Preserve existing
app keys, secrets, uploaded files and queued jobs.

## Removal and operating limits

Stop all owned services, including active scheduler jobs. Remove owned generated
files and the owned Ubuntu account only after checking its identity and remaining
processes. Preserve project files, home directories, secrets, databases, database
accounts and credentials. Never recursively delete a project or kill unknown
processes. Draining FPM workers may require waiting and retrying removal.

SSH authentication and the administrator account remain owner-controlled.
Setup preserves reachable SSH ports before enabling UFW and preserves existing
rules. Do not attach Ubuntu Pro or add personal shell preferences automatically.
No automated backups, database imports/deletion, wildcard certificate setup,
interactive menu, or zero-downtime deployment is promised in this version.

## Verification

Run gofmt, go vet, go test, race tests and the CGO-free Linux AMD64 build locally.
Tests use temporary config/state and mocked host commands; no production access.
GitHub Actions packages executable/templates/example config and SHA256 artifacts
on pushes, pull requests and manual runs. A separate disposable Ubuntu job tests
actual setup, Laravel/FPM/MySQL, shared RoadRunner/Octane, SSR and SPA Nuxt,
direct local HTTPS, and lifecycle.
Verify that job succeeds before using the implementation on a real VPS.
