#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-ndp.sh — Functional verification for the L2 NDP speaker path
# (Neighbor Solicitation → Neighbor Advertisement for a NAT-mode IPv6 VIP).
# Delegates to internal/speaker.TestNDPRespondsOnVeth which needs root and
# a Linux veth pair (same privilege model as the other selftest scripts).
#
# Usage: sudo ./scripts/selftest-ndp.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-ndp.sh must run as root (veth + raw ICMPv6)." >&2
    exit 1
fi

cd "$ROOT"
echo "=== NDP speaker (veth) ==="
if go test ./internal/speaker -run '^TestNDPRespondsOnVeth$' -count=1 -timeout 60s; then
    echo "  [pass] NDP speaker answered Neighbor Solicitation for NAT IPv6 VIP"
    echo ""
    echo "summary: pass=1 fail=0"
    exit 0
fi
echo "  [FAIL] NDP speaker selftest"
echo ""
echo "summary: pass=0 fail=1"
exit 1
