# Security Policy

## Reporting a vulnerability

Do **not** open a public GitHub issue for security vulnerabilities.

Report privately via GitHub's private vulnerability reporting on this
repository (**Security** tab → **Report a vulnerability**). Include a
description, affected version/commit, and reproduction steps. You will
receive an acknowledgment within 3 business days.

## Scope

Of particular interest:

- Bypasses of the deny-by-default authorization evaluation (ADR-0001 §6.1),
  including path-normalization tricks against wildcard patterns (§6.2).
- Workload identity weaknesses: certificate impersonation, SAN/name mismatch
  handling, revocation windows, private-key exposure in the generation flow
  (§6.3).
- Admin panel and session weaknesses (§8).
- Header smuggling that could forge end-user context (§6.3.4).

## Hard guarantees in the design

- The gateway and its database never store service private keys.
- Access audit records never contain credentials, tokens, or bodies.
- There is no auth-free operating mode.

## Debug logging (opt-in, sensitive by nature)

RouteGate can optionally capture sanitized headers, cookies, and bounded
request/response payloads for troubleshooting (panel-managed via
`settings.debug_log`, ADR-0001 §6.6). This is a deliberate exception to "no
bodies in logs" and must be treated as a sensitive artifact:

- **Default off; panel-managed.** The only configuration source is the
  `settings.debug_log` row, maintained on the panel Settings page
  (superadmin-only writes, optimistic concurrency, audited). Instances apply
  changes on their next sync tick. There are no debug environment variables.
  The admin listener serves plain HTTP by owner decision — protect it at the
  deployment layer (management network or a TLS-terminating front proxy) so
  debug payload detail is not exposed to untrusted networks.
- **What is excluded.** No explicit network-address fields are recorded
  (`RemoteAddr`, `Forwarded`/`X-Forwarded-*`, `X-Real-IP`,
  `X-Client-IP`/`True-Client-IP`, upstream addresses). Authorization,
  proxy-authorization, API-key/token/signature/CSRF headers are dropped
  entirely; cookies whose names look like session/token/auth/secret/csrf/
  password/key material are recorded as `<redacted>`. Body or ordinary header
  *values* may still naturally contain IP strings or secrets that do not match
  these rules — the sanitizer is rule-based, not a full-content scanner.
- **Bounded.** Payload captures are capped per request (default 64 KiB body /
  32 KiB headers, configurable) and the sink is an asynchronous bounded queue:
  a full queue drops records instead of slowing traffic.
- **Access control.** The metadata list endpoint is available to admin users;
  the payload detail endpoint is superadmin-only, served with
  `Cache-Control: no-store`, and every view writes an `admin_audit` row that
  contains only the request ID and actor — never the payload.
- **Operational guidance.** Do not leave debug logging on in production; mount
  the debug log directory with restrictive permissions (the sink writes
  `0600` files, the directory `0755` — tighten it to your needs), include the
  directory in your retention/secret-handling policies, and delete segments
  when the investigation is done. Anyone with read access to the file can see
  sanitized but still-sensitive traffic content.

## v0.1.0 security self-review (2026-11-08)

Pre-release self-check against ADR-0001; findings are statements of current
behavior, not marketing:

- **Identity & authorization.** mTLS (pinned-certificate fingerprints with
  SAN consistency) or JWT per `client_auth` mode; no auth-free mode exists.
  Policy evaluation is deny-by-default with segment-normalization
  anti-bypass, fuzz-tested (§6.1–6.2). Fail-closed on missing snapshot;
  fail-static on DB outage (stale-config window documented, §14).
- **Hot path resource safety.** Access log, debug log, and alert sinks are
  bounded asynchronous queues that drop and count instead of ever blocking
  traffic; the counters are exposed via `/__metrics` (§10). Request-body
  capture for verdict rules is 8 KiB and only on routes with a rule.
- **Credentials.** Alert webhook tokens are masked on read, redacted in
  audit rows, never logged, and send-test responses never echo them;
  empty-token edits keep the stored credential (§9.2). Admin one-time
  bootstrap passwords appear only in the container log and must be rotated
  at first login (§8.1). DSNs and tokens flow only through environment
  variables; local credential files such as `.routegate/config.json` are
  git-ignored by convention.
- **Admin plane.** Two-level RBAC enforced server-side (the panel login
  response is never trusted for authorization), sessions with an 8-hour
  sliding window and 24-hour absolute cap (cookie persistence aligned to
  the absolute cap), all mutations audited (§7–8). The listener is plain
  HTTP by owner decision — deployments must restrict it to a management
  network or front it with TLS.
- **Health listener.** Plaintext by design (probes cannot present client
  certificates); carries only read-only status endpoints and
  counters (`/__metrics` — no request data, no credentials, no DB I/O on
  scrape).
- **Debug logging.** Opt-in, panel-managed, superadmin-only payload detail,
  rule-based sanitizer; see the section above for the full contract.
- **Shutdown.** SIGTERM drains under `ROUTEGATE_DRAIN_TIMEOUT` with
  `/__ready` → 503 immediately; in-flight requests are still cut at the
  grace boundary and the alert/debug queues are not drained (drops visible
  in `/__metrics`) — callers must retry writes idempotently (§14).
- **Accepted residual risks for v0.1.0.** No alert noise suppression or
  rate limiting (every matching request fires); no per-route request
  metrics yet; the alert queue is not replayed after restart. Each is
  recorded in ADR-0001 with an owner decision or a follow-up candidate.

## Supported versions

Security fixes target the latest release. During v0.x, that means the most
recent tag on `master`.
