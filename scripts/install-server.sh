#!/usr/bin/env bash
# Install the rewardd server as a hardened systemd service on Linux.
#
# Usage (run as root):
#   sudo ./scripts/install-server.sh [path-to-rewardd-server-binary]
#
# If no binary path is given it looks for ./dist/rewardd-server-linux-amd64,
# then ./bin/rewardd-server. It installs the binary, a dedicated system user,
# the env file (without clobbering an existing one), and the systemd unit.
set -euo pipefail

PREFIX=/usr/local/bin
CONF_DIR=/etc/rewardd
STATE_DIR=/var/lib/rewardd
UNIT=/etc/systemd/system/rewardd-server.service
SERVICE_USER=rewardd

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ $EUID -ne 0 ]]; then
  echo "error: run as root (sudo $0)" >&2
  exit 1
fi

# Locate the binary to install.
BIN="${1:-}"
if [[ -z "$BIN" ]]; then
  for c in "$here/dist/rewardd-server-linux-amd64" "$here/bin/rewardd-server"; do
    [[ -x "$c" ]] && BIN="$c" && break
  done
fi
if [[ -z "$BIN" || ! -f "$BIN" ]]; then
  echo "error: server binary not found. Build it first ('make server') or pass its path." >&2
  exit 1
fi

echo "==> creating system user '$SERVICE_USER'"
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi

echo "==> installing binary to $PREFIX/rewardd-server"
install -m 0755 "$BIN" "$PREFIX/rewardd-server"

echo "==> creating $CONF_DIR and $STATE_DIR"
install -d -m 0755 "$CONF_DIR"
install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0750 "$STATE_DIR"

if [[ ! -f "$CONF_DIR/server.env" ]]; then
  echo "==> installing default env file (EDIT IT: set REWARDD_TOKEN)"
  install -m 0600 "$here/build/systemd/server.env.example" "$CONF_DIR/server.env"
  # Generate a strong token so a fresh install is not left with the placeholder.
  if command -v openssl >/dev/null 2>&1; then
    tok="$(openssl rand -hex 32)"
    sed -i "s|^REWARDD_TOKEN=.*|REWARDD_TOKEN=$tok|" "$CONF_DIR/server.env"
    echo "    generated REWARDD_TOKEN: $tok"
  fi
  chmod 0600 "$CONF_DIR/server.env"
else
  echo "==> keeping existing $CONF_DIR/server.env"
fi

echo "==> installing systemd unit"
install -m 0644 "$here/build/systemd/rewardd-server.service" "$UNIT"

echo "==> enabling and starting service"
systemctl daemon-reload
systemctl enable --now rewardd-server.service

echo
echo "Done. rewardd-server is running."
echo "  status:  systemctl status rewardd-server"
echo "  logs:    journalctl -u rewardd-server -f"
echo "  config:  $CONF_DIR/server.env  (edit then: systemctl restart rewardd-server)"
