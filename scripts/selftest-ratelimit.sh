#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-ratelimit.sh — Functional verification for opt-in per-source-IP
# SYN-flood rate limiting.
#
# Builds the same isolated veth/netns/bridge topology as selftest.sh /
# selftest-weighted.sh (never a host/production interface), runs rivorad in
# NAT mode against a single VIP with one backend, and checks:
#   - with a deliberately tight rate limit enabled, a fast burst of
#     connection attempts is mostly dropped (no response at all — XDP_DROP,
#     not a TCP-level refusal — so the probe uses a short timeout, not the
#     long default, or this test would take a minute-plus on a local link
#     with no real network latency to time out against)
#   - after the bucket has had time to refill, a fresh attempt succeeds
#     again — proving it's a rate limit, not a permanent block
#   - with rateLimit left disabled (the default), the same burst all
#     succeeds — zero behavior change when the feature isn't configured
#
# Must run as root (network namespaces, BPF program attach).
# Usage: sudo ./scripts/selftest-ratelimit.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-ratelimit.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

# Prefer this checkout's freshly-built binary/objects over anything
# already installed system-wide — see selftest-weighted.sh for why.
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
# Isolated topology: brtest0 (fresh bridge, not a host interface) + 2 netns.
#   client (10.80.0.2) --+-- rbrtest0 --- lb (10.80.0.1, rveth-lb)
#   backend(10.80.0.11) -/
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrtest${SUFFIX}"
NS_LB="riv-rlb-${SUFFIX}"
NS_CLIENT="riv-rclient-${SUFFIX}"
NS_BACKEND="riv-rbackend-${SUFFIX}"
VIP="10.80.0.100"
PORT="8080"
CONFIG="/tmp/rivora-selftest-ratelimit-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-selftest-ratelimit-${SUFFIX}-rivorad.log"
RIVORAD_PID=""
BACKEND_PID=""

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    [ -n "$BACKEND_PID" ] && ip netns exec "$NS_BACKEND" kill "$BACKEND_PID" 2>/dev/null
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BACKEND"; do
        ip netns del "$ns" 2>/dev/null
    done
    ip link del "$BR" 2>/dev/null
    rm -f "$CONFIG" "$RIVORAD_LOG"
}
trap cleanup EXIT

for role in lb client be; do
    ip link del "rveth-${role}" 2>/dev/null || true
    ip link del "rveth-${role}-br" 2>/dev/null || true
done

ip link add "$BR" type bridge
ip link set "$BR" up

for pair in "lb:$NS_LB:10.80.0.1" "client:$NS_CLIENT:10.80.0.2" "be:$NS_BACKEND:10.80.0.11"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
    ip netns add "$ns"
    ip link add "rveth-${role}" type veth peer name "rveth-${role}-br"
    ip link set "rveth-${role}" netns "$ns"
    ip link set "rveth-${role}-br" master "$BR" up
    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" ip link set "rveth-${role}" up
    ip netns exec "$ns" ip addr add "${ip_}/24" dev "rveth-${role}"
done

ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev rveth-lb
ip netns exec "$NS_BACKEND" ip route add 10.80.0.2/32 via 10.80.0.1 dev rveth-be

backend_server() {
    cat <<'PYEOF'
import socket, sys
addr, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, port))
s.listen(64)
while True:
    conn, _ = s.accept()
    conn.sendall(b"OK\n")
    conn.close()
PYEOF
}

# Usage: client_burst <vip> <port> <count> <timeout_seconds> — prints one
# line per attempt: "OK" or "ERROR:...". A short timeout is essential here:
# a rate-limited SYN gets XDP_DROP'd, i.e. genuinely zero response ever, not
# a fast TCP-level refusal, so a long default timeout would make this test
# take count*timeout seconds to run.
client_burst() {
    cat <<'PYEOF'
import socket, sys
vip, port, count, timeout = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), float(sys.argv[4])
for _ in range(count):
    try:
        s = socket.create_connection((vip, port), timeout=timeout)
        print(s.recv(64).decode().strip())
        s.close()
    except Exception as e:
        print("ERROR:" + str(e))
PYEOF
}

run_rivorad() {
    ip netns exec "$NS_BACKEND" python3 -c "$(backend_server)" "10.80.0.11" "$PORT" >/dev/null 2>&1 &
    BACKEND_PID=$!
    sleep 0.3

    ip netns exec "$NS_LB" bash -c \
        "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" \
        >"$RIVORAD_LOG" 2>&1 &
    RIVORAD_PID=$!
    sleep 1

    if ! kill -0 "$RIVORAD_PID" 2>/dev/null; then
        return 1
    fi
    for _ in $(seq 1 20); do
        ip netns exec "$NS_LB" curl -sf "http://127.0.0.1:9872/api/v1/status" >/dev/null 2>&1 && return 0
        sleep 0.5
    done
    return 1
}

stop_rivorad() {
    kill "$RIVORAD_PID" 2>/dev/null
    wait "$RIVORAD_PID" 2>/dev/null
    RIVORAD_PID=""
    ip netns exec "$NS_BACKEND" kill "$BACKEND_PID" 2>/dev/null
    BACKEND_PID=""
}

# ---------------------------------------------------------------------------
section "Functional: rate limit enabled (tight burst)"
# ---------------------------------------------------------------------------
cat > "$CONFIG" <<EOF
interface: rveth-lb
apiListen: 127.0.0.1:9872
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
rateLimit: {enabled: true, perSourcePacketsPerSecond: 5, burst: 5}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.80.0.11, port: ${PORT}}]}
EOF

if ! run_rivorad; then
    fail "enabled: rivorad did not come up — see ${RIVORAD_LOG}"
    tail -n 40 "$RIVORAD_LOG" || true
else
    pass "enabled: rivorad API is up"

    count=40
    results=$(ip netns exec "$NS_CLIENT" python3 -c "$(client_burst)" "$VIP" "$PORT" "$count" 0.3)
    ok=$(grep -c "^OK$" <<<"$results")
    echo "  observed: ${ok}/${count} connections succeeded in the initial burst"
    if [ "$ok" -gt 0 ] && [ "$ok" -lt "$((count / 2))" ]; then
        pass "rate limit dropped a clear majority of the burst (${ok}/${count} succeeded)"
    else
        fail "expected a small minority to succeed (rate limit working, not all-or-nothing), got ${ok}/${count}"
    fi

    sleep 2 # let the bucket refill at least one token, whatever the per-CPU-divided rate landed on
    retry=$(ip netns exec "$NS_CLIENT" python3 -c "$(client_burst)" "$VIP" "$PORT" 1 0.3)
    if [ "$retry" = "OK" ]; then
        pass "a connection succeeds again after the bucket refills"
    else
        fail "expected a connection to succeed after refill, got: ${retry}"
    fi

    stop_rivorad
fi

# ---------------------------------------------------------------------------
section "Functional: rate limit disabled (default)"
# ---------------------------------------------------------------------------
cat > "$CONFIG" <<EOF
interface: rveth-lb
apiListen: 127.0.0.1:9872
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.80.0.11, port: ${PORT}}]}
EOF

if ! run_rivorad; then
    fail "disabled: rivorad did not come up — see ${RIVORAD_LOG}"
    tail -n 40 "$RIVORAD_LOG" || true
else
    pass "disabled: rivorad API is up"

    count=40
    results=$(ip netns exec "$NS_CLIENT" python3 -c "$(client_burst)" "$VIP" "$PORT" "$count" 1.5)
    ok=$(grep -c "^OK$" <<<"$results")
    echo "  observed: ${ok}/${count} connections succeeded with rate limiting disabled"
    if [ "$ok" -eq "$count" ]; then
        pass "every connection succeeded with rate limiting left at its default (disabled)"
    else
        fail "expected all ${count} to succeed with rate limiting disabled, got ${ok}/${count} — results: $(echo "$results" | tr '\n' ' ')"
    fi

    stop_rivorad
fi

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
