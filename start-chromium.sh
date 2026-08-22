#!/bin/bash
set -euo pipefail

exec chromium \
    --display="$DISPLAY" \
    --user-data-dir="$BROWSER_HOME/profile" \
    --no-first-run \
    --no-default-browser-check \
    --disable-session-crashed-bubble \
    --remote-debugging-address=0.0.0.0 \
    --remote-debugging-port="$CDP_PORT" \
    --remote-allow-origins=* \
    --window-size="${SCREEN_WIDTH},${SCREEN_HEIGHT}" \
    "$START_URL"
