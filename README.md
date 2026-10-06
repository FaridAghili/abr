# sites

A small Go CLI for an owner-managed VPS. Develop on macOS Apple Silicon; ship a
CGO-free Linux AMD64 executable for Ubuntu 26.04 LTS. Use the Go version declared
in `go.mod` (currently 1.27.1).

The CLI version stays at `0.1.0` during stabilization, including CI builds.

Milestone one implements `version`, `config validate`, `list`, `register`,
`ports`, and a portable `doctor`. Registration is flag-based. Running `sites`
prints help. Service lifecycle, provisioning, deployment, template rendering,
and the interactive menu are deferred; invoking those commands returns failure.
Nothing in this milestone contacts the production server or starts services.

## Build and try locally

From the repository root:

```sh
go build -o bin/sites ./cmd/sites
./bin/sites version
./bin/sites register --help

work=$(mktemp -d)
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --name quiet --dir /srv/quiet --user quiet --type laravel \
  --domain quiet.example.com --web-driver fpm --queue-workers 2 --scheduler
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --name mango --dir /srv/mango --user mango --type laravel \
  --domain mango.example.com --web-driver octane --octane-workers 2 \
  --nightwatch --inertia-ssr
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --name frontend --dir /srv/frontend --user frontend --type nuxt \
  --domain frontend.example.com --nuxt-mode ssr
./bin/sites --config "$work/config.toml" config validate
./bin/sites --config "$work/config.toml" list
./bin/sites --state-dir "$work/state" ports
./bin/sites --config "$work/config.toml" --state-dir "$work/state" doctor
```

These directory/user values describe the future Linux host. Validation does not
require them to exist on your Mac. Registration creates a missing config with
defaults, validates all applications, reserves required endpoints, and saves TOML
and JSON. Existing application names, directories, or exact domain names cannot
be registered twice. Root cannot be the runtime user. Empty registries have no
assignments. Path flags work before or after the command, with no root access
needed for temporary directories.

Additional registration flags: repeated `--alias`, `--serving-domain`, and
`--wildcard`; `--deploy-file`; and `--health-check`. Deployment files and health
checks are stored only. `--scheduler`, `--nightwatch`, `--inertia-ssr`, and
`--queue-workers` apply to Laravel. `--octane-workers` applies to Octane.
Nuxt uses `--nuxt-mode ssr` or `--nuxt-mode static`.

## Configuration and port reservations

Default paths are `/etc/sites/config.toml` and `/var/lib/sites/ports.json`.
See [`examples/config.toml`](examples/config.toml) for the full schema. TOML uses
[go-toml v2](https://github.com/pelletier/go-toml), pinned in `go.mod`. Unknown
keys, invalid types, duplicate registrations, and invalid component choices fail
validation. Default automatic port range: 10000–19999; override `[ports].first`
and `[ports].last`. Configure each Laravel application with
`[apps.web] driver = "fpm"` or `driver = "octane"`.

| Enabled component | Registry endpoint |
| --- | --- |
| Laravel Octane HTTP | `octane-http` |
| RoadRunner RPC | `roadrunner-rpc` |
| Nuxt SSR HTTP | `nuxt-http` |
| Laravel Inertia SSR | `inertia-ssr` |
| Laravel Nightwatch ingest | `nightwatch-ingest` |
| FPM (Unix socket), queues, scheduler, Nuxt static | No TCP ports |

Import specific free ports with repeated `--port ENDPOINT=PORT` flags:

```sh
./bin/sites --config "$work/config.toml" --state-dir "$work/state" register \
  --name legacy --dir /srv/legacy --user legacy --type laravel \
  --domain legacy.example.com --web-driver octane \
  --port octane-http=23000 --port roadrunner-rpc=23001
```

Imported ports may be outside the automatic range, but must be in 1024–65535,
unreserved, and free. Stop existing listeners before importing. Probes check
IPv4 and IPv6 wildcard binds, so loopback/interface listeners are also detected.
A reservation does not keep the TCP socket open: another process can take the
port later. Startup verification belongs to the future service lifecycle.

After manually editing TOML, reserve any newly enabled components:

```sh
./bin/sites --config "$work/config.toml" --state-dir "$work/state" ports --allocate
./bin/sites --config "$work/config.toml" --state-dir "$work/state" doctor
```

`ports` shows saved reservations. `ports --allocate` fills missing reservations
for all configured apps. Saved assignments remain stable even when the range
changes, components are disabled, or a saved port becomes occupied. Reservations
are never automatically released in this milestone; stopped-service removal
will come later. An allocation failure does not save partial new assignments.

`doctor` checks config/state validity, missing active reservations, orphan
reservations, and occupied active ports. It returns nonzero on any issue. It
cannot distinguish a managed listener from an unrelated process; an occupied
port is reported even if the application's own service owns it. It does not
check systemd, packages, Caddy, PHP, or application health.

Writers use permanent advisory lock files (`CONFIG.lock`, `STATE/ports.lock`)
and wait at most 10 seconds per lock. Readers of both files use the same locks. Leave lock
files in place; the OS releases the lock when a process exits. All writers must
use these locks; coordinate manual edits yourself. Atomic writes use same-directory
temp files, file sync, rename, and directory sync, with private file permissions.
Corrupt registry state is refused rather than replaced.

Config and registry are separate files. Registration saves reservations first,
then config; an interruption can leave reservations for an unregistered app.
The command reports a write failure, retries reuse those reservations, and
`doctor` reports orphan reservations if the config exists. TOML is rewritten on
registration, so comments/formatting are not preserved. Use a single matching
config/state pair for each installation. Keep secrets in application `.env` files.

## Local checks and production archive

```sh
gofmt -w cmd internal
test -z "$(gofmt -l cmd internal)"
go vet ./...
go test ./...
go test -race ./...

mkdir -p dist/package
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags '-s -w' -o dist/package/sites ./cmd/sites
cp -R templates dist/package/templates
cp examples/config.toml dist/package/config.example.toml
cp README.md dist/package/README.md
COPYFILE_DISABLE=1 tar -czf dist/sites-linux-amd64.tar.gz -C dist/package \
  sites templates config.example.toml README.md
(cd dist && shasum -a 256 sites-linux-amd64.tar.gz > sites-linux-amd64.tar.gz.sha256)
tar -tzf dist/sites-linux-amd64.tar.gz
(cd dist && shasum -a 256 -c sites-linux-amd64.tar.gz.sha256)
```

The Linux executable cannot run directly on macOS. Local Go tests run natively;
GitHub Actions runs the tests on Ubuntu 26.04. Tests use temporary files and
include real IPv4/IPv6 conflicts and locking across processes.

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs for pushes, pull
requests, and manual runs. It reads the Go version from `go.mod`, checks
formatting, vets/tests, builds Linux AMD64 with CGO disabled, creates the archive
and SHA256 file, and uploads both in the downloadable `sites-linux-amd64`
workflow artifact. The official actions are
[checkout v7](https://github.com/actions/checkout/releases/tag/v7.0.1),
[setup-go v7](https://github.com/actions/setup-go/releases/tag/v7.0.0), and
[upload-artifact v7](https://github.com/actions/upload-artifact/releases/tag/v7.0.1).
No credentials or production-server connection are needed.

The archive contains `sites`, `templates/`, `config.example.toml`, and this
README. The standalone template drafts are shipped for milestone two and are
not rendered or installed yet. Keep that directory alongside the executable.

On a disposable Ubuntu machine, after downloading the archive and checksum:

```sh
sha256sum -c sites-linux-amd64.tar.gz.sha256
mkdir -p sites-distribution
tar -xzf sites-linux-amd64.tar.gz -C sites-distribution
cd sites-distribution
./sites version
work=$(mktemp -d)
cp config.example.toml "$work/config.toml"
./sites --config "$work/config.toml" config validate
./sites --config "$work/config.toml" --state-dir "$work/state" ports --allocate
./sites --config "$work/config.toml" --state-dir "$work/state" doctor
```
