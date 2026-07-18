#!/usr/bin/env bash
set -euo pipefail

# Build the Next.js dashboard as a static export and copy it into the Go
# embedded asset directory. Idempotent: overwrites any previous dist contents.

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DASHBOARD_DIR="$REPO_ROOT"
ASSETS_DIR="$REPO_ROOT/internal/adapter/transport/http/dashboard_assets"
OUT_DIR="$DASHBOARD_DIR/out"

cd "$DASHBOARD_DIR"

# Run the Next.js static export using bun.
bun run build

if [[ ! -f "$OUT_DIR/index.html" ]]; then
  echo "error: Next.js export did not produce $OUT_DIR/index.html" >&2
  exit 1
fi

# Replace embedded assets with the new export.
rm -rf "$ASSETS_DIR"
mkdir -p "$ASSETS_DIR"
cp -a "$OUT_DIR"/. "$ASSETS_DIR/"

if [[ ! -f "$ASSETS_DIR/index.html" ]]; then
  echo "error: dashboard_assets/index.html missing after copy" >&2
  exit 1
fi

echo "dashboard built: $ASSETS_DIR/index.html"
