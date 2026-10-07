# Changelog

## 0.1.0 — initial development version

- Redirect every managed HTTP domain to HTTPS and set six-month HSTS plus
  Laravel's recommended frame and content-type headers on HTTPS responses.
- Set an explicit referrer policy and return direct 404s for missing build
  assets; restrict immutable caching to successful versioned asset responses.
- Install shared npm tools without root privileges, freeze the published files,
  and clean staging caches and recorded unused previous installations.
- Clone repositories without root privileges using a temporary SSH identity;
  publish only completed checkouts and clean temporary identities on failure.
- Resolve privileged commands with a fixed PATH and clean environment. Prevent
  app commands and systemd services from gaining privileges through setuid tools.
  Reject zero UID/GID accounts and untrusted config/template/state files.
- Bound project secret-file reads, keep npm script output private, synchronize
  concurrent command logging, and batch Brotli asset compression.
- Check Composer/npm script privileges and service protections in disposable
  Ubuntu CI; remove root npm commands from fixture generation.
- Manage Ubuntu 26.04 AMD64 hosts with native packages, Caddy HTTPS, shared
  runtimes, dedicated app users, and Laravel/Nuxt deployments.
- Provide scriptable CLI operations and an interactive menu, editable embedded
  templates, canonical host redirects, and private database backups/imports.
- Provision MySQL accounts with grants scoped to literal database names; preserve
  credentials and database contents on repeated operations.
- Validate privileged directory ownership and permissions, reject managed-home
  symlinks, and run public subtree ACL changes under each application's user.
  Bound SSH-key reads before allocation and reject symlinks and special files.
- Stream long command output, bound captured results, and keep Artisan/Composer
  exception output out of deployment logs. Reject credentials in health URLs.
- Block `.env`, `.env.*`, and `.git` paths in the default Caddy site template.
- Pin GitHub Actions to commits, run Linux/macOS ARM64 race tests and vulnerability
  checks, and test the packaged binary on a disposable Ubuntu host. Provide
  development artifacts; version 1 release preparation is deferred until requested.
