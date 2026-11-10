# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
During v0.x, breaking changes may occur without a major version bump.

## [Unreleased]

### Added

- Business failure alert cards now include the request API path as plain text.
- User-scoped temporary API tokens for AI/machine clients: every admin
  endpoint (except `auth/login`/`auth/logout`, session-only) honors
  `Authorization: Bearer rgt_<64 hex>`. A token authenticates as its user —
  no independent permission scope in v1, the role is resolved live from the
  user row on every request. Expiry is mandatory (1–365 days at creation),
  revocation is instant, secrets are stored as SHA-256 hashes and shown
  exactly once, never logged; creation/revocation are audited
  (`token.create`/`token.revoke`). Invalid/expired/revoked tokens fail with
  `401 admin.token_invalid`; a Bearer header suppresses the session-cookie
  fall-through. Self-service endpoints: `GET/POST /admin/api/v1/auth/tokens`,
  `DELETE /admin/api/v1/auth/tokens/{id}`.
- Machine-readable endpoint discovery: `GET /admin/api/v1/meta/endpoints`
  catalogs every endpoint (group, method, path, auth, summary, params, body
  example) and `GET /admin/api/v1/openapi.json` serves the same catalog as
  OpenAPI 3.1 with `bearerAuth` + `cookieAuth` security schemes.
- The panel gains a 开发者 (Developer) page: 我的凭证 (my tokens — mint and
  revoke), API docs (mirrors the endpoint catalog), and an API console with an
  optional memory-only Bearer field for ad-hoc calls.
- The repository bundles the `routegate-admin` agent skill
  (`skills/routegate-admin/SKILL.md`) teaching AI coding agents to obtain,
  configure, and use a temporary admin API token safely — credential setup
  without exposure, base-URL resolution, request/error conventions,
  optimistic concurrency, and live-configuration safety rules.
- A paste-ready AI bootstrap template for the bundled skill
  (`skills/routegate-admin/AI-PROMPT.md`): the human pastes it into an agent
  session and the agent installs the skill, configures
  `ROUTEGATE_ADMIN_URL`/`ROUTEGATE_ADMIN_TOKEN` together with the human
  (never invented), verifies connectivity via the now-PUBLIC
  `GET /admin/api/v1/meta/endpoints` + `GET /admin/api/v1/openapi.json`
  (no credential needed; the backend change lands via a parallel PR), and
  only then operates routes/policies under the skill's live-configuration
  safety rules.
- The skill documents an IPv6-first network strategy mirroring the platform
  CLI's `--ip-family prefer6` philosophy: try IPv6 first with a short connect
  timeout (`curl -6 --connect-timeout 5 … || curl -4 --connect-timeout 5 …`),
  retry connection errors at most twice then stop and report, never retry
  HTTP 4xx/5xx, and use the public meta/endpoints call as the
  pre-credential connectivity check.
- The bundled skill gains a concept layer: `skills/routegate-admin/ARCHITECTURE.md`
  (what RouteGate is, the three-listener architecture and the
  snapshot/revision consequence, domain concepts with their API surface,
  the design invariants an AI must respect, a goal-oriented capability map,
  and the operational semantics that surprise outsiders); SKILL.md adds an
  "Understand before you write" section directing agents to read it in full
  before any non-trivial task; the AI bootstrap prompt (both zh and en
  blocks) now requires reading SKILL.md AND ARCHITECTURE.md, and the embedded
  panel copy (`internal/adminapi/prompt/AI-PROMPT.md`) is re-synced
  byte-identical under the drift-guard test.
- 90-day retention for admin audit and alert events with hot-query indexes
  (migration 0008); sessions/instances GC and audit listing no longer
  full-scan.
- Route prefixes and policy patterns are validated against the §6.2
  normalization grammar at write time; legacy non-normalized prefixes are
  normalized on load with a log line.
- The admin API answers unknown paths and disallowed methods with the unified
  error envelope instead of Go's plain-text defaults.
- Alert rules can match the route's business fields directly: two new
  condition sources `biz_code` and `biz_message` (alert-only; the shared
  route success-rule grammar keeps status/body) read the per-route
  biz-field extraction with the global fallback (§6.7). An empty extraction
  is "missing" and keeps the owner-confirmed asymmetry — positive matchers
  fail, `not_in`/`neq`/`not_contains` stay vacuously true. Alert firings now
  persist `biz_message` alongside `biz_code` (migration 0013) and the
  events API + panel records table show the text field.

### Changed

- Access-log sinks (stdout and rotating file) are asynchronous bounded queues:
  a slow sink can no longer block request handling; drops are counted on
  `/__metrics` (`routegate_accesslog_dropped_total`). `request_bytes` in
  access records now counts forwarded client bytes on every request (0 for
  early rejects).
- Per-tick side loads (debug/biz/alert settings) are skipped while the config
  revision is unchanged, cutting steady-state DB chatter.
- The admin panel source is split from a single 1,755-line file into per-page
  modules (no behavior change).
- The developer page gains a one-click "copy AI prompt" action backed by the
  new public `GET /admin/api/v1/meta/prompt` endpoint (the embedded
  AI-PROMPT.md template, drift-guarded against the canonical
  `skills/routegate-admin/AI-PROMPT.md`).
- The routegate-admin skill is self-hosted by the gateway: `SKILL.md`,
  `ARCHITECTURE.md` and `AI-PROMPT.md` are anonymously downloadable at
  `GET /admin/api/v1/meta/skill/<file>` (text/markdown, drift-guarded against
  the repository sources), so an AI agent can install the skill from the admin
  host alone; the AI bootstrap template's install steps now download from
  these endpoints instead of requiring a repository checkout.
- Local credential configuration now follows the `.routegate/config.json`
  convention: the file holds
  per-environment `baseUrl` / `apiBasePath` / `auth.tokenEnv` — the token's
  environment-variable NAME, never the token value; git-ignored, preserved
  unknown fields, redacted summaries, and missing-value reporting are part
  of the skill's setup discipline.

### Fixed

- Success verdicts and business-envelope extraction no longer misjudge large
  responses: the bounded 8 KiB verdict capture cuts big bodies mid-value, and
  the whole-document JSON decode then discarded everything — a 200 +
  `common.success` dictionaries response (~16 KB) was recorded as `failure`
  with empty business fields. The shared decode now recovers the top-level
  members completed before the cut (envelope `code`/`message` sit at the body
  start); members cut mid-value stay missing, and syntactically invalid
  bodies still count as non-JSON. Alert body conditions use the same decode
  over the same capture (ADR-0001 §6.7).

## [v0.1.0] - 2026-11-08

### Added
- Graceful drain on SIGTERM (owner-approved ADR-0001 §14 candidate, "建议
  尽早补上"): the shutdown sequence now flips `/__ready` to 503
  immediately (accelerating LB removal), stops accepting new connections,
  and lets in-flight requests finish under `ROUTEGATE_DRAIN_TIMEOUT`
  (default 20s; expiry closes hard). Liveness `/__health` stays 200 during
  drain so the orchestrator does not kill a draining instance, and the
  management/health listeners close after the data plane so probes observe
  the drain. Previously SIGTERM did not stop the data plane at all (the
  listener only died with the process); callers should still retry writes
  with `Idempotency-Key` — long-lived SSE responses are cut at the grace
  boundary.

- Prometheus metrics endpoint (owner request, post-MVP gap): `GET /__metrics`
  on the plaintext health listener renders the text exposition format
  (v0.0.4) with no new dependencies. Exposed groups: build info + instance,
  process uptime and Go runtime gauges, config snapshot state (loaded,
  revision, routes, identities, loaded-at), sync readiness and last success
  timestamp, alert rules/channels loaded (+enabled) and the dispatch-queue
  drop counter, debug capture state and drop counter, and the access-log
  sink drop counter (new `DroppedReporter` on the rotating file sink, which
  previously discarded silently on rotation/write failures). Scraping never
  touches the database; the endpoint exposes only counters and status
  values, no request data or credentials.

- Per-route success verdict (owner request, ADR-0001 §6.7): a composable rule
  over the HTTP status and response-body JSON fields decides whether a
  proxied request counts as success or failure in the audit — no more
  inferring from the status code alone when business failures return 200
  (e.g. `{"code":1}`). Conditions (`status` / `body` dotted path) compare
  against value lists with `in` / `not_in` (a single value is a length-1
  list; JSON numbers and strings compare equal after stringification) and
  combine per rule with `all` (AND, default) or `any` (OR). A missing field
  or non-JSON body fails `in` and passes `not_in` (owner decision). Rules
  live in `routes.success_rule` (migration 0004, route-level with an
  operation scope reserved), are validated on write and at load (malformed
  rules are dropped with a counter, the route still serves), and are edited
  in the panel route drawer with a dynamic condition table. The data plane
  captures the first 8 KiB of the response body only for routes with a rule
  (independent of debug mode) and writes `outcome: success|failure` into the
  access record; gateway-generated early rejects get no verdict, upstream
  failures are judged like any response. The audit access-log tab gains a
  Result column (成功/失败/—) and an outcome filter; the file-sink API
  filters by outcome.

- Deployment & migration guide (docs/DEPLOYMENT.md, owner-approved as the
  migration standard): environment rollout in four phases (gateway first,
  per-service link onboarding without traffic switch, per-service cutover
  with instant rollback), per-service onboarding checklist (identity →
  route → policy → curl link verification), operations notes (audit
  verdicts, debug mode, fail-static), and the APISIX two-layer integration
  standard — one RouteGate identity per APISIX plane (admin/user/webhook),
  the v1 downstream-identity contract (single trusted X-Gateway-User-Id with
  namespace prefixes; configurable pass-through allowlist deferred to v1.x),
  path-rewrite layering rules, upstream mTLS and health-check guidance.
  ADR-0001 §6.8 records the architecture decisions.

- Founding design document as ADR-0001 (architecture, MySQL schema, wildcard
  policy semantics, certificate lifecycle, admin account model, roadmap).
- Repository bootstrap: Apache-2.0 license, Go module skeleton
  (`cmd/routegate` with a `version` subcommand), CI workflow, community files.
- M1 data plane implementation (fresh code per ADR-0001, no migration of the
  predecessor): embedded migrations for the ten-table schema; config sync
  worker (revision probe + forced reload every N ticks + instance heartbeat +
  stale-heartbeat/session GC); data plane with longest-prefix routing,
  weighted round-robin targets, keep-alive + DNS-cache upstreams,
  strip-prefix, request-ID propagation, inbound identity/proxy header
  stripping and gateway assertion headers, trusted end-user-context
  forwarding, SSE-friendly flushing; pinned-certificate mTLS identity with
  fingerprint match, validity window, and URI-SAN name consistency;
  wildcard policy engine (deny > allow > default deny) with segment
  normalization anti-bypass; plaintext health listener (`/__health`,
  `/__ready`, `/__status`); JSON-lines access log sink; `serve` / `migrate` /
  `config bump` / `version` subcommands.
- Quickstart demo: three demo services (two upstreams + one mTLS caller
  workload), demo certificate generation, seed SQL, compose stack, and
  one-shot bring-up script (`scripts/demo-up.sh`).

- M3 admin plane (ADR-0001 §7–§9): Admin API on :8081 with Argon2id
  authentication, server-side sessions (hashed tokens, sliding + absolute
  expiry), two-role RBAC (superadmin writes / user read-only), first-run
  bootstrap with forced password change, login rate limiting and exponential
  lockout, optimistic concurrency (`expected_revision` → 409) and a
  three-in-one write transaction (business row + revision bump + audit).
  Routes/policies/identities/users CRUD, policy tester sharing the live
  evaluation path, PEM validation, server-side key-pair generation with
  one-time private-key return, admin audit and access-audit queries, gateway
  config/instances/snapshot endpoints. React 18 + Vite + Ant Design 5 panel
  (login, forced password change, dashboard with propagation state, routes,
  policy matrix + tester, identities with generate/download, users, audit,
  status) embedded into the binary under the `panel` build tag.

- Route-level operation registry (owner request): each route can register the
  concrete operations it supports, stored in a dedicated `route_operations`
  table (unique per route+method+path; interim JSON column migrated and
  dropped by migration 0003). The routes page maintains operations through a
  per-route drawer with a modal editor; the policy editor offers the selected
  route's registered operations as dropdown options for the path pattern and
  auto-fills the HTTP method; free-form wildcard entry is unchanged.
  Enforcement semantics are unaffected (policies remain the sole gate).

- Route drawer caller-certificate section (owner request): the drawer lists
  the identities allowed to call the route (via its policies) with certificate
  status/expiry and registers new certificates in place — the same identity
  store as the Identities page, exposed as a second entry point. Route form
  targets move from a JSON textarea to a dynamic add/remove table
  (host/port/weight).

- Complete in-memory snapshot observability (owner request): the snapshot now
  carries registered route operations (`snapshot.Operation` nested per route)
  and raw policy source metadata (path pattern, effect, expiry, remark)
  alongside the parsed matcher fields; the loader reads operations in the same
  transaction with deterministic ordering and drops malformed/orphan rows with
  a dedicated counter. `/admin/api/v1/gateway/snapshot` now emits the full
  document — routes with ids/methods/weighted targets/nested operations, all
  loaded policies with effective allow/deny state, and identity metadata
  (no certificate material) — still sourced exclusively from the in-memory
  holder.

- Rotating file access-log sink (owner request): ROUTEGATE_ACCESS_LOG_SINK=file
  now writes JSON lines into daily-segmented files named
  <stem>-<YYYYMMDD>-<seq>.log with a hard per-segment size cap (default
  2 MiB, ROUTEGATE_ACCESS_LOG_MAX_MB) and retention of segments older than
  ROUTEGATE_ACCESS_LOG_RETAIN_DAYS days (default 7, hourly janitor plus a
  startup sweep using the filename-encoded date). Restarts continue today's
  latest segment; unmatched files in the log directory are never touched.

- Panel access-log viewer: the Audit page now has 管理审计 and 访问日志
  tabs. The access-log tab reads rotating file-sink segments through an
  authenticated Admin API endpoint (`/admin/api/v1/logs/access`), newest first,
  with limit and exact caller/decision/route filters; it displays request time,
  caller, route, method, path, decision, matched policy, duration, and upstream.
  When the file sink is disabled, the API returns an explicit configuration note
  instead of pretending the log store is empty.

- Local public-edge demo: added `demo-edge`, a plain-HTTP entrypoint on
  `127.0.0.1:8090` that exposes only `/alpha/*`, `/beta/*`, and local
  `/healthz`, then calls RouteGate over mTLS with the `demo-edge` workload
  identity. External curl/browser callers need no certificate; RouteGate
  still enforces the edge certificate and policies. Added edge user-context
  demonstration via `X-Demo-User-Id`, edge allow/deny/404 seed policies,
  cert generation, scratch image, Compose service, and e2e hints.

- Panel-managed debug settings (owner decision, ADR-0001 §6.6 revised): debug
  logging configuration moves entirely into the settings store
  (`settings.debug_log`) with a new panel 设置 page (nav Settings, superadmin
  writes, optimistic concurrency via expected_revision, audited as
  `debug.settings_updated`). `GET/PUT /admin/api/v1/settings/debug` expose it.
  Instances apply values on their next sync tick — the sink is created/stopped
  and capture caps updated at runtime through an atomic DebugManager, so debug
  mode is now a live toggle with no restart. All `ROUTEGATE_DEBUG_*`
  environment variables are removed; the settings store is the single source
  of truth. The demo seed enables debug logging against the compose log mount.

- Opt-in debug logging (owner request, ADR-0001 §6.6): a separate, default-off
  debug sink records sanitized request/response headers, sanitized cookies,
  and bounded request/response payloads (default 64 KiB per body, base64 for
  binary content) for troubleshooting real traffic. Explicit network-address
  fields (RemoteAddr, Forwarded/X-Forwarded-*, X-Real-IP, client-IP variants,
  upstream addresses) are never recorded; authorization/token/signature
  headers are dropped and credential-looking cookie values are redacted.
  Requires admin TLS at startup (plain-HTTP admin + debug payloads is refused).
  Records go to their own rotating JSON-lines file (daily segments, 2 MiB cap,
  retention janitor) through an asynchronous bounded queue that drops rather
  than blocks. Admin API: `/admin/api/v1/logs/debug` serves metadata to all
  admin users; `/admin/api/v1/logs/debug/{request_id}` serves the full
  sanitized record to superadmin only with `Cache-Control: no-store` and an
  `admin_audit` view event. The panel access-log tab gains a superadmin-only
  调试详情 drawer (lazy-loaded, sanitized-content warning, truncation and
  unavailable markers) in English and Chinese. Ordinary access records gain
  `status`/`request_bytes`/`response_bytes` backfill; response statuses were
  previously always recorded as 0.

### Changed

- Admin TLS removed (owner decision, 2026-11-07): `ROUTEGATE_ADMIN_TLS_CERT/KEY`
  and every related mechanism (TLS admin listener branch, cookie Secure
  attribute, the debug-requires-TLS startup validation) are deleted. The admin
  listener always serves plain HTTP; production deployments are advised to
  restrict it to a management network or front it with a TLS-terminating proxy
  (SECURITY.md updated). The demo panel URL returns to
  http://127.0.0.1:8081/admin/.

- Design revision v0.4.1 (ADR-0001): full architecture review folded in —
  dedicated configurable health listener (default `:8082`) resolving the
  mTLS-vs-probe conflict; generated-certificate default validity reduced to
  90 days; optimistic concurrency on admin writes; periodic force-reload plus
  a `config bump` CLI for break-glass DB edits; upstream keep-alive with DNS
  caching; protocol support matrix; self-loop target validation; stale
  instance-heartbeat cleanup; service-aggregated policy matrix view.
  Owner-declined items recorded with risk disclosure (single DB account, no
  graceful drain in v1).

- Rule JSON editing and global business-field fallback: the route form's
  success-rule editor gains 表单/JSON tabs — the condition list serializes to
  the canonical rule JSON for copying into other routes or applications, and
  pasted JSON maps back into the editor (client-side mapping, backend
  validation on save). The Settings page gains a 业务字段(全局兜底) card:
  global dotted paths for the business status code and message
  (`settings.biz_fields`, `GET/PUT /admin/api/v1/settings/biz`, superadmin
  writes with optimistic concurrency and `biz.settings_updated` audit),
  applied by instances per sync tick. Routes that define their own fields
  win per field; a configured-but-missing route path yields empty rather
  than cross-field guessing.

- Persistent admin session cookie (owner request): the login cookie now
  carries MaxAge aligned with the 24h absolute server-side expiry, so
  closing the browser no longer forces a re-login. The sliding 8h window
  and 24h absolute DB checks are unchanged — the security window does not
  grow; only the browser-side cookie lifetime matches the policy.

- Multiple alert channels with a dedicated panel entry (owner request,
  evolving the previous round): channels become a managed entity in a
  dedicated `alert_channels` table (migration 0006, carrying over the
  interim single-channel settings row). New superadmin-only panel page
  告警渠道 (nav entry): table with masked tokens, create/edit (empty token
  keeps the stored credential), per-channel send-test, delete. Admin API:
  `GET/POST /admin/api/v1/alerts/channels`,
  `PUT/DELETE /alerts/channels/{id}`, `POST /alerts/channels/{id}/test`
  (replaces the single-channel settings endpoints from the previous
  round). Tokens are masked in list responses, redacted in audit rows, and
  never logged; unique names; instances load the channel list per sync
  tick for the future alert-trigger engine.

- Alert rules engine with firing records (owner request): composable
  triggers (alert_rules, migration 0007) scoped to all or selected routes,
  firing into one channel per rule — conditions reuse the route success-rule
  grammar plus an `outcome` source fed by the route's own success verdict;
  combination all (default) / any. No noise suppression or rate limiting in
  v1 (explicit owner decision): every matching request fires through a
  bounded asynchronous queue (drop + counter on overflow, never blocking
  traffic), the webhook send honors HTTPS_PROXY with a 5s timeout, and
  delivery is judged from the response body (Feishu answers HTTP 200 even
  for invalid tokens). Every firing is recorded in alert_events (matched
  conditions, HTTP status, outcome, biz code, sent/send_error bounded) and
  shown on a new superadmin panel page 告警规则 with an events table and
  rule filter. Admin API: `GET/POST /admin/api/v1/alerts/rules`,
  `PUT/DELETE /alerts/rules/{id}`, `GET /alerts/events`.

- Nested alert rules in JSON form (owner request): the alert-rule match
  document upgrades to a recursive node tree — leaves are conditions (same
  grammar), groups combine nested nodes with all/any (depth ≤ 3, ≤ 32
  nodes); legacy flat `{mode, conditions}` documents stay fully compatible
  (wrapped on read). The panel rule editor gains Form/JSON tabs — nested
  documents are edited as JSON with copy/paste and backend validation; the
  panel emits `{mode, children}` documents. Evaluation is recursive in the
  engine with the same leaf semantics.

### Fixed

- Admin sessions expiring instantly on non-UTC MySQL servers (found during
  e2e on a CST server): the Go driver writes DATETIME parameters as UTC
  wall-clock strings while MySQL evaluates `NOW()` in the session time zone
  (default SYSTEM), so `gcExpiredSessions` (`expires_at < NOW()`) deleted
  freshly created sessions within one sync tick — an 8-hour sliding expiry
  sat only ~1s ahead of `NOW()` on a +08:00 server. `store.Open` now pins the
  connection session time zone to UTC (`time_zone='+00:00'` via the DSN)
  unless the DSN already pins one; an explicit non-UTC `loc` without
  `time_zone` is respected but warned about. This also aligns policy-expiry
  checks and heartbeat GC on such servers. Covered by a live regression test
  plus a deterministic control that reproduces the bug without the pin
  (both run only when `ROUTEGATE_TEST_MYSQL_DSN` is provided).
