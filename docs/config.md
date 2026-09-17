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

## Applications (`apps`)

| Field | Type | Default | Description |
|---|---|---|---|
| `id` | string | required | Stable identity used by store, locks, and reports |
| `display_name` | string | `id` | Human label for status and dashboard |
| `enabled` | bool | `true` | Set `false` to keep the app configured but unscheduled |
| `interval` | duration | `5m` | How often the source is checked for changes |
| `timeout` | duration | `10m` | Overall budget for one update attempt |
| `retry.base` / `retry.max` | duration | `30s` / `30m` | Bounded exponential backoff after transient failures |

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
configurations do not churn.

## Validation summary

Rejected with path-aware errors: unknown/duplicate fields, unsupported or
missing `schema_version`, invalid durations, missing mode-specific blocks,
relative work dirs/paths, duplicate app IDs, duplicate step names within a
list, two apps sharing a deploy target, invalid image refs/ports/binds,
bad probe status ranges, literal secrets (they are not representable),
and reporting configuration that enables itself without the required
identity and key references.
