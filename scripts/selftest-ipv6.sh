#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-ipv6.sh — Functional verification for Rivora's IPv6 dataplane
# (v0.3), mirroring scripts/selftest.sh's DSR/full-NAT scenarios and Maglev-
# spread/failover checks but with an all-IPv6 topology (ULA prefix
# fd00:77::/64, echoing the v4 test's 10.77.0.0/24).
#
# One deliberate difference from selftest.sh: DSR mode's "backends mustn't
# answer neighbor discovery for the VIP" problem doesn't have as clean an
# equivalent to v4's arp_ignore/arp_announce sysctls, and solving IPv6
# Neighbor Discovery suppression correctly is arguably part of the future
# NDP-speaker milestone's job, not this dataplane-foundation one. So this
# script sidesteps it entirely: the client gets a static, permanent IPv6
# neighbor entry pointing the VIP directly at the LB's own MAC, and never
# relies on live NDP resolution for the VIP at all. This isolates "does the
# XDP/TCX forwarding and checksum logic work" (this script's actual job)
# from "does NDP-based VIP ownership announcement work" (a separate,
# later concern) — it isn't an oversight.
#
# Must run as root (network namespaces, BPF program attach).
# Usage: sudo ./scripts/selftest-ipv6.sh [--quick]
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
    echo "selftest-ipv6.sh must run as root (netns + BPF attach)." >&2
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
# Isolated topology: brtest6<suffix> (fresh bridge, not a host interface) +
# 4 netns, all on the fd00:77::/64 ULA test prefix.
#   client (fd00:77::2)  --\
#   be1    (fd00:77::11) ---+-- brtest6 --- lb (fd00:77::1, veth6-lb)
#   be2    (fd00:77::12) --/
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="brtest6${SUFFIX}"
NS_LB="riv6-lb-${SUFFIX}"
NS_CLIENT="riv6-client-${SUFFIX}"
NS_BE1="riv6-be1-${SUFFIX}"
NS_BE2="riv6-be2-${SUFFIX}"
PREFIX="fd00:77::"
VIP="fd00:77::100"
PORT="8080"
CONFIG="/tmp/rivora-selftest6-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-selftest6-${SUFFIX}-rivorad.log"
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
    # CI runners (and many Docker-enabled hosts generally) commonly ship
    # ip6tables with a default-DROP FORWARD policy while leaving iptables
    # (v4) permissive — v4's selftest.sh's identical bridge+veth topology
    # never hits this because its traffic is v4. This test's bridge lives
    # in the root netns, so if br_netfilter is loaded, bridged IPv6
    # traffic between the veth pairs below is subject to the root netns's
    # ip6tables FORWARD chain. Best-effort or this instead: environments
    # without ip6tables (or where it's already permissive) just no-op here.
    echo "  [diag] ip6tables FORWARD policy before: $(ip6tables -L FORWARD -n 2>&1 | head -1)"
    ip6tables -P FORWARD ACCEPT 2>/dev/null || true
    echo "  [diag] ip6tables FORWARD policy after:  $(ip6tables -L FORWARD -n 2>&1 | head -1)"

    # Defensive clean slate — see selftest.sh's identical comment: an
    # interrupted previous run can leave orphaned veth halves behind even
    # though their namespace is long gone.
    for role in lb client be1 be2; do
        ip link del "veth6-${role}" 2>/dev/null || true
        ip link del "veth6-${role}-br" 2>/dev/null || true
    done

    ip link add "$BR" type bridge
    ip link set "$BR" up

    for pair in "lb:$NS_LB:${PREFIX}1" "cli:$NS_CLIENT:${PREFIX}2" "be1:$NS_BE1:${PREFIX}11" "be2:$NS_BE2:${PREFIX}12"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "veth6-${role}" type veth peer name "veth6-${role}-br"
        ip link set "veth6-${role}" netns "$ns"
        ip link set "veth6-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        # Disable Duplicate Address Detection on this test's throwaway
        # links — DAD's ~1s "tentative" window before an address becomes
        # usable is pure flake risk here (no real duplicate is possible on
        # a bridge we just created), not a meaningful safety check.
        ip netns exec "$ns" sysctl -qw "net.ipv6.conf.veth6-${role}.accept_dad=0"
        ip netns exec "$ns" ip link set "veth6-${role}" up
        ip netns exec "$ns" ip -6 addr add "${ip_}/64" dev "veth6-${role}"
    done

    ip netns exec "$NS_LB" sysctl -qw net.ipv6.conf.all.forwarding=1
}

backend_server() {
    cat <<'PYEOF'
import socket, sys
addr, port, ident = sys.argv[1], int(sys.argv[2]), sys.argv[3]
s = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
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
    # An IPv6 literal needs no brackets at the socket API level (brackets
    # are a URL/text-representation convention, not something the stdlib
    # socket module needs) — create_connection((host, port)) resolves the
    # family via getaddrinfo regardless.
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
    section "Functional (IPv6): ${mode} mode"

    local lb_mac be1_mac be2_mac
    lb_mac=$(ip netns exec "$NS_LB" cat /sys/class/net/veth6-lb/address)
    be1_mac=$(ip netns exec "$NS_BE1" cat /sys/class/net/veth6-be1/address)
    be2_mac=$(ip netns exec "$NS_BE2" cat /sys/class/net/veth6-be2/address)

    if [ "$mode" = "dsr" ]; then
        # DSR: VIP lives on each backend's lo (so it accepts traffic
        # addressed to it) — see the top-of-file comment for why this
        # script sidesteps live NDP resolution rather than trying to
        # suppress backend NDP responses the way selftest.sh disables ARP.
        ip netns exec "$NS_BE1" ip -6 addr add "${VIP}/128" dev lo
        ip netns exec "$NS_BE2" ip -6 addr add "${VIP}/128" dev lo
        ip netns exec "$NS_LB" ip -6 addr add "${VIP}/128" dev veth6-lb
        ip netns exec "$NS_CLIENT" ip -6 neigh add "$VIP" lladdr "$lb_mac" dev veth6-cli nud permanent
        cat > "$CONFIG" <<EOF
interface: veth6-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: dsr, backends: [
      {address: ${PREFIX}11, port: ${PORT}, mac: "${be1_mac}"},
      {address: ${PREFIX}12, port: ${PORT}, mac: "${be2_mac}"}]}
EOF
        ip netns exec "$NS_BE1" python3 -c "$(backend_server)" "::" "$PORT" "BACKEND-1" >/dev/null 2>&1 &
        BE1_PID=$!
        ip netns exec "$NS_BE2" python3 -c "$(backend_server)" "::" "$PORT" "BACKEND-2" >/dev/null 2>&1 &
        BE2_PID=$!
    else
        ip netns exec "$NS_LB" ip -6 addr add "${VIP}/128" dev veth6-lb
        ip netns exec "$NS_CLIENT" ip -6 neigh add "$VIP" lladdr "$lb_mac" dev veth6-cli nud permanent
        # Force backend->client return traffic through the LB (its TCX
        # egress program un-NATs it there), same reasoning as selftest.sh.
        ip netns exec "$NS_BE1" ip -6 route add "${PREFIX}2/128" via "${PREFIX}1" dev veth6-be1
        ip netns exec "$NS_BE2" ip -6 route add "${PREFIX}2/128" via "${PREFIX}1" dev veth6-be2
        cat > "$CONFIG" <<EOF
interface: veth6-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: ${PREFIX}11, port: ${PORT}},
      {address: ${PREFIX}12, port: ${PORT}}]}
EOF
        ip netns exec "$NS_BE1" python3 -c "$(backend_server)" "${PREFIX}11" "$PORT" "BACKEND-1" >/dev/null 2>&1 &
        BE1_PID=$!
        ip netns exec "$NS_BE2" python3 -c "$(backend_server)" "${PREFIX}12" "$PORT" "BACKEND-2" >/dev/null 2>&1 &
        BE2_PID=$!
    fi
    sleep 0.3

    # Temporary diagnostic for CI iteration: is direct LB->backend IPv6
    # connectivity (what the active health checker itself needs, entirely
    # separate from XDP-forwarded VIP traffic) actually working, and how
    # long does neighbor resolution take before rivorad's own health
    # checker (500ms probe timeout) ever gets a chance to try it?
    echo "  [diag] pre-warm: LB->${PREFIX}11:${PORT} connect test:"
    ip netns exec "$NS_LB" timeout 3 python3 -c "
import socket, time
t0 = time.time()
try:
    s = socket.create_connection(('${PREFIX}11', ${PORT}), timeout=2.5)
    print('    connected in %.3fs' % (time.time() - t0))
    s.close()
except Exception as e:
    print('    FAILED after %.3fs: %s' % (time.time() - t0, e))
"
    echo "  [diag] LB neigh table after pre-warm:"
    ip netns exec "$NS_LB" ip -6 neigh show 2>&1 | sed 's/^/    /'

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
        # Temporary diagnostics for CI iteration — remove once the cause is
        # understood. Dump backend health as rivorad itself sees it, plus
        # its recent log, right at the point of failure.
        echo "  [diag] /api/v1/backends:"
        ip netns exec "$NS_LB" curl -s "http://127.0.0.1:9870/api/v1/backends" 2>&1 | sed 's/^/    /'
        echo "  [diag] /api/v1/vips:"
        ip netns exec "$NS_LB" curl -s "http://127.0.0.1:9870/api/v1/vips" 2>&1 | sed 's/^/    /'
        echo "  [diag] rivorad log tail:"
        tail -n 60 "$RIVORAD_LOG" 2>&1 | sed 's/^/    /'
        echo "  [diag] bridge fdb:"
        bridge fdb show br "$BR" 2>&1 | sed 's/^/    /'
        echo "  [diag] client neigh table:"
        ip netns exec "$NS_CLIENT" ip -6 neigh show 2>&1 | sed 's/^/    /'
    fi

    kill "$RIVORAD_PID" 2>/dev/null
    wait "$RIVORAD_PID" 2>/dev/null
    RIVORAD_PID=""
    ip netns exec "$NS_BE1" kill "$BE1_PID" 2>/dev/null
    BE1_PID=""
    ip netns exec "$NS_CLIENT" ip -6 neigh del "$VIP" dev veth6-cli 2>/dev/null
    ip netns exec "$NS_LB" ip -6 addr del "${VIP}/128" dev veth6-lb 2>/dev/null
    ip netns exec "$NS_BE1" ip -6 addr del "${VIP}/128" dev lo 2>/dev/null
    ip netns exec "$NS_BE2" ip -6 addr del "${VIP}/128" dev lo 2>/dev/null
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
