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

Current (#8):

- `cmd/yukariko` — process entry, signal context, process exit
- `internal/cli` — cobra routing, global flags, `learn` command, command stubs
- `internal/config` — strict YAML schema, defaults, path-aware validation, round-trippable rendering
- `internal/store` — SQLite state/event store, forward-only migrations
- `internal/runner` — argv-based controlled command runner with redaction
- `internal/docker` — read-only Docker discovery, Compose project grouping
- `internal/learn` — proposals + interactive import flow (diff, merge, backup, atomic write)
- `internal/schedule` — per-app loops, state machine, locks, backoff, preflight
- `internal/version` — build-time Version/Commit/Date
- `internal/exitcode` — stable process codes

Reserved packages (create only when the owning issue lands):

| Package | Issue |
|---|---|
| `internal/git` | #9 |
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

On the Windows ARM64 dev host, Smart App Control may block freshly built test
binaries that spawn or kill child processes (observed with the runner tests).
Run the suite through the WSL toolchain instead; WSL Go also supports `-race`,
which windows/arm64 does not:

```text
wsl.exe -e bash -lc "export PATH=/usr/local/go/bin:$PATH && cd /mnt/e/apps/yukariko && go test ./... && go test -race ./..."
```

## Go conventions

- Module: `github.com/Dragonshorn-Studios/yukariko`
- Stdlib first. Cobra is the CLI router only; do not add Viper. YAML parsing is `gopkg.in/yaml.v3` with `KnownFields(true)` strict decoding (`internal/config`); keep it the only YAML dependency. SQLite runs on `modernc.org/sqlite` (pure Go, no cgo) behind `database/sql` (`internal/store`): WAL, busy timeout, forward-only migrations recorded in `schema_migrations` — never edit an applied migration, append a new one.
- Docker access is CLI-only (`#5`): `internal/docker` builds `docker ps` / `docker inspect` argv and executes them through `internal/runner`; no Docker SDK or socket dependency, now or later. Discovery records environment-variable **names only** — inspect values must never enter results, logs, or storage — and the runner used for discovery must not attach a persistence `Sink`. Docker daemon access is host-equivalent privilege; see `docs/docker-access.md`.
- Learn classification is deterministic and side-effect free (`#6`): its only host interaction is read-only Git probing (`git rev-parse`, `git remote get-url`) at a Compose project's declared working directory — never an arbitrary disk scan. Proposals carry evidence + confirmations: uncertain values block import, and standalone specs with unsupported semantics (privileged, shared namespaces, anonymous volumes, one-offs) refuse auto-import. Remote URLs are credential-stripped before entering a proposal.
- Config merge ownership (`#7`): learn owns `source` + `deploy` only; a user's `interval`, `timeout`, `retry`, `steps`, `health`, `enabled`, and `display_name` always survive a merge. Rendering uses `config.DecodeRaw` + omitempty so defaults the user never wrote are never materialized into the file; generated documents must pass `config.Parse` before the original is touched. Writes go backup → temp → validate → atomic rename; declining or failing leaves the original byte-identical.
- Scheduler control plane (`#8`): one loop goroutine per app; a global semaphore caps concurrent check→deploy passes (default 2); a keyed per-app lock serializes scheduled and manual passes (scheduled = try-and-skip, manual = queue with timeout). The explicit state machine (`idle/checking/preflight/deploying/postchecks/succeeded/failed/backoff/interrupted`) lives in `internal/schedule`; transitions are validated and every outcome is an event for the #3 store. Cancellation marks a pass interrupted and can never record success, so a cancelled pass cannot advance the deployed version. Timing goes through the injectable `Clock`; jitter (default ≤ 1/10) applies to intervals and backoff (`retry.base/max`). Source checks and deployments are `Checker`/`Deployer` seams implemented by #9–#12; preflight gates deployment (binaries, Docker reachability, compose/git context, secret-ref resolvability, data-dir writability) while observation stays available. Reporting is never on the critical path.
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
