#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest.sh — Functional verification for Rivora's DSR and full-NAT modes.
#
# Builds an isolated topology (fresh network namespaces + veths + a bridge
# device created just for this test, never a host/production interface),
# runs rivorad against it, drives real TCP traffic through the VIP, and
# checks that:
#   - Maglev spreads connections across all healthy backends
#   - killing a backend removes it from rotation within the health-check
#     interval, and traffic keeps flowing through the survivors
#
# Must run as root (network namespaces, BPF program attach).
# Usage: sudo ./scripts/selftest.sh [--quick]
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
QUICK=false
[[ "${1:-}" == "--quick" ]] && QUICK=true

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

RIVORAD="$(command -v rivorad || echo "${ROOT}/bin/rivorad")"
RIVORACTL="$(command -v rivoractl || echo "${ROOT}/bin/rivoractl")"
RIVORA_DOCTOR="$(command -v rivora-doctor || echo "${ROOT}/bin/rivora-doctor")"
BPF_DIR="/usr/local/share/rivora/bpf"
[ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="${ROOT}/bpf"

section "Binaries"
[ -x "$RIVORAD" ] && pass "rivorad: $RIVORAD" || fail "rivorad not found"
[ -x "$RIVORACTL" ] && pass "rivoractl: $RIVORACTL" || fail "rivoractl not found"
[ -x "$RIVORA_DOCTOR" ] && pass "rivora-doctor: $RIVORA_DOCTOR" || fail "rivora-doctor not found"
[ -f "${BPF_DIR}/xdp_ingress.o" ] && pass "xdp_ingress.o: ${BPF_DIR}/xdp_ingress.o" || fail "xdp_ingress.o missing"
[ -f "${BPF_DIR}/tc_nat.o" ] && pass "tc_nat.o: ${BPF_DIR}/tc_nat.o" || fail "tc_nat.o missing"

section "Host readiness (rivora-doctor)"
if [ -x "$RIVORA_DOCTOR" ]; then
    "$RIVORA_DOCTOR" --require-tcx || true
fi

if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "summary: pass=${PASS} fail=${FAIL} — skipping functional tests (missing binaries)"
    exit 1
fi

# ---------------------------------------------------------------------------
# Isolated topology: brtest0 (fresh bridge, not a host interface) + 4 netns.
#   client (10.77.0.2)  --\
#   be1    (10.77.0.11) ---+-- brtest0 --- lb (10.77.0.1, veth-lb)
#   be2    (10.77.0.12) --/
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="brtest${SUFFIX}"
NS_LB="riv-lb-${SUFFIX}"
NS_CLIENT="riv-client-${SUFFIX}"
NS_BE1="riv-be1-${SUFFIX}"
NS_BE2="riv-be2-${SUFFIX}"
VIP="10.77.0.100"
PORT="8080"
CONFIG="/tmp/rivora-selftest-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-selftest-${SUFFIX}-rivorad.log"
RIVORAD_PID=""
BE1_PID=""
BE2_PID=""

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    [ -n "$BE1_PID" ] && ip netns exec "$NS_BE1" kill "$BE1_PID" 2>/dev/null
    [ -n "$BE2_PID" ] && ip netns exec "$NS_BE2" kill "$BE2_PID" 2>/dev/null
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE1" "$NS_BE2"; do
        ip netns del "$ns" 2>/dev/null
    done
    ip link del "$BR" 2>/dev/null
    rm -f "$CONFIG"
}
trap cleanup EXIT

setup_topology() {
    # Defensive clean slate: an interrupted previous run (e.g. a killed SSH
    # session) can leave orphaned veth halves behind even though their
    # namespace is long gone — these are plain, fixed device names shared
    # by any run, not suffixed, so a stale one would collide with ours.
    for role in lb client be1 be2; do
        ip link del "veth-${role}" 2>/dev/null || true
        ip link del "veth-${role}-br" 2>/dev/null || true
    done

    ip link add "$BR" type bridge
    ip link set "$BR" up

    for pair in "lb:$NS_LB:10.77.0.1" "client:$NS_CLIENT:10.77.0.2" "be1:$NS_BE1:10.77.0.11" "be2:$NS_BE2:10.77.0.12"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "veth-${role}" type veth peer name "veth-${role}-br"
        ip link set "veth-${role}" netns "$ns"
        ip link set "veth-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "veth-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "veth-${role}"
    done

    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
}

backend_server() {
    cat <<'PYEOF'
import socket, sys
addr, port, ident = sys.argv[1], int(sys.argv[2]), sys.argv[3]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, port))
s.listen(16)
while True:
    conn, _ = s.accept()
    conn.sendall((ident + "\n").encode())
    conn.close()
PYEOF
}

client_probe() {
    # Usage: client_probe <vip> <port> <count> ; prints one backend id per line.
    cat <<'PYEOF'
import socket, sys
vip, port, count = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
for _ in range(count):
    try:
        s = socket.create_connection((vip, port), timeout=1.5)
        print(s.recv(64).decode().strip())
        s.close()
    except Exception as e:
        print("ERROR:" + str(e))
PYEOF
}

run_probes() {
    local count="$1"
    ip netns exec "$NS_CLIENT" python3 -c "$(client_probe)" "$VIP" "$PORT" "$count"
}

wait_for_api() {
    for _ in $(seq 1 20); do
        if ip netns exec "$NS_LB" curl -sf "http://127.0.0.1:9870/api/v1/status" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
    done
    return 1
}

test_mode() {
    local mode="$1"
    section "Functional: ${mode} mode"

    local be1_mac be2_mac
    be1_mac=$(ip netns exec "$NS_BE1" cat /sys/class/net/veth-be1/address)
    be2_mac=$(ip netns exec "$NS_BE2" cat /sys/class/net/veth-be2/address)

    if [ "$mode" = "dsr" ]; then
        # Classic DSR/LVS-DR setup: the VIP lives on lo (so the backend
        # accepts traffic addressed to it) but the backend must NOT answer
        # ARP for it on its real interface — otherwise, on a flat L2
        # segment like this test's bridge, the client would resolve the
        # VIP straight to a backend's MAC and traffic would bypass the LB
        # entirely, whether or not rivorad's forwarding is even correct.
        for ns in "$NS_BE1" "$NS_BE2"; do
            ip netns exec "$ns" sysctl -qw net.ipv4.conf.all.arp_ignore=1
            ip netns exec "$ns" sysctl -qw net.ipv4.conf.all.arp_announce=2
        done
        ip netns exec "$NS_BE1" ip addr add "${VIP}/32" dev lo
        ip netns exec "$NS_BE2" ip addr add "${VIP}/32" dev lo
        # The LB is the VIP's real owner on the wire (answers ARP, receives
        # the client's frames first) — same as a real DSR director node.
        ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev veth-lb
        cat > "$CONFIG" <<EOF
interface: veth-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: dsr, backends: [
      {address: 10.77.0.11, port: ${PORT}, mac: "${be1_mac}"},
      {address: 10.77.0.12, port: ${PORT}, mac: "${be2_mac}"}]}
EOF
        ip netns exec "$NS_BE1" python3 -c "$(backend_server)" "0.0.0.0" "$PORT" "BACKEND-1" >/dev/null 2>&1 &
        BE1_PID=$!
        ip netns exec "$NS_BE2" python3 -c "$(backend_server)" "0.0.0.0" "$PORT" "BACKEND-2" >/dev/null 2>&1 &
        BE2_PID=$!
    else
        ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev veth-lb
        # Force backend->client return traffic through the LB (its TCX
        # egress program un-NATs it there) even though client and backends
        # share this test's single L2 segment and could otherwise reach
        # each other directly, bypassing the LB entirely.
        ip netns exec "$NS_BE1" ip route add 10.77.0.2/32 via 10.77.0.1 dev veth-be1
        ip netns exec "$NS_BE2" ip route add 10.77.0.2/32 via 10.77.0.1 dev veth-be2
        cat > "$CONFIG" <<EOF
interface: veth-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.77.0.11, port: ${PORT}},
      {address: 10.77.0.12, port: ${PORT}}]}
EOF
        ip netns exec "$NS_BE1" python3 -c "$(backend_server)" "10.77.0.11" "$PORT" "BACKEND-1" >/dev/null 2>&1 &
        BE1_PID=$!
        ip netns exec "$NS_BE2" python3 -c "$(backend_server)" "10.77.0.12" "$PORT" "BACKEND-2" >/dev/null 2>&1 &
        BE2_PID=$!
    fi
    sleep 0.3

    # `ip netns exec` gives each invocation its own private, ephemeral mount
    # namespace with /sys freshly remounted — the host's bpffs mount at
    # /sys/fs/bpf isn't visible there. Mount it in the *same* invocation
    # that execs rivorad (a separate `ip netns exec ... mount` beforehand
    # wouldn't persist: each call gets its own throwaway mount namespace).
    ip netns exec "$NS_LB" bash -c \
        "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" \
        >"$RIVORAD_LOG" 2>&1 &
    RIVORAD_PID=$!
    sleep 1

    if ! kill -0 "$RIVORAD_PID" 2>/dev/null; then
        fail "${mode}: rivorad exited immediately — see ${RIVORAD_LOG}"
        tail -n 40 "$RIVORAD_LOG" || true
        RIVORAD_PID=""
        ip netns exec "$NS_BE1" kill "$BE1_PID" 2>/dev/null; BE1_PID=""
        ip netns exec "$NS_BE2" kill "$BE2_PID" 2>/dev/null; BE2_PID=""
        return
    fi

    if wait_for_api; then
        pass "${mode}: rivorad API is up"
    else
        fail "${mode}: rivorad API did not come up"
    fi

    local results seen1 seen2
    results=$(run_probes 20)
    seen1=$(grep -c "BACKEND-1" <<<"$results")
    seen2=$(grep -c "BACKEND-2" <<<"$results")
    if [ "$seen1" -gt 0 ] && [ "$seen2" -gt 0 ]; then
        pass "${mode}: Maglev spread traffic across both backends (${seen1}/${seen2} of 20)"
    else
        fail "${mode}: traffic did not reach both backends (${seen1}/${seen2} of 20) — results: $(echo "$results" | tr '\n' ' ')"
    fi

    ip netns exec "$NS_BE2" kill "$BE2_PID" 2>/dev/null
    BE2_PID=""
    sleep 3 # > failThreshold * interval

    results=$(run_probes 10)
    seen1=$(grep -c "BACKEND-1" <<<"$results")
    seen2=$(grep -c "BACKEND-2" <<<"$results")
    if [ "$seen1" -eq 10 ] && [ "$seen2" -eq 0 ]; then
        pass "${mode}: failover removed BACKEND-2 from rotation, all traffic reached BACKEND-1"
    else
        fail "${mode}: expected all 10 probes on BACKEND-1 after failover, got ${seen1}/${seen2} — results: $(echo "$results" | tr '\n' ' ')"
    fi

    kill "$RIVORAD_PID" 2>/dev/null
    wait "$RIVORAD_PID" 2>/dev/null
    RIVORAD_PID=""
    ip netns exec "$NS_BE1" kill "$BE1_PID" 2>/dev/null
    BE1_PID=""
    ip netns exec "$NS_LB" ip addr flush dev veth-lb 2>/dev/null
    ip netns exec "$NS_LB" ip addr add "10.77.0.1/24" dev veth-lb 2>/dev/null
    ip netns exec "$NS_BE1" ip addr del "${VIP}/32" dev lo 2>/dev/null
    ip netns exec "$NS_BE2" ip addr del "${VIP}/32" dev lo 2>/dev/null
    rm -f "$RIVORAD_LOG"
}

setup_topology
test_mode dsr
if [ "$QUICK" != true ]; then
    test_mode nat
fi

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
