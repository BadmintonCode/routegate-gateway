#!/usr/bin/env bash
# Cross-compile the gateway and demo services for linux/arm64 (or amd64), then
# build the demo images. Run from the repository root or anywhere.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ARCH="${TARGETARCH:-$(uname -m | sed 's/aarch64/arm64/;s/x86_64/amd64/')}"
VERSION="${VERSION:-0.1.0-demo}"

echo "==> cross-compiling for linux/$ARCH (CGO off)"
cd "$ROOT"
export CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH"
mkdir -p deploy/bin demo/bin
"$ROOT/scripts/build-panel.sh"
go build -trimpath -tags panel -ldflags "-s -w -X main.version=$VERSION" -o "deploy/bin/routegate-linux-$ARCH" ./cmd/routegate
go build -trimpath -ldflags "-s -w" -o "demo/bin/demo-alpha-linux-$ARCH" ./demo/demo-service
go build -trimpath -ldflags "-s -w" -o "demo/bin/demo-beta-linux-$ARCH"  ./demo/demo-service
go build -trimpath -ldflags "-s -w" -o "demo/bin/demo-caller-linux-$ARCH" ./demo/demo-caller
go build -trimpath -ldflags "-s -w" -o "demo/bin/demo-edge-linux-$ARCH" ./demo/demo-edge
go build -trimpath -ldflags "-s -w" -o "demo/bin/mock-llm-linux-$ARCH" ./demo/mock-llm
unset CGO_ENABLED GOOS GOARCH

echo "==> building images"
docker compose -f "$ROOT/deploy/docker-compose.quickstart.yml" build
echo "==> done"
