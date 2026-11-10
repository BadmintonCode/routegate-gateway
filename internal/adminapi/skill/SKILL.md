---
name: routegate-admin
description: Operate the RouteGate admin API with a temporary user-scoped Bearer token, grounded in the gateway's architecture, design philosophy, and capability map (ARCHITECTURE.md — data plane vs admin plane, snapshot/revision model, default-deny, audit-first). Discover endpoints, query gateway state (routes, policies, identities, alerts, audit), and mutate live configuration only with human confirmation. Use whenever a task needs to read or manage RouteGate's gateway configuration over HTTP as an AI/machine client.
---

# RouteGate admin API (temporary user-scoped token)

## Overview

RouteGate is a MySQL-backed service-to-service gateway: all service-to-service
calls traverse it, workloads authenticate with mTLS client certificates
(pinned per service, hub-and-spoke), and authorization is deny-by-default per
interface. One binary ships three planes — the mTLS data plane, a plaintext
health listener, and the admin plane (a web panel plus the same-origin
`/admin/api/v1/*` REST API) that writes gateway configuration to MySQL.

This skill covers using the admin API as an AI/machine client with a
**temporary, user-scoped Bearer token** (`rgt_…`). The token authenticates AS
the panel user who minted it — there is no independent permission scope in v1;
the user's role is resolved live from the user row on every request, so
disabling the user or changing their role takes effect immediately.

## Understand before you write

RouteGate is a MySQL-backed, default-deny service-to-service gateway. The
admin plane you are calling (:8081) only WRITES MySQL; the mTLS data plane
(:8080) enforces an in-memory snapshot that each instance's sync worker
reloads from MySQL on its next tick (default 60s) — nothing you write pushes
to instances, so changes are live within one sync interval, not instantly.
Every mutation bumps a global `config_revision` inside one transaction that
also writes an audit row; concurrent writers arbitrate through
`expected_revision`, and a 409 `admin.revision_conflict` is the
"another admin moved the world" signal — re-read and retry, never swallow it.
Identity = a service with a pinned client certificate (what policies
reference); policies are allow/deny rules evaluated deny-beats-allow with no
matching allow = 403; human admins are separate (`users`, plus your `rgt_`
token acting AS its user).

**Read `ARCHITECTURE.md` (same directory) IN FULL before any non-trivial
task** — before your first create/update/delete, before policy work, and
whenever a 409 or an unfamiliar concept appears. Reading it is cheap; a blind
write changes production traffic.

## Credential setup (REQUIRED before first call)

Humans can paste `skills/routegate-admin/AI-PROMPT.md` (next to this file) into
an agent session to bootstrap all of this: the template instructs the agent to
install the skill, configure the credential and endpoint with the human, and
verify connectivity before spending the token.

### 1. Get a token from the human

- Ask the human operator to mint a token in the panel: **开发者** page →
  **我的凭证** → **新建凭证**, entering a name and a validity period (recommend
  **7–30 days** for AI use), then copying the raw `rgt_…` token shown once.
- Alternative when the human already has a panel session (or you are driving
  the panel API on their behalf): mint via
  `POST /admin/api/v1/auth/tokens` with body `{"name":"ai-agent","expires_in_days":30}`.
  The raw token appears **exactly once** in the response; after that only its
  SHA-256 hash is stored.
- Never attempt to log in with username/password to obtain credentials, and
  never ask the human to paste the token into chat.

### 2. Store the configuration in `.routegate/config.json`

Local client configuration lives in the project's `.routegate/config.json`
(client-side convention; the server never reads
it). One file covers all environments: top-level keys
are environment names, and each environment records the endpoint and the NAME
of the environment variable holding the token — **never the token value**:

```json
{
  "test": {
    "baseUrl": "http://127.0.0.1:28081",
    "apiBasePath": "/admin/api/v1",
    "auth": { "type": "bearer", "tokenEnv": "ROUTEGATE_ADMIN_TOKEN" }
  },
  "production": {
    "baseUrl": "https://routegate.example.com",
    "apiBasePath": "/admin/api/v1",
    "auth": { "type": "bearer", "tokenEnv": "ROUTEGATE_ADMIN_TOKEN" }
  }
}
```

Rules:

- The token itself lives ONLY in the environment variable named by `tokenEnv`
  (`ROUTEGATE_ADMIN_TOKEN`); the file stores the reference, never the secret.
- Create/update the file with the human: preserve unrelated fields, do not
  add schema versions, "active environment" wrappers, or invented values —
  leave unknown values unset and report them.
- Make sure it is git-ignored: if `.routegate/config.json` is not covered,
  add exactly that path to the nearest project `.gitignore`.
- Validate the JSON, show a REDACTED summary (structure, never values), and
  list the values the human must supply (token env var content per
  environment).
- Do not assume environments share base URLs or tokens.
- For a quick one-off session without a project, environment variables alone
  (`ROUTEGATE_ADMIN_URL` + `ROUTEGATE_ADMIN_TOKEN`) remain acceptable; the
  file is the default for project work.

- Never hardcode, commit, log, or echo the token. Never include it in command
  output, error reports, URLs, or query parameters. Do not run commands that
  print environment values (`set -x`, `echo "$ROUTEGATE_ADMIN_TOKEN"`).
- Read the token from the named environment variable into the Authorization
  header at call time; treat any response or log line that echoes it as an
  incident to report.

### 3. Configure the endpoint (base URL)

- Sources, in order: read `ROUTEGATE_ADMIN_URL` from the environment first;
  if it is unset, ask the human. Never invent a host, port, or path — see
  **Base URL** below for what a valid address looks like.
- The IPv6-first strategy documented under **Network strategy (IPv6-first)**
  applies to the endpoint too: probe it (via the public
  `GET /admin/api/v1/meta/endpoints`) before the first credentialed call.

### 4. Token validity

- Expiry is mandatory: 1–365 days, fixed at creation. Expired or revoked
  tokens fail with `401 admin.token_invalid`.
- When the token expires or is revoked, stop and ask the human to mint a new
  one. Never attempt to extend, regenerate, or replace it silently, and never
  fall back to session login.

## Base URL

- The admin plane is plain HTTP on a management network (default port
  `:8081`). It must never be exposed publicly.
- The test deployment publishes `127.0.0.1:28081` on the host; from a laptop
  it is typically reached via an SSH tunnel such as
  `ssh -L 28081:127.0.0.1:28081 <host>`.
- Resolve the base URL from `ROUTEGATE_ADMIN_URL` first; if it is unset, ask
  the human. All endpoints are same-origin under `/admin/api/v1/…`.

## Network strategy (IPv6-first)

- Prefer IPv6: when the admin endpoint host resolves to both A and AAAA
  records, try the IPv6 address FIRST with a short connect timeout, then fall
  back to IPv4:

  ```sh
  curl -6 --connect-timeout 5 … || curl -4 --connect-timeout 5 …
  ```

  Concretely, the public connectivity pre-check (below) becomes:

  ```sh
  curl -6 --connect-timeout 5 -fsS \
    "$ROUTEGATE_ADMIN_URL/admin/api/v1/meta/endpoints" \
    || curl -4 --connect-timeout 5 -fsS \
    "$ROUTEGATE_ADMIN_URL/admin/api/v1/meta/endpoints"
  ```

  This mirrors the platform CLI's `--ip-family prefer6` philosophy: IPv6
  preferred, with a fast fallback instead of hanging on a broken stack.
- Failure policy: connection errors (timeout / refused / unreachable) may be
  retried at most twice with a short backoff; then STOP and report to the
  human with the exact command and the exact error. NEVER retry on HTTP
  4xx/5xx — those are answered responses; interpret them through the
  status-code table below instead of retrying.
- Connectivity pre-check: `GET /admin/api/v1/meta/endpoints` is public (no
  credential needed) — use it to validate DNS resolution, IPv6 reachability,
  and base-URL correctness BEFORE spending the API token.
- If both the IPv6 and IPv4 stacks fail, ask the human for the correct
  address. Do not guess other ports or hosts.

## Request conventions

- Send `Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN` on every call and
  `Content-Type: application/json` on writes.
- The Bearer token is honored on **every** admin endpoint **except**
  `POST /admin/api/v1/auth/login` and `POST /admin/api/v1/auth/logout`
  (session-only semantics). When a Bearer header is present there is no
  cookie fall-through — the Bearer token alone decides authentication.
- Success responses are the resource JSON itself — no envelope.
- Error responses use the HTTP status plus
  `{"error":{"code","message","request_id"?}}`; the `X-Request-Id` response
  header carries the same request id. Cite `request_id` when reporting errors
  to the human.

### Status-code discipline

| Status | `error.code` | Meaning and what to do |
| --- | --- | --- |
| 400 | `admin.bad_request`, `admin.invalid_*` | Validation failure. Fix the payload; do not retry unchanged. |
| 401 | `admin.unauthenticated` | No credentials were sent. Attach the Bearer header. |
| 401 | `admin.token_invalid` | The API token is invalid, expired, or revoked. Ask the human to mint a new one; do not retry. |
| 403 | `admin.forbidden` | The token's user lacks the required role — writes are superadmin-only, and the token acts as its user. Report; do not attempt to work around. |
| 404 | `admin.not_found` | Unknown path or id. Revoking a token id that belongs to another user also 404s. |
| 405 | `admin.method_not_allowed` | Wrong HTTP method for the path. |
| 409 | `admin.revision_conflict` | The config changed concurrently. The response carries a top-level `config_revision`; re-GET the resource and retry with the new revision. |
| 409 | `admin.duplicate` | A unique value already exists (e.g. duplicate route name). |
| 500 | `admin.error`, `admin.write_failed`, `admin.storage_unavailable` | Infrastructure failure. Do NOT retry blindly — report to the human with the request id. |

### Optimistic concurrency

Why this exists (the revision/snapshot model behind it) is explained in
ARCHITECTURE.md §2–3; mechanically:

Mutating routes, policies, identities, or settings requires optimistic
concurrency: read the current `config_revision` first (it is returned by
`GET /admin/api/v1/gateway/config`, `GET /admin/api/v1/gateway/instances`,
and the settings GETs), then pass it as `expected_revision` in the write
body. On `409 admin.revision_conflict`, re-GET and retry with the fresh
revision. Users and alert channels/rules are exempt (last-write-wins).

## Endpoint discovery

- Both discovery endpoints below are **PUBLIC** — no credential needed. They
  serve as the pre-auth discovery and connectivity step (see the network
  strategy above) before the API token is configured or sent.
- At session start, enumerate `GET /admin/api/v1/meta/endpoints` — a
  machine-readable catalog of every endpoint (group, method, path, auth,
  summary, params, body example). Treat it as the source of truth for what
  this deployment offers.
- **This skill is self-hosted by the gateway**: `SKILL.md`, `ARCHITECTURE.md`
  and `AI-PROMPT.md` are downloadable without a credential at
  `GET /admin/api/v1/meta/skill/<file>` on the same admin host. An agent can
  install the skill from the host alone (save the files into your tool's
  skill directory and read them) — no repository checkout required. The
  served copies are drift-guarded against the repository sources in CI.
- `GET /admin/api/v1/openapi.json` serves the same catalog as OpenAPI 3.1
  (with `bearerAuth` and `cookieAuth` security schemes) for tooling.
- The panel's 开发者 → API docs tab mirrors the same catalog.

## Worked examples

All examples read the base URL and token from the environment — never
substitute literal secrets.

List routes (no query parameters; response is `{"items":[…]}`):

```sh
curl -fsS -H "Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN" \
  "$ROUTEGATE_ADMIN_URL/admin/api/v1/routes"
```

Get the gateway snapshot — the in-memory view this instance currently
enforces (routes with targets and operations, policies, identity metadata):

```sh
curl -fsS -H "Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN" \
  "$ROUTEGATE_ADMIN_URL/admin/api/v1/gateway/snapshot"
```

List alert events (`page`, `page_size` ≤ 100, optional `rule_id`; response
carries `items`/`page`/`page_size`, no total):

```sh
curl -fsS -H "Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN" \
  "$ROUTEGATE_ADMIN_URL/admin/api/v1/alerts/events?page=1&page_size=50"
```

Other read filters that exist in the handlers (all optional):

- `GET /admin/api/v1/audit/admin` — `page`, `page_size` (≤100, default 50),
  `actor` (exact match on the actor name), `action` (prefix match, e.g.
  `action=token` matches `token.create` and `token.revoke`); response
  includes `total`.
- `GET /admin/api/v1/logs/access` — `limit` (≤1000, default 200; newest
  first) and exact-match `caller`, `decision`, `route`, `outcome`. Requires
  the file sink on that instance.

Mint a token — **only when the human explicitly asks**; the raw token is
returned once:

```sh
curl -fsS -X POST \
  -H "Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"ai-agent","expires_in_days":30}' \
  "$ROUTEGATE_ADMIN_URL/admin/api/v1/auth/tokens"
```

Interpret a 409 revision conflict: re-GET the current revision, then retry
the write with it.

```sh
# Write rejected: 409 {"error":{"code":"admin.revision_conflict",...},"config_revision":42}
curl -fsS -H "Authorization: Bearer $ROUTEGATE_ADMIN_TOKEN" \
  "$ROUTEGATE_ADMIN_URL/admin/api/v1/gateway/config"   # read config_revision
# Retry the PUT/DELETE with "expected_revision": 42
```

## Safety notes

- This API mutates **live gateway configuration** (routes, policies,
  identities) and can affect production traffic. Always confirm with the
  human before any POST/PUT/DELETE; prefer reads for exploration.
- The Bearer token inherits its user's role — with a superadmin's token,
  every write is authorized. The confirmation rule above is the only guard.
- Token creation and revocation are themselves audited (`token.create` /
  `token.revoke`, resource type `api_token`); the raw token is shown once at
  creation and never logged.
