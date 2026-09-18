# Operations guide

Installing, running, upgrading, and recovering the Yukariko MVP. For the
configuration schema see [`config.md`](config.md); for the privilege model
see [`docker-access.md`](docker-access.md) and [`security.md`](security.md).

## Installation

1. Build or download the binary:
   - One-liner (Linux): `curl -fsSL https://raw.githubusercontent.com/Dragonshorn-Studios/yukariko/main/scripts/install.sh | sudo sh`
     — detects the architecture, verifies checksums, installs to
     `/usr/local/bin/yukariko`, and as root also provisions everything
     below: the `yukariko` user, `/etc/yukariko` (an existing config is
     never overwritten), `/var/lib/yukariko`, and the systemd unit. Docker
     group membership stays opt-in (`--docker-group`). Steps 2-4 are only
     needed for manual installs.
   - Release: download `yukariko-<version>-linux-<arch>.tar.gz` and
     `SHA256SUMS` from the GitHub Releases page, verify with
     `sha256sum -c --ignore-missing`, unpack, and install the binary to
     `/usr/local/bin/yukariko`.
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

## Git-source apps under systemd

The shipped unit is deliberately strict: only `/var/lib/yukariko` is
writable (`ProtectSystem=strict`) and `/home` is inaccessible
(`ProtectHome=yes`). Registry and standalone apps work under it unchanged,
but git sources write to their worktrees — `git fetch` updates `.git` and
the fast-forward merge updates the working tree — so every git app's
worktree must be exposed to the unit through a drop-in before the daemon
can deploy it:

```ini
# /etc/systemd/system/yukariko.service.d/writables.conf
[Service]
ReadWritePaths=/srv/ghost        # appends; the data-dir entry stays
# ProtectHome=read-only          # uncomment if configs live under /home
```

Then `systemctl daemon-reload && systemctl restart yukariko`. The
installer automates this — repeat `--read-write` per worktree:

```sh
curl -fsSL https://raw.githubusercontent.com/Dragonshorn-Studios/yukariko/main/scripts/install.sh \
  | sudo sh -s -- --read-write /srv/ghost --home-read
```

Keep the grant minimal: one entry per worktree, nothing broader. The unit
stays strict by default; drop-ins are the operator's explicit decision.

The same grant, one command at a time: `sudo yukariko expose` from inside
a project directory (or `sudo yukariko expose <path>`) appends the resolved
path to the same drop-in idempotently, reloads systemd, and handles the
`ProtectHome` case automatically for paths under `/home` and `/run/user`.
It also accepts rootless Docker sockets
(`sudo yukariko expose /run/user/<uid>/docker.sock`); the installer
suggests this when it detects rootless sockets on the host.

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
