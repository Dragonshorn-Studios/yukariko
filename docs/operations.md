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
a project directory (or `sudo yukariko expose <path>`) adds the resolved
path to the same drop-in idempotently, always reloads systemd (a rerun
heals a previously failed reload), and handles the `ProtectHome` case
automatically for paths under `/home`, `/root`, and `/run/user`. The
installer and `expose` merge into this drop-in — neither truncates the
other's entries.
It also accepts rootless Docker sockets
(`sudo yukariko expose /run/user/<uid>/docker.sock`); the installer
suggests this when it detects rootless sockets on the host.

### Watching several users' projects

Yukariko is designed for homelabs — one administrator, one daemon — but
projects and rootless daemons may belong to different login users. Two
supported layouts:

- **Root daemon (simplest for multi-user setups):** a unit drop-in with
  `[Service]` `User=root` / `Group=root`. Root reaches every worktree and
  every rootless socket; the unit's `ProtectSystem=strict` sandbox plus
  one `expose` per project/socket remains the real boundary. Manual
  `sudo yukariko …` passes become consistent with the daemon.
- **Unprivileged daemon + ACLs:** keep the `yukariko` user and let
  `expose` apply the filesystem ACLs for it — `rwX` (with defaults) on
  worktrees, `rw` on sockets, traverse on the owning `/home/<user>` or
  `/run/user/<uid>` tree. `expose` reapplies them on every run, which
  matters: rootless dockerd recreates its socket on restart and
  `/run/user` is recreated at login, both dropping ACLs — after a rootless
  daemon restart, re-run `sudo yukariko expose <socket>`. The installer
  provisions the `acl` package; manual runs on worktrees as the service
  user need explicit flags:
  `sudo -u yukariko yukariko --config /etc/yukariko/yukariko.yaml --data-dir /var/lib/yukariko …`
  (the flagless defaults are root-only). Root-run passes write root-owned
  files into the worktree and the store; the store is healed
  automatically, the worktree is not — prefer one layout consistently.

**Rootless Docker endpoints** need the same treatment: `ProtectHome=yes`
blocks `/run/user` entirely, so expose the daemon's socket the same way
(`sudo yukariko expose /run/user/<uid>/docker.sock`) and ensure the socket
exists at boot with `loginctl enable-linger <user>`. The `yukariko`
service user must be able to reach that socket — see
[`docker-access.md`](docker-access.md) for the permission realities, and
point the daemon at it via the `docker.host` endpoint in the config. On a
rootless-only host you may also want a drop-in removing the unit's
`Requires=docker.service` (it assumes the system daemon).

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

## Publishing the dashboard (OIDC with Authentik)

By default the HTTP surface binds loopback and carries no authentication.
To publish it on an untrusted network, put Yukariko behind your own
TLS-terminating reverse proxy and enable the optional OIDC gate (issue
#61) so Yukariko itself decides who may read — proxy headers are never
trusted.

Layout: browsers reach `https://yukariko.example.com` (your proxy, with a
certificate); the proxy forwards to `server.bind`
(`127.0.0.1:8484` by default) over plain HTTP. No special proxy
integration is required — no forward-auth outpost, no header passing.

Authentik side (any compliant OIDC provider works):

1. Create an application with an **OAuth2/OpenID Connect** provider of
   type *Web*.
2. Redirect URI: `https://yukariko.example.com/auth/callback` (exactly
   `auth.oidc.redirect_base` + `/auth/callback`).
3. Note the client ID and client secret; put the secret in an env var or
   a root-readable file and reference it from
   `auth.oidc.client_secret_ref`.
4. The issuer is the provider's issuer URL Authentik shows (for example
   `https://auth.example.com/application/o/yukariko/`) — use it exactly,
   trailing slash included.
5. Optional: add a scope mapping that includes the `groups` claim in the
   ID token if you want `allowed_groups` enforcement in Yukariko.
6. Optional: add `https://yukariko.example.com/ui` to the provider's
   redirect URIs so signing out lands back on the dashboard.

Yukariko side:

```yaml
server: {enabled: true, bind: 127.0.0.1:8484}
auth:
  oidc:
    enabled: true
    issuer: https://auth.example.com/application/o/yukariko/
    client_id: yukariko
    client_secret_ref: {file: /etc/yukariko/secrets/oidc-client}
    redirect_base: https://yukariko.example.com
    allowed_groups: [yukariko-admins]   # optional; empty = provider policy decides
    session_ttl: 12h
```

Behavior: unauthenticated dashboard visits redirect to the provider
(authorization code + PKCE); `/api/v1/*` answers JSON 401s for scripts.
Verified logins open a server-side session — cookie flags are HttpOnly,
SameSite=Lax, and `Secure` with a `__Host-` prefix because the external
origin is https. Sessions expire absolutely after `session_ttl` and are
swept at startup; sign out from the dashboard footer. Peer reports
(`/report/v1/events`) are unaffected: they authenticate with their own
HMAC channel and are never behind the browser session. Login initiation is
rate-limited per source address as a backstop. If `allowed_groups` is
non-empty, membership is enforced in Yukariko even if the Authentik
application's policy bindings drift open.

Notes: the provider is discovered lazily at the first login, so an
unreachable Authentik never blocks daemon startup — it surfaces as a clear
sign-in-unavailable page. Yukariko still serves plain HTTP on its bind;
the TLS story belongs to your proxy, and the daemon should stay bound to
loopback or a private interface behind it.

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
- **Bounce a stuck app**: `yukariko restart --app <id>` runs
  `docker compose … restart` with the exact configured project context
  (`-f`, env files, profiles, `-p`, workdir) or `docker restart <name>`
  for a standalone container, honoring the app's `docker:` endpoint. It
  takes the same per-app lock as `update`, so a concurrent deploy is
  refused with a clear message. The deployed SHA/digest is not advanced.
  Health probes never restart containers on their own; this command is
  the operator's explicit bounce. There is no `--all`, no remote restart,
  and no HTTP/dashboard button.

## Scope freeze (non-goals)

Yukariko is not Coolify, Portainer, Traefik, or Kubernetes. There is no
remote execution, web configuration editing, secret management, automatic
health restart, automatic rollback, blue-green deployment, or zero-downtime
guarantee. Feature requests outside `AGENTS.md`'s fence are declined.
