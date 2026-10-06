# Simple VPS Manager — Revised Plan

Build a small Go CLI named `sites`. Go suits configuration validation, port tracking, command execution, and service management while producing a single executable for the VPS.

Keep configuration and templates separate from the source code. Use native packages, systemd, Caddy, PHP-FPM, and RoadRunner.

## Files

Use a straightforward layout:

```text
/etc/sites/config.toml          Server settings and registered applications
/etc/sites/templates/          Editable service, FPM, and Caddy templates
/var/lib/sites/ports.json       Saved application port assignments

PROJECT/deploy.sh              Application-specific deployment commands
```

PHP and Node remain shared system installations, upgraded manually by the owner. No per-project runtime versions.

## Application configuration

Each registered application has:

- Name, directory, runtime user, and application type.
- Main domain, redirect aliases, additional serving domains, and optional wildcards.
- Laravel web driver: `fpm` or `octane`.
- Octane and queue worker counts.
- Scheduler, Nightwatch, and Inertia SSR settings.
- Nuxt mode: `ssr` or `static`.
- Deployment file path.
- Optional health-check URL.

Initially, Mango, Medfolio, and Menugar use Octane. The other Laravel applications use PHP-FPM. This choice remains configurable for every application.

Application secrets stay in existing `.env` files.

## Commands

```text
sites register
sites list
sites enable APP
sites disable APP
sites remove APP
sites status [APP]
sites restart APP [SERVICE]
sites logs APP [SERVICE]
sites deploy APP...
sites deploy --all
sites ports
sites doctor
sites setup
```

Running `sites` without arguments opens a simple menu. The menu calls the same commands available for direct use.

Registration asks for essential settings, assigns required ports, and saves the application configuration. Existing applications can import their current ports.

## Port tracking

Maintain a small persistent registry recording the application, endpoint purpose, and assigned port.

Allocate ports only for enabled components:

| Component           | Endpoint                 |
| ------------------- | ------------------------ |
| Octane              | HTTP and RoadRunner RPC  |
| Nuxt SSR            | HTTP                     |
| Inertia SSR         | SSR server               |
| Nightwatch          | Agent ingest             |
| PHP-FPM             | Unix socket; no TCP port |
| Queue and scheduler | No ports                 |

Before allocating, check both saved reservations and actual listeners. Keep allocations stable across restarts and redeployments.

Protect registry changes with a lock and save them atomically. Refuse corrupt state rather than silently replacing it.

Disabling a component retains its reservations. Removal releases them only after its services have stopped. Verify startup because another process can occupy a port after the initial check.

Internal TCP services bind to loopback where supported. RoadRunner RPC is distinct from gRPC.

## Standalone templates

Provide editable templates for:

- Octane service.
- Queue worker service.
- Nightwatch agent service.
- Inertia SSR service.
- Nuxt SSR service.
- Scheduler service and timer.
- PHP-FPM pool.
- Caddy site.

The manager fills placeholders, installs its generated configuration, and handles enable, disable, remove, status, and restart.

Use an identifiable naming prefix for generated files. Manage only files owned by this tool.

For quieter applications, start with FPM `ondemand` pools and configurable child limits. FPM pools share one master service; adding or removing pools requires a shared reload.

Validate generated configuration before reloading FPM or Caddy.

## Project deployment files

Each project owns a normal shell deployment file. Go executes it rather than trying to interpret individual commands.

Example:

```sh
composer install --no-dev --optimize-autoloader
npm ci
npm run build
php artisan migrate --force
php artisan optimize
```

Execute the file using Bash with error checking and pipeline failure detection, in the project directory, as the configured deployment user.

This supports ordinary command-per-line files while also allowing variables, conditionals, and multiline commands when needed. Stop immediately when a command fails.

Deployment files are trusted owner-controlled code. They never run as root.

The manager provides the application’s managed environment, including Nightwatch and SSR endpoints, consistently to deployment commands and services.

## Deployment workflow

1. Lock the application against concurrent deployment or configuration changes.
2. Check the working directory and reject uncommitted Git changes.
3. Run `git pull --ff-only`.
4. Execute the project’s deployment file.
5. Restart its configured long-running services.
6. Run the optional health check.
7. Show the outcome and retain deployment output.

Deploy applications sequentially to limit build memory usage. For multiple selections, report each result and return failure if any deployment fails.

Allow optional `pre-deploy.sh` and `post-deploy.sh` hooks for maintenance mode or application-specific work.

A cleanup hook may restore availability after failure, but the original failure must still be reported.

Queue restarts should let legitimate jobs finish. `queue:restart` means worker recycling; avoid clearing queued jobs. Coordinate deployment hooks and manager restarts so workers are not restarted twice unnecessarily.

Deploy directly in existing directories. Automatic code and database rollback are outside this version.

## Switching web drivers

Changing `web.driver` between `fpm` and `octane`, then enabling the app again, reconciles its configuration:

1. Prepare and validate the new backend.
2. Start it and check that it responds.
3. Update and reload Caddy.
4. Check the website.
5. Drain and disable the previous backend.

Keep queues, scheduler, Nightwatch, and SSR independent of the web driver.

## Small additions worth including

- **Preview:** `--dry-run` shows intended changes without applying them.
- **Doctor:** checks required executables, permissions, configuration, and port conflicts.
- **Useful status:** shows configured components, service failures, and assigned ports.
- **Deployment history:** records timestamp, Git commit, duration, and result.
- **Clear errors:** identify the application and failed command or service.
- **Safe removal:** preserve project files, uploads, `.env`, and databases.
- **Runtime checks:** detect missing PHP extensions or inconsistent CLI/FPM installations after a manual upgrade.

Keep deployment logs private because application commands can print sensitive information.

## Server setup and Caddy

Keep initial setup limited to packages, runtime users, required directories, and firewall configuration. Preserve SSH access and existing unrelated rules.

Caddy handles domains, redirects, static resources, and routing to the selected backend.

Support TCP 80/443, UDP 443 for HTTP/3, dynamic gzip/zstd, and precompressed Brotli assets. Apply long immutable caching only to verified versioned assets.

Wildcard certificate setup remains a separate initial task, with explicit certificate renewal arrangements.

Initial cloning, database imports, uploads, and `.env` preparation remain manual. Backups remain deferred.

## Implementation order

1. Configuration, registration, port registry, and doctor.
2. Templates and service lifecycle commands.
3. Deployment files, selection menu, logging, and health checks.
4. Basic server setup.

Verify the Go logic locally and test host operations on disposable Ubuntu before using the production VPS.
