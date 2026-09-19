#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-vipratelimit.sh — per-VIP SYN rate limits (the `rateLimit` block on
# a VIP, which a Kubernetes ServicePolicy also sets), IPv4 and IPv6.
#
# The node-wide rate limit is covered by selftest-ratelimit.sh; this checks
# what is new:
#   1. a VIP with its own tight limit is throttled, and a VIP without one is
#      not, on the same node with the node-wide limit off
#   2. VIPs do not share buckets: emptying one VIP's bucket leaves another
#      limited VIP's bucket full (the bucket key includes the service)
#   3. a VIP's own limit REPLACES the node-wide one, in both directions: a
#      generous own limit exempts a VIP from a tight node-wide limit, and a
#      VIP with no limit of its own still follows the node-wide one
#   4. removing a VIP's limit on a config reload (SIGHUP) actually lifts it
#   5. the drops are counted against the right VIP
# Every check runs for both address families (separate BPF code paths and
# maps).
#
# Isolated veth/netns/bridge topology (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-vipratelimit.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-vipratelimit.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

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
#   client (.2) --+-- bridge --- lb (.1, veth-lb)
#   backend (.11) -/                      10.81.0.0/24 and fd00:81::/64
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrvl${SUFFIX}"
NS_LB="riv-vlb-${SUFFIX}"
NS_CLIENT="riv-vclient-${SUFFIX}"
NS_BACKEND="riv-vbackend-${SUFFIX}"
PORT="8080"
API="127.0.0.1:9873"
CONFIG="/tmp/rivora-selftest-vrl-${SUFFIX}.yaml"
RIVORAD_LOG="/tmp/rivora-selftest-vrl-${SUFFIX}-rivorad.log"
RIVORAD_PID=""
BACKEND_PIDS=""

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for p in $BACKEND_PIDS; do ip netns exec "$NS_BACKEND" kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BACKEND"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -f "$CONFIG" "$RIVORAD_LOG"
}
trap cleanup EXIT

for role in lb client be; do
    ip link del "vveth-${role}" 2>/dev/null || true
done

ip link add "$BR" type bridge
ip link set "$BR" up
for pair in "lb:$NS_LB:1" "client:$NS_CLIENT:2" "be:$NS_BACKEND:11"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
    ip netns add "$ns"
    ip link add "vveth-${role}" type veth peer name "vveth-${role}-br"
    ip link set "vveth-${role}" netns "$ns"
    ip link set "vveth-${role}-br" master "$BR" up
    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" sysctl -qw "net.ipv6.conf.vveth-${role}.accept_dad=0"
    ip netns exec "$ns" ip link set "vveth-${role}" up
    ip netns exec "$ns" ip addr add "10.81.0.${n}/24" dev "vveth-${role}"
    ip netns exec "$ns" ip -6 addr add "fd00:81::${n}/64" dev "vveth-${role}"
done
ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
# Replies to the client must come back through the load balancer to be un-NATed.
ip netns exec "$NS_BACKEND" ip route add 10.81.0.2/32 via 10.81.0.1 dev vveth-be
ip netns exec "$NS_BACKEND" ip -6 route add fd00:81::2/128 via fd00:81::1 dev vveth-be
# VIPs on the LB so the client can ARP/ND for them.
for n in 100 101 102 103; do
    ip netns exec "$NS_LB" ip addr add "10.81.0.${n}/32" dev vveth-lb
    ip netns exec "$NS_LB" ip -6 addr add "fd00:81::${n}/128" dev vveth-lb nodad
done

backend_server() {
    cat <<'PYEOF'
import socket, sys
fam = socket.AF_INET6 if ":" in sys.argv[1] else socket.AF_INET
s = socket.socket(fam, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((sys.argv[1], int(sys.argv[2])))
s.listen(64)
while True:
    conn, _ = s.accept()
    conn.sendall(b"OK\n")
    conn.close()
PYEOF
}

# client_burst <vip> <count> <timeout> prints "OK" or "ERROR:..." per attempt. A
# rate-limited SYN is XDP_DROPped, so there is no refusal to wait for: a short
# timeout keeps the test fast.
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

burst() { ip netns exec "$NS_CLIENT" python3 -c "$(client_burst)" "$1" "$PORT" "$2" "$3"; }
count_ok() { grep -c "^OK$" <<<"$1"; }

# vip_field <address> <json field>: a VIP's status field from the API.
vip_field() {
    ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" | python3 -c "
import json, sys
for v in json.load(sys.stdin):
    if v['vipAddress'] == '$1':
        print(v.get('$2', 0)); break
else:
    print('MISSING')"
}

start_rivorad() {
    ip netns exec "$NS_BACKEND" python3 -c "$(backend_server)" 10.81.0.11 "$PORT" >/dev/null 2>&1 &
    BACKEND_PIDS="$!"
    ip netns exec "$NS_BACKEND" python3 -c "$(backend_server)" fd00:81::11 "$PORT" >/dev/null 2>&1 &
    BACKEND_PIDS="$BACKEND_PIDS $!"
    # The backends must be accepting before rivorad starts probing them, or a slow
    # interpreter start-up marks them down and every VIP drops for the first seconds.
    for be in 10.81.0.11 fd00:81::11; do
        for _ in $(seq 1 50); do
            ip netns exec "$NS_LB" python3 -c "import socket,sys; socket.create_connection((sys.argv[1], $PORT), timeout=0.5).close()" "$be" 2>/dev/null && break
            sleep 0.2
        done
    done
    ip netns exec "$NS_LB" bash -c \
        "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" \
        >"$RIVORAD_LOG" 2>&1 &
    RIVORAD_PID=$!
    sleep 1
    kill -0 "$RIVORAD_PID" 2>/dev/null || return 1
    for _ in $(seq 1 20); do
        START_ERR=$(ip netns exec "$NS_LB" curl -sSf "http://${API}/api/v1/vips" 2>&1 >/dev/null) && return 0
        sleep 0.5
    done
    echo "    last curl error: ${START_ERR}"
    return 1
}

stop_rivorad() {
    kill "$RIVORAD_PID" 2>/dev/null; wait "$RIVORAD_PID" 2>/dev/null; RIVORAD_PID=""
    for p in $BACKEND_PIDS; do ip netns exec "$NS_BACKEND" kill "$p" 2>/dev/null; done
    BACKEND_PIDS=""
    # Start the next scenario from an empty datapath: pinned maps survive a restart.
    ip netns exec "$NS_LB" rm -rf /sys/fs/bpf/rivora-lb 2>/dev/null
    sleep 0.3
}

# vip_line <address> <extra yaml> emits one VIP entry for both families' backends.
vip_line() {
    local addr="$1" extra="$2" be="10.81.0.11"
    [[ "$addr" == *:* ]] && be="fd00:81::11"
    echo "  - {address: '${addr}', port: ${PORT}, protocol: tcp, mode: nat, ${extra} backends: [{address: '${be}', port: ${PORT}}]}"
}

TIGHT="rateLimit: {perSourcePacketsPerSecond: 5, burst: 5},"
WIDE="rateLimit: {perSourcePacketsPerSecond: 1000000, burst: 1000000},"

# diagnose <vip>: what the load balancer thinks of that VIP, for a failure report.
diagnose() {
    ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" | python3 -c "
import json, sys
for v in json.load(sys.stdin):
    if v['vipAddress'] == '$1':
        print('    diagnostics: backends', [(b['address'], b['state']) for b in v['backends']],
              'rate_limited', v['droppedRateLimited'], 'no_backend', v['droppedNoBackend'], 'unserved', v['unserved'])" 2>&1
    echo "    diagnostics: client neighbours: $(ip netns exec "$NS_CLIENT" ip neigh show | tr '\n' ';')"
}

# check_limited/check_free <label> <vip>: a 40-connection burst is mostly dropped /
# fully served.
check_limited() {
    local r ok
    r=$(burst "$2" 40 0.3); ok=$(count_ok "$r")
    if [ "$ok" -lt 20 ]; then pass "$1: throttled (${ok}/40 succeeded)"; else fail "$1: expected a throttled burst, ${ok}/40 succeeded"; fi
}
# check_free asserts two things. The exact one is the VIP's rate_limited counter, which
# must not move: nothing was throttled by the limiter. The other is that nearly every
# connection got through. That is not required to be 40/40 because this host's bridged
# IPv4 traffic also crosses its own iptables/conntrack (br_netfilter), which now and then
# loses a NAT'd flow for reasons unrelated to rivorad; a throttled VIP passes at most a
# fifth of the burst, so 30/40 cannot be mistaken for it.
check_free() {
    local r ok before after
    burst "$2" 1 3 >/dev/null # warm-up: resolve the VIP's neighbour entry so the timed burst measures the limiter, not ARP/ND
    before=$(vip_field "$2" droppedRateLimited)
    r=$(burst "$2" 40 1.5); ok=$(count_ok "$r")
    after=$(vip_field "$2" droppedRateLimited)
    if [ "$after" != "$before" ]; then
        fail "$1: the limiter dropped $((after - before)) SYNs on a VIP that should be unthrottled"
    elif [ "$ok" -ge 30 ]; then
        pass "$1: unthrottled (${ok}/40 succeeded, limiter dropped none)"
    else
        fail "$1: expected nearly every connection to succeed, ${ok}/40 did: $(grep -v '^OK$' <<<"$r" | sort | uniq -c | tr '\n' ' ')"
        diagnose "$2"
    fi
}

# ---------------------------------------------------------------------------
section "1-2. Per-VIP limits with the node-wide limit off"
# ---------------------------------------------------------------------------
{
    echo "interface: vveth-lb"
    echo "apiListen: ${API}"
    echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
    echo "vips:"
    vip_line 10.81.0.100 "$TIGHT"          # A4: own tight limit
    vip_line 10.81.0.101 ""                # B4: none
    vip_line 10.81.0.103 "$TIGHT"          # D4: own tight limit, a separate bucket from A4
    vip_line fd00:81::100 "$TIGHT"         # A6
    vip_line fd00:81::101 ""               # B6
    vip_line fd00:81::103 "$TIGHT"         # D6
} >"$CONFIG"

if ! start_rivorad; then
    fail "rivorad did not come up"; tail -n 30 "$RIVORAD_LOG"; stop_rivorad
else
    pass "rivorad is up with per-VIP limits"
    check_limited "v4 VIP with its own limit" 10.81.0.100
    # A4's bucket is now empty. D4 has a limit of the same size but its own bucket.
    r=$(burst 10.81.0.103 1 0.3)
    [ "$(count_ok "$r")" -eq 1 ] && pass "v4: a second limited VIP still has a full bucket after the first was emptied (buckets are per VIP)" \
        || fail "v4: the first VIP's burst also emptied the second VIP's bucket (got: $r)"
    check_free "v4 VIP with no limit" 10.81.0.101

    check_limited "v6 VIP with its own limit" fd00:81::100
    r=$(burst fd00:81::103 1 0.3)
    [ "$(count_ok "$r")" -eq 1 ] && pass "v6: a second limited VIP still has a full bucket after the first was emptied (buckets are per VIP)" \
        || fail "v6: the first VIP's burst also emptied the second VIP's bucket (got: $r)"
    check_free "v6 VIP with no limit" fd00:81::101

    # 5. The drops land on the VIP that was limited, not on the free one.
    a4=$(vip_field 10.81.0.100 droppedRateLimited); b4=$(vip_field 10.81.0.101 droppedRateLimited)
    a6=$(vip_field fd00:81::100 droppedRateLimited); b6=$(vip_field fd00:81::101 droppedRateLimited)
    [ "$a4" -gt 0 ] 2>/dev/null && pass "v4: the limited VIP counted its rate-limited drops (${a4})" || fail "v4: limited VIP counted no rate_limited drops (${a4})"
    [ "$b4" = "0" ] && pass "v4: the unlimited VIP counted none" || fail "v4: the unlimited VIP counted ${b4} rate_limited drops"
    [ "$a6" -gt 0 ] 2>/dev/null && pass "v6: the limited VIP counted its rate-limited drops (${a6})" || fail "v6: limited VIP counted no rate_limited drops (${a6})"
    [ "$b6" = "0" ] && pass "v6: the unlimited VIP counted none" || fail "v6: the unlimited VIP counted ${b6} rate_limited drops"

    # 4. Removing the limit on a reload lifts it.
    section "4. Removing a VIP's limit on reload"
    {
        echo "interface: vveth-lb"
        echo "apiListen: ${API}"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        vip_line 10.81.0.100 ""
        vip_line 10.81.0.101 ""
        vip_line 10.81.0.103 "$TIGHT"
        vip_line fd00:81::100 ""
        vip_line fd00:81::101 ""
        vip_line fd00:81::103 "$TIGHT"
    } >"$CONFIG"
    kill -HUP "$RIVORAD_PID"
    sleep 2
    check_free "v4 VIP after its limit was removed by reload" 10.81.0.100
    check_free "v6 VIP after its limit was removed by reload" fd00:81::100
    check_limited "v4 VIP whose limit was kept across the reload" 10.81.0.103
    stop_rivorad
fi

# ---------------------------------------------------------------------------
section "3. A VIP's own limit replaces the node-wide limit"
# ---------------------------------------------------------------------------
{
    echo "interface: vveth-lb"
    echo "apiListen: ${API}"
    echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
    echo "rateLimit: {enabled: true, perSourcePacketsPerSecond: 5, burst: 5}"
    echo "vips:"
    vip_line 10.81.0.101 "$WIDE"           # B4: generous own limit, exempt from the tight node-wide one
    vip_line 10.81.0.102 ""                # C4: none, so the node-wide limit applies
    vip_line fd00:81::101 "$WIDE"
    vip_line fd00:81::102 ""
} >"$CONFIG"

if ! start_rivorad; then
    fail "rivorad did not come up (node-wide limit scenario)"; tail -n 30 "$RIVORAD_LOG"; stop_rivorad
else
    pass "rivorad is up with a node-wide limit and one VIP overriding it"
    check_limited "v4 VIP with no limit of its own follows the node-wide limit" 10.81.0.102
    check_free "v4 VIP with a generous own limit ignores the tight node-wide limit" 10.81.0.101
    check_limited "v6 VIP with no limit of its own follows the node-wide limit" fd00:81::102
    check_free "v6 VIP with a generous own limit ignores the tight node-wide limit" fd00:81::101
    stop_rivorad
fi

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
