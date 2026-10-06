# Standalone template drafts

These editable Go `text/template` drafts are shipped with the distribution.
Milestone one does not load, render, install, or validate host templates.
They are starting points for milestone two, not production-ready generated files.

Future generated files must use the `sites-` prefix. Paths, executable locations,
systemd quoting, runtime versions, permissions, managed environment, graceful
shutdown, Caddy routing, and shared FPM reloads must be validated on disposable
Ubuntu before enabling services. Never interpolate raw TOML into host files.

The draft placeholders describe future rendering inputs; they are not additional
TOML settings. Each draft expects Name, User, Directory, and component-specific
values (PHPBinary, NodeBinary, port fields, FPMSocket, etc.).
ManagedEnvironmentFile is a future per-app environment file, not the project's
.env file; Laravel loads that itself. Inertia's built SSR bundle must read the
managed port and bind to loopback. RoadRunner RPC must also bind to loopback in
its application configuration. The future renderer must supply escaped values.

The Caddy draft covers only the main domain/backend. Alias redirects, serving
and wildcard domains, static Nuxt output, versioned asset caching, and certificate
arrangements are deferred to the lifecycle milestone.
