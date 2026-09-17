# Yukariko

Yukariko is a local single-binary agent for Docker Compose and standalone Docker deploys driven by Git or image digests.

Host Docker, Compose, and Git remain the source of truth. Yukariko observes, records, and runs controlled local commands; it does not replace Compose and is not Coolify, Portainer, or a remote orchestration platform.

## Prerequisites

- Go 1.24 or later to build from source
- Linux is the intended operations target (systemd packaging comes later)
- Windows and macOS are supported for development and tests
- Docker Engine, Compose, and Git are required only once those features land

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

The binary currently routes these commands. Each one fails with an explicit `not implemented` error until its dedicated issue lands.

| Command | Purpose |
|---|---|
| `run` | Daemon, scheduler, health monitor, optional HTTP server |
| `check` | Observe sources and health without deploying |
| `update` | Request updates through preflight and per-app locks |
| `status` | Process, health, and version status |
| `logs` | Bounded structured event history |
| `learn` | Import local Docker/Compose apps into configuration |

Global flags: `--config`, `--data-dir`, `--version`. The commands do not consume configuration yet; their issues wire that up.

`yukariko --help` and `yukariko --version` work today.

## Configuration

Yukariko's declarative YAML contract is defined and validated by `internal/config`: strict unknown-field rejection, path-aware validation errors, documented defaults, and secret **references only** (env/file indirection; literal secrets are not representable). See [`docs/config.md`](docs/config.md) for the full schema reference and [`examples/yukariko.yaml`](examples/yukariko.yaml) for a safe starting point.

## Scope

In scope for the MVP: local Git/Compose/standalone Docker deploys, read-only status API and dashboard, and optional signed peer reporting.

Out of scope: Coolify/Portainer behavior, Traefik or Kubernetes management, remote command execution, web configuration editing, automatic rollback, and treating Yukariko as the source of truth instead of the host's Docker/Compose/Git state.

Agents working on this repository should follow `AGENTS.md`.
