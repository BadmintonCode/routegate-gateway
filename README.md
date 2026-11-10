# RouteGate

> A MySQL-backed service-to-service gateway with mTLS workload identity,
> default-deny authorization policies, and a built-in admin panel.

**Status: work in progress (v0).** The founding design is complete and the
repository is bootstrapped; implementation milestones are tracked below.
RouteGate was built for production service-to-service traffic in a real
deployment and generalized into a standalone open-source project.

## What it is

RouteGate is the single enforcement point for all service-to-service traffic in
a deployment — including the hop where your public edge gateway (for example
APISIX or Nginx) calls backend services. The edge gateway is just another
caller holding its own identity.

Four capabilities, in one binary:

1. **One gateway for all internal traffic** — east-west service-to-service
   calls *and* edge-to-backend calls all traverse RouteGate. The hot path never
   touches the database.
2. **Certificate authentication with hub-and-spoke trust** — every service
   maintains exactly one trust relationship, with the gateway. Services never
   need certificates for each other. The gateway stores public certificate
   material only; private keys never live in the gateway or its database, and
   the admin panel can generate key pairs in your browser.
3. **Interface-level authorization matrix** — declare which service may call
   which interfaces of which route, with wildcard and partial-wildcard path
   patterns (`/v1/users/*`, `/v1/*/detail`, `/v1/users/**`). Evaluation is
   fixed: deny beats allow, and anything unmatched is denied.
4. **AI/machine clients via temporary admin API tokens** — panel users mint
   user-scoped Bearer tokens (mandatory expiry, revocable, shown once) so AI
   agents and scripts can call the admin API without sharing a browser
   session; the bundled `routegate-admin` agent skill guides AI tools through
   safe, credential-hygienic usage.

## How it works

```text
                    Admin panel (management network only)
                          │
                          ▼
                    Admin API :8081 ──writes──► MySQL (the only control-plane
                                                 dependency; config_revision
                                                 signals changes)
                                             │
 call mTLS client certs          read-only poll (default 60s, env-configurable)
 edge-gw ──cert──┐                          ▼
 service-a ─cert─┤  ►  RouteGate :8080  ──► upstream A :8080
 service-c ─cert─┘     (N stateless          ──► upstream B :8080
                        instances)           (dedicated private network)
```

- Configuration lives in MySQL (routes, policies, identities, admin accounts).
  An in-process worker loads a full snapshot every
  `ROUTEGATE_CONFIG_SYNC_INTERVAL` (default **60s**) and atomically swaps it
  into memory. Request handling is database-free.
- Deploying the gateway needs exactly one binary and one MySQL schema. No
  etcd, no control-plane cluster, no Redis.
- Deny-by-default and fail-closed: empty snapshot denies everything; a
  database outage keeps the last snapshot serving (fail-static) while
  `/__ready` degrades so the load balancer can remove stale instances.

See [docs/adr/0001-routegate-design.md](docs/adr/0001-routegate-design.md) for
the complete design: architecture, MySQL schema, wildcard policy semantics,
certificate lifecycle, admin account model, security notes, and the roadmap.

## Roadmap

| Milestone | Scope | Status |
| --- | --- | --- |
| M0 | Repository bootstrap, design ADR-0001, CI skeleton | ✅ done |
| M1 | Data plane MVP: schema + migrations, sync worker, routing/proxy, quickstart | ✅ implemented (data plane + cert identity + wildcard policies; demo verified) |
| M2 | Identity (JWT fallback), access-log file/MySQL sinks | in progress (mTLS identity, policy engine and tester already shipped) |
| M3 | Admin API + admin panel (React + Ant Design 5) | ✅ implemented (API + embedded panel, demo verified) |
| M4 | v0.1.0 open-source release: docs, demo, security review | ✅ done (v0.1.0 released 2026-11-08) |

## Quickstart

One command brings up MySQL, the gateway, three demo services, and a
local deterministic OpenAI-compatible mock endpoint (two upstreams plus a
caller workload that talks to them **through** the gateway with its own client
certificate):

```bash
scripts/demo-up.sh
```

Then verify — allow, deny, unauthenticated, unknown route, health:

```bash
# health listener (plaintext; the data plane requires client certificates)
curl -s http://127.0.0.1:8082/__ready
curl -s http://127.0.0.1:8082/__status
curl -s http://127.0.0.1:8082/__metrics

# no certificate -> 401
curl -sk --resolve routegate:8080:127.0.0.1 https://routegate:8080/alpha/v1/ping

# caller certificate -> 200 (allowed), 403 (denied by /admin/** rule), 404 (no route)
curl -sk --resolve routegate:8080:127.0.0.1 \
  --cert deploy/certs/caller.crt --key deploy/certs/caller.key \
  https://routegate:8080/alpha/v1/ping
curl -sk --resolve routegate:8080:127.0.0.1 \
  --cert deploy/certs/caller.crt --key deploy/certs/caller.key \
  https://routegate:8080/alpha/admin/secret

curl http://127.0.0.1:8090/healthz
curl http://127.0.0.1:8090/alpha/v1/ping
curl -H "X-Demo-User-Id: user-123" http://127.0.0.1:8090/alpha/v1/ping
curl http://127.0.0.1:8090/alpha/admin/secret   # 403
curl http://127.0.0.1:8090/no-such-route       # 404

docker logs -f demo-edge

docker logs -f demo-caller   # east-west allow/deny loop
```

### AI Agent demo

The panel has an **AI Agent** menu with **对话** and **模型** tabs. In the
**模型** tab, add an endpoint with:

- Base URL: `http://mock-llm:8080/v1`
- Model: `routegate-demo`
- API key: leave empty (the local mock does not authenticate)
- Enabled: on; Default: on

Use **测试** and then open **对话**. The deterministic mock performs one
`list_routes` read-only tool call before returning a streamed answer. It is
included only for the local quickstart; configure a private or approved
OpenAI-compatible endpoint before using the assistant outside the demo.

### Success verdict per route

Business failures often return HTTP 200 (`{"code":1}`). Each route can carry
a composable success rule — HTTP status and/or response-body JSON fields,
`in`/`not_in` value lists, combined with AND/OR — and the audit then shows a
real success/failure verdict per request instead of a status-code guess.
Configure it in the panel (Routes → edit → 成功判定) or via
`routes.success_rule` in the API; see ADR-0001 §6.7.

### Debug logging (opt-in, panel-managed)

Ordinary access logs never contain bodies, credentials, or addresses. When you
need to see real traffic while troubleshooting, enable the separate debug sink
on the panel Settings page (设置 → Debug logging, superadmin only) or via the
API (`PUT /admin/api/v1/settings/debug`). The configuration lives in the
settings store — no environment variables — and instances apply it on their
next sync tick, so it can be toggled at runtime and off again after the
investigation.

It records sanitized headers and cookies plus bounded request/response bodies
(64 KiB per body by default, base64 for binary). Explicit address fields
(`X-Forwarded-*`, `X-Real-IP`, client-IP variants, remote/upstream addresses)
are never recorded. View records in the panel (Audit → 访问日志 → Debug detail,
superadmin only) or via `/admin/api/v1/logs/debug`. Sensitive by nature — see
[SECURITY.md](SECURITY.md) before enabling it in production.

What the demo shows: configuration lives in MySQL (routes, pinned
certificates, wildcard policies); the gateway reloads it every
`ROUTEGATE_CONFIG_SYNC_INTERVAL` (5s in the demo, 60s default) without touching
the DB on the request path; identity is the client certificate (fingerprint
match); evaluation is `deny > allow > default deny`. Revoke an identity in the
database (or via the panel) and it stops working within one sync
interval. Open the panel at http://127.0.0.1:8081/admin/ (demo bootstrap: `admin` /
`admin-demo-change-me`, forced password change at first login). Configuration
can now be managed in the panel or the Admin API; `deploy/demo-seed.sql`
remains as an alternative.

> If Docker image pulls need a proxy, configure it for the Docker daemon (the
> pull happens daemon-side), e.g. for colima:
> `colima start --env HTTP_PROXY=... --env HTTPS_PROXY=...`.

## Documentation

- [Design & founding decisions (ADR-0001)](docs/adr/0001-routegate-design.md)
- [Deployment & migration guide](docs/DEPLOYMENT.md) — environment rollout
  phases, per-service onboarding, APISIX two-layer integration standard
- Configuration reference, certificate operations manual, deployment topology:
  added per milestone under [docs/](docs/)
- English is the primary documentation language; zh-CN translations are planned.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). The project uses DCO
(`Signed-off-by`); no CLA. Security issues: [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
