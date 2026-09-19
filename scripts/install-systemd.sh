#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ─────────────────────────────────────────────────────────────
# Install / enable rivorad as a systemd service with a publicly
# reachable API (0.0.0.0:9870). Intended for lab remote access.
#
# The API and web console are bound to every interface, so an API key is
# mandatory: the script refuses to run without one. Provide it either in the
# environment (RIVORA_API_KEY=... ./install-systemd.sh) or already present in
# /etc/rivora/rivorad.env.
# ─────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

ENV_FILE=/etc/rivora/rivorad.env
has_key() {
    $SUDO test -f "$ENV_FILE" && $SUDO grep -qE '^[[:space:]]*RIVORA_API_KEY=[^[:space:]]+' "$ENV_FILE"
}
if ! has_key && [ -z "${RIVORA_API_KEY:-}" ]; then
    echo "refusing to expose the rivorad API on 0.0.0.0 without authentication." >&2
    echo "Set a key, then re-run:  RIVORA_API_KEY=\$(openssl rand -hex 24) $0" >&2
    echo "(or put RIVORA_API_KEY=... in ${ENV_FILE})" >&2
    exit 1
fi

$SUDO mkdir -p /etc/rivora /usr/local/share/rivora/bpf

if [ ! -x /usr/local/bin/rivorad ]; then
    echo "rivorad not installed under /usr/local/bin — run deploy/build first" >&2
    exit 1
fi

if [ ! -f /etc/rivora/config.yaml ]; then
    $SUDO install -m644 "${PROJECT_DIR}/config/examples/remote-api.yaml" /etc/rivora/config.yaml
else
    # Ensure the API is reachable from other hosts (idempotent edit).
    if grep -qE '^apiListen:\s*127\.0\.0\.1:' /etc/rivora/config.yaml; then
        $SUDO sed -i 's/^apiListen:.*/apiListen: 0.0.0.0:9870/' /etc/rivora/config.yaml
    fi
fi

if [ ! -f /etc/rivora/rivorad.env ]; then
    $SUDO install -m600 "${PROJECT_DIR}/deploy/systemd/rivorad.env.example" /etc/rivora/rivorad.env
fi
# Persist the key from the environment if the env file doesn't have one yet.
# Written via stdin so it never appears in the process list.
if ! has_key; then
    printf 'RIVORA_API_KEY=%s\n' "${RIVORA_API_KEY}" | $SUDO tee -a "$ENV_FILE" >/dev/null
    $SUDO chmod 600 "$ENV_FILE"
fi
# Ensure self-signed HTTPS for remote laptop access (idempotent).
if ! $SUDO grep -qE '^[[:space:]]*RIVORA_TLS_SELF_SIGNED=' /etc/rivora/rivorad.env; then
    echo 'RIVORA_TLS_SELF_SIGNED=1' | $SUDO tee -a /etc/rivora/rivorad.env >/dev/null
    $SUDO chmod 600 /etc/rivora/rivorad.env
elif $SUDO grep -qE '^[[:space:]]*RIVORA_TLS_SELF_SIGNED=$' /etc/rivora/rivorad.env || \
     $SUDO grep -qE '^[[:space:]]*RIVORA_TLS_SELF_SIGNED=0[[:space:]]*$' /etc/rivora/rivorad.env; then
    $SUDO sed -i 's/^[[:space:]]*RIVORA_TLS_SELF_SIGNED=.*/RIVORA_TLS_SELF_SIGNED=1/' /etc/rivora/rivorad.env
fi

$SUDO install -m644 "${PROJECT_DIR}/deploy/systemd/rivorad.service" /etc/systemd/system/rivorad.service
$SUDO systemctl daemon-reload
$SUDO systemctl enable rivorad.service
$SUDO systemctl restart rivorad.service

# Best-effort: open TCP/9870 if ufw is active.
if command -v ufw >/dev/null 2>&1 && $SUDO ufw status 2>/dev/null | grep -qi 'Status: active'; then
    $SUDO ufw allow 9870/tcp comment 'rivorad API' || true
fi

HOST_IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
[ -z "${HOST_IP}" ] && HOST_IP="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}')"

echo ""
echo "rivorad systemd: $($SUDO systemctl is-active rivorad)"
echo "UX URL:  https://${HOST_IP:-<host>}:9870/"
echo "API:     https://${HOST_IP:-<host>}:9870/api/v1/status"
echo "  (self-signed — accept the browser warning, or curl -k)"
echo "Auth:    Bearer token = RIVORA_API_KEY from ${ENV_FILE} (paste it into the console login)"
echo ""
