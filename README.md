# Yukariko

Yukariko is a local single-binary agent for Docker Compose and standalone Docker deploys driven by Git or image digests.

Host Docker, Compose, and Git remain the source of truth. Yukariko observes, records, and runs controlled local commands; it does not replace Compose and is not Coolify, Portainer, or a remote orchestration platform.

## Prerequisites

- Go 1.25 or later to build from source
- Linux is the intended operations target (systemd packaging comes later)
- Windows and macOS are supported for development and tests
- Docker Engine, Compose, and Git are required only once those features land

## Download

Prebuilt Linux binaries are published on the [Releases page](https://github.com/Dragonshorn-Studios/yukariko/releases) for every `v*` tag:

1. Download `yukariko-<version>-linux-<arch>.tar.gz` (amd64 or arm64) and `SHA256SUMS`.
2. Verify with `sha256sum -c --ignore-missing SHA256SUMS`.
3. Unpack and install the binary to `/usr/local/bin/yukariko` (full walkthrough in [`docs/operations.md`](docs/operations.md)).

## Build

Windows:

```text
go build -o yukariko.exe ./cmd/yukariko
```

Unix:

```text
go build -o yukariko ./cmd/yukariko
```

Inject version metadata at link time:

```text
go build -ldflags "-X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=<ver> -X github.com/Dragonshorn-Studios/yukariko/internal/version.Commit=<sha> -X github.com/Dragonshorn-Studios/yukariko/internal/version.Date=<date>" -o yukariko.exe ./cmd/yukariko
```

Development builds fall back to `dev` / `none` / `unknown`.

## Test

```text
go test ./...
go vet ./...
```

## Commands

Every command is operational. Each themed alias is the same command with identical behavior — use whichever you prefer.

| Command | Aliases | Purpose |
|---|---|---|
| `run` | `materialize` | Daemon: per-app scheduling, deployments, health monitoring, reporting |
| `check` | `divine` | Observe Git branches and registry digests without deploying |
| `update` | `bless` | One deployment pass for `--app <id>` or `--all` through preflight and the per-app lock (`--dry-run` previews) |
| `status` | `observe` | Deployed vs observed versions, pending updates, health, last deployment (`--json`) |
| `logs` | `chronicle` | Bounded, filterable structured event history (`--json`) |
| `learn` | — | One-time onboarding: scan local Docker read-only, select candidates, resolve required fields, preview a YAML diff, import after explicit confirmation |

`yukariko learn --config yukariko.yaml` scans running and stopped containers (never mutating Docker), proposes one app per Compose project and per standalone container, and writes only after you confirm the diff — with a timestamped backup, atomic replacement, and full re-validation. Existing manual settings (intervals, retries, steps, health, enabled) survive merges. Flags: `--dry-run` previews without writing, `--json` prints machine-readable proposals without prompts, `--include-system` offers system/infrastructure candidates (excluded by default). learn never deploys anything; monitoring starts when the daemon runs.

Global flags: `--config`, `--data-dir` (default `./data`), `--version`. Exit codes for scripts: 0 ok, 1 error, 2 usage, 130 interrupted.

`yukariko --help` documents every command; `yukariko --version` reports the build.

## Operations and security

Installation, systemd hardening, upgrades, SQLite backup, private Git/registry auth, reporting key rotation, retention, troubleshooting, and recovery steps: [`docs/operations.md`](docs/operations.md). The threat model, security checklist, and Docker-privilege implications: [`docs/security.md`](docs/security.md) and [`docs/docker-access.md`](docs/docker-access.md). Release builds with checksums: `scripts/build-release.sh <version>`. The end-to-end acceptance suite: `go test -tags e2e ./internal/e2e/`.

## Configuration

Yukariko's declarative YAML contract is defined and validated by `internal/config`: strict unknown-field rejection, path-aware validation errors, documented defaults, and secret **references only** (env/file indirection; literal secrets are not representable). See [`docs/config.md`](docs/config.md) for the full schema reference and [`examples/yukariko.yaml`](examples/yukariko.yaml) for a safe starting point.

## Scope

In scope for the MVP: local Git/Compose/standalone Docker deploys, read-only status API and dashboard, and optional signed peer reporting.

Out of scope: Coolify/Portainer behavior, Traefik or Kubernetes management, remote command execution, web configuration editing, automatic rollback, and treating Yukariko as the source of truth instead of the host's Docker/Compose/Git state.

Agents working on this repository should follow `AGENTS.md`.
