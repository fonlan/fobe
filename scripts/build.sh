#!/usr/bin/env bash
# Local/CI build of the three artifacts (design.md §17): server binary,
# linux/amd64 agent with sha256 manifest, and the web dist.
# The Docker image builds the same set in stages; this script exists for
# non-container workflows.
#
# The two binaries are optimized for different goals on purpose (design §17):
#
#   agent  → size. It is downloaded to every probe on each §5.5 self-update and
#            kept in router flash, so it is stripped (-s -w), trimmed of build
#            paths (-trimpath), given an empty build id, and — when upx is on
#            PATH — packed. Packing happens BEFORE the sha256 manifest so a
#            version number always maps to exactly one byte sequence;
#            FOBE_AGENT_UPX=0 turns packing off.
#
#   server → speed. DWARF is kept (pprof needs the symbols to symbolize; -s -w
#            costs nothing at runtime), the build consumes a PGO profile
#            (FOBE_PGO, default `auto` = cmd/server/default.pgo when present),
#            and FOBE_SERVER_GOAMD64 (default v1) can trade old-CPU
#            compatibility for a few percent. The agent never follows that
#            setting: its target CPUs are unknown and SIGILL means a dead probe.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/dist}"
VERSION="${VERSION:-$(date -u +%Y%m%d.%H%M%S)}"
PGO="${FOBE_PGO:-auto}"
SERVER_GOAMD64="${FOBE_SERVER_GOAMD64:-v1}"

cd "$ROOT"

# upx is optional: without it the agent is simply larger. The version number is
# content-addressed (design §5.5), so an unpacked build gets its own id and can
# never be mistaken for a packed one.
UPX=""
if [ "${FOBE_AGENT_UPX:-1}" = "0" ]; then
    echo "    FOBE_AGENT_UPX=0: agent left unpacked"
elif command -v upx >/dev/null 2>&1; then
    UPX="upx -q --best --lzma"
else
    echo "    upx not on PATH: agent left unpacked (install upx, or set FOBE_AGENT_UPX=0 to silence)"
fi

# pack_agent compresses a binary in place; $UPX is word-split on purpose.
pack_agent() {
    [ -z "$UPX" ] || $UPX "$1"
}

echo "==> server (host os/arch, pgo=$PGO, GOAMD64=$SERVER_GOAMD64)"
CGO_ENABLED=0 GOAMD64="$SERVER_GOAMD64" go build -trimpath -pgo="$PGO" \
    -ldflags "-X main.version=$VERSION" -o "$OUT/fobe-server" ./cmd/server

echo "==> agent (linux/amd64, static)"
mkdir -p "$OUT/dl/agent/$VERSION"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOAMD64=v1 go build -trimpath \
    -ldflags "-s -w -buildid= -X github.com/fonlan/fobe/internal/agent.Version=$VERSION" \
    -o "$OUT/dl/agent/$VERSION/linux-amd64" ./cmd/agent
pack_agent "$OUT/dl/agent/$VERSION/linux-amd64"

# sha256 manifest consumed by install.sh and the panel version picker — always
# taken over the final (packed) bytes. sha256sum is the Linux spelling, shasum
# the macOS one: this script runs on both (the Dockerfile only ever sees the
# former, which is why it can hardcode it).
sha256() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}
(
  cd "$OUT/dl/agent/$VERSION"
  sum="$(sha256 linux-amd64)"
  printf '%s  linux-amd64\n' "$sum" > linux-amd64.sha256
  printf '{"version":"%s","artifacts":[{"os":"linux","arch":"amd64","file":"linux-amd64","sha256":"%s"}]}\n' \
    "$VERSION" "$sum" > manifest.json
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
