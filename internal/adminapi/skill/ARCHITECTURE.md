# RouteGate — architecture, design philosophy, and capability map

Agent-facing primer for the `routegate-admin` skill. SKILL.md teaches you HOW
to call the API; this document teaches you WHAT the system is and WHY it is
designed the way it is. Read it IN FULL before any non-trivial task — before
your first create/update/delete, before policy work, and whenever a 409 or an
unfamiliar concept appears. Reading this is cheap; a blind write changes
production traffic. Distilled from `docs/adr/0001-routegate-design.md` and
verified against the code under `internal/`.

## 1. What RouteGate is

RouteGate is a MySQL-backed service-to-service gateway for internal east-west
traffic (including the edge-gateway-to-backend hop) — the single enforcement
point through which ALL service-to-service calls flow:

- Services authenticate with **mTLS workload identity** — client certificates
  pinned per service by SHA-256 fingerprint (hub-and-spoke: each service
  maintains only its trust relationship with the gateway, never with peers).
- Every request is authorized against **default-deny** policies — no matching
  allow rule means 403. There is no open mode anywhere.
- Every decision (allow and deny) and every admin mutation is audited.

The ADMIN API you are calling manages that configuration; a separate DATA
plane enforces it — a write here edits the source of truth that live gateways
serve traffic from, so treat every write as a production-traffic change.

## 2. Architecture in one picture

One binary (`routegate`), three listeners:

```text
  callers (east-west + edge gateway)             admin network
  service A ──cert:service-a──┐                  browser ──► admin panel (embedded SPA)
  service B ──cert:service-b──┤                                 │ same origin
  edge-gw  ──cert:edge-gw─────┘                                 ▼
                       │                     ADMIN PLANE :8081 (panel + /admin/api/v1)
                       ▼                            │  writeTx: business row
             ┌───────────────────┐                  │  + config_revision bump
   mTLS      │  DATA PLANE :8080 │                  │  + admin_audit row
   required  │  in-memory        │    sync worker   ▼
  ──────────►│  snapshot         │◄────────────► MySQL (sole source of truth:
             └─────────┬─────────┘  read-only     settings/routes/policies/identities/
                       │            poll per      users/sessions/admin_audit/instances)
                       │            tick          alert_channels/alert_rules
                       ▼          (default 60s;
              upstream services    forced reload every N ticks)
  HEALTH LISTENER :8082 (plaintext, NO certs):
  /__health  /__ready  /__status  /__metrics — LBs and orchestrators probe here
```

- **Data plane (:8080)**: requires client certificates, serves requests purely
  from the in-memory snapshot — never MySQL.
- **Admin plane (:8081)**: web panel plus the REST API this skill uses; the
  only component that writes MySQL.
- **Health listener (:8082)**: plaintext HTTP, no certificate requirement (a
  TLS-forced listener would kill certificate-less LB/K8s probes at
  handshake). `/__ready` returns 503 when the snapshot is stale, and 503s
  immediately on SIGTERM so LBs pull the instance during drain.

**The KEY consequence: the data plane never touches MySQL.** Your writes
change MySQL and bump `config_revision`; each instance's sync worker picks the
change up on its next tick (default 60s, `ROUTEGATE_CONFIG_SYNC_INTERVAL`) or
on a forced reload every N ticks (default 10, self-healing for direct-DB
edits). Nothing you write "pushes" to instances. This is why every mutating
call requires `expected_revision` and why the 409 flow exists: the revision is
the sync signal, and concurrent writers are arbitrated through it.

## 3. Domain concepts

The vocabulary. Each concept lists its admin-API surface (all paths under
`/admin/api/v1`).

**Identity (workload identity)** — a service. It authenticates with a client
certificate pinned by SHA-256 fingerprint (`key_id`); the caller NAME (the
identity name, bound to the certificate's URI SAN
`https://routegate.local/identities/<name>`) is what policies reference. The
gateway stores only public material — never a private key. Identity ≠ human
user: human admins are `users` (panel sessions and API tokens); services are
`identities`. API: `GET/POST /identities`, `PUT/DELETE /identities/{id}`,
`POST /identities/{id}/revoke`, `POST /identities/validate` (PEM precheck),
`POST /identities/generate` (private key returned exactly once; the panel's
browser-side generation is preferred).

**Route** — a normalized path prefix (`/svc-a/v1`) mapped to a named service
with weighted targets `[{"host","port","weight"}]` (round-robin), optional
`strip_prefix`, an optional method set, and optional verdict/biz-field config.
Matching is longest-prefix-first; a more specific prefix whose method set
excludes the request falls back to shorter prefixes, and 404 `route_not_found`
only when nothing matches. Prefixes must be normalized (no trailing slash, no
dot segments) and contain no wildcards; targets that point back at the
gateway's own listener are rejected (self-loop protection). Deleting a route
cascades: its policies and registered operations die with it. API:
`GET/POST /routes`, `PUT/DELETE /routes/{id}`,
`GET/POST /routes/{id}/operations`, `PUT/DELETE /operations/{id}`,
`GET /routes/{id}/callers` (who is authorized here and whether their certs
are registered/valid).

**Policy** — an allow/deny rule over `(route, caller, method, path_pattern)`
with optional `expires_at`, anchored to a route. The pattern applies to the
path AFTER prefix stripping: `*` matches exactly one segment, `**` (last
segment only) matches one or more, `/**` means the whole route. Evaluation
order is fixed: **deny beats allow, and no matching allow = 403 (default
deny)**. Wide rules (`caller='*'` or `pattern='/**'`) are forced to carry
`expires_at`; expired rules stop being loaded; the caller must be an active
identity (verified at write time). Pattern normalization (percent-decode,
dot-segment removal, trailing-slash) is applied identically to stored
patterns and to request paths — that is the anti-bypass mechanism, so never
try to "fix" a pattern to a non-normalized form. API: `GET/POST /policies`,
`PUT/DELETE /policies/{id}`, `GET /policies/expiring`,
`POST /policies/test` (dry-run: same evaluation path as the live data plane,
against the current snapshot).

**Success verdict (ADR §6.7)** — per-route rules (`success_rule`) that
classify a proxied response as business success or failure by inspecting the
HTTP status and/or JSON body fields (dotted paths) with `in`/`not_in`
comparisons, combined with `all` (default) or `any`. This exists because
business failures often return HTTP 200 with an error code in the body. The
result feeds the access record's `outcome` (and `biz_code`/`biz_message` via
route-level or global biz-field paths) and alert-rule conditions. Only routes
WITH a rule pay the cost: the gateway captures the first 8 KiB of the
response body there. Missing body field: `in` judges failure, `not_in` judges
success. The gateway's own 401/403/404 are not judged; an upstream 502/504 is
judged (typically failure). Set on the route payload.

**Alert rules & channels** — outbound Feishu/Lark webhook cards fired by
composable conditions (sources: `status`, `outcome`, body fields; nested
condition groups with `all`/`any`). Rules are scoped to all routes or a
route list, each bound to one channel; there is no rate-limiting or
deduplication — every matching request fires. Delivery goes through a bounded
async queue (drops rather than blocks); every firing is recorded in
`alert_events` with `sent`/`send_error` (Feishu returns 200 even for invalid
tokens, so delivery is judged from the response body). Channel webhook tokens
are credentials: masked in lists, an empty token on update keeps the stored
one, never logged. API: `GET/POST /alerts/channels`,
`PUT/DELETE /alerts/channels/{id}`, `POST /alerts/channels/{id}/test`,
`GET/POST /alerts/rules`, `PUT/DELETE /alerts/rules/{id}`,
`GET /alerts/events`.

**Admin users & API tokens** — two-level RBAC (`superadmin`/`user`); all
config writes are superadmin-only. A temporary Bearer token (`rgt_` + 64
hex) authenticates AS its user — no independent scope; the role is resolved
live from the user row on every request, so disabling the user takes effect
immediately. Raw secrets are shown exactly once, stored only as SHA-256
hashes; expiry is mandatory (1–365 days). API: `GET/POST /auth/tokens`,
`DELETE /auth/tokens/{id}`, `GET/POST /users`, `PUT /users/{id}`.

**config_revision & optimistic concurrency** — a single global counter in
`settings`. Every mutation runs one "three-in-one" transaction: business row +
revision bump + audit row. Mutating routes, policies, identities, route
operations, or settings requires `expected_revision` (the revision you read
before deciding); on mismatch you get `409 admin.revision_conflict` with the
current revision in a top-level `config_revision` field — that is the
"another admin moved the world" signal, not an error to swallow: re-GET and
retry with the fresh revision. Users and alert channels/rules are deliberately
exempt (last-write-wins, low-contention superadmin resources).

**Audit** — every mutation appends an `admin_audit` row (actor, action,
resource, before/after JSON, IP, request_id) in the same transaction as the
write — there is no un-audited path through the API. Retention is 90 days
(operators needing longer must export).

## 4. Design philosophy — the invariants an AI must respect

1. **Default-deny + fail-closed.** Missing config = reject: an empty snapshot
   (fresh boot without DB) denies everything; no matching allow = 403; deny
   beats allow. Never "temporarily" add a broad allow (`caller='*'`,
   `/**`) to make something work — those are forced to expire and pollute the
   security posture. Onboard precisely: the narrowest pattern that satisfies
   the real call.
2. **Fail-static.** During a MySQL outage, instances keep serving their last
   snapshot (config freshness degrades; `/__ready` goes 503 after the stale
   window and LBs pull the instance, but existing traffic keeps flowing). A
   DB blip mid-write means your write failed cleanly — the transaction
   rolled back; re-read the revision and retry.
3. **Audit-first, least-scope.** Tokens expire by construction, credentials
   are hashed, request/response bodies never enter ordinary logs, private
   keys never persist. Don't ask for more scope, validity, or payload access
   than the task needs — and say why when you need something sensitive (debug
   capture is superadmin, opt-in, and audited).
4. **Never block the hot path.** Access-log sinks, debug capture, and the
   alert queue drop rather than block when full; drop counters are visible in
   `/__metrics`. Logs can legitimately miss bursts — do not treat log
   absence as proof of inactivity.
5. **Everything is versioned and audited.** Treat `expected_revision` as
   mandatory discipline — GET → decide → write with the revision you read —
   not an obstacle. It is what makes concurrent admins safe, and the 409 is
   your signal to re-read, not to force.

## 5. Capability map — what you can accomplish

Organized by goal, not by endpoint.

**Onboard a service end-to-end.** Create the identity (`POST
/identities/generate` for a one-time keypair+cert, or `POST
/identities/validate` then `POST /identities` for an uploaded PEM) → create
the route (`POST /routes`: prefix, targets, strip_prefix) → create allow
policies (`POST /policies` per caller; precise patterns, expiry on wide
rules) → dry-run `POST /policies/test` → curl-verify through the data plane
with the service certificate → confirm the decision in `GET /logs/access` and
the mutation in `GET /audit/admin?action=route`. Caveat: nothing is live
until each instance's next sync tick (≤60s default); check `GET
/gateway/instances` for propagation (`loaded_revision` vs `config_revision`).

**Operate (know what is live).** `GET /gateway/snapshot` — what THIS instance
enforces right now (per-instance; others may lag); `GET /gateway/instances` —
heartbeats, versions, per-instance loaded revisions, stale flags; `GET
/gateway/config` — the DB-side configuration plus `config_revision` (the
number you need for writes). Caveat: DB-side config and instance snapshots
can legitimately differ by one sync interval — never treat the DB view as
proof of what traffic currently sees.

**Secure.** `GET /policies/expiring` (wide and soon-expiring rules, ≤7 days
flagged); rotate an identity by generating/uploading a new cert under the
same name (old and new coexist one cycle; then `POST /identities/{id}/revoke`
the old); `DELETE /identities/{id}` only when no policy references it; manage
users (`GET/POST /users`, `PUT /users/{id}` — disabling a user revokes their
sessions immediately). Caveat: the precondition for an EMERGENCY revocation
is that MySQL is reachable — during a DB outage instances keep honoring their
snapshot (fail-static).

**Observe.** `GET /logs/access` — access decisions from the file sink
(filters: `caller`, `decision`, `route`, `outcome`; requires the file sink on
that instance); `GET /audit/admin` — the admin trail (filters: `actor`,
`action` prefix; every AI mutation lands here under your username); `GET
/alerts/events` — alert firings; `GET /logs/debug` (metadata, all admins) and
`GET /logs/debug/{request_id}` (full payload — superadmin only, no-store,
viewing is audited; contains sensitive captured traffic — opt in via
`PUT /settings/debug` only with an explicit reason). Caveat: debug capture is
off by default; audit and alert events are retained 90 days.

**Alert.** Wire a Feishu/Lark channel (`POST /alerts/channels`, test with
`POST /alerts/channels/{id}/test`), compose rules with nested conditions
(`POST /alerts/rules`; sources `status`/`outcome`/body fields), review
firings (`GET /alerts/events`). Caveat: no rate-limiting — a chatty route
fires a card per matching request, so scope rules to specific routes and
conditions.

**Automate yourself.** Mint your own tokens (`POST /auth/tokens {name,
expires_in_days}` — raw secret shown once) and revoke them (`DELETE
/auth/tokens/{id}`); list yours with `GET /auth/tokens`. Caveat: expiry is
mandatory and fixed at creation; on `401 admin.token_invalid` stop and ask
the human to mint a new one — never extend or silently re-mint.

## 6. Operational semantics that surprise outsiders

- **Config sync lag is bounded, not instant.** A write is live on every
  instance within one sync interval (default 60s) or at the next forced
  reload tick. There is no push.
- **Fail-static window.** MySQL down = instances keep serving the last
  snapshot; `/__ready` degrades to 503 after `ROUTEGATE_READY_STALE_AFTER`
  (default 3× the sync interval) and LBs pull the instance.
- **Drain on SIGTERM.** `/__ready` 503s immediately, in-flight requests drain
  up to `ROUTEGATE_DRAIN_TIMEOUT` (default 20s), then listeners close.
  Callers still need retry + idempotency.
- **MySQL availability bounds restart survival.** A restart during a DB
  outage joins with an empty snapshot = deny everything.
- **Verdict capture is conditional.** Only routes with a success rule pay the
  8 KiB response-body capture; there is no blanket body buffering.
- **Direct-DB edits are eventually healed.** Every N ticks (default 10) a
  forced full reload ignores the revision — but the supported path is
  `routegate config bump` (break-glass) after direct SQL edits.
- **Stale instance rows disappear.** Heartbeats older than 10 minutes are
  GC'd; liveness is judged by freshness, not row count.
