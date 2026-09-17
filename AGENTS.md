# Yukariko

Local Go single-binary CLI/agent for Docker Compose and standalone Docker deploys driven by Git or image digests. Host Docker, Compose, and Git remain the source of truth. Yukariko observes, records, and runs controlled local commands; it does not become the platform.

Human docs: `README.md`. This file is the canonical agent contract for Cursor, Codex, OpenCode, and ZCode. Keep it updated when an issue makes a real architecture decision. Do not duplicate it into `CLAUDE.md`, `.cursorrules`, or tool-specific instruction files.

## Issue protocol

Work GitHub issues **#1 → #19 in order**.

1. Read that issue’s Goal, Scope, Out of scope, Acceptance criteria, and Tests before coding.
2. Implement only that issue. Do not pull in later issues “while you are here.”
3. Stub missing behavior with an explicit error (non-zero exit). Never silent success.
4. Do not add themed CLI aliases (`materialize`, `divine`, `bless`, `observe`, `chronicle`) until #14.
5. After a real architecture decision, update this file in the same change.

Repo: `https://github.com/Dragonshorn-Studios/yukariko`

## Product fence

- One binary from `cmd/yukariko`. Daemon, CLI, HTTP API, and UI all live in that binary.
- Local Docker/Compose/Git state is authoritative. Yukariko must not reconstruct Compose networks, ports, volumes, or environment as its own source of truth.
- HTTP API and web UI are **read-only**. No deploy buttons, YAML editors, or mutation routes.
- Peer reports are status only. Network payloads must never select or provide commands.
- Health monitoring must never restart or roll back containers by itself.
- Secrets are env/file references only. Never persist, log, or embed secret values.

Non-goals (do not implement): Coolify, Portainer, Traefik/proxy management, Kubernetes, GitHub Actions runner, blue-green, remote execution, SSH/reverse control, web config editing, secret manager product, automatic health restart, automatic rollback, zero-downtime promises.

## Layout

Current (#3):

- `cmd/yukariko` — process entry, signal context, process exit
- `internal/cli` — cobra routing, global flags, command stubs
- `internal/config` — strict YAML schema, defaults, path-aware validation
- `internal/store` — SQLite state/event store, forward-only migrations
- `internal/version` — build-time Version/Commit/Date
- `internal/exitcode` — stable process codes

Reserved packages (create only when the owning issue lands):

| Package | Issue |
|---|---|
| `internal/runner` | #4 |
| `internal/docker` | #5–#6 |
| `internal/learn` | #7 |
| `internal/schedule` | #8 |
| `internal/git` | #9 |
| `internal/registry` | #10 |
| `internal/deploy` | #11–#12 |
| `internal/health` | #13 |
| `internal/httpapi` | #15 |
| `internal/ui` | #16 |
| `internal/report` | #17–#18 |

## Commands

Windows:

```text
go build -o yukariko.exe ./cmd/yukariko
go test ./...
go test -race ./...
go vet ./...
```

Unix:

```text
go build -o yukariko ./cmd/yukariko
go test ./...
go test -race ./...
go vet ./...
```

Version injection:

```text
go build -ldflags "-X github.com/Dragonshorn-Studios/yukariko/internal/version.Version=<ver> -X github.com/Dragonshorn-Studios/yukariko/internal/version.Commit=<sha> -X github.com/Dragonshorn-Studios/yukariko/internal/version.Date=<date>" -o yukariko.exe ./cmd/yukariko
```

Do not add CI, Makefiles, or extra toolchains unless the current issue requires them.

## Go conventions

- Module: `github.com/Dragonshorn-Studios/yukariko`
- Stdlib first. Cobra is the CLI router only; do not add Viper. YAML parsing is `gopkg.in/yaml.v3` with `KnownFields(true)` strict decoding (`internal/config`); keep it the only YAML dependency. SQLite runs on `modernc.org/sqlite` (pure Go, no cgo) behind `database/sql` (`internal/store`): WAL, busy timeout, forward-only migrations recorded in `schema_migrations` — never edit an applied migration, append a new one.
- Source files must be UTF-8 without a BOM. Go rejects UTF-16.
- `log/slog` for logs. Redact secrets and credential-bearing URLs.
- Table-driven tests. Fake host dependencies; do not require live Docker/Git in unit tests.
- Interfaces belong at the consumer. Later runner must be argv-based with no implicit shell.
- Global flags `--config` and `--data-dir` are parsed only until #2; do not load YAML early.

## Security

- Never commit `.env`, key files, or credentials.
- Never copy Docker/Git registry credentials into YAML, SQLite, or logs.
- UI assets must be original. No copyrighted Mai-HiME/Mai-Otome (or other third-party) art, characters, logos, or layouts.

## Issue map

- #1 Bootstrap Go module and single-binary CLI (current foundation)
- #2 YAML configuration schema and validation
- #3 SQLite migrations and durable state/event store
- #4 Controlled command runner with redaction
- #5 Read-only Docker discovery
- #6 Classify discovery into safe learn proposals
- #7 `learn` preview, selection, and safe config merge
- #8 Scheduler, preflight, backoff, per-app locks
- #9 Git branch change detection without destroying local work
- #10 Registry digest resolution (auth, platform, retry)
- #11 Compose deployment pipeline
- #12 Safe standalone container recreation
- #13 Post-deploy checks and independent health monitoring
- #14 Operational CLI commands and plain/climate aliases
- #15 Read-only HTTP API
- #16 Embedded read-only web dashboard
- #17 Authenticated inbound reporting with replay protection
- #18 Outbound heartbeat, durable offline queue, retry
- #19 Package, harden, document, and E2E-verify the MVP
