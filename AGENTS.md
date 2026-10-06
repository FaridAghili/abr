# Development rules

- Keep implementation small: native packages, Caddy with direct HTTPS, shared runtimes/RoadRunner, managed per-app users, and standard deployment commands.
- Do not use Nginx, Cloudflare, project deployment scripts, or an interactive menu.
- Preserve projects, secrets, uploads, and databases on removal. Delete only accounts and generated files whose ownership this tool recorded.
- Keep the CLI version at 0.1.0 during stabilization; bump it only when requested.
- Use the Go version in go.mod. Develop on macOS ARM64; ship a CGO-free Linux AMD64 binary.
- Keep portable configuration and registry logic independent of Linux service/package operations.
- Use temporary config/state paths for development and tests. Never contact production from tests or CI.
- Keep templates as editable files under templates/ and include them in distributions.
- Reject unknown configuration, corrupt state, and unsupported commands; never claim an operation succeeded unless it ran.
- Before finishing, run gofmt, go vet ./..., go test ./..., and the Linux AMD64 cross-build. Test locking, stable allocations, corruption, and listener conflicts when changing registry behavior.
