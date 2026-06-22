#!/bin/bash
# Stop + remove the strand web UI LaunchAgent. Logs are preserved.
set -euo pipefail

LABEL="com.strand.web"
PLIST_DST="$HOME/Library/LaunchAgents/${LABEL}.plist"

if launchctl list "$LABEL" &>/dev/null; then
    echo "stopping $LABEL..."
    launchctl bootout "gui/$(id -u)" "$PLIST_DST" 2>/dev/null || true
fi

if [ -f "$PLIST_DST" ]; then
    rm "$PLIST_DST"
    echo "removed $PLIST_DST"
else
    echo "no plist found at $PLIST_DST"
fi

echo "uninstalled $LABEL"
echo "  logs preserved at: ~/.strand/logs/serve.log"
