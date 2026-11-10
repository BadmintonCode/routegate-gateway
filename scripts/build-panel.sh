#!/usr/bin/env bash
# Build the admin panel (React + Vite + AntD) into internal/web/dist.
# Requires Node.js; network access goes through the standard proxy env vars.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/web/admin"

if [ ! -d node_modules ]; then
  echo "==> installing panel dependencies"
  npm install --no-audit --no-fund
fi

echo "==> building panel"
npm run build

# Vite's emptyOutDir removes our marker; restore it so git stays clean.
printf '*\n!.gitignore\n' > "$ROOT/internal/web/dist/.gitignore"

echo "==> panel built into internal/web/dist"
