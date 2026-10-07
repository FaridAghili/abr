# Changelog

## 1.0.0

- Manage Ubuntu 26.04 AMD64 hosts with native packages, Caddy HTTPS, shared
  runtimes, dedicated app users, and Laravel/Nuxt deployments.
- Provide scriptable CLI operations and an interactive menu, editable embedded
  templates, canonical host redirects, and private database backups/imports.
- Restrict MySQL grants to literal database names and repair wildcard grants on
  existing recorded accounts without changing passwords or database contents.
- Validate privileged directory ownership and permissions, reject managed-home
  symlinks, and run public subtree ACL changes under each application's user.
  Bound SSH-key reads before allocation and reject symlinks and special files.
- Stream long command output, bound captured results, and keep Artisan/Composer
  exception output out of deployment logs. Reject credentials in health URLs.
- Block `.env`, `.env.*`, and `.git` paths in the default Caddy site template.
- Pin GitHub Actions to commits, run Linux/macOS ARM64 race tests and vulnerability
  checks, and test the packaged binary on a disposable Ubuntu host. Publish only
  after required jobs pass and the tag matches the executable version.

### Upgrading

Run `sudo abr database APP` for each managed database to replace legacy wildcard
grants. Deployment also reconciles grants. Existing installed templates are
preserved: merge the private-file `handle` block from
`templates/caddy-site.caddy.tmpl` into `/etc/abr/templates/caddy-site.caddy.tmpl`,
then enable or deploy the application. Host configuration/state/template
directories and project parents must be root-owned and not writable by other
users. Projects, homes, credentials, uploads, and databases remain preserved on
application removal.
