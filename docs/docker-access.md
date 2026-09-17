# Docker daemon access: privilege and minimization

Yukariko talks to the Docker daemon exclusively through the Docker CLI
(`docker ps`, `docker inspect` for discovery; later issues add `docker
compose` and `git` invocations). There is no Docker SDK and no socket
client. This page explains what that access is worth, and how to keep the
exposure small.

## Daemon access is host-equivalent privilege

Any process that can reach the Docker daemon — through
`/var/run/docker.sock`, a `tcp://` endpoint, or an `ssh://` context — can:

- bind-mount any host path into a container (`-v /:/host`),
- run containers with `--privileged`,
- load kernel modules, reach the host network namespace, and in practice
  act as **root on the machine**.

Docker has no per-user authorization layer on the standard socket: membership
in the `docker` group is effectively passwordless root. Granting a user or a
binary (including Yukariko) access to the daemon therefore grants that
privilege, whether or not the binary ever asks for it.

## Minimizing exposure

- **Run Yukariko under a dedicated local account**, not your personal user
  and not root, with daemon access as its only notable grant.
- **Prefer the local unix socket.** Avoid `tcp://` endpoints; they are hard
  to scope and historically lacked TLS by default. Avoid `ssh://` contexts
  unless you specifically need a remote daemon.
- **Scope socket access to one group.** Create a group (e.g. `yukariko`) and
  add only the service account to it, rather than widening the `docker`
  group.
- **Rootless Docker** removes the root-equivalence for the daemon itself and
  is a good fit for Yukariko's target scenario (a personal deploy agent);
  note that image/volume paths then live under the service user's home.
- **Socket-filtering proxies** (docker-socket-proxy) can restrict the HTTP
  API to a read-only subset for *observation-only* setups. They are **not**
  compatible with Yukariko's deploy pipeline, which needs the Compose API
  subset; if you use one, scope it to exactly the endpoints the deploy
  pipeline documents, and expect breakage otherwise.

## What Yukariko itself does with the access

- Discovery (`internal/docker`) issues only the read verbs `docker ps` and
  `docker inspect`, in argv mode through the command runner — no shell, no
  interpolation. The `Client` interface has no mutating method, so discovery
  cannot start, stop, pull, create, or remove anything.
- Inspect output contains raw environment values. Discovery records
  environment variable **names only**, and the runner driving discovery must
  not persist command output; values never enter results, logs, or storage.
- Deploy-time writes (`docker compose pull/up`, standalone recreation) arrive
  only with the deploy issues (#11–#12) and are executed as explicit,
  logged, argv-based commands built from your configuration — never from
  network input.

Related: [`config.md`](config.md) (secrets are references only), the package
documentation in `internal/docker/docker.go`, and the security fence in
`AGENTS.md`.
