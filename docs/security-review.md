# Security and server-readiness review

Reviewed on 2026-10-07 for the initial development version, 0.1.0.
No production server was contacted or changed.

## Changes

| Finding | Resolution |
| --- | --- |
| Global npm installation processed third-party packages as root, even with scripts disabled. | Install once into an isolated prefix as Ubuntu's `_apt` account, with scripts disabled and `no_new_privs`. Root changes ownership, rejects escaping/dangling symlinks and special files, normalizes permissions, and publishes command links. Failed staging trees and package caches are removed; only recorded unused previous trees are retired. |
| Privileged execution inherited PATH and arbitrary caller environment variables. | Resolve commands using a fixed host PATH before execution and replace the environment. Explicit command overrides remain internal to the host package. |
| Switching users alone allowed later setuid privilege escalation. | All app commands use `setpriv --no-new-privs` after `runuser`. Reject root runtime users and zero or invalid UID/GID accounts before changing their files. |
| Initial Git clone processed repository contents as root. | Clone as `_apt` with no new privileges, disabled hooks/templates and a temporary private SSH identity. Root publishes the checkout; temporary keys/staging are removed on success and failure. |
| Services had no additional privilege or filesystem protections. | All app systemd units have `NoNewPrivileges`, an empty capability set, setuid restrictions, protected system/home directories and kernel settings, and private temporary directories. A shared FPM drop-in also prevents its pool workers from elevating. App projects/storage remain writable. |
| Root-owned directories could contain app-writable configuration files. | Verify config, lock, template and managed-state file ownership/type/permissions; reject symlinks. |
| Root read app-controlled `.env` files without a bound or final-component symlink protection. | Read through a no-follow/nonblocking descriptor, require a regular file, and bound allocation to 1 MiB. |
| npm scripts could disclose project secrets into deployment logs. | Hide npm output, alongside existing Composer/Artisan privacy. Retain command exit status and private deployment result records. |
| stdout/stderr could concurrently corrupt an unsynchronized log writer. | Serialize writes to the shared command log sink. |
| Asset compression spawned one user-switching process for every file. | Compress in batches of up to 64 files/32 KiB of path arguments, with the same app privileges and fail-fast behavior. |
| Private Composer package credentials were unavailable to app users' clean deployment environments. | Save shared HTTP Basic credentials from a hidden prompt or bounded stdin, never command arguments. Root owns the shared Composer home; named app ACLs allow reading credentials while per-user caches remain writable. Revoke ACLs before deleting an app user and retain shared secrets. |

Composer and npm scripts execute with the installing user's access, so dropping
privileges is required even when the package manager itself is trusted.
See [Composer's security guidance](https://getcomposer.org/doc/faqs/how-to-install-untrusted-packages-safely.md)
and [npm's script/install options](https://docs.npmjs.com/cli/install/).
Service restrictions follow the
[systemd execution documentation](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html).

## Validation

Regression tests exercise hostile PATH/environment values, root UID/GID aliases,
setuid prevention arguments, state and secret-file symlinks, FIFOs/oversized
secret files, concurrent logging, shared-tool publication/failure cleanup and
bounded asset compression. Package installation and privileged host operations
are simulated in local tests; temporary paths isolate all filesystem changes.

The disposable Ubuntu CI fixture now runs real Composer and npm project scripts
that fail unless their effective UID is nonzero and `/proc/self/status` reports
`NoNewPrivs: 1`. It checks published tool ownership/write denial and actual service
settings/process status, alongside existing deployment, database, Caddy, storage
and removal checks. It also exercises unprivileged initial clone against an
offline bare-repository fixture. It generates npm fixtures without sudo.
The Composer scripts also check shared authentication and per-app cache paths
across two apps and a repeated deployment. The fixture checks write denial,
unrelated-user read denial, token-free deployment history and ACL revocation.

Local verification uses Go 1.27.1 on macOS ARM64: gofmt, vet, all tests, race tests,
module verification, reachable vulnerability scanning and a CGO-free Linux AMD64
build. The actual Ubuntu/systemd test must pass in CI before installing on a server;
it cannot be executed on this macOS workspace.

## Operational limits

- Dependency and build scripts can access the app's `.env`, home and database.
  `no_new_privs` prevents setuid elevation; it does not isolate hostile code from
  the app's data or prevent kernel/runtime exploits. Use reviewed dependencies
  and committed lockfiles. No blanket install-script bypass is added.
- Applications share one GitHub identity and can read it after Git access is
  granted. Use a GitHub account restricted to the repositories this server needs.
  Saved Composer accounts are also shared: apps can read their package tokens
  after Composer access is granted, and project-local authentication can override
  the shared file. App removal revokes access before UID reuse.
  Redis is loopback-only but shared without per-app authentication. These choices
  do not provide isolation between mutually untrusted tenants.
- Native package installation and system configuration still require root.
  Initial Git clone runs as `_apt` with inherited Git configuration, hooks and
  templates disabled; application Git updates run as the app user. Composer
  verification and PHP image/extension checks run unprivileged.
- Shared npm/Composer/RoadRunner setup resolves current upstream versions.
  Application lockfiles make app dependencies repeatable, but setup is not an
  offline or fully pinned toolchain build. Native APT verification, Composer's
  official SHA256 and RoadRunner's release digest remain in use.
- Deployments update files in place and have downtime. Failed builds stay failed;
  there is no automatic code/database rollback. Health-check failure after
  enabling services is reported as a failed deployment, but does not roll back.
- Setup preserves template customizations when rerun and restarts the shared
  FPM service to apply its privilege restrictions.
- Keep and test off-server backups. Logs/backups have no automatic deletion
  policy. Shared database/process memory settings require workload measurement;
  no throughput or production load-test claim is made.

Application removal continues to preserve projects, homes, credentials, uploads
and databases. Removal only deletes recorded managed resources.
