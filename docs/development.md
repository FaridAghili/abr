# Development

Use the Go version in `go.mod`. Development supports macOS ARM64;
release binaries target Linux AMD64 with CGO disabled. Keep version 1.0.0
until a version change is requested.

Use temporary paths for local work:

```sh
task_dir=$(mktemp -d)
go run ./cmd/abr register --config "$task_dir/config.toml" \
  --state-dir "$task_dir/state" --config-only --name demo \
  --type laravel --domain demo.example.com
go run ./cmd/abr --config "$task_dir/config.toml" --state-dir "$task_dir/state" \
  --templates-dir ./templates deploy demo --dry-run
```

Before submitting changes:

```sh
git ls-files -z '*.go' | xargs -0 gofmt -w
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/abr-linux-amd64 ./cmd/abr
```

Use `go test -race ./...` for concurrency changes. Run the configuration benchmark
with `go test ./internal/config -run '^$' -bench BenchmarkValidateApps -benchmem`.
Tests must use temporary paths or disposable hosts, never production.
See [AGENTS.md](https://github.com/FaridAghili/abr/blob/main/AGENTS.md) for project rules.

Templates and the generic example config are embedded in the executable.
Rebuild after editing them. Installed templates remain editable under
`/etc/abr/templates`; setup preserves existing files.

## CI

CI runs on main pushes, version tags, pull requests and manual requests.
Two jobs run concurrently:

- Formatting, workflow validation, vet, race tests, module verification and a
  reachable-vulnerability scan.
- A CGO-free Linux AMD64 build, followed by real setup, deployment, backup,
  SSH transfer, restore and Caddy tests on a disposable Ubuntu runner.

Both jobs cache Go dependencies/build outputs and use the Go version from
`go.mod`. Superseded runs are canceled. Parallel jobs reduce the serial wait
before host testing; actual duration depends on runner and package availability.

The binary is uploaded after host tests pass. Version tags must match the binary.
Release publication requires **both jobs** to pass; the bundle includes templates,
example config and documentation. Main pushes and pull requests do not publish.
Only create/push a release tag when a release is explicitly requested.

`scripts/host-smoke.sh` changes an entire host. Run it only on disposable Ubuntu.
