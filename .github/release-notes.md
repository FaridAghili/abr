Abr 1.0.0 is the initial release for fresh Ubuntu 26.04 LTS AMD64 servers.

- Scriptable Go CLI and guided interactive menu for Laravel and Nuxt apps.
- Native PHP 8.5, shared RoadRunner/Octane, Node 24, Caddy HTTPS, MySQL and Redis.
- Dedicated app users, managed services and ports, deployment preflight checks,
  database backup/import, and removal that preserves app data unless purged.
- VPS setup with key-only SSH, UFW, Fail2ban and automatic security updates.
- Continuous VPS setup through GitHub keys, optional Composer credentials and
  the TablePlus MySQL login.
- Clone-to-deploy workflow with framework detection, optional advanced settings,
  automatic `.env` database values and an app-user editor.
- Server updates for apt packages, Composer and shared global npm tools.
- Online app backups with saved source, managed SQL, secrets and uploads,
  sequential SSH transfer and SHA256 verification.
- Guided SSH host fingerprint confirmation and clear backup destination errors.
- Full-server and per-app archive restore onto a fresh Ubuntu server.

Download `abr-linux-amd64` for standalone installation. The
`abr_1.0.0_linux_amd64.tar.gz` bundle contains the same executable as `abr`, editable
templates, the generic example config and the README. `SHA256SUMS` covers both
downloads. The executable embeds its default templates and example config.

Follow the README's fresh-VPS setup instructions. Release publishing requires
formatting, vet, race tests, dependency verification, a reachable-vulnerability
scan, a CGO-free Linux AMD64 build and disposable Ubuntu host tests to pass.
