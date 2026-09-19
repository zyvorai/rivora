#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-bgp.sh — BGP peer options across a routed hop, with a real rivorad.
#
#   rivorad (LB) --- 10.90.1.0/24 --- router --- 10.90.2.0/24 --- BGP peer
#
# The peer is two hops from rivorad, so the session only comes up if the options
# that make that possible actually reach the wire:
#   1. multihop + TCP MD5: the session establishes across the router, and the peer
#      receives the VIP route carrying the global and the per-VIP communities
#   2. no multihop: an eBGP session to a peer that is not directly connected does
#      NOT establish (the default TTL is too small to cross the router)
#   3. a mismatched MD5 password: the session does NOT establish
#   4. no password where the peer requires one: the session does NOT establish
#
# The peer is scripts' test fixture internal/bgp/testpeer (built here with go). Needs
# root, Linux, and a Go toolchain (or a prebuilt one in $TESTPEER). Isolated
# netns topology (never a host interface). TCP MD5 needs CAP_NET_ADMIN.
# Usage: sudo ./scripts/selftest-bgp.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-bgp.sh must run as root (netns, BGP on port 179, TCP MD5)." >&2
    exit 1
fi

RIVORAD="${ROOT}/bin/rivorad"
[ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
BPF_DIR="${ROOT}/bpf"
[ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"
WORK="$(mktemp -d /tmp/rivora-selftest-bgp.XXXXXX)"
TESTPEER="${TESTPEER:-}"

section "Binaries"
[ -x "$RIVORAD" ] && pass "rivorad: $RIVORAD" || fail "rivorad not found"
[ -f "${BPF_DIR}/xdp_ingress.o" ] && pass "xdp_ingress.o: ${BPF_DIR}/xdp_ingress.o" || fail "xdp_ingress.o missing"
if [ -z "$TESTPEER" ]; then
    TESTPEER="${WORK}/testpeer"
    if (cd "$ROOT" && go build -o "$TESTPEER" ./internal/bgp/testpeer) 2>"${WORK}/build.log"; then
        pass "built the BGP peer fixture"
    else
        fail "could not build internal/bgp/testpeer: $(head -3 "${WORK}/build.log")"
    fi
elif [ -x "$TESTPEER" ]; then
    pass "BGP peer fixture: $TESTPEER"
else
    fail "TESTPEER=$TESTPEER is not executable"
fi
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "summary: pass=${PASS} fail=${FAIL} — skipping functional test"
    rm -rf "$WORK"
    exit 1
fi

SUFFIX="$$"
NS_LB="riv-glb-${SUFFIX}"
NS_R="riv-gr-${SUFFIX}"
NS_P="riv-gp-${SUFFIX}"
API="127.0.0.1:9878"
RIVORAD_PID=""
PEER_PID=""
BE_PID=""

stop_procs() {
    [ -n "$RIVORAD_PID" ] && { kill "$RIVORAD_PID" 2>/dev/null; wait "$RIVORAD_PID" 2>/dev/null; }
    [ -n "$PEER_PID" ] && { kill "$PEER_PID" 2>/dev/null; wait "$PEER_PID" 2>/dev/null; }
    RIVORAD_PID=""; PEER_PID=""
}
cleanup() {
    stop_procs
    [ -n "$BE_PID" ] && kill "$BE_PID" 2>/dev/null
    for ns in "$NS_LB" "$NS_R" "$NS_P"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
    for ns in "$NS_LB" "$NS_R" "$NS_P"; do ip netns del "$ns" 2>/dev/null; done
    rm -rf "$WORK"
}
trap cleanup EXIT

in_ns() { local ns="$1"; shift; ip netns exec "$ns" "$@"; }

for d in bgl-lb bgl-r1 bgl-r2 bgl-p; do ip link del "$d" 2>/dev/null || true; done
for _ in $(seq 1 50); do
    ip -o link 2>/dev/null | grep -qE ' bgl-(lb|r1|r2|p)[:@]' || break
    sleep 0.1
done
for ns in "$NS_LB" "$NS_R" "$NS_P"; do ip netns add "$ns"; in_ns "$ns" ip link set lo up; done
ip link add bgl-lb type veth peer name bgl-r1
ip link add bgl-r2 type veth peer name bgl-p
ip link set bgl-lb netns "$NS_LB"; ip link set bgl-r1 netns "$NS_R"
ip link set bgl-r2 netns "$NS_R";  ip link set bgl-p netns "$NS_P"
in_ns "$NS_LB" ip addr add 10.90.1.1/24 dev bgl-lb; in_ns "$NS_LB" ip link set bgl-lb up
in_ns "$NS_R" ip addr add 10.90.1.2/24 dev bgl-r1;  in_ns "$NS_R" ip link set bgl-r1 up
in_ns "$NS_R" ip addr add 10.90.2.1/24 dev bgl-r2;  in_ns "$NS_R" ip link set bgl-r2 up
in_ns "$NS_P" ip addr add 10.90.2.2/24 dev bgl-p;   in_ns "$NS_P" ip link set bgl-p up
in_ns "$NS_R" sysctl -qw net.ipv4.ip_forward=1
in_ns "$NS_LB" ip route add 10.90.2.0/24 via 10.90.1.2
in_ns "$NS_P" ip route add 10.90.1.0/24 via 10.90.2.1

# The VIP's backend is a local listener: it only has to answer the health probe.
ip netns exec "$NS_LB" python3 -c '
import socket
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("10.90.1.1", 8080)); s.listen(16)
while True:
    try:
        c, _ = s.accept(); c.close()
    except OSError:
        pass
' >/dev/null 2>&1 &
BE_PID=$!
sleep 0.4

# scenario <name> <rivorad-side peer yaml> <peer -password> : starts both ends
start_scenario() {
    local peer_yaml="$1" peer_pw="$2"
    cat >"${WORK}/config.yaml" <<EOF
interface: bgl-lb
apiListen: ${API}
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
bgp:
  enabled: true
  asn: 65001
  routerId: 10.90.1.1
  communities: ["65001:100"]
  peers:
    - ${peer_yaml}
vips:
  - {address: 10.90.1.100, port: 8080, protocol: tcp, mode: nat, bgpCommunities: ["65001:7"], backends: [{address: 10.90.1.1, port: 8080}]}
EOF
    : >"${WORK}/peer.out"
    if [ -n "$peer_pw" ]; then
        ip netns exec "$NS_P" "$TESTPEER" -asn 65002 -neighbor 10.90.1.1 -neighbor-asn 65001 -password "$peer_pw" >"${WORK}/peer.out" 2>&1 &
    else
        ip netns exec "$NS_P" "$TESTPEER" -asn 65002 -neighbor 10.90.1.1 -neighbor-asn 65001 >"${WORK}/peer.out" 2>&1 &
    fi
    PEER_PID=$!
    sleep 1
    ip netns exec "$NS_LB" bash -c "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '${WORK}/config.yaml' -bpf-dir '$BPF_DIR'" >"${WORK}/rivorad.log" 2>&1 &
    RIVORAD_PID=$!
}

# established <seconds>: did the peer report Established within that long?
established() {
    for _ in $(seq 1 "$(( $1 * 2 ))"); do
        grep -q "^STATE ESTABLISHED" "${WORK}/peer.out" && return 0
        sleep 0.5
    done
    return 1
}

section "1. multihop + TCP MD5 across a router"
start_scenario '{address: 10.90.2.2, asn: 65002, multihop: 3, password: "s3cr3t-pass"}' "s3cr3t-pass"
if established 40; then
    pass "the session established across the router with multihop and a matching MD5 password"
    for _ in $(seq 1 30); do grep -q "^ROUTE 10.90.1.100/32" "${WORK}/peer.out" && break; sleep 0.5; done
    route=$(grep "^ROUTE 10.90.1.100/32" "${WORK}/peer.out" | tail -1)
    if [ "$route" = "ROUTE 10.90.1.100/32 [65001:100 65001:7]" ]; then
        pass "the peer received 10.90.1.100/32 with the global and per-VIP communities ($route)"
    else
        fail "unexpected route at the peer: '${route}' (want ROUTE 10.90.1.100/32 [65001:100 65001:7])"
    fi
else
    fail "the session did not establish; peer saw: $(tail -3 "${WORK}/peer.out" | tr '\n' ' '); rivorad: $(grep -iE 'bgp|error' "${WORK}/rivorad.log" | tail -3 | tr '\n' ' ')"
fi
stop_procs

section "2. no multihop: an eBGP peer that is two hops away must not establish"
start_scenario '{address: 10.90.2.2, asn: 65002, password: "s3cr3t-pass"}' "s3cr3t-pass"
if established 15; then
    fail "a session to a peer across a router established without multihop"
else
    pass "no session without multihop (the default TTL does not cross the router)"
fi
stop_procs

section "3. MD5 password mismatch"
start_scenario '{address: 10.90.2.2, asn: 65002, multihop: 3, password: "the-wrong-one"}' "s3cr3t-pass"
if established 15; then
    fail "a session with a mismatched MD5 password established"
else
    pass "no session with a mismatched MD5 password"
fi
stop_procs

section "4. peer requires MD5, rivorad configured none"
start_scenario '{address: 10.90.2.2, asn: 65002, multihop: 3}' "s3cr3t-pass"
if established 15; then
    fail "a session established although the peer requires MD5 and none was configured"
else
    pass "no session when rivorad sends no MD5 signature the peer requires"
fi
stop_procs

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
