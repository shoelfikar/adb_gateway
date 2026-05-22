#!/usr/bin/env bash
# One-liner installer for adb-gateway.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/shoelfikar/adb_gateway/main/scripts/get.sh | sudo bash
#
# Pin a specific version:
#   curl -fsSL https://raw.githubusercontent.com/shoelfikar/adb_gateway/main/scripts/get.sh \
#     | sudo VERSION=v0.1.0 bash
#
# What it does:
#   1. Detect OS + arch.
#   2. Resolve the version to install (env override > latest GitHub release).
#   3. Download the matching release tarball + checksums.txt.
#   4. Verify SHA-256.
#   5. Extract to a temp dir, run install.sh, clean up.

set -euo pipefail

OWNER="${ADB_GW_OWNER:-shoelfikar}"
REPO="${ADB_GW_REPO:-adb_gateway}"
VERSION="${VERSION:-}"
TMPDIR_PATH=""

log()   { printf "\033[1;36m[get.sh]\033[0m %s\n" "$*"; }
warn()  { printf "\033[1;33m[get.sh]\033[0m %s\n" "$*" >&2; }
die()   { printf "\033[1;31m[get.sh]\033[0m %s\n" "$*" >&2; exit 1; }

cleanup() {
    if [ -n "$TMPDIR_PATH" ] && [ -d "$TMPDIR_PATH" ]; then
        rm -rf "$TMPDIR_PATH"
    fi
}
trap cleanup EXIT

require_cmd() {
    command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "this installer must be run as root (try: curl ... | sudo bash)"
    fi
}

detect_platform() {
    local os arch
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    case "$os" in
        linux)  ;;
        darwin) die "macOS install via get.sh not supported (no systemd). Download the darwin archive manually." ;;
        *)      die "unsupported OS: $os" ;;
    esac

    arch=$(uname -m)
    case "$arch" in
        x86_64|amd64) arch=amd64 ;;
        aarch64|arm64) arch=arm64 ;;
        *) die "unsupported architecture: $arch" ;;
    esac

    PLATFORM="${os}_${arch}"
}

resolve_version() {
    if [ -n "$VERSION" ]; then
        log "using pinned version: $VERSION"
        return
    fi
    log "resolving latest release from github.com/$OWNER/$REPO ..."
    # GitHub redirects /releases/latest to /releases/tag/vX.Y.Z — grab the tag
    # from the Location header. No jq, no auth, works on a fresh Ubuntu.
    local url
    url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$OWNER/$REPO/releases/latest") \
        || die "failed to query latest release"
    VERSION="${url##*/}"
    if [ -z "$VERSION" ] || [ "$VERSION" = "latest" ]; then
        die "could not resolve latest release tag (got: '$VERSION'). Set VERSION=vX.Y.Z explicitly."
    fi
    log "latest release: $VERSION"
}

download_and_verify() {
    local archive="adb-gateway_${VERSION}_${PLATFORM}.tar.gz"
    local base="https://github.com/$OWNER/$REPO/releases/download/$VERSION"

    TMPDIR_PATH=$(mktemp -d)
    cd "$TMPDIR_PATH"

    log "downloading $archive ..."
    curl -fsSL -o "$archive" "$base/$archive" \
        || die "failed to download $base/$archive"

    log "downloading checksums.txt ..."
    curl -fsSL -o checksums.txt "$base/checksums.txt" \
        || die "failed to download $base/checksums.txt"

    log "verifying SHA-256 ..."
    # `sha256sum -c --ignore-missing` exits 0 only if our archive's line passes.
    if ! sha256sum --check --ignore-missing --quiet checksums.txt; then
        die "checksum mismatch — archive may be corrupted or tampered with"
    fi
    log "checksum OK"

    log "extracting ..."
    tar xzf "$archive"

    EXTRACT_DIR="$TMPDIR_PATH/adb-gateway_${VERSION}_${PLATFORM}"
    if [ ! -d "$EXTRACT_DIR" ]; then
        # Fallback: some archive layouts extract flat. Find install.sh.
        EXTRACT_DIR=$(dirname "$(find "$TMPDIR_PATH" -name install.sh -print -quit)")
    fi
    [ -x "$EXTRACT_DIR/install.sh" ] || die "install.sh not found in archive"
}

run_installer() {
    log "running install.sh ..."
    cd "$EXTRACT_DIR"
    ./install.sh
}

main() {
    require_root
    require_cmd curl
    require_cmd tar
    require_cmd sha256sum
    require_cmd uname

    detect_platform
    resolve_version
    download_and_verify
    run_installer

    cat <<EOF

[get.sh] adb-gateway $VERSION installed.
         Next: edit /etc/adb-gateway/config.yaml (set api_key_primary)
               then: sudo systemctl restart adb-gateway
         Logs: journalctl -u adb-gateway -f
EOF
}

main "$@"
