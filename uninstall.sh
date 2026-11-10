#!/usr/bin/env bash
set -euo pipefail

BIN_FILE="$HOME/.local/bin/steamx"
SERVICE_FILE="$HOME/.config/systemd/user/steamx.service"

# ── 1. Stop and disable the systemd service ───────────────────────────────────
# Stopping steamx hands any controller that is in XInput mode back to Steam.
if systemctl --user is-active --quiet steamx 2>/dev/null; then
    echo "Stopping steamx service..."
    systemctl --user stop steamx
fi

if systemctl --user is-enabled --quiet steamx 2>/dev/null; then
    echo "Disabling steamx service..."
    systemctl --user disable steamx
fi

if pkill -x steamx; then
    echo "Stopped a copy of steamx that was started by hand."
fi

# ── 2. Remove the service unit file ───────────────────────────────────────────
if [ -f "$SERVICE_FILE" ]; then
    echo "Removing $SERVICE_FILE..."
    rm -f "$SERVICE_FILE"
    systemctl --user daemon-reload
fi

# ── 3. Remove the binary ──────────────────────────────────────────────────────
if [ -f "$BIN_FILE" ]; then
    echo "Removing $BIN_FILE..."
    rm -f "$BIN_FILE"
fi

echo ""
echo "Uninstall complete."
