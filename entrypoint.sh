#!/bin/bash
set -euo pipefail

: "${BROWSER_HOME:=/home/chromium}"
: "${VNC_PASSWORD:=}"

if [[ -z "$VNC_PASSWORD" ]]; then
    echo "ERROR: VNC_PASSWORD must be set" >&2
    exit 64
fi

mkdir -p "$BROWSER_HOME/profile" "$BROWSER_HOME/.vnc" /tmp/chromium-runtime /tmp/supervisor

# Container hostname changes on every recreate, so Singleton* symlinks from a
# previous container look like locks held "on another computer" and chromium
# refuses to start. No other chromium runs at this point, so they are always stale.
rm -f "$BROWSER_HOME/profile"/SingletonLock \
    "$BROWSER_HOME/profile"/SingletonCookie \
    "$BROWSER_HOME/profile"/SingletonSocket

x11vnc -storepasswd "$VNC_PASSWORD" "$BROWSER_HOME/.vnc/passwd" >/dev/null
chmod 600 "$BROWSER_HOME/.vnc/passwd"

exec /usr/bin/supervisord -n -c /etc/supervisor/supervisord.conf
