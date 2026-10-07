# Standalone templates

Distributions include these runtime files. Setup copies missing files into
`/etc/sites/templates` and preserves edits. `--templates-dir` selects another
installed template directory. App template changes apply on the next
`enable`/`deploy`; shared setup configuration changes apply on the next `setup`.
Edit the source templates, which are trusted root configuration, and validate
changes on a disposable Ubuntu host.

App templates use Go text/template with validated values and escaped paths.
Generated files have ownership markers. Caddy/FPM configurations are validated
before reload, with restoration on ordinary configuration failures.
Shared SSH/MySQL/Redis/PHP/update templates are copied as native configuration.

Nuxt has one service regardless of SSR settings. Caddy handles direct HTTPS,
redirect aliases and additional serving domains. FPM uses an ondemand pool with
five children; queue timeout/shutdown grace are 60s/120s. Customize limits here.
Laravel reads `.env`; Nuxt reads it using Node's `--env-file-if-exists`.
Inertia's built bundle must honor `SSR_PORT`; Laravel receives `INERTIA_SSR_URL`.
Nightwatch receives its ingest endpoint. Octane finds shared RoadRunner through
PATH; remove app-local RoadRunner binaries to use the shared version.
