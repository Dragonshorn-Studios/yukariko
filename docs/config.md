# Yukariko configuration reference

Yukariko configuration is a single YAML file passed with `--config`. It is
**declarative intent**, not state: host Docker, Compose files, Git
repositories, and directories remain the source of truth. Yukariko never
writes back your settings, never guesses missing paths or names, and never
stores secret values in this file.

A complete worked example lives at [`examples/yukariko.yaml`](../examples/yukariko.yaml).

## Loading rules

- `schema_version` is required. The current version is **1**. Any other value
  is rejected with a migration hint; forward-only migration steps for future
  versions will be documented in this file.
- **Unknown fields are rejected** anywhere in the document, as are duplicate
  YAML keys, multiple documents, and empty files. This catches typos such as
  `secrect_ref` instead of silently ignoring them.
- Type errors, duration errors, and validation findings are reported together
  with a configuration path, for example:

  ```text
  apps[1].source.git.branch: is required when source.mode is "git"
  ```

- Loading is side-effect free: no files are read beyond the config itself, no
  commands run, no network contacted, and no secret resolved.
- App IDs must match `[a-z0-9][a-z0-9-]{0,62}` and be unique. Two apps must
  also never deploy the same target (same Compose work dir/project/files or
  same standalone container name), so per-app deploy locks always cover a
  whole target.

## Durations

Durations are strings parsed by Go's `time.ParseDuration`: `"30s"`, `"5m"`,
`"1h30m"`. Bare integers are rejected so units are never ambiguous. Zero is
not a valid duration; omit optional fields instead — omission applies the
documented default.

## Secrets

Secrets are **references only**. There is no way to write a literal secret
value into valid Yukariko configuration; unknown-field rejection enforces
this. References resolve lazily, at the moment of use, and are redacted in
logs and errors (a reference always renders as `secretRef(env:NAME)` or
`secretRef(file:/path)`):

```yaml
secret_ref:
  env: GHOST_DB_PASSWORD      # exactly one of env or file
secret_ref:
  file: /etc/yukariko/secrets/ghost-db   # absolute path, trimmed
```

Env values are used verbatim; file contents are trimmed of surrounding
whitespace (secret files conventionally end with a newline).

Wherever a plain non-secret value is accepted (`value:`), a `secret_ref:`
may be given instead — never both.

## Top-level fields

| Field | Type | Default | Description |
|---|---|---|---|
| `schema_version` | int | required | Config schema version; `1` |
| `server` | table | see below | Read-only HTTP API/dashboard (issues #15/#16) |
| `server.enabled` | bool | `false` | Serve the read-only API when `yukariko run` is active |
| `server.bind` | string | `127.0.0.1:8484` | Listen address; deliberately loopback-only by default |
| `reporting` | table | disabled | Optional peer status reporting (issues #17/#18) |
| `retention.events_days` | int | `30` | Event history retention (days) |
| `retention.health_days` | int | `14` | Health sample retention (days) |
| `retention.deployments_days` | int | `365` | Deployment history retention (days) |
| `limits.command_output_bytes` | int | `65536` | Bounded captured command output per stream |
| `docker` | table | unset | Machine-wide Docker endpoint: exactly one of `docker.context` or `docker.host` (issue #42); per-app `docker` overrides it |

## Applications (`apps`)

| Field | Type | Default | Description |
|---|---|---|---|
| `id` | string | required | Stable identity used by store, locks, and reports |
| `display_name` | string | `id` | Human label for status and dashboard |
| `enabled` | bool | `true` | Set `false` to keep the app configured but unscheduled |
| `interval` | duration | `5m` | How often the source is checked for changes |
| `timeout` | duration | `10m` | Overall budget for one update attempt |
| `retry.base` / `retry.max` | duration | `30s` / `30m` | Bounded exponential backoff after transient failures |
| `docker` | table | inherit | Per-app Docker endpoint override; same shape as the top-level `docker` block |

### `docker` — which daemon an app talks to

Rootless Docker and other local daemons are selected per app (or machine-wide) with exactly one of `context` or `host`:

```yaml
docker:
  host: unix:///run/user/1000/docker.sock   # rootless daemon of uid 1000
  # context: rootless                       # or a named docker context
```

- Yukariko stays CLI-only: the endpoint becomes `docker --context …` / `docker -H …` argv on every discovery, deploy, preflight, and health invocation. User-owned argv (compose `steps`, health `command` probes) instead receives `DOCKER_HOST`/`DOCKER_CONTEXT` in its environment — write the flags yourself there if you prefer.
- `learn` scans every local daemon — Docker contexts plus rootless sockets under `/run/user/<uid>/docker.sock`, which need no context (remote `ssh://`/`tcp://` endpoints are excluded — Yukariko is a local agent) — and stamps non-default candidates with the resolved `host` URL, since context definitions are per-user and the service user may not share them. The endpoint field is learn-owned discovered reality: re-running learn refreshes it.
- Under systemd, `ProtectHome=yes` blocks `/run/user` entirely: see `docs/operations.md` ("Git-source apps under systemd" and rootless notes) for the required drop-in and `loginctl enable-linger`.

### `source` — where changes come from

Exactly one mode; the matching block is required and the other must be absent.

```yaml
source:
  mode: git            # or: registry
  git:
    dir: /srv/ghost    # absolute path to the existing worktree
    branch: main       # required
    remote: origin     # optional, default "origin"
```

```yaml
source:
  mode: registry
  registry:
    images:
      - ref: ghcr.io/example/wiki:latest
        platform: linux/arm64   # optional; digest refs (name@sha256:...) are immutable
```

- `mode: git` polls the configured remote branch using the **host's existing
  Git credentials**; Yukariko never persists or logs them.
- `mode: registry` tracks one or more image references via registry manifest
  digests (issue #10). Docker's configured credential helpers are used; a
  successful lookup is never treated as a deployment.

### `deploy` — how updates are applied

Exactly one mode; the matching block is required and the other must be absent.

```yaml
deploy:
  mode: compose
  compose:
    work_dir: /srv/ghost        # absolute; never guessed
    files: [compose.yaml]       # at least one; passed verbatim as -f
    env_files: [.env]           # optional; passed verbatim as --env-file
    profiles: [full]            # optional; passed verbatim as --profile
    project_name: ghost         # optional; passed verbatim as -p
```

```yaml
deploy:
  mode: standalone      # requires source.mode: registry
  standalone:
    image: louislam/uptime-kuma:1
    name: uptime-kuma
    entrypoint: ["/docker-entrypoint.sh"]   # optional
    command: ["npm", "run", "server"]       # optional
    env:                    # value (non-secret) or secret_ref, never both
      - {name: TZ, value: Europe/Berlin}
      - {name: KUMA_SECRET, secret_ref: {env: KUMA_SECRET}}
    binds: ["/srv/kuma/data:/app/data:rw"]
    ports: ["127.0.0.1:3001:3001/tcp"]
    networks: [kuma-net]
    restart: unless-stopped # no | always | unless-stopped | on-failure
    labels: {yukariko.managed: "true"}
    user: "1000:1000"
    work_dir: /app
    health_check: {test: ["CMD", "node", "health.js"], interval: 30s, timeout: 5s, retries: 3}
```

Every Compose invocation uses the configured work dir, files, env files,
profiles, and project name **verbatim**. Yukariko never reconstructs ports,
networks, volumes, or environment for Compose apps. Standalone recreation
(issue #12) replays exactly the supported options above and refuses to touch
the container when a field would be silently lost.

Standalone `ports` entries must pin the host port explicitly
(`"127.0.0.1:3001:3001"`): an ephemeral host port would make the recreated
container non-reproducible, so it is rejected rather than guessed.

**Declarative vs discovered values.** Everything in this file is declarative
state: written by hand or explicitly accepted during import. Values discovered
from Docker inspection enter configuration only through `yukariko learn`
(issue #7), which refuses to write unresolved guesses; after import they are
ordinary declarative values, indistinguishable from hand-written ones.

### `steps` — optional hook commands

```yaml
steps:
  pre:    [{name: backup, command: [/usr/local/bin/ghost-backup.sh], dir: /srv/ghost, timeout: 2m}]
  deploy: []   # empty uses the documented safe default for the mode
  post:   [{name: notify, command: ["/usr/local/bin/notify.sh", "deployed"]}]
```

Commands are argv lists executed by the controlled runner (issue #4) — no
shell interpretation. `shell: true` on a step opts into shell execution and
is documented as a deliberate risk. All steps are optional.

!!! **Stateful applications and major updates: back up first.** A Compose
deployment runs `pull` (registry apps) and `up -d --wait` in place. For
stateful applications — databases, Ghost, anything with schema migrations —
a major-version update can migrate data irreversibly, and Yukariko has no
rollback path by design: it never redeploys an older version automatically.
Use a `pre` step (as in the Ghost example above) to take a backup before
every deploy, and prefer pinned versions you have reviewed over floating
tags for such apps.

### `health` — post-deploy checks and monitoring

```yaml
health:
  required: true            # failed required checks fail the deployment
  interval: 30s             # periodic monitoring cadence
  http:
    url: http://127.0.0.1:2368/
    timeout: 10s
    status: [200, 399]      # inclusive accepted range
    headers:                # value or secret_ref
      - {name: X-Api-Key, secret_ref: {file: /etc/yukariko/secrets/api-key}}
  docker:
    required: true          # require the container's Docker health to be good
  command: ["/usr/local/bin/verify.sh"]   # optional extra local check
```

All sub-checks are optional. Health state never triggers restarts or
rollbacks; a failed **required** check only fails the deployment checkpoint
(issue #13).

## Reporting

```yaml
reporting:
  outbound:
    enabled: false
    url: https://peer.example:8443/report/v1/events
    host_id: this-host
    secret_ref: {env: YUKARIKO_REPORT_SECRET}
    heartbeat_interval: 1m
  inbound:
    enabled: false
    require_tls: true          # http allowed only to loopback
    max_body_bytes: 1048576    # 1 KiB..10 MiB
    clock_skew: 5m
    replay_window: 24h
    rate_limit: {events: 60, per: 1m}
    hosts:
      - id: garage-pi
        keys:
          - {key_id: k1, secret_ref: {file: /etc/yukariko/secrets/garage-pi-k1}}
```

Reports carry status only — never commands. Inbound host IDs are an
allowlist: removing a host (or a key) revokes it; multiple keys support
rotation. Details land with issues #17/#18; this schema is stable now so
configurations do not churn. Inbound reporting shares the `server` listener:
a receiver host must also set `server.enabled: true` (and `server.bind` to
a reachable address) — `yukariko run` refuses to start an inbound listener
with no configured address rather than bind a random port.

## Authentication

```yaml
auth:
  oidc:
    enabled: false
    issuer: https://auth.example.com/application/o/yukariko/
    client_id: yukariko
    client_secret_ref: {env: YUKARIKO_OIDC_CLIENT_SECRET}
    redirect_base: https://yukariko.example.com   # external origin, no path
    scopes: [openid, profile, email]              # default when unset
    allowed_groups: []                            # optional; empty = provider policy decides
    session_ttl: 12h                              # 1m..720h
```

Optional but strong (issue #61). When enabled, the dashboard (`/ui`) and the
read-only API (`/api/v1`) sit behind an OpenID Connect login: authorization
code + PKCE, ID tokens verified in-binary (signature, issuer, audience,
expiry via go-oidc; nonce compared by Yukariko against the single-use
login state). Any compliant provider works; Authentik is the
documented instance — see the operations guide for provider setup and the
reverse-proxy TLS layout. Sessions are server-side rows in the data dir; the
cookie carries only a random token whose hash is stored, so a database copy
cannot resurrect sessions. `redirect_base` is the origin browsers actually
reach (the redirect URI is `<redirect_base>/auth/callback`) and must be
https like the issuer — plain http is a loopback test affordance.

`allowed_groups` is defense in depth: when non-empty, Yukariko itself
default-denies any verified login whose `groups` claim does not intersect
the list (a missing claim satisfies nothing), regardless of how the
provider's application policy is bound. `client_secret_ref` follows the
secret rules above (env or file, resolved only at token exchange, never
logged or stored). Enabling the gate requires `server.enabled: true` —
`yukariko run` refuses a half-configured listener. The peer report channel
(`/report/v1/events`) is never session-gated; it keeps its own HMAC
authentication.

```yaml
auth:
  api_keys:
    enabled: false
```

API keys (issue #64) are machine credentials for the read-only API,
managed entirely through the CLI — `yukariko apikey create`, `apikey list`,
`apikey revoke` — and stored by SHA-256 hash only; the bearer token is
printed exactly once at creation and never persisted, logged, or written to
YAML. Enabling the feature is fail-closed in the same way the OIDC gate is:
`auth.api_keys.enabled` requires `server.enabled`, and every `/api/v1`
request must then carry a valid key (or, when OIDC is also configured, a
session cookie). A presented-but-invalid key is rejected even when a valid
session exists — credentials that fail never fall through. Revocation is a
CLI command away and takes effect immediately (validation hits the store;
nothing is cached). The dashboard (`/ui`) is never unlocked by API keys, and
`/report/v1/events` keeps its HMAC channel regardless. With the feature off
the HTTP surface behaves exactly as before.

## Webhooks

```yaml
webhooks:
  - name: amadeus
    url: https://amadeus.example.com/hooks/yukariko
    secret_ref: {env: AMADEUS_HOOK_SECRET}   # optional HMAC signing key
    timeout: 15s                             # per attempt, 1s..60s
    headers:                                 # optional static headers
      - {name: X-Tenant, value: ops}
      - {name: Authorization, secret_ref: {file: /etc/yukariko/secrets/hook-auth}}
```

Outbound deployment notifications (issue #65). Every configured target
receives a signed `POST` (JSON) shortly after any app's deployment succeeds
or fails — nothing else triggers a delivery, and no event carries commands.
The body is a bounded, versioned state description:

```json
{
  "version": 1,
  "event": "deployment.succeeded",
  "host": "docker01",
  "app": "web",
  "deployment": {
    "id": "…", "cause": "scheduled",
    "from_version": "…", "to_version": "…",
    "status": "succeeded", "started_at": "…", "ended_at": "…"
  },
  "detail": "pull; up -d --wait",
  "time": "2026-09-28T12:00:00Z"
}
```

Delivery is at-least-once from a durable queue in the data dir: the attempt
is persisted before the network call, so a crash mid-flight is redelivered
on restart. Failures back off exponentially with jitter; a receiver
`Retry-After` wins when longer; after 8 attempts the delivery is abandoned
with its final error kept for diagnosis (a receiver `410` retires it
early). Enqueue and delivery failures never affect the deployment itself —
the only coupling to the deploy path is a local insert. Removing a target
from the configuration retires its queued deliveries at the next drain.

When `secret_ref` is set, deliveries carry `X-Yukariko-Signature`:
`v1=hex(HMAC-SHA256(key, "<unix-seconds>." + sha256hex(body)))` plus
`X-Yukariko-Timestamp` (the same unix seconds), `X-Yukariko-Event`, and
`X-Yukariko-Delivery` (a stable per-delivery id, useful as an idempotency
key). The secret is a reference only, resolved at send time, never logged
or stored. URLs must be https (plain http is a loopback test affordance),
carry no userinfo, and names must be unique. Targets are operator-trusted
endpoints: registering one asks Yukariko to POST state there, nothing more.

## Validation summary

Rejected with path-aware errors: unknown/duplicate fields, unsupported or
missing `schema_version`, invalid durations, missing mode-specific blocks,
relative work dirs/paths, duplicate app IDs, duplicate step names within a
list, two apps sharing a deploy target, invalid image refs/ports/binds,
bad probe status ranges, literal secrets (they are not representable),
reporting configuration that enables itself without the required identity
and key references, an `auth.oidc` section that enables itself without
`server.enabled`, an issuer, a client id, a secret reference, or an external
`redirect_base` (or that carries non-https origins off loopback, origins
with a path, scopes without `openid`, a `session_ttl` outside 1m–720h, or
empty group names), `auth.api_keys` enabled without `server.enabled`,
webhooks with empty/duplicate names, non-https URLs off loopback, userinfo
in a URL, timeouts outside 1s–60s, or headers without exactly one of a
plain value or a secret reference, and
Docker endpoints setting both `context` and `host`
or carrying a host without a `unix://`, `tcp://`, `ssh://`, or `npipe://`
scheme.
