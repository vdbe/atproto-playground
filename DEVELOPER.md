# Developer Guide

This guide assumes a **Nix-free** development environment: Go and the
development tools are installed directly, and the local AT Protocol stack
runs on Docker Compose or Podman Compose. The repository's `flake.nix` and
`.envrc.example` are for Nix/direnv users.

## 1. Install the prerequisites

- **Go 1.26.5 or later**: download from <https://go.dev/dl/> or use your
  distribution's package manager.
- **Docker with Compose, or Podman with Compose**: Docker Desktop or the
  `docker compose` plugin on Linux; alternatively `podman` with `podman
  compose` (provided by the `podman-compose` package on most distributions).
  All stack commands below use `docker compose`; Podman users run the
  identical commands with `podman compose`.
- **OpenSSL** and **curl**: present by default on macOS and most Linux
  distributions.

Ensure Go's binary directory is on your `PATH` so tools installed with
`go install` resolve:

```bash
export PATH="$(go env GOPATH)/bin:$PATH"
```

## 2. Install the development toolchain

[treefmt](https://treefmt.com/) is the repository's formatter multiplexer: it
formats and lints Go, shell, and text files in one command (plus `*.nix` when
the optional Nix formatters are installed). treefmt skips a configured formatter
that is missing from your `PATH` with a warning rather than failing, so start
with the core tools and add the optional ones only if you work on the matching
file types.

Prefer your system package manager where it provides these tools; otherwise use
the official install docs linked in the table.

| Tool | What it does | Official docs / repo |
| --- | --- | --- |
| treefmt | runs every formatter below | <https://treefmt.com/installation/> |
| golangci-lint | Go linter (linters: govet, revive, gosec, staticcheck, errcheck, …) | <https://golangci-lint.run/docs/welcome/install/local/> |
| gofumpt | stricter gofmt | <https://github.com/mvdan/gofumpt> |
| shfmt | shell formatter (2-space indent) | <https://github.com/mvdan/sh> |
| shellcheck | shell linter | <https://github.com/koalaman/shellcheck> |
| typos | typo checker (all files) | <https://github.com/crate-ci/typos> |
| keep-sorted | sorts `# keep-sorted` blocks | <https://github.com/google/keep-sorted> |

After installing, confirm the toolchain works with the checks in
[step 7](#7-format-and-lint).

## 3. Create the local configuration

```bash
cp .env.example .env
```

## 4. Generate the three blank PDS secrets

Run this command three times and assign a different result to each
of `PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX`, `PDS_JWT_SECRET`, and
`PDS_ADMIN_PASSWORD` in `.env`:

```bash
openssl rand -hex 32
```

## 5. Start the stack and verify its health checks

```bash
docker compose -f deployments/compose.yaml up --build --wait
# Podman: identical command, just swap the driver
podman compose -f deployments/compose.yaml up --build --wait
```

Then verify the stack's health checks (same for both runtimes):

```bash
curl --fail http://localhost:2582/_health
curl --fail http://localhost:2583/xrpc/_health
curl --fail http://localhost:2584/healthz
curl --fail http://localhost:5050/misc/ping
```

## 6. Create the local account and run the bot

Install [Goat](https://github.com/bluesky-social/goat) (see its README),
create the account configured in `.env`, then run the bot. The bot loads `.env`
automatically on startup, so no manual exporting or direnv is needed; the
account script reads `.env` itself.

```bash
./scripts/create-local-account.sh
go run ./cmd/bot
```

Stop the bot with `Ctrl-C` or any other `SIGINT`/`SIGTERM` signal.

## 7. Format and lint

```bash
treefmt
```

Formats and lints everything in one pass: Go (gofumpt + golangci-lint via
`scripts/treefmt-golangci.sh`), shell (`shfmt` + `shellcheck`), `*.nix`
(`nixpkgs-fmt`), plus `typos` and `keep-sorted` over all files. The
configuration in `.treefmt.toml` enables `fail-on-change`, so treefmt exits
non-zero whenever it had to change a file — commit those changes rather than
silently ignoring them. Use `treefmt --clear-cache` if you suspect stale
results.

Run a subset of formatters when iterating on one area:

```bash
treefmt -f shfmt -f shellcheck
treefmt -f golangci-lint
```

Or lint Go directly without treefmt:

```bash
golangci-lint run ./...
```

The linter set is defined in `.golangci.yml` and includes govet, revive,
gocritic, gosec, staticcheck, errcheck, and the rest of the best-practice
bundle; the goal is zero issues.
## 8. Run tests

Ordinary tests, or race-enabled tests (the race command removes the race
detector's exit delay):

```bash
go test ./...
go test -race ./...
```

The real-PDS integration tests are opt-in. Start the Compose stack first, then
run:

```bash
go test -tags=integration -count=1 -v ./test/integration
```

The tests read only the current process environment and do not load `.env`.
Export it before running the tests, for example with `direnv` or:

```bash
set -a
source .env
set +a
```

The tests use `PDS_TEST_URL`, then `ACCOUNT_PDS`, and finally
`http://localhost:2583`. `PDS_ADMIN_PASSWORD` is required for reliable account
cleanup. Exported OTLP endpoint variables send test logs, metrics, and traces
to the stack's collector. To exercise the PDS behavior without an OpenTelemetry
backend, disable the SDK explicitly:

```bash
OTEL_SDK_DISABLED=true go test -tags=integration -count=1 -v ./test/integration
```

Every test process generates one `run_id` from the source commit and UTC start
time. Set `ATPROTO_TEST_RUN_ID` to coordinate the same value across shards or
separate invocations. Telemetry also includes the full source revision as the
`vcs.revision` resource attribute. In Grafana, filter telemetry by `run_id`,
`vcs.revision`, and `service.name=it`. Select an individual test with the
per-signal `test.name` or `test.name.fingerprint` attribute. Older per-test
`it-<sanitized-test-name>` service selectors are obsolete; test identity is not
part of the shared process resource.
Navigate each test trace through the stable `test.setup.*`, `test.fixture.*`,
`test.action.*`, and `test.observe.*` spans. Test-owned HTTP spans are children
of those semantic operations, while production behavior remains under
`bot.label_profile` and `bot.create_post`.

Integration accounts are deleted through the PDS admin API after each test.
If a process is interrupted before cleanup, remove all persistent test accounts
and records by resetting the Compose volumes as described in step 10.

## 9. Use the local services

- Grafana dashboard: <http://localhost:3000/d/atproto-dev-services> (`admin` / `admin`)
- PDS: <http://localhost:2583/>
- PLC directory: <http://localhost:2582/>
- PDSls repository listing: <http://localhost:2584/localhost:2583>
- pgAdmin: <http://localhost:5050/> (defaults from `.env`: `admin@example.com` / `test-only-pgadmin-password`)

## 10. Stop and reset the stack

Stop the Compose stack while preserving its named volumes:

```bash
docker compose -f deployments/compose.yaml down
# or with Podman:
podman compose -f deployments/compose.yaml down
```

To fully reset the isolated environment, including local accounts and persisted
service data, remove the named volumes:

```bash
docker compose -f deployments/compose.yaml down --volumes
# or with Podman:
podman compose -f deployments/compose.yaml down --volumes
```

## Troubleshooting

- **`go: command not found` for tools installed with `go install`** — add
  `$(go env GOPATH)/bin` to your `PATH` (see [step 1](#1-install-the-prerequisites)).
- **treefmt warns about a missing formatter** — the formatter is not on your
  `PATH`; install it (see [step 2](#2-install-the-development-toolchain)) or
  ignore the warning if it is an optional Nix-only tool.
- **`treefmt` exits non-zero after formatting** — this is `fail-on-change`
  working as intended; review the changed files and commit them.
