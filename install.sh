#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$HOME/.local/bin"
SERVICE_DIR="$HOME/.config/systemd/user"
SERVICE_FILE="$SERVICE_DIR/steamx.service"
GITHUB_REPO="cdleveille/steamx"
RELEASE_BINARY_URL="https://github.com/${GITHUB_REPO}/releases/latest/download/steamx"

if [ "$EUID" -eq 0 ]; then
    echo "Error: run this script as your normal user, not as root." >&2
    exit 1
fi

cd "$SCRIPT_DIR"

# ── 1. Obtain binary ──────────────────────────────────────────────────────────
mkdir -p dist
echo "How would you like to install the steamx binary?"
echo "  1) Download the latest release binary from GitHub (default)"
echo "  2) Build from source"
read -r -p "Select an option [1]: " install_method
install_method="${install_method:-1}"

if [ "$install_method" = "2" ]; then
    if ! command -v go &>/dev/null; then
        echo "Error: go is required to build from source. Install go, or re-run and choose to download the release binary." >&2
        echo "https://go.dev" >&2
        exit 1
    fi

    echo "Compiling from source..."
    CGO_ENABLED=0 go build -C src -trimpath -ldflags="-s -w" -o "$SCRIPT_DIR/dist/steamx" .
else
    if ! command -v curl &>/dev/null; then
        echo "Error: curl is required to download the release binary. Install curl, or re-run and choose to build from source." >&2
        exit 1
    fi

    echo "Downloading latest release binary from $RELEASE_BINARY_URL..."
    curl -fsSL "$RELEASE_BINARY_URL" -o "$SCRIPT_DIR/dist/steamx"
    chmod +x "$SCRIPT_DIR/dist/steamx"
fi

# ── 2. Install binary ─────────────────────────────────────────────────────────
echo "Installing binary to $BIN_DIR..."
mkdir -p "$BIN_DIR"
install -m755 dist/steamx "$BIN_DIR/steamx"

# ── 3. Create systemd user service ────────────────────────────────────────────
# steamx is a tray app, so it runs with the desktop session: it starts when you
# log in and stops (handing the controller back to Steam) when you log out.
echo "Creating systemd user service at $SERVICE_FILE..."
mkdir -p "$SERVICE_DIR"
cat > "$SERVICE_FILE" <<'EOF'
[Unit]
Description=steamx (Steam Controller XInput mode)
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%h/.local/bin/steamx
Restart=on-failure
RestartSec=5

[Install]
WantedBy=graphical-session.target
EOF

# ── 4. Enable and (re)start the service ───────────────────────────────────────
echo "Enabling and starting steamx.service..."
systemctl --user daemon-reload
systemctl --user enable steamx
systemctl --user stop steamx
# A copy started by hand would hold steamx's single-instance lock.
pkill -x steamx && sleep 1
systemctl --user start steamx

echo ""
echo "Done. Check service status with:"
echo "  systemctl --user status steamx"
