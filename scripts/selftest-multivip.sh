#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-multivip.sh — v0.2 foundation verification: two independent VIPs
# on one rivorad sharing the same physical backends (on different ports),
# and graceful draining (a backend excluded from *new* flow selection
# without disrupting its already-established connections).
#
# Builds the same kind of isolated netns/veth/bridge topology as
# selftest.sh, in full-NAT mode (simplest correct topology for two VIPs
# sharing one set of physical backends). Draining is exercised by writing
# backend_health_map directly via bpftool — the Kubernetes reconciler that
# will call Dataplane.SetBackendDraining doesn't exist yet (that's v0.2
# step 3); this validates the BPF-side 3-state health logic on its own,
# which is what step 1 owns.
#
# Must run as root. Usage: sudo ./scripts/selftest-multivip.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-multivip.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

RIVORAD="$(command -v rivorad || echo "${ROOT}/bin/rivorad")"
RIVORACTL="$(command -v rivoractl || echo "${ROOT}/bin/rivoractl")"
BPF_DIR="/usr/local/share/rivora/bpf"
[ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="${ROOT}/bpf"

SUFFIX="$$"
BR="brmvip${SUFFIX}"
NS_LB="riv-lb-mv-${SUFFIX}"
NS_CLIENT="riv-cl-mv-${SUFFIX}"
NS_BE1="riv-b1-mv-${SUFFIX}"
NS_BE2="riv-b2-mv-${SUFFIX}"
VIP_A="10.78.0.100"
VIP_B="10.78.0.101"
PORT_A="8080"
PORT_B="9090"
CONFIG="/tmp/rivora-mvtest-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-mvtest-${SUFFIX}-rivorad.log"
RIVORAD_PID=""
BE_PIDS=()

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for pid in "${BE_PIDS[@]:-}"; do
        [ -n "$pid" ] && { ip netns exec "$NS_BE1" kill "$pid" 2>/dev/null; ip netns exec "$NS_BE2" kill "$pid" 2>/dev/null; }
    done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE1" "$NS_BE2"; do
        ip netns del "$ns" 2>/dev/null
    done
    for role in lb client be1 be2; do
        ip link del "veth-${role}" 2>/dev/null
        ip link del "veth-${role}-br" 2>/dev/null
    done
    ip link del "$BR" 2>/dev/null
    rm -f "$CONFIG"
}
trap cleanup EXIT

setup_topology() {
    for role in lb client be1 be2; do
        ip link del "veth-${role}" 2>/dev/null || true
        ip link del "veth-${role}-br" 2>/dev/null || true
    done

    ip link add "$BR" type bridge
    ip link set "$BR" up

    for pair in "lb:$NS_LB:10.78.0.1" "client:$NS_CLIENT:10.78.0.2" "be1:$NS_BE1:10.78.0.11" "be2:$NS_BE2:10.78.0.12"; do
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
    ip netns exec "$NS_LB" ip addr add "${VIP_A}/32" dev veth-lb
    ip netns exec "$NS_LB" ip addr add "${VIP_B}/32" dev veth-lb
    # Force backend->client return traffic through the LB so tc_nat's TCX
    # egress program gets a chance to un-NAT it (see selftest.sh for why).
    ip netns exec "$NS_BE1" ip route add 10.78.0.2/32 via 10.78.0.1 dev veth-be1
    ip netns exec "$NS_BE2" ip route add 10.78.0.2/32 via 10.78.0.1 dev veth-be2
}

backend_server() {
    # Usage: backend_server <bind_addr> <port> <ident>
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

start_backend() {
    local ns="$1" addr="$2" port="$3" ident="$4"
    ip netns exec "$ns" python3 -c "$(backend_server)" "$addr" "$port" "$ident" >/dev/null 2>&1 &
    BE_PIDS+=("$!")
}

client_probe() {
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
    local vip="$1" port="$2" count="$3"
    ip netns exec "$NS_CLIENT" python3 -c "$(client_probe)" "$vip" "$port" "$count"
}

# backend_id_for <vip_addr> <backend_addr> <backend_port> -> backend_id,
# found via rivoractl's own API — the same thing an operator would use,
# and avoids bpftool's map enumeration (`map show`) entirely, which this
# test found unreliable across bpftool/kernel version combinations (CI's
# runner kernel has no matching bpftool package and warns-then-fails even
# on basic invocations). Only set_health() below still needs a raw bpftool
# map write, since there's no API for it yet — SetBackendDraining is wired
# up by the Kubernetes reconciler in a later v0.2 step, not this one.
backend_id_for() {
    local vip="$1" addr="$2" port="$3"
    ip netns exec "$NS_LB" "$RIVORACTL" vips --format json --api 127.0.0.1:9870 | python3 -c "
import json, sys
vip, addr, port = '$vip', '$addr', $port
for st in json.load(sys.stdin):
    if st['vipAddress'] != vip:
        continue
    for b in st['backends']:
        if b['address'] == addr and b['port'] == port:
            print(b['id']); sys.exit(0)
sys.exit(1)
"
}

# set_health writes backend_health_map directly via its bpffs pin —
# `bpftool map update pinned /sys/fs/bpf/rivora-lb/backend_health_map`.
# Two things make that path work reliably here, where a naive call from
# this script's own shell would not:
#
# 1. Mount namespace: rivorad runs via `ip netns exec ... bash -c "mount
#    -t bpf bpf /sys/fs/bpf; exec rivorad"` — that mount is private and
#    ephemeral, scoped to that one invocation. A *different* shell (this
#    script's own, or a fresh `ip netns exec`) sees a *different*
#    /sys/fs/bpf with none of rivorad's pins in it. `nsenter --mount
#    /proc/$RIVORAD_PID/ns/mnt` joins that exact same mount namespace
#    instead of creating another new one, so the pin path resolves.
#
# 2. `bpftool map show`'s global enumeration (the alternative,
#    kernel-ID-based approach this test used before) turned out to be
#    unreliable on CI's runner regardless of namespace or output
#    filtering — it returns no usable output there at all, on a kernel
#    with no matching bpftool package. Going straight to the pin path
#    sidesteps enumeration entirely, so that limitation doesn't apply.
set_health() {
    # backend_id is a little-endian __u32 key; this test only ever has a
    # handful of backends so it always fits in the first byte.
    local backend_id="$1" value="$2"
    nsenter --mount="/proc/${RIVORAD_PID}/ns/mnt" \
        bpftool map update pinned /sys/fs/bpf/rivora-lb/backend_health_map \
        key "$backend_id" 0 0 0 value "$value" >/dev/null
}

wait_for_api() {
    for _ in $(seq 1 20); do
        if ip netns exec "$NS_LB" curl -sf "http://127.0.0.1:9870/api/v1/vips" >/dev/null 2>&1; then
            return 0
        fi
        sleep 0.5
    done
    return 1
}

section "Binaries"
[ -x "$RIVORAD" ] && pass "rivorad: $RIVORAD" || fail "rivorad not found"
[ -x "$RIVORACTL" ] && pass "rivoractl: $RIVORACTL" || fail "rivoractl not found"
[ -f "${BPF_DIR}/xdp_ingress.o" ] && pass "xdp_ingress.o: ${BPF_DIR}/xdp_ingress.o" || fail "xdp_ingress.o missing"
[ -f "${BPF_DIR}/tc_nat.o" ] && pass "tc_nat.o: ${BPF_DIR}/tc_nat.o" || fail "tc_nat.o missing"
if [ "$FAIL" -gt 0 ]; then
    echo "summary: pass=${PASS} fail=${FAIL} — skipping functional tests (missing binaries)"
    exit 1
fi

setup_topology

start_backend "$NS_BE1" 10.78.0.11 "$PORT_A" "VIPA-BACKEND-1"
start_backend "$NS_BE1" 10.78.0.11 "$PORT_B" "VIPB-BACKEND-1"
start_backend "$NS_BE2" 10.78.0.12 "$PORT_A" "VIPA-BACKEND-2"
start_backend "$NS_BE2" 10.78.0.12 "$PORT_B" "VIPB-BACKEND-2"
sleep 0.3

cat > "$CONFIG" <<EOF
interface: veth-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 30s, timeout: 1s, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP_A}, port: ${PORT_A}, protocol: tcp, mode: nat, backends: [
      {address: 10.78.0.11, port: ${PORT_A}}, {address: 10.78.0.12, port: ${PORT_A}}]}
  - {address: ${VIP_B}, port: ${PORT_B}, protocol: tcp, mode: nat, backends: [
      {address: 10.78.0.11, port: ${PORT_B}}, {address: 10.78.0.12, port: ${PORT_B}}]}
EOF

ip netns exec "$NS_LB" bash -c \
    "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" \
    >"$RIVORAD_LOG" 2>&1 &
RIVORAD_PID=$!
sleep 1

if ! kill -0 "$RIVORAD_PID" 2>/dev/null; then
    fail "rivorad exited immediately — see ${RIVORAD_LOG}"
    tail -n 40 "$RIVORAD_LOG" || true
    echo "summary: pass=${PASS} fail=${FAIL}"
    exit 1
fi
wait_for_api && pass "rivorad API is up (two VIPs configured)" || fail "rivorad API did not come up"

section "Two independent VIPs"
resultsA=$(run_probes "$VIP_A" "$PORT_A" 20)
seenA1=$(grep -c "VIPA-BACKEND-1" <<<"$resultsA")
seenA2=$(grep -c "VIPA-BACKEND-2" <<<"$resultsA")
seenWrongVIPonA=$(grep -c "VIPB-" <<<"$resultsA")
if [ "$seenA1" -gt 0 ] && [ "$seenA2" -gt 0 ] && [ "$seenWrongVIPonA" -eq 0 ]; then
    pass "VIP A spread across both backends (${seenA1}/${seenA2} of 20), no cross-VIP leakage"
else
    fail "VIP A: ${seenA1}/${seenA2} of 20, cross-VIP leakage=${seenWrongVIPonA} — results: $(echo "$resultsA" | tr '\n' ' ')"
fi

resultsB=$(run_probes "$VIP_B" "$PORT_B" 20)
seenB1=$(grep -c "VIPB-BACKEND-1" <<<"$resultsB")
seenB2=$(grep -c "VIPB-BACKEND-2" <<<"$resultsB")
seenWrongVIPonB=$(grep -c "VIPA-" <<<"$resultsB")
if [ "$seenB1" -gt 0 ] && [ "$seenB2" -gt 0 ] && [ "$seenWrongVIPonB" -eq 0 ]; then
    pass "VIP B spread across both backends (${seenB1}/${seenB2} of 20), no cross-VIP leakage"
else
    fail "VIP B: ${seenB1}/${seenB2} of 20, cross-VIP leakage=${seenWrongVIPonB} — results: $(echo "$resultsB" | tr '\n' ' ')"
fi

section "Graceful draining (backend_health_map, direct BPF verification)"
id_a1=$(backend_id_for "$VIP_A" 10.78.0.11 "$PORT_A")
if [ -z "$id_a1" ]; then
    fail "could not find backend_map entry for 10.78.0.11:${PORT_A}"
else
    pass "found VIP A's backend-1 as backend_id=${id_a1}"
    set_health "$id_a1" 2 # RIVORA_HEALTH_DRAINING
    resultsDrain=$(run_probes "$VIP_A" "$PORT_A" 10)
    seenDrain1=$(grep -c "VIPA-BACKEND-1" <<<"$resultsDrain")
    seenDrain2=$(grep -c "VIPA-BACKEND-2" <<<"$resultsDrain")
    if [ "$seenDrain1" -eq 0 ] && [ "$seenDrain2" -eq 10 ]; then
        pass "draining backend-1 excluded from new VIP A flows, all 10 probes reached backend-2"
    else
        fail "expected 0/10 on draining backend-1, got ${seenDrain1}/${seenDrain2} — results: $(echo "$resultsDrain" | tr '\n' ' ')"
    fi

    resultsB2=$(run_probes "$VIP_B" "$PORT_B" 10)
    seenB2Drain1=$(grep -c "VIPB-BACKEND-1" <<<"$resultsB2")
    seenB2Drain2=$(grep -c "VIPB-BACKEND-2" <<<"$resultsB2")
    if [ "$seenB2Drain1" -gt 0 ] && [ "$seenB2Drain2" -gt 0 ]; then
        pass "VIP B unaffected by VIP A's backend draining (${seenB2Drain1}/${seenB2Drain2} of 10)"
    else
        fail "VIP B should be unaffected by VIP A's draining, got ${seenB2Drain1}/${seenB2Drain2} — results: $(echo "$resultsB2" | tr '\n' ' ')"
    fi
fi

kill "$RIVORAD_PID" 2>/dev/null
wait "$RIVORAD_PID" 2>/dev/null
RIVORAD_PID=""

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
