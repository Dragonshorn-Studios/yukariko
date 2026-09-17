# Operations guide

Installing, running, upgrading, and recovering the Yukariko MVP. For the
configuration schema see [`config.md`](config.md); for the privilege model
see [`docker-access.md`](docker-access.md) and [`security.md`](security.md).

## Installation

1. Build or download the binary:
   - Release: unpack `yukariko-<version>-linux-<arch>` and its SHA256SUMS,
     verify with `sha256sum -c`, install to `/usr/local/bin/yukariko`.
   - From source: `scripts/build-release.sh <version>` (reproducible flags,
     checksums emitted).
2. Create a dedicated user and directories:

   ```sh
   useradd --system --home /var/lib/yukariko --shell /usr/sbin/nologin yukariko
   adduser yukariko docker            # Docker access is root-equivalent; see docs/docker-access.md
   mkdir -p /etc/yukariko /var/lib/yukariko
   chown yukariko:yukariko /var/lib/yukariko
   chmod 750 /var/lib/yukariko
   ```

3. Write `/etc/yukariko/yukariko.yaml` (start from
   `examples/yukariko.yaml`; the schema reference is
   [`config.md`](config.md)).
4. Install `deploy/yukariko.service` into `/etc/systemd/system/`, then
   `systemctl daemon-reload && systemctl enable --now yukariko`.

A clean host following these steps can `learn`/import, `run`, `status`,
`update`, and inspect `logs` — `yukariko --help` lists the commands and
their documented exit codes (0 ok, 1 error, 2 usage, 130 interrupted).

## Upgrades

- Replace the binary (same path), then `systemctl restart yukariko`.
- The configuration schema is versioned; a build refuses a newer schema
  with a migration hint. Migrations are documented in `docs/config.md`
  before any bump.
- The SQLite store upgrades itself with forward-only migrations on first
  open. Never run a new binary against a copied-downgraded data directory.

## SQLite backup and recovery

- The durable state lives in `<data-dir>/yukariko.db` (WAL mode). Back up
  with the SQLite online API or by stopping the daemon and copying the
  `yukariko.db`, `-wal`, and `-shm` files together.
- The database is reconstructable observation history: the deployed-version
  checkpoint is the only state that matters for safe behavior, and losing
  it merely means the next deploy re-establishes the baseline.

## Private Git and registry auth

- Git sources use the worktree's own remotes and the host's Git credential
  helpers/agents. Yukariko never stores Git credentials; remote URLs are
  credential-stripped before they can appear in status or logs.
- Registry lookups use Docker's own configuration: `~/.docker/config.json`
  `auths`, `credHelpers`, and the default `credsStore` via
  `docker-credential-*`. Keys for outbound reporting are file/env
  references (`secret_ref`), resolved at use and never persisted.

## Compose profiles, env files, and project names

`deploy.compose` reproduces the exact invocation: `work_dir` becomes the
command directory, `files` are passed in order as `-f` (later files
override earlier), `env_files` as `--env-file`, `profiles` as `--profile`,
and `project_name` as `-p`. Compose remains the source of truth — Yukariko
never reconstructs ports, networks, volumes, or environment.

## Reporting keys and rotation

Inbound hosts are allowlisted in `reporting.inbound.hosts`, each with one
or more `secret_ref` keys. Add the new key alongside the old, redeploy the
sender, then remove the old key — revocation is immediate. Outbound senders
resolve their key from `reporting.outbound.secret_ref` at send time.

## Retention

`retention.events_days` (default 30), `health_days` (14),
`deployments_days` (365) bound the durable history; cleanup runs at daemon
start. Deployment audit records are never silently dropped by the outbound
queue.

## Troubleshooting and recovery

- **`status` says failed**: read the deployment error in
  `yukariko logs --app <id>`; the deployed version is unchanged, so fixing
  the cause and re-running `update` is always safe.
- **Standalone recreation failed midway**: the previous container is
  stopped under `<name>-yukariko-old-<ts>` and the new one holds the
  configured name. Recover manually:
  `docker stop <name> && docker rename <name>-yukariko-old-<ts> <name> && docker start <name>`,
  then investigate. There is no automatic rollback.
- **Preflight refuses**: fix the named finding (missing binary, compose
  file, env file, secret reference, data-dir permission) and re-run
  `update --dry-run` to re-check.
- **Leftover temp files** in the data directory from a crashed write are
  removed automatically on the next run.

## Scope freeze (non-goals)

Yukariko is not Coolify, Portainer, Traefik, or Kubernetes. There is no
remote execution, web configuration editing, secret management, automatic
health restart, automatic rollback, blue-green deployment, or zero-downtime
guarantee. Feature requests outside `AGENTS.md`'s fence are declined.
