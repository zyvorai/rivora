#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ─────────────────────────────────────────────────────────────
# Install the `rivora` cluster-lifecycle CLI (cmd/rivora) from a GitHub
# Release — detects OS/arch, downloads the matching tarball + checksum,
# verifies, and installs to $INSTALL_DIR (default /usr/local/bin).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/zyvorai/rivora/main/scripts/install-cli.sh | bash
#   ./install-cli.sh --version v0.3.0 --install-dir ~/bin
# ─────────────────────────────────────────────────────────────
set -euo pipefail

REPO="zyvorai/rivora"
VERSION=""
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

usage() {
    cat <<EOF
Install the rivora CLI.

Usage: $0 [--version vX.Y.Z] [--install-dir DIR]

Options:
  --version vX.Y.Z    Install this release instead of the latest
  --install-dir DIR   Install location (default: \$INSTALL_DIR or /usr/local/bin)
  -h, --help          Show this help
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version) VERSION="$2"; shift 2 ;;
        --install-dir) INSTALL_DIR="$2"; shift 2 ;;
        -h|--help) usage; exit 0 ;;
        *) echo "unknown argument: $1" >&2; usage; exit 1 ;;
    esac
done

_use_color() { [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; }
if _use_color; then
    C_OK=$'\033[32m'; C_FAIL=$'\033[31m'; C_INFO=$'\033[36m'; C_RST=$'\033[0m'
else
    C_OK= C_FAIL= C_INFO= C_RST=
fi
ok()   { echo "${C_OK}  [ok] $*${C_RST}"; }
fail() { echo "${C_FAIL}  [fail] $*${C_RST}" >&2; exit 1; }
info() { echo "${C_INFO}  [info] $*${C_RST}"; }

case "$(uname -s)" in
    Linux)  GOOS=linux ;;
    Darwin) GOOS=darwin ;;
    *) fail "unsupported OS: $(uname -s) — rivora CLI releases cover linux and darwin only" ;;
esac
case "$(uname -m)" in
    x86_64|amd64)  GOARCH=amd64 ;;
    arm64|aarch64) GOARCH=arm64 ;;
    *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ -z "$VERSION" ]; then
    info "resolving latest release..."
    VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep -o '"tag_name": *"[^"]*"' | head -1 | cut -d'"' -f4)"
    [ -n "$VERSION" ] || fail "could not resolve the latest release — pass --version vX.Y.Z"
fi

NAME="rivora_${VERSION}_${GOOS}_${GOARCH}"
BASE_URL="https://github.com/${REPO}/releases/download/${VERSION}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

info "downloading ${NAME}.tar.gz (${VERSION})..."
curl -fsSL "${BASE_URL}/${NAME}.tar.gz" -o "${TMP_DIR}/${NAME}.tar.gz" \
    || fail "download failed — is ${VERSION} a real release? see https://github.com/${REPO}/releases"
curl -fsSL "${BASE_URL}/${NAME}.tar.gz.sha256" -o "${TMP_DIR}/${NAME}.tar.gz.sha256" \
    || fail "checksum download failed"

info "verifying checksum..."
(
    cd "$TMP_DIR"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum -c "${NAME}.tar.gz.sha256"
    else
        # macOS has no sha256sum by default — shasum -a 256 is the portable
        # equivalent, but its -c wants a "<hash>  <file>" line with a
        # relative filename matching what's on disk, same format ours is in.
        shasum -a 256 -c "${NAME}.tar.gz.sha256"
    fi
) || fail "checksum verification failed — do not trust this download"
ok "checksum verified"

tar xzf "${TMP_DIR}/${NAME}.tar.gz" -C "$TMP_DIR"

if [ -w "$INSTALL_DIR" ]; then
    install -m755 "${TMP_DIR}/${NAME}/rivora" "${INSTALL_DIR}/rivora"
else
    info "sudo required to write to ${INSTALL_DIR}"
    sudo install -m755 "${TMP_DIR}/${NAME}/rivora" "${INSTALL_DIR}/rivora"
fi

ok "installed ${INSTALL_DIR}/rivora ($("${INSTALL_DIR}/rivora" version 2>/dev/null || echo "$VERSION"))"
echo "Next: rivora install --set rivorad.interface=<iface>"
