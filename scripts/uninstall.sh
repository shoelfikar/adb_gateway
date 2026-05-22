#!/usr/bin/env bash
# Remove the adb-gateway systemd service. Preserves /etc/adb-gateway and
# /var/lib/adb-gateway by default — pass --purge to delete them as well.
set -euo pipefail

PURGE=0
for arg in "$@"; do
    case "$arg" in
        --purge) PURGE=1 ;;
        *) echo "Unknown arg: $arg" >&2; exit 2 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "ERROR: uninstall.sh must be run as root (try: sudo ./uninstall.sh)" >&2
    exit 1
fi

if systemctl list-unit-files adb-gateway.service >/dev/null 2>&1; then
    systemctl stop adb-gateway.service || true
    systemctl disable adb-gateway.service || true
fi
rm -f /etc/systemd/system/adb-gateway.service
systemctl daemon-reload

rm -f /usr/local/bin/adb-gateway

if [ "$PURGE" -eq 1 ]; then
    rm -rf /etc/adb-gateway /var/lib/adb-gateway
    if id -u adb-gateway >/dev/null 2>&1; then
        userdel adb-gateway || true
    fi
    if getent group adb-gateway >/dev/null; then
        groupdel adb-gateway || true
    fi
    echo "Purged config, state, user, and group."
else
    echo "Removed binary and service unit. Config preserved at /etc/adb-gateway."
    echo "Re-run with --purge to delete config, state, user, and group."
fi
