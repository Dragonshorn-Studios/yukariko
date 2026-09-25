# Security model and checklist

Yukariko's security posture in one line: **a local single binary that
observes Docker/Git/registries and runs a fixed set of argv commands from
user-owned configuration; everything on the network is read-only or
state-only.**

## Threat model

| Asset | Threat | Mitigation |
|---|---|---|
| Host (root-equivalent) | Docker daemon access is root-equivalent; a compromised Yukariko can deploy anything the daemon allows | Dedicated service account; unix socket over tcp; scoping documented in docs/docker-access.md; no shell anywhere — every command is argv through the controlled runner |
| Secrets (env, files, Docker config) | Leakage into YAML, SQLite, logs, HTTP responses | Secrets are `secret_ref` indirection only, resolved at use and dropped; discovery records env names, never values; remote-report URLs are credential-stripped; redaction runs over all runner output |
| Config file | Hand-editing mistakes, secret literals | Strict YAML (unknown fields rejected), path-aware validation, `secret_ref` makes literals unrepresentable; generated documents re-validate before any write |
| Registry | Malicious/lying registry serving wrong content | Manifest bodies are digest-verified against `Docker-Content-Digest`; token realms must be https or loopback — injected challenges cannot redirect credentials |
| Git | History destruction, credential leakage | Fast-forward-only merges; `reset --hard` and lossy commands are forbidden by policy and absent from the code; divergence blocks with reasons; URLs sanitized |
| Peer reports | Forged, replayed, or oversized reports; report-driven execution | HMAC-SHA256 over the exact body + timestamp + IDs, constant-time compare, per-host allowlisted keys with rotation, durable event-ID dedup, clock window, body-size cap, per-host rate limits, closed set of state-only report types — reports cannot name commands |
| HTTP API + dashboard | Accidental exposure, mutation via web | GET/HEAD-only routes (matrix-tested), no forms or scripts, security headers, loopback bind by default, exposure documented as an operator decision |
| Published dashboard/API (optional OIDC, #61) | Session theft, token forgery, login CSRF/replay, open redirect, provider spoofing | Authorization code + PKCE (S256); ID tokens verified in-binary via go-oidc (JWKS signature with kid rotation, issuer, audience, expiry, nonce) with RS256/ES256 allowlisted — never a hand-rolled verifier and never reverse-proxy headers; `state`/`nonce` are single-use server-side rows with a 10-minute TTL; sessions are server-side rows keyed by a SHA-256-hashed 256-bit cookie token (HttpOnly, SameSite=Lax, Secure + `__Host-` prefix on https origins) — a database copy resurrects nothing; post-login redirect is local-only; optional `allowed_groups` enforces a default-deny membership check in Yukariko itself; issuer and external origin must be https (loopback http for tests); the client secret resolves only at token exchange |
| Supply chain | Third-party assets/dependencies | Stdlib-first (five direct deps: cobra, yaml.v3, modernc sqlite, go-oidc, x/oauth2 — the last two are the vetted OIDC relying-party pair, chosen deliberately over hand-rolling auth verification); original assets only, inventoried in docs/ASSETS.md |

## Security checklist (release review)

- [x] No secret literals in the repository (scanned by
      `internal/e2e` compliance tests; fixtures use obviously fake keys).
- [x] No third-party or copyrighted theme assets; `docs/ASSETS.md`
      inventories every asset as original.
- [x] All host commands are argv-based through `internal/runner` (no
      shell unless a user opts in per step, documented as a risk).
- [x] Discovery and reporting paths carry no secret values (env names
      only; URLs credential-stripped; regression tests pin this).
- [x] HTTP API is GET/HEAD-only with security headers and bounded reads;
      the only write endpoint is the signed, rate-limited, replay-protected
      reporting receiver on a separate path.
- [x] The optional OIDC gate fails closed (enabled requires `server.enabled`
      and complete identity/origin/secret configuration), never gates the
      HMAC report channel, and stores only hashed session tokens with a
      safe claims subset — verified by table tests against a fake provider
      covering replay, nonce/audience/issuer/expiry violations, unknown
      signing keys, PKCE, group policy, cookie flags, and rate limits.
- [x] Deploy checkpoints advance only inside a store transaction after
      required health checks; cancellation cannot record success.
- [x] Docker daemon access is documented as root-equivalent
      (docs/docker-access.md) with minimization steps.

## Residual risks (accepted, documented)

- The service account's Docker access is root-equivalent regardless of
  Yukariko's own logic; compromise of that account is host compromise.
- Outbound reporting signs with a shared symmetric key per peer pair;
  asymmetric signatures are out of scope for the MVP.
- Insecure (plain-http) non-loopback registries are unsupported rather
  than half-supported.
- Sign-out is a GET link (the dashboard stays form-free): a cross-site
  forced logout is an annoyance, not exposure, and RP-initiated logout
  depends on the provider honoring `post_logout_redirect_uri`.
- Yukariko's login rate limit keys on the transport peer, not a forwarded
  header: behind a reverse proxy all browsers share one bucket, which
  still bounds total volume; trusting `X-Forwarded-For` was deliberately
  rejected.
- The OIDC gate authenticates reads only. Even a verified session can
  observe, never mutate — there is no mutation route anywhere.
