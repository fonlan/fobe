#!/usr/bin/env bash
# scripts/pgo.sh — produce cmd/server/default.pgo (design §17).
#
# Go applies that file automatically: `-pgo=auto` is the default and a profile
# named default.pgo sitting next to the main package is picked up by every
# build (scripts/build.sh, scripts/dev.sh, the Docker image), so nothing else
# has to pass -pgo around. The gain is typically a few percent of CPU.
#
# The profile is only as good as the workload it came from. The default mode
# profiles the Go test suite, which is a decent stand-in for a fresh checkout
# but NOT production traffic; once the panel has run for a while, re-collect
# with --url and commit the result.
#
#   scripts/pgo.sh                        # profile the test suite (no server needed)
#   scripts/pgo.sh ./internal/...          # ... over selected package patterns
#   scripts/pgo.sh --file a.pprof [--file b.pprof …]   # merge profiles you collected
#   scripts/pgo.sh --url http://127.0.0.1:6060 [--secs 30]
#
# A live profile needs the server to run with FOBE_PPROF (loopback only, §17).
# Inside the container there is no need to publish a port:
#   docker compose exec server wget -qO- \
#     'http://127.0.0.1:6060/debug/pprof/profile?seconds=30' > cpu.pprof
#   scripts/pgo.sh --file cpu.pprof
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$ROOT/cmd/server/default.pgo"
MODE=test
URL=""
SECS=30
# Package patterns and profile paths are space-separated on purpose: both are
# word-split when used, and neither may contain spaces.
PKGS="./internal/server/..."
FILES=""

usage() {
    sed -n '2,25p' "$0"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --url)  MODE=url;  URL="${2:-}"; [ -n "$URL" ] || { echo "pgo.sh: --url needs a base URL" >&2; exit 1; }; shift 2 ;;
        --secs) SECS="${2:-}"; [ -n "$SECS" ] || { echo "pgo.sh: --secs needs a number" >&2; exit 1; }; shift 2 ;;
        --file) MODE=file; shift; [ -n "${1:-}" ] || { echo "pgo.sh: --file needs a profile path" >&2; exit 1; }; FILES="$FILES $1"; shift ;;
        --out)  OUT="${2:-}"; [ -n "$OUT" ] || { echo "pgo.sh: --out needs a path" >&2; exit 1; }; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        -*) echo "pgo.sh: unknown option: $1" >&2; usage >&2; exit 1 ;;
        *)  PKGS="$1"; shift ;;
    esac
done

cd "$ROOT"
command -v go >/dev/null || { echo "pgo.sh: go not found" >&2; exit 1; }

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

case "$MODE" in
url)
    if command -v curl >/dev/null 2>&1; then FETCH="curl -fsS"; else FETCH="wget -qO-"; fi
    echo "==> collecting ${SECS}s CPU profile from $URL"
    # shellcheck disable=SC2086 # deliberate word split (curl/wget + flags)
    $FETCH "$URL/debug/pprof/profile?seconds=$SECS" > "$TMP/live.pprof"
    [ -s "$TMP/live.pprof" ] || { echo "pgo.sh: empty profile — is the server running with FOBE_PPROF=$URL?" >&2; exit 1; }
    ;;
file)
    n=0
    for f in $FILES; do
        [ -s "$f" ] || { echo "pgo.sh: no such profile: $f" >&2; exit 1; }
        # Normalized name: the merge below globs *.pprof, and the input's own
        # extension is whatever the operator's tooling produced (.pprof, .pb.gz,
        # or nothing at all).
        n=$((n + 1))
        cp "$f" "$TMP/f$n.pprof"
    done
    ;;
test)
    # Only packages that actually have tests can emit a profile.
    n=0
    for pkg in $(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' $PKGS); do
        n=$((n + 1))
        echo "==> profiling $pkg"
        if ! go test -count=1 -cpuprofile="$TMP/p$n.pprof" -o "$TMP/p$n.test" "$pkg" > "$TMP/p$n.log" 2>&1; then
            # A red test still yields a usable profile when the binary built at
            # all; a compile failure yields none, which the glob below catches.
            echo "    ($pkg: tests failed, profile kept — $TMP/p$n.log)"
        fi
    done
    [ "$n" -gt 0 ] || { echo "pgo.sh: no test packages matched: $PKGS" >&2; exit 1; }
    ;;
esac

profiles=""
for f in "$TMP"/*.pprof; do
    [ -s "$f" ] && profiles="$profiles $f"
done
[ -n "$profiles" ] || { echo "pgo.sh: no profile was produced" >&2; exit 1; }

mkdir -p "$(dirname "$OUT")"
# shellcheck disable=SC2086 # $profiles is a deliberate list
go tool pprof -proto -output="$OUT" $profiles

echo "==> $OUT ($(wc -c < "$OUT" | tr -d ' ') bytes)"
echo "    rebuild to apply it: scripts/build.sh, or docker compose build"
