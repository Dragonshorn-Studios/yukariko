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

Complete (#1–#19): the MVP scope is frozen.

- `cmd/yukariko` — process entry, signal context, process exit
- `internal/cli` — cobra routing, global flags, all operational commands + themed aliases
- `internal/daemon` — component assembly, source/deploy dispatch, store adapters
- `internal/config` — strict YAML schema, defaults, path-aware validation, round-trippable rendering
- `internal/store` — SQLite state/event store, forward-only migrations
- `internal/runner` — argv-based controlled command runner with redaction, bounded stdin support
- `internal/docker` — read-only Docker discovery, Compose project grouping
- `internal/git` — branch change detection, fast-forward-only worktree preparation
- `internal/learn` — proposals + interactive import flow (diff, merge, backup, atomic write)
- `internal/schedule` — per-app loops, state machine, locks, backoff, preflight
- `internal/registry` — OCI/Distribution digest resolution (auth, platform, typed errors)
- `internal/health` — HTTP/docker/command probes, post-deploy checks, independent monitor
- `internal/deploy` — compose pipeline (#11); standalone recreation (#12); Docker CLI engine
- `internal/version` — build-time Version/Commit/Date
- `internal/exitcode` — stable process codes

New packages require an issue that updates this file first.

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

CI lives in `.github/workflows`: `ci.yml` gates pushes and pull requests (vet, race tests, Windows cross-compile smoke); `release.yml` builds tag-driven releases (`v*` tags) with `scripts/build-release.sh` and publishes them via the runner's `gh` CLI (a `workflow_dispatch` run is a no-publish dry-run). Release assets are the per-target tarballs, `SHA256SUMS`, and `deploy/yukariko.service`; `scripts/install.sh` consumes them — as root it provisions the `yukariko` user, config (never overwriting), data dir, and unit, docker-group membership stays opt-in via `--docker-group`, and git-source worktrees reach the deliberately strict unit only through `ReadWritePaths` drop-ins (`--read-write`, docs/operations.md). Do not add Makefiles, goreleaser, or extra toolchains.

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
- Config merge ownership (`#7`): learn owns `source` + `deploy` only; a user's `interval`, `timeout`, `retry`, `steps`, `health`, `enabled`, and `display_name` always survive a merge. Rendering uses `config.DecodeRaw` + omitempty so defaults the user never wrote are never materialized into the file; generated documents must pass `config.Parse` before the original is touched. Writes go backup → temp → validate → atomic rename, preserving the original's mode and ownership (a root `learn` must not leave a config the service user cannot read); declining or failing leaves the original byte-identical.
- Scheduler control plane (`#8`): one loop goroutine per app; a global semaphore caps concurrent check→deploy passes (default 2); a keyed per-app lock serializes scheduled and manual passes (scheduled = try-and-skip, manual = queue with timeout). The explicit state machine (`idle/checking/preflight/deploying/postchecks/succeeded/failed/backoff/interrupted`) lives in `internal/schedule`; transitions are validated and every outcome is an event for the #3 store. Cancellation marks a pass interrupted and can never record success, so a cancelled pass cannot advance the deployed version. Timing goes through the injectable `Clock`; jitter (default ≤ 1/10) applies to intervals and backoff (`retry.base/max`). Source checks and deployments are `Checker`/`Deployer` seams implemented by #9–#12; preflight gates deployment (binaries, Docker reachability, compose/git context, secret-ref resolvability, data-dir writability) while observation stays available. Reporting is never on the critical path.
- Git update policy (`#9`): `internal/git` polls `ls-remote`, fetches only when the remote is actually ahead (fetch updates remote-tracking refs, never the worktree), and prepares deployments with `merge --ff-only` after re-validating dirty/detached/diverged conditions. `git reset --hard` and every lossy or history-rewriting command are forbidden — divergence is blocked with an actionable reason, never auto-resolved. The observed SHA is informational; only the deploy pipeline's success checkpoint (#11) advances the deployed SHA. Remote URLs never enter results: details name remotes by configured name, and command output is runner-redacted before use.
- Registry resolution (`#10`): `internal/registry` speaks OCI/Distribution v2 over stdlib HTTP — no registry SDK. Docker Hub aliases normalize onto registry-1.docker.io; loopback registries use plain http; everything else is https. Auth is Docker's own token flow with credentials delegated to `~/.docker/config.json` (`auths`, `credsStore`, `credHelpers` → `docker-credential-*` over the runner with bounded stdin); credentials live in memory only and never reach YAML, SQLite, logs, or errors. Token realms must be https or loopback http — an injected challenge can never redirect credentials. Manifest bodies are digest-verified against `Docker-Content-Digest` (mismatch = malformed); multi-arch indexes resolve to the deterministic platform digest (`os/arch[/variant]`). Failures are typed and carry a Retryable flag for the #8 backoff; the resolver never retries internally and never pulls images — a successful lookup is never a deployed version.
- Health is observation only (`#13`): `internal/health` probes HTTP (call-time SecretRef headers, query-less URL rendering, bounded diagnostics), Docker health via the read-only #5 interface (standalone container names only in this build), and explicitly configured commands. `RunPostDeployChecks` failing a required check fails the deployment — the caller must not advance SHA/digest. The monitor records samples every tick and transitions only on state change, backing off up to 4× the interval on consecutive errors; there is no restart or rollback path anywhere.
- Compose deploys (`#11`): `internal/deploy.ComposePipeline` runs every command with the exact configured context (`-f` files in override order, `--env-file`, `--profile`, `-p`, workdir as the command's Dir) — compose stays the source of truth and nothing is reconstructed. Registry apps: resolve expected digests → `pull` → verify local RepoDigests contain each expected manifest digest → `up -d --wait`; git apps: `git.Prepare` → configured pre/deploy/post steps (empty deploy list defaults to `up -d --build --wait`). Order: commands → required health checks (`#13`) → exactly one `VersionCheckpoint.MarkDeployed`; any failure or cancellation leaves the deployed version untouched, and the pipeline refuses to run without a checkpoint (fail closed).
- Standalone recreation (`#12`): `internal/deploy.StandalonePipeline` runs resolve → pull+verify → refusal preflight (compose-managed, privileged, shared pid/ipc namespaces, env names missing from the spec, healthcheck absent from the spec — all refused BEFORE stopping) → stop old → rename old to `<name>-yukariko-old-<ts>` → `docker run` from the exact golden argv → connect extra networks → required health checks → remove old → checkpoint the resolved digest. Unchanged digests are a no-op; a first-time creation (no existing container) skips stop/rename/remove. Failures after the stop stage leave the old container stopped under its backup name with the manual recovery command in the error — no automatic rollback exists, and secret values travel only through the runner environment.
- Daemon wiring (`#14`): `internal/daemon.Assemble` builds every component and the store adapters; `SourceChecker` routes git/registry checks and records observations (kind `git_sha`, or `digest:<canonical ref>` per image) separately from deployed versions; `DeployDispatcher` opens a deployment row per run and binds its checkpoint to `CommitDeploymentSuccess` — the only path that advances the deployed version. Commands `run/check/update/status/logs` each have a themed alias implemented as the same cobra command (behaviorally identical by construction); `check` never deploys, `update --dry-run` runs check+preflight only, and all commands take `--config` plus a data dir: a root invocation on Linux defaults to the installer layout (`/etc/yukariko/yukariko.yaml`, `/var/lib/yukariko` — provisioned by `scripts/install.sh`; a missing default config errors with an installer pointer), otherwise `--config` is required and the data dir defaults to `./data` (`internal/cli/defaults.go`); a root CLI pass also hands root-created store files to the data dir's owner so the systemd service user is never locked out of its store. `expose` (no alias, `#44`) is the operator-side counterpart to the strict unit: as root it appends one resolved git-worktree or rootless-socket path to the unit's `ReadWritePaths` drop-in idempotently (auto `ProtectHome=read-only` under `/home` and `/run/user`) and reloads systemd; the installer suggests it when it finds rootless sockets under `/run/user/*/docker.sock` but never grants anything itself.
- Read-only API (`#15`): `internal/httpapi` serves GET/HEAD-only JSON (`/api/v1/apps`, `/apps/{id}`, `/hosts`, `/deployments`, `/logs`, `/health`) with security headers, capped pagination, and JSON errors; route projections live in `internal/state` (shared with the CLI) to keep `httpapi` free of daemon imports. Remote-host availability derives deterministically from heartbeat age (online ≤ stale-after ≤ stale ≤ offline-after; default 3m/10m) and absence is never healthy. Local/remote records expose source and age. The bind defaults to loopback; exposure is an operator decision behind existing access controls. No route can mutate anything.
- Outbound reporting (`#18`): `internal/report.Reporter` persists every report to the #3 outbox BEFORE delivery (enqueue is local and cannot fail from receiver outages), then drains with bounded exponential backoff (+jitter, Retry-After respected). Heartbeats coalesce; deployment/audit events never coalesce and are never dropped under queue pressure — the high-water policy only drops coalesced heartbeats and emits a diagnostic. Permanent rejections (401/403) retire an event with the reason in its attempt history; 409 on retry means delivered (lost-ACK dedup). Reporting failures never block checks or deploys: enqueue is the only local coupling, and the drain loop is an independent goroutine. Secrets sign requests at send time via SecretRef and are never logged.
- Dashboard (`#16`): `internal/ui` server-renders with html/template + embed — no JavaScript, no forms, no build chain. The four sections (Sanctuary overview, Vestments versions, Chronicle history, Divination health) render from the same `internal/state` projections as the CLI/API. Running/health/update/deployment are separate columns and separate badge classes (they cannot be confused visually or textually); remote source/age and stale/offline are prominent. All assets are original (docs/ASSETS.md inventories them); the palette is navy/ivory/lapis/restrained gold with reduced-motion and responsive rules.
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
- #44 Rootless socket discovery hints and a yukariko expose command
