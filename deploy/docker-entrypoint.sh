#!/bin/sh
# fobe image entrypoint (design.md §17, §4.4).
#
# Zero-input boot: FOBE_MASTER_KEY is the one credential the panel cannot
# start without (the server refuses to run without it and never degrades to
# plaintext, §4.4), so the entrypoint resolves a key in this order:
#
#   1. FOBE_MASTER_KEY env        — operator-managed (.env / compose env)
#   2. FOBE_MASTER_KEY_FILE       — Docker secret / bind-mounted file
#   3. generate 32 random bytes   — persisted at /data/.master_key (0600) so
#                                   restarts reuse the same key and already
#                                   encrypted settings stay readable
#
# The server's fail-closed semantics are untouched: if /data is not writable
# the generation fails, the exec never happens and the container exits —
# there is no plaintext fallback anywhere.

set -eu

if [ "${1:-}" = "fobe-server" ]; then
    shift
fi

# The admin CLI is the escape hatch that never needs the web (§4.1) — it does
# not require a master key, so it must also not require a writable /data.
# Skip the key dance for it instead of blocking recovery on generation.
if [ "${1:-}" != "admin" ]; then
    if [ -z "${FOBE_MASTER_KEY:-}" ] && [ -n "${FOBE_MASTER_KEY_FILE:-}" ]; then
        FOBE_MASTER_KEY="$(tr -d ' \t\r\n' < "$FOBE_MASTER_KEY_FILE")"
        export FOBE_MASTER_KEY
    fi

    if [ -z "${FOBE_MASTER_KEY:-}" ]; then
        KEY_FILE="${FOBE_MASTER_KEY_FILE:-/data/.master_key}"
        if [ -s "$KEY_FILE" ]; then
            FOBE_MASTER_KEY="$(tr -d ' \t\r\n' < "$KEY_FILE")"
        else
            # base64(32 bytes) is exactly what ParseMasterKey accepts (§4.4).
            FOBE_MASTER_KEY="$(head -c 32 /dev/urandom | base64)"
            mkdir -p "$(dirname "$KEY_FILE")"
            # Persisted next to the ciphertext it unlocks: acceptable for the
            # zero-input default (the operator physically controls this volume
            # anyway) — set FOBE_MASTER_KEY to keep the key elsewhere.
            ( umask 077 && printf '%s\n' "$FOBE_MASTER_KEY" > "$KEY_FILE" )
            echo "fobe: generated master key at $KEY_FILE (back this file up, or set FOBE_MASTER_KEY to manage the key yourself)" >&2
        fi
        export FOBE_MASTER_KEY
    fi
fi

# `docker run <image> admin …` and compose `command: admin …` both land here.
exec /usr/local/bin/fobe-server "$@"
