#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-weighted.sh — Functional verification for weighted Maglev
# backend selection.
#
# Builds the same isolated veth/netns/bridge topology as selftest.sh (never
# a host/production interface), runs rivorad in NAT mode against a single
# VIP with two backends weighted 9:1, drives a batch of connections through
# the VIP, and checks the observed split is clearly skewed toward the
# heavier-weighted backend — not exactly 90/10 (a single client's hash
# distribution over a modest sample isn't that precise), but decisively
# majority, not anywhere near an even 50/50 split.
#
# Must run as root (network namespaces, BPF program attach).
# Usage: sudo ./scripts/selftest-weighted.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-weighted.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

# Prefer this checkout's freshly-built binary/objects over anything
# already installed system-wide (e.g. from an earlier non-test deploy on
# a shared dev host) — this script exists to test the code just built,
# not whatever happens to already be on PATH.
RIVORAD="${ROOT}/bin/rivorad"
[ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
BPF_DIR="${ROOT}/bpf"
[ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"

section "Binaries"
[ -x "$RIVORAD" ] && pass "rivorad: $RIVORAD" || fail "rivorad not found"
[ -f "${BPF_DIR}/xdp_ingress.o" ] && pass "xdp_ingress.o: ${BPF_DIR}/xdp_ingress.o" || fail "xdp_ingress.o missing"
[ -f "${BPF_DIR}/tc_nat.o" ] && pass "tc_nat.o: ${BPF_DIR}/tc_nat.o" || fail "tc_nat.o missing"

if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "summary: pass=${PASS} fail=${FAIL} — skipping functional test (missing binaries)"
    exit 1
fi

# ---------------------------------------------------------------------------
# Isolated topology: brtest0 (fresh bridge, not a host interface) + 3 netns.
#   client (10.78.0.2)  --\
#   heavy  (10.78.0.11) ---+-- brtest0 --- lb (10.78.0.1, veth-lb)
#   light  (10.78.0.12) --/
# heavy carries weight 9, light carries weight 1.
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="wbrtest${SUFFIX}"
NS_LB="riv-wlb-${SUFFIX}"
NS_CLIENT="riv-wclient-${SUFFIX}"
NS_HEAVY="riv-wheavy-${SUFFIX}"
NS_LIGHT="riv-wlight-${SUFFIX}"
VIP="10.78.0.100"
PORT="8080"
CONFIG="/tmp/rivora-selftest-weighted-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-selftest-weighted-${SUFFIX}-rivorad.log"
RIVORAD_PID=""
HEAVY_PID=""
LIGHT_PID=""

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    [ -n "$HEAVY_PID" ] && ip netns exec "$NS_HEAVY" kill "$HEAVY_PID" 2>/dev/null
    [ -n "$LIGHT_PID" ] && ip netns exec "$NS_LIGHT" kill "$LIGHT_PID" 2>/dev/null
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_HEAVY" "$NS_LIGHT"; do
        ip netns del "$ns" 2>/dev/null
    done
    ip link del "$BR" 2>/dev/null
    rm -f "$CONFIG" "$RIVORAD_LOG"
}
trap cleanup EXIT

for role in lb client heavy light; do
    ip link del "wveth-${role}" 2>/dev/null || true
    ip link del "wveth-${role}-br" 2>/dev/null || true
done

ip link add "$BR" type bridge
ip link set "$BR" up

for pair in "lb:$NS_LB:10.78.0.1" "client:$NS_CLIENT:10.78.0.2" "heavy:$NS_HEAVY:10.78.0.11" "light:$NS_LIGHT:10.78.0.12"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
    ip netns add "$ns"
    ip link add "wveth-${role}" type veth peer name "wveth-${role}-br"
    ip link set "wveth-${role}" netns "$ns"
    ip link set "wveth-${role}-br" master "$BR" up
    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" ip link set "wveth-${role}" up
    ip netns exec "$ns" ip addr add "${ip_}/24" dev "wveth-${role}"
done

section "Functional: weighted NAT mode (9:1)"

ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev wveth-lb
ip netns exec "$NS_HEAVY" ip route add 10.78.0.2/32 via 10.78.0.1 dev wveth-heavy
ip netns exec "$NS_LIGHT" ip route add 10.78.0.2/32 via 10.78.0.1 dev wveth-light

cat > "$CONFIG" <<EOF
interface: wveth-lb
apiListen: 127.0.0.1:9871
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.78.0.11, port: ${PORT}, weight: 9},
      {address: 10.78.0.12, port: ${PORT}, weight: 1}]}
EOF

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

ip netns exec "$NS_HEAVY" python3 -c "$(backend_server)" "10.78.0.11" "$PORT" "HEAVY" >/dev/null 2>&1 &
HEAVY_PID=$!
ip netns exec "$NS_LIGHT" python3 -c "$(backend_server)" "10.78.0.12" "$PORT" "LIGHT" >/dev/null 2>&1 &
LIGHT_PID=$!
sleep 0.3

ip netns exec "$NS_LB" bash -c \
    "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" \
    >"$RIVORAD_LOG" 2>&1 &
RIVORAD_PID=$!
sleep 1

if ! kill -0 "$RIVORAD_PID" 2>/dev/null; then
    fail "rivorad exited immediately — see ${RIVORAD_LOG}"
    tail -n 40 "$RIVORAD_LOG" || true
    RIVORAD_PID=""
else
    for _ in $(seq 1 20); do
        ip netns exec "$NS_LB" curl -sf "http://127.0.0.1:9871/api/v1/status" >/dev/null 2>&1 && break
        sleep 0.5
    done

    count=200
    results=$(ip netns exec "$NS_CLIENT" python3 -c "$(client_probe)" "$VIP" "$PORT" "$count")
    heavy=$(grep -c "HEAVY" <<<"$results")
    light=$(grep -c "LIGHT" <<<"$results")
    total=$((heavy + light))
    echo "  observed: HEAVY(weight 9)=${heavy} LIGHT(weight 1)=${light} of ${count} probes"

    if [ "$total" -lt "$((count * 9 / 10))" ]; then
        fail "too many failed probes (${total}/${count} succeeded) — results: $(echo "$results" | tr '\n' ' ')"
    elif [ "$heavy" -gt "$light" ] && [ "$heavy" -ge "$((total * 6 / 10))" ]; then
        pass "weighted split is decisively skewed toward the weight-9 backend (${heavy}/${total})"
    else
        fail "expected HEAVY to clearly dominate (>=60% of successful probes), got ${heavy}/${total}"
    fi

    kill "$RIVORAD_PID" 2>/dev/null
    wait "$RIVORAD_PID" 2>/dev/null
    RIVORAD_PID=""
fi

ip netns exec "$NS_HEAVY" kill "$HEAVY_PID" 2>/dev/null; HEAVY_PID=""
ip netns exec "$NS_LIGHT" kill "$LIGHT_PID" 2>/dev/null; LIGHT_PID=""

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
