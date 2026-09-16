#!/usr/bin/env bash
# Local/CI build of the three artifacts (design.md §17): server binary,
# linux/amd64 agent with sha256 manifest, and the web dist.
# The Docker image builds the same set in stages; this script exists for
# non-container workflows.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/dist}"
VERSION="${VERSION:-$(date -u +%Y%m%d.%H%M%S)}"

cd "$ROOT"

echo "==> server (host os/arch)"
CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION" -o "$OUT/fobe-server" ./cmd/server

echo "==> agent (linux/amd64, static)"
mkdir -p "$OUT/dl/agent/$VERSION"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-s -w -X github.com/fonlan/fobe/internal/agent.Version=$VERSION" \
    -o "$OUT/dl/agent/$VERSION/linux-amd64" ./cmd/agent

# sha256 manifest consumed by install.sh and the panel version picker
(
  cd "$OUT/dl/agent/$VERSION"
  shasum -a 256 linux-amd64 > linux-amd64.sha256
  printf '{"version":"%s","artifacts":[{"os":"linux","arch":"amd64","file":"linux-amd64","sha256":"%s"}]}\n' \
    "$VERSION" "$(awk '{print $1}' linux-amd64.sha256)" > manifest.json
)

# Same artifacts in the image's seed layout (§5.5): point FOBE_AGENT_SEED_DIR at
# $OUT/agent-seed and a non-container server seeds its artifact volume on start,
# exactly like the container does with /srv/agent-seed.
echo "==> agent seed layout (FOBE_AGENT_SEED_DIR=$OUT/agent-seed)"
mkdir -p "$OUT/agent-seed/agent/$VERSION"
cp -R "$OUT/dl/agent/$VERSION/." "$OUT/agent-seed/agent/$VERSION/"

echo "==> web (if toolchain present)"
if command -v npm >/dev/null 2>&1 && [ -d "$ROOT/web" ]; then
    (cd "$ROOT/web" && [ -d node_modules ] || npm install && npm run build)
    mkdir -p "$OUT/web"
    cp -R "$ROOT/web/dist/." "$OUT/web/"
else
    echo "    npm not found; skipping web build"
fi

echo "==> done: $OUT"
