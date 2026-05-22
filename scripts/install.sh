#!/usr/bin/env bash
# Install adb-gateway as a systemd service on Ubuntu/Debian.
# Run from inside an extracted release tarball: `sudo ./install.sh`.
set -euo pipefail

SERVICE_USER="adb-gateway"
SERVICE_GROUP="adb-gateway"
BIN_DIR="/usr/local/bin"
CONFIG_DIR="/etc/adb-gateway"
STATE_DIR="/var/lib/adb-gateway"
UNIT_PATH="/etc/systemd/system/adb-gateway.service"

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        echo "ERROR: install.sh must be run as root (try: sudo ./install.sh)" >&2
        exit 1
    fi
}

require_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        echo "ERROR: systemctl not found — this installer targets systemd hosts." >&2
        exit 1
    fi
}

warn_if_no_adb() {
    if ! command -v adb >/dev/null 2>&1; then
        cat >&2 <<'EOF'
WARNING: `adb` binary not found on PATH.
adb-gateway connects to a local ADB server on localhost:5037; you must
install the Android platform-tools and ensure `adb start-server` runs
before the gateway. On Ubuntu: `sudo apt install adb`.
EOF
    fi
}

script_dir() {
    cd "$(dirname "$0")" && pwd
}

main() {
    require_root
    require_systemd
    warn_if_no_adb

    local src
    src="$(script_dir)"

    if [ ! -x "$src/adb-gateway" ]; then
        echo "ERROR: binary $src/adb-gateway not found or not executable." >&2
        exit 1
    fi
    if [ ! -f "$src/adb-gateway.service" ]; then
        echo "ERROR: $src/adb-gateway.service missing from the release archive." >&2
        exit 1
    fi

    # 1. Create system user/group (idempotent).
    if ! getent group "$SERVICE_GROUP" >/dev/null; then
        groupadd --system "$SERVICE_GROUP"
    fi
    if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
        useradd \
            --system \
            --gid "$SERVICE_GROUP" \
            --home-dir "$STATE_DIR" \
            --shell /usr/sbin/nologin \
            --comment "ADB Gateway service" \
            "$SERVICE_USER"
    fi

    # 2. Install binary.
    install -m 0755 -o root -g root "$src/adb-gateway" "$BIN_DIR/adb-gateway"

    # 3. Install config dir + seed config (do not overwrite an existing config).
    install -d -m 0750 -o root -g "$SERVICE_GROUP" "$CONFIG_DIR"
    if [ ! -f "$CONFIG_DIR/config.yaml" ]; then
        if [ -f "$src/config.yaml.example" ]; then
            install -m 0640 -o root -g "$SERVICE_GROUP" "$src/config.yaml.example" "$CONFIG_DIR/config.yaml"
            echo "Seeded $CONFIG_DIR/config.yaml from config.yaml.example — edit before starting!"
        else
            echo "WARNING: no config.yaml.example bundled; you must create $CONFIG_DIR/config.yaml manually." >&2
        fi
    else
        echo "Keeping existing $CONFIG_DIR/config.yaml (use config.yaml.example to diff against the new release)."
        if [ -f "$src/config.yaml.example" ]; then
            install -m 0640 -o root -g "$SERVICE_GROUP" "$src/config.yaml.example" "$CONFIG_DIR/config.yaml.example"
        fi
    fi

    # 4. Ship THIRD_PARTY_NOTICES (Apache-2.0 attribution for scrcpy server.jar).
    if [ -f "$src/THIRD_PARTY_NOTICES" ]; then
        install -m 0644 -o root -g root "$src/THIRD_PARTY_NOTICES" "$CONFIG_DIR/THIRD_PARTY_NOTICES"
    fi

    # 5. State dir for recordings/logs/runtime artifacts.
    install -d -m 0750 -o "$SERVICE_USER" -g "$SERVICE_GROUP" "$STATE_DIR"

    # 6. Install systemd unit.
    install -m 0644 -o root -g root "$src/adb-gateway.service" "$UNIT_PATH"

    # 7. Enable + (re)start.
    systemctl daemon-reload
    systemctl enable adb-gateway.service
    if systemctl is-active --quiet adb-gateway.service; then
        systemctl restart adb-gateway.service
        echo "adb-gateway restarted."
    else
        systemctl start adb-gateway.service
        echo "adb-gateway started."
    fi

    cat <<EOF

Install complete.
  Binary:  $BIN_DIR/adb-gateway
  Config:  $CONFIG_DIR/config.yaml
  Service: adb-gateway.service ($(systemctl is-enabled adb-gateway.service))
  Status:  systemctl status adb-gateway.service
  Logs:    journalctl -u adb-gateway -f

If you edited the config or env file, run: systemctl restart adb-gateway
EOF
}

main "$@"
