# Development rules

- Keep version 1.0.0 until asked to bump it. Use Go from go.mod; develop on macOS ARM64 and ship CGO-free Linux AMD64.
- Treat this as the initial release. Do not add backward-compatibility code, legacy migrations or upgrade notes. Publish releases only when explicitly asked.
- Keep portable logic separate from Ubuntu host operations. Keep the TUI thin and CLI scriptable; never report unexecuted work as success.
- Keep the TUI minimal: small menus, one input field per step, and a short description and example for every field. Make advanced settings optional and guide users to the next step after successful work.
- Clone and register apps by name under /srv/apps/NAME by default; do not ask for the full project path in the TUI.
- Use native packages, direct Caddy HTTPS, shared runtimes, dedicated app users and standard deploy commands. No Nginx, CDN integration or custom deploy scripts.
- Keep editable templates standalone and package them with the executable and generic example config. Do not include personal data or credentials.
- Preserve projects, secrets, uploads and databases on ordinary removal. Full removal (--purge) explicitly deletes this app's project and recorded managed data; require --yes in the CLI and confirmation in the TUI. Delete only recorded managed resources. Keep SQL/passwords out of logs and process arguments.
- Tests use temporary paths or disposable hosts; never contact production. Confirm destructive imports in the TUI and require --yes in the CLI.
- Before finishing: gofmt, go vet ./..., go test ./..., and CGO-free Linux AMD64 build. Test locking/port conflicts when touching the registry and real host behavior in CI when touching provisioning.
