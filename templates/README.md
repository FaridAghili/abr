# Standalone templates

These files are loaded at runtime with Go text/template and shipped in every
archive. Setup copies missing files into /etc/sites/templates; existing edits
remain. Use --templates-dir for another source directory. Source edits take
effect on the next enable/deploy. Missing templates/placeholders return errors.

The renderer supplies validated application values, escaped paths, required
ports, and a private managed environment file. Generated files have a sites-
prefix and ownership marker. Edit source templates instead of generated files.
Caddy and FPM configurations are validated before reload; ordinary failures
restore previous generated files. User-edited templates remain trusted root
configuration and must be tested on a disposable Ubuntu machine.

Nuxt has one service template, independent of ssr: true/false. Caddy serves
directly with automatic HTTPS, redirects aliases, and proxies Nuxt/Octane or
uses a per-app FPM socket. No Nginx, Cloudflare or static-Nuxt branch is used.
FPM pools use ondemand with five children by default; customize the pool template
for other limits. Queue timeout is 60s and shutdown grace is 120s.

Laravel reads its own .env. Nuxt reads .env through Node's --env-file-if-exists.
Managed environment values reach services, FPM, and deployment commands.
Inertia's bundle must explicitly honor SSR_PORT; INERTIA_SSR_URL configures the
Laravel client. Nightwatch uses the reserved ingest endpoint. Octane finds the
shared /usr/local/bin/rr through PATH, though an app-local rr would take precedence.
