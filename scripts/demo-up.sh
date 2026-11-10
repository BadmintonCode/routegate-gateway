#!/usr/bin/env bash
# One-shot demo bring-up:
#   1. generate demo certificates + seed SQL
#   2. build images (cross-compiled binaries, scratch images)
#   3. compose up (MySQL + gateway + 3 demo services)
#   4. seed the database (identities, routes, policies) and bump the revision
#   5. print verification hints
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

"$ROOT/scripts/gen-demo-certs.sh"
"$ROOT/scripts/build-demo.sh"

echo "==> starting stack"
docker compose -f "$ROOT/deploy/docker-compose.quickstart.yml" up -d

echo "==> waiting for MySQL"
for i in $(seq 1 60); do
  if docker exec routegate-mysql mysqladmin ping -h 127.0.0.1 -proot-demo --silent 2>/dev/null; then
    break
  fi
  sleep 2
done

echo "==> seeding database"
docker exec -i routegate-mysql mysql -uroot -proot-demo routegate < "$ROOT/deploy/demo-seed.sql" 2>/dev/null || \
docker exec -i routegate-mysql mysql -uroot -proot-demo routegate < "$ROOT/deploy/demo-seed.sql"

echo
echo "Demo stack is up. Verification:"
echo "  curl -s http://127.0.0.1:8082/__ready"
echo "  curl -s http://127.0.0.1:8082/__status"
echo "  # without a certificate -> 401:"
echo "  curl -sk --resolve routegate:8080:127.0.0.1 https://routegate:8080/alpha/v1/ping"
echo "  # with the caller certificate -> 200 / 403 / 404:"
echo "  curl -sk --resolve routegate:8080:127.0.0.1 --cert deploy/certs/caller.crt --key deploy/certs/caller.key https://routegate:8080/alpha/v1/ping"
echo "  curl -sk --resolve routegate:8080:127.0.0.1 --cert deploy/certs/caller.crt --key deploy/certs/caller.key https://routegate:8080/alpha/admin/secret"
echo "  curl -i http://127.0.0.1:8090/healthz"
echo "  curl -i http://127.0.0.1:8090/alpha/v1/ping"      # no client cert: edge adds mTLS
echo "  curl -i -H 'X-Demo-User-Id: user-123' http://127.0.0.1:8090/alpha/v1/ping"
echo "  curl -i http://127.0.0.1:8090/alpha/admin/secret"      # 403
echo "  curl -i http://127.0.0.1:8090/no-such-route"          # 404
echo "  docker logs -f demo-edge   # public edge -> RouteGate loop"
echo "  docker logs -f demo-caller # direct workload -> RouteGate loop"
echo "  # debug logging is ON in the demo via the seed (panel http://127.0.0.1:8081/admin/ → 设置 → Debug logging):"
echo "  ls deploy/access-logs/   # access-*.log (metadata only) and debug-*.log (sanitized payloads)"
