# Deployment & Migration Guide

Operational standard for bringing RouteGate into an environment and migrating
services onto it. The architecture rationale lives in
[ADR-0001](adr/0001-routegate-design.md); this document is the runbook.

## 1. Topology and listeners

| Listener | Default | Purpose | Exposure |
| --- | --- | --- | --- |
| Data plane | `:8080` | mTLS service traffic (`ROUTEGATE_LISTEN`) | service network only |
| Admin plane | `:8081` | Admin API + panel (`ROUTEGATE_ADMIN_LISTEN`) | management network / bastion tunnel only (plain HTTP by decision — front with TLS or restrict at the network layer) |
| Health | `:8082` | `/__health` `/__ready` `/__status` `/__metrics` (`ROUTEGATE_HEALTH_LISTEN`) | platform probes, plaintext |

**Probes must target `:8082/__ready`.** The data plane requires client
certificates at the HTTP layer — a plaintext probe against `:8080` will always
fail, and that is by design (ADR-0001 §2.2, §14).

## 2. Prerequisites

- **Dedicated MySQL database and account** (only that database). Do not share
  a business database. The gateway tolerates DB outages at runtime
  (fail-static) but the database must exist and be reachable at first boot;
  point `ROUTEGATE_DB_DSN` at a stable endpoint (VIP/DNS), not a single host.
- **Schema**: `ROUTEGATE_AUTO_MIGRATE=true` (default) creates/updates the
  schema at startup; or run `routegate migrate` manually first.
- **Pinned version**: deploy a fixed version (image digest or tag). Track
  `test` after CI; do not deploy un-merged branches.
- **Server certificate** for the data plane (`ROUTEGATE_SERVER_TLS_CERT/KEY`):
  one gateway serving certificate; client identities are pinned separately.

Minimal environment (file-sink example):

```bash
ROUTEGATE_DB_DSN='routegate:<pw>@tcp(mysql.internal:3306)/routegate?parseTime=true'
ROUTEGATE_SERVER_TLS_CERT=/certs/gateway.crt
ROUTEGATE_SERVER_TLS_KEY=/certs/gateway.key
ROUTEGATE_ACCESS_LOG_SINK=file
ROUTEGATE_ACCESS_LOG_FILE=/var/log/routegate/access.log
ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS=apisix-admin,apisix-user,apisix-webhook
                                          # required for the APISIX identity contract (§4.2):
                                          # only these callers may pass X-Gateway-User-Id;
                                          # omit when no APISIX layer fronts the gateway yet
ROUTEGATE_ADMIN_BOOTSTRAP_USERNAME=admin   # one-time password printed at first boot when password omitted
ROUTEGATE_DRAIN_TIMEOUT=20s                # SIGTERM drain grace (Go duration); set compose
                                           # stop_grace_period to >= this value or in-flight
                                           # requests get cut early
ROUTEGATE_INSTANCE_ID=routegate-test-1
```

The access-log `mysql` sink named in ADR-0001 §13 is not implemented in this
release: `ROUTEGATE_ACCESS_LOG_SINK=mysql` is rejected at load. Use `stdout`
or `file`.

Config sync defaults to 60 s (`ROUTEGATE_CONFIG_SYNC_INTERVAL`); the
management panel applies changes within one interval per instance.

## 3. Rollout phases

### Phase 0 — prerequisites (above), then Phase 1 — bring up the gateway

1. Deploy one instance (stateless; scale horizontally later).
2. Platform health checks pass on `:8082/__ready`.
3. Open the panel through the management network, log in with the bootstrap
   one-time password, change it (forced at first login).
4. Acceptance: no-certificate request to `:8080` returns **401** (mTLS
   enforced); panel can create a route.

### Phase 2 — per-service onboarding (link only, no traffic switch)

For each service, in this order:

1. **Identity**: panel → Identities → generate (or upload) the service
   certificate; the URI SAN binds `https://routegate.local/identities/<name>`,
   the fingerprint is pinned on creation, and the private key is shown once —
   hand it to the service owner (config management later). A service that is
   also a **caller** of other services needs its own caller identity too.
2. **Route**: create the route with the service's prefix (e.g. `/order-svc`)
   and its real network address as targets; decide `strip_prefix`.
3. **Policy**: allow-list the real call relationships (caller × method ×
   path pattern). **Default deny means an onboarded service that receives no
   policy rejects everything** — onboarding is invisible to existing traffic.
4. **Link verification (still no production traffic)**, from a bastion host:

   ```bash
   curl -sk --resolve routegate:8080:<gateway-ip> \
     --cert order-svc.crt --key order-svc.key \
     https://routegate:8080/order-svc/v1/ping        # 200 = route + identity + policy
   ```

   Check in order: no cert → 401; registered cert → 200; unauthorized path →
   403; unknown route → 404. The audit page shows the request with its
   outcome; optional per-route success rules and business-envelope fields
   (ADR-0001 §6.7) surface business failures here already.

### Phase 3 — per-service traffic cutover

- The only caller change: point the caller at the gateway
  (`https://routegate:8080/<prefix>/...`) **with its own client certificate**.
  Budget for this mTLS configuration per caller — it is the main onboarding
  cost, so verify it in Phase 2 with the caller's own identity.
- Cut over a small, stateless, read-mostly service first; observe the audit
  (Result column, business code/message) for a cycle before the next service.
- **Rollback is instant**: callers re-point to the direct service address.
  No gateway-side change is needed, and default-deny prevents accidental
  forwarding during the window.

### Operations during migration

- **Debug mode** (panel → Settings, superadmin, runtime): bounded sanitized
  captures for troubleshooting; switch off afterwards. Per-route success
  rules and business fields (§6.7) give the audit a real success/failure
  verdict instead of a status-code guess.
- **Multi-instance**: any instance serves independently; configuration
  propagates within one sync interval. DB outage = fail-static (existing
  traffic keeps flowing with the last-known configuration).

## 4. APISIX integration (customer-facing two-layer architecture)

APISIX stays the northern (customer-facing) gateway: authentication, rate
limiting, and external API shape. RouteGate enforces workload identity and
default-deny for what reaches the service network.

```
client ──► APISIX (auth plugins per prefix, path rewrite)
              │  mTLS upstream, identity: apisix-admin / apisix-user / apisix-webhook
              ▼
           RouteGate (default-deny policies, audit)
              │  plain HTTP one hop inside the service network (v1; upstream TLS ≙ v1.5)
              ▼
           services
```

### 4.1 Identity model

Create one RouteGate identity per APISIX plane and pin it as the upstream
client certificate of the corresponding APISIX routes:

| APISIX prefix | Auth plugin | RouteGate identity | Typical policy |
| --- | --- | --- | --- |
| `/admin/*` | admin session/token | `apisix-admin` | broad internal paths |
| `/user/*` | user JWT | `apisix-user` | user-scoped paths |
| `/webhook/*` | signature verification | `apisix-webhook` | narrow callback paths |

Two APISIX prefixes may target the **same** service (e.g. `/admin/user/v1/api`
and `/user/user/v1/api` both reach the user service): RouteGate sees the same
route but **different callers**, so the audit distinguishes the planes and
policies can diverge per plane.

### 4.2 Passing the authenticated identity downstream (v1 contract)

RouteGate strips all inbound identity/forwarding headers and trusts exactly
one user-context header from callers on `ROUTEGATE_TRUSTED_USER_CONTEXT_CALLERS`
(e.g. `apisix-admin,apisix-user,apisix-webhook`):

```text
X-Gateway-User-Id: admin:1234      # admin plane, subject from the admin plugin
X-Gateway-User-Id: user:5678       # user plane, subject from the user JWT
X-Gateway-User-Id: webhook:acme    # webhook plane, vendor identity
```

APISIX maps each plugin's subject into this single header with the namespace
prefixes above (zero gateway changes, fully auditable). A configurable
per-caller pass-through header allowlist is a planned v1.x extension — do not
rely on arbitrary custom headers surviving the gateway today (unlisted
headers currently pass through, but this is not a contract).

### 4.3 Path rewrite layering

APISIX may decouple the external API shape from the internal one
(`proxy-rewrite`), e.g. `/admin/user/v1/api` → `/admin/userabcd/v1/api`:

- RouteGate sees and audits the **post-rewrite** path; author its routes and
  policies against the internal namespace. The external path lives in the
  APISIX access log; forward it as a custom header (e.g. `X-External-Path`)
  if the service needs it.
- **One strip per layer**: APISIX rewrites external → gateway namespace;
  RouteGate's `strip_prefix` rewrites gateway → service namespace. Do not
  duplicate rewrites across layers.
- Prefer a **mechanical convention** (e.g. `/{plane}/{svc}/…` →
  `/{svc}-{plane}/…`, route names `user-admin`) over free-form per-route
  rewrites so any 403/404 can be traced by hand. Prefer **identity-based
  plane distinction** (4.1) over path rewrites unless the service itself
  routes on the plane-specific path.

Example APISIX route (3.x):

```yaml
uri: /admin/*
upstream_id: routegate
plugins:
  proxy-rewrite:
    regex_uri: ["^/admin/(.*)", "/$1"]
  # ... auth plugin sets X-Gateway-User-Id: admin:<subject>
```

```yaml
upstreams:
  - id: routegate
    type: roundrobin
    nodes: { "routegate.internal:8080": 1 }
    scheme: https
    tls:
      client_cert: /usr/local/apisix/certs/apisix-admin.crt
      client_key:  /usr/local/apisix/certs/apisix-admin.key
```

### 4.4 Health checks and timeouts

- APISIX active upstream health checks would need client certificates;
  prefer **no active probes** and rely on RouteGate's own multi-instance
  `__ready` handling by the platform, or probe per-instance `:8082`.
- Align APISIX route timeouts/retries with the gateway's error envelope
  (`gateway.upstream_timeout` / `gateway.upstream_unavailable`); the gateway
  does not retry (fail-fast by design).

## 5. Checklist

- [ ] Dedicated MySQL DB + account, stable endpoint
- [ ] Gateway deployed, `:8082/__ready` probe green, panel reachable via management network
- [ ] Bootstrap password changed; admin plane network-restricted
- [ ] Per service: identity (+ caller identity) → route → policy → link verified with curl
- [ ] Callers carry their client certificates (mTLS) before cutover
- [ ] Success rules / biz fields configured where business verdicts matter
- [ ] Debug mode OFF after investigations
