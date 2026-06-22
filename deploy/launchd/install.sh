#!/bin/bash
# Install + load the strand web UI LaunchAgent (macOS).
# Modeled on trixi's deploy/launchd/install.sh — no secrets/DB, strand is a
# local web server that shells out to `bd`.
#
# Usage: install.sh [addr]
#   addr defaults to 127.0.0.1:7777 (override or set STRAND_ADDR).
set -euo pipefail

[ "$(uname -s)" = "Darwin" ] || { echo "install.sh: macOS-only (uses launchctl)" >&2; exit 1; }

LABEL="com.strand.web"
HERE="$(cd "$(dirname "$0")" && pwd)"
PLIST_SRC="${HERE}/${LABEL}.plist"
PLIST_DST="$HOME/Library/LaunchAgents/${LABEL}.plist"
LOG_DIR="$HOME/.strand/logs"
ADDR="${1:-${STRAND_ADDR:-127.0.0.1:7777}}"

# Require the strand binary — resolve full path for the plist.
STRAND_BIN="$(command -v strand)" || {
    echo "error: strand not found in PATH" >&2
    echo "run: make install" >&2
    exit 1
}

mkdir -p "$LOG_DIR" "$HOME/Library/LaunchAgents"

# Unload existing agent if loaded.
if launchctl list "$LABEL" &>/dev/null; then
    echo "unloading existing $LABEL..."
    launchctl bootout "gui/$(id -u)" "$PLIST_DST" 2>/dev/null || true
fi

# Rotate serve.log now that the daemon is down — the one window with no writer
# (see rotate-log.sh). Best-effort: a rotation failure must not abort the load.
bash "${HERE}/rotate-log.sh" "$LOG_DIR/serve.log" \
    || echo "warning: serve.log rotation failed; continuing install" >&2

# Generate plist from template — substitute non-secret placeholders. Pin PATH so
# the launchd daemon (which inherits a minimal PATH) can find `bd` and friends.
sed -e "s|__REPLACE_STRAND_BIN__|${STRAND_BIN}|" \
    -e "s|__REPLACE_ADDR__|${ADDR}|" \
    -e "s|__REPLACE_LOG_DIR__|${LOG_DIR}|" \
    -e "s|__REPLACE_PATH__|$(dirname "$STRAND_BIN"):${HOME}/go/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin|" \
    "$PLIST_SRC" > "$PLIST_DST"
chmod 600 "$PLIST_DST"

# Load agent.
launchctl bootstrap "gui/$(id -u)" "$PLIST_DST"

# Wait for the server to answer (up to 10s).
echo -n "waiting for strand..."
for i in $(seq 1 20); do
    if curl -sf "http://${ADDR}/" >/dev/null 2>&1; then
        echo " ready."
        break
    fi
    sleep 0.5
    if [ "$i" -eq 20 ]; then
        echo " timed out (check $LOG_DIR/serve.log)" >&2
        exit 1
    fi
done

echo "installed and started $LABEL"
echo "  binary: $STRAND_BIN"
echo "  addr:   http://${ADDR}"
echo "  logs:   $LOG_DIR/serve.log"
echo "  status: launchctl list $LABEL"
