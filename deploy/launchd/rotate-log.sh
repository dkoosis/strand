#!/bin/bash
# Rotate strand's serve.log.
#
# SAFE ONLY while the daemon is booted out. launchd opens StandardOutPath once,
# in append mode, and never reopens it — so the only moment serve.log has no
# writer is the gap between bootout and bootstrap. Call this from that window
# (see Makefile `deploy` and install.sh), never against a running daemon: a live
# fd keeps writing to the rotated inode, so any rename-based rotation outside
# this window silently does nothing. (modeled on trixi-b6x6)
set -euo pipefail

SERVE_LOG="${1:-$HOME/.strand/logs/serve.log}"
KEEP=5

[ -s "$SERVE_LOG" ] || exit 0 # nothing to rotate

echo "rotating $(du -h "$SERVE_LOG" | cut -f1) ${SERVE_LOG} (keep ${KEEP})..."
rm -f "${SERVE_LOG}.${KEEP}.gz"
for i in $(seq $((KEEP - 1)) -1 1); do
    [ -f "${SERVE_LOG}.${i}.gz" ] && mv "${SERVE_LOG}.${i}.gz" "${SERVE_LOG}.$((i + 1)).gz"
done
gzip -c "$SERVE_LOG" >"${SERVE_LOG}.1.gz"
: >"$SERVE_LOG" # truncate in place — keeps inode/perms; launchd appends to empty
