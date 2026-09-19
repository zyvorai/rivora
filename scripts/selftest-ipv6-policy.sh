#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-ipv6-policy.sh — IPv6 session affinity and IPv6 drop counters.
#
# The IPv4 versions live in selftest-affinity.sh and selftest-drops.sh; IPv6 takes
# different BPF code paths (rivora_hash_src_v6, rate_limit_exceeded_v6, vip_map6,
# backend_map6, drop_stats bumped from handle_ipv6), which those do not touch.
#
#   1. affinity clientIP over IPv6: every connection from one source address lands
#      on one backend, different sources spread over both, and a VIP without
#      affinity spreads a single source across both
#   2. drop counters over IPv6, each counted against the right VIP and reason:
#        rate_limited   a SYN burst over a per-VIP limit
#        no_backend     every backend of a VIP is down
#        unserved       the backend's backend_map6 entry deleted behind rivorad's
#                       back: the packet matches the VIP but cannot be served
#
# Isolated veth/netns/bridge topology (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-ipv6-policy.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-ipv6-policy.sh must run as root (netns + BPF attach)." >&2
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
    echo ""; echo "summary: pass=${PASS} fail=${FAIL} — skipping functional test (missing binaries)"; exit 1
fi

# ---------------------------------------------------------------------------
#   client (::2 plus 24 source addresses in fd00:92:1::/64) --+
#   be1 (::11), be2 (::12) ----------------------------------+-- bridge --- lb (::1)
# The client's extra prefix is NOT on the backends' segment, so replies must return
# through the load balancer (whose TCX program un-NATs them) instead of straight
# back over the bridge.
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrp6${SUFFIX}"
NS_LB="riv-6lb-${SUFFIX}"; NS_CLIENT="riv-6cl-${SUFFIX}"; NS_B1="riv-6b1-${SUFFIX}"; NS_B2="riv-6b2-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-selftest-p6.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9879"
RIVORAD_PID=""
BE_PIDS=()
SOURCES=24

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_B1" "$NS_B2"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_B1" "$NS_B2"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

for role in lb cl b1 b2; do ip link del "p6-${role}-br" 2>/dev/null || true; ip link del "p6-${role}" 2>/dev/null || true; done
for _ in $(seq 1 50); do ip -o link 2>/dev/null | grep -qE ' p6-(lb|cl|b1|b2)(-br)?[:@]' || break; sleep 0.1; done

ip link add "$BR" type bridge; ip link set "$BR" up
for pair in "lb:$NS_LB:1" "cl:$NS_CLIENT:2" "b1:$NS_B1:11" "b2:$NS_B2:12"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
    ip netns add "$ns"
    ip link add "p6-${role}" type veth peer name "p6-${role}-br"
    ip link set "p6-${role}" netns "$ns"
    ip link set "p6-${role}-br" master "$BR" up
    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" sysctl -qw "net.ipv6.conf.p6-${role}.accept_dad=0"
    ip netns exec "$ns" ip link set "p6-${role}" up
    ip netns exec "$ns" ip -6 addr add "fd00:92::${n}/64" dev "p6-${role}"
done
ip netns exec "$NS_LB" sysctl -qw net.ipv6.conf.all.forwarding=1
for n in 100 101 110 111 112; do ip netns exec "$NS_LB" ip -6 addr add "fd00:92::${n}/128" dev p6-lb nodad; done
for i in $(seq 1 "$SOURCES"); do ip netns exec "$NS_CLIENT" ip -6 addr add "fd00:92:1::${i}/128" dev p6-cl nodad; done
ip netns exec "$NS_LB" ip -6 route add fd00:92:1::/64 via fd00:92::2 dev p6-lb
ip netns exec "$NS_B1" ip -6 route add fd00:92:1::/64 via fd00:92::1 dev p6-b1
ip netns exec "$NS_B2" ip -6 route add fd00:92:1::/64 via fd00:92::1 dev p6-b2

SERVER='
import socket, sys
ident, addr = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_INET6); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, 8080)); s.listen(128)
while True:
    c, _ = s.accept()
    try:
        c.sendall((ident + "\n").encode())
    except OSError:
        pass
    finally:
        c.close()
'
start_backends() {
    ip netns exec "$NS_B1" python3 -c "$SERVER" BACKEND-1 fd00:92::11 >/dev/null 2>&1 & BE1=$!
    ip netns exec "$NS_B2" python3 -c "$SERVER" BACKEND-2 fd00:92::12 >/dev/null 2>&1 & BE2=$!
    BE_PIDS=("$BE1" "$BE2")
    sleep 0.5
}
start_backends

BACKENDS="backends: [{address: 'fd00:92::11', port: 8080}, {address: 'fd00:92::12', port: 8080}]"
HDR="interface: p6-lb
apiListen: ${API}
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:"

start_rivorad() {
    ip netns exec "$NS_LB" bash -c "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" >"$LOG" 2>&1 &
    RIVORAD_PID=$!
    for _ in $(seq 1 40); do
        ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1 && { sleep 3; return 0; }
        sleep 0.25
    done
    return 1
}
stop_rivorad() { kill "$RIVORAD_PID" 2>/dev/null; wait "$RIVORAD_PID" 2>/dev/null; RIVORAD_PID=""; sleep 0.5; }

# probe <src> <vip> <timeout>: the backend that answered from that source address, or "fail".
probe() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
src, vip, to = sys.argv[1], sys.argv[2], float(sys.argv[3])
try:
    s = socket.socket(socket.AF_INET6); s.settimeout(to)
    s.bind((src, 0)); s.connect((vip, 8080))
    print(s.recv(32).decode().strip() or "fail")
except Exception:
    print("fail")
' "$1" "$2" "${3:-1.5}"
}

vip_field() {   # vip_field <address> <field>
    ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" | python3 -c "
import json, sys
for v in json.load(sys.stdin):
    if v['vipAddress'] == '$1':
        print(v.get('$2', 0)); break
else:
    print('MISSING')"
}

# ---------------------------------------------------------------------------
section "1. IPv6 session affinity"
# ---------------------------------------------------------------------------
cat >"$CONFIG" <<EOF
${HDR}
  - {address: 'fd00:92::100', port: 8080, protocol: tcp, mode: nat, sessionAffinity: clientIP, ${BACKENDS}}
  - {address: 'fd00:92::101', port: 8080, protocol: tcp, mode: nat, ${BACKENDS}}
EOF
if ! start_rivorad; then
    fail "rivorad did not come up"; tail -n 20 "$LOG"
else
    pass "rivorad is up with an affinity VIP and a plain VIP over IPv6"
    split=0; used1=0; used2=0; failed=0
    for i in $(seq 1 "$SOURCES"); do
        seen=""
        for _ in $(seq 1 8); do
            b=$(probe "fd00:92:1::${i}" fd00:92::100)
            [ "$b" = fail ] && failed=$((failed + 1))
            seen="${seen}${b}\n"
        done
        distinct=$(printf "$seen" | grep -v '^fail$' | grep -v '^$' | sort -u | wc -l)
        [ "$distinct" -gt 1 ] && split=$((split + 1))
        printf "$seen" | grep -q BACKEND-1 && used1=$((used1 + 1))
        printf "$seen" | grep -q BACKEND-2 && used2=$((used2 + 1))
    done
    [ "$split" = 0 ] && pass "every source address kept to one backend across 8 connections (${SOURCES} sources)" \
        || fail "${split} of ${SOURCES} sources were split across backends despite sessionAffinity: clientIP"
    [ "$used1" -ge 4 ] && [ "$used2" -ge 4 ] && pass "the sources spread over both backends (${used1} on BACKEND-1, ${used2} on BACKEND-2)" \
        || fail "the sources did not spread over both backends (${used1}/${used2})"
    [ "$failed" -le 5 ] && pass "connections completed (${failed} of $((SOURCES * 8)) lost)" || fail "too many lost connections: ${failed}"

    seen=""
    for _ in $(seq 1 40); do seen="${seen}$(probe fd00:92:1::1 fd00:92::101)\n"; done
    n=$(printf "$seen" | grep -c '^BACKEND-')
    d=$(printf "$seen" | grep '^BACKEND-' | sort -u | wc -l)
    [ "$d" = 2 ] && pass "without affinity a single source spreads over both backends (${n}/40 answered)" \
        || fail "without affinity one source stayed on ${d} backend(s)"
    stop_rivorad
fi

# ---------------------------------------------------------------------------
section "2. IPv6 drop counters"
# ---------------------------------------------------------------------------
cat >"$CONFIG" <<EOF
${HDR}
  - {address: 'fd00:92::110', port: 8080, protocol: tcp, mode: nat, rateLimit: {perSourcePacketsPerSecond: 5, burst: 5}, ${BACKENDS}}
  - {address: 'fd00:92::111', port: 8080, protocol: tcp, mode: nat, backends: [{address: 'fd00:92::13', port: 8080}]}
  - {address: 'fd00:92::112', port: 8080, protocol: tcp, mode: nat, ${BACKENDS}}
EOF
# fd00:92::13 has no listener anywhere, so VIP ::111's only backend goes down.
if ! start_rivorad; then
    fail "rivorad did not come up (drop counters)"; tail -n 20 "$LOG"
else
    pass "rivorad is up with three IPv6 VIPs"
    for f in droppedRateLimited droppedNoBackend unserved; do
        [ "$(vip_field fd00:92::110 $f)" = 0 ] && [ "$(vip_field fd00:92::111 $f)" = 0 ] && [ "$(vip_field fd00:92::112 $f)" = 0 ] \
            && pass "$f starts at 0 on every VIP" || fail "$f is not 0 to begin with"
    done

    # rate_limited: a SYN burst at the limited VIP.
    for _ in $(seq 1 40); do probe fd00:92:1::1 fd00:92::110 0.25 >/dev/null; done
    rl=$(vip_field fd00:92::110 droppedRateLimited)
    [ "$rl" -gt 0 ] 2>/dev/null && pass "rate_limited counted the throttled SYNs on the limited VIP ($rl)" || fail "rate_limited stayed at ${rl}"
    [ "$(vip_field fd00:92::111 droppedRateLimited)" = 0 ] && [ "$(vip_field fd00:92::112 droppedRateLimited)" = 0 ] \
        && pass "no other VIP counted a rate-limited drop" || fail "another VIP counted rate_limited drops"

    # no_backend: the VIP whose only backend is down.
    for _ in $(seq 1 6); do probe fd00:92:1::2 fd00:92::111 0.3 >/dev/null; done
    nb=$(vip_field fd00:92::111 droppedNoBackend)
    [ "$nb" -gt 0 ] 2>/dev/null && pass "no_healthy_backend counted SYNs with nowhere to go ($nb)" || fail "no_backend stayed at ${nb}"
    [ "$(vip_field fd00:92::112 droppedNoBackend)" = 0 ] && pass "the VIP with healthy backends counted none" || fail "a healthy VIP counted no_backend drops"

    # unserved: delete VIP ::112's backend entries from backend_map6 behind rivorad's back.
    ids=$(ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" | python3 -c "
import json, sys
for v in json.load(sys.stdin):
    if v['vipAddress'] == 'fd00:92::112':
        print(' '.join(str(b['id']) for b in v['backends']))")
    cat >"${WORK}/bpfmap.py" <<'PYEOF'
import ctypes, os, platform, struct, sys
NR = {"x86_64": 321, "aarch64": 280}.get(platform.machine())
if NR is None:
    print("UNSUPPORTED-ARCH"); sys.exit(3)
libc = ctypes.CDLL(None, use_errno=True)
def bpf(cmd, attr):
    buf = ctypes.create_string_buffer(attr, 128)
    r = libc.syscall(NR, cmd, buf, 128)
    return r, ctypes.get_errno()
def addr(b): return ctypes.addressof(b)
p = ctypes.create_string_buffer(sys.argv[2].encode())
fd, e = bpf(7, struct.pack("=QII", addr(p), 0, 0))                      # BPF_OBJ_GET
if fd < 0: print("OBJ_GET-FAILED " + os.strerror(e)); sys.exit(2)
for key in sys.argv[3:]:
    k = ctypes.create_string_buffer(struct.pack("=I", int(key)))
    if sys.argv[1] == "present":
        v = ctypes.create_string_buffer(24)                              # backend_map6's value is 24 bytes
        r, e = bpf(1, struct.pack("=IIQQQ", fd, 0, addr(k), addr(v), 0)) # BPF_MAP_LOOKUP_ELEM
        print("PRESENT" if r == 0 else "ABSENT")
    else:
        r, e = bpf(3, struct.pack("=IIQ", fd, 0, addr(k)))               # BPF_MAP_DELETE_ELEM
        print("DELETED" if r == 0 else "ERROR " + os.strerror(e))
PYEOF
    # rivorad's bpffs is private to its mount namespace, so run the helper inside it.
    in_lb() { nsenter -t "$RIVORAD_PID" -m python3 "${WORK}/bpfmap.py" "$@"; }
    BM=/sys/fs/bpf/rivora-lb/backend_map6
    # shellcheck disable=SC2086
    before=$(in_lb present "$BM" $ids | sort -u | tr '\n' ' ')
    [ "$before" = "PRESENT " ] && pass "precondition: the VIP's backends are in backend_map6" || fail "precondition: backend_map6 said '${before}' for ids ${ids}"
    # shellcheck disable=SC2086
    del=$(in_lb delete "$BM" $ids | sort -u | tr '\n' ' ')
    [ "$del" = "DELETED " ] && pass "the backend_map6 entries were deleted" || fail "the delete said '${del}'"
    for _ in 1 2 3; do probe fd00:92:1::3 fd00:92::112 0.3 >/dev/null; sleep 1.2; done
    un=$(vip_field fd00:92::112 unserved)
    [ "$un" -gt 0 ] 2>/dev/null && pass "unserved counted the packets that bypassed the load balancer ($un)" \
        || fail "unserved stayed at ${un} after the backend entries were deleted"
    [ "$(vip_field fd00:92::110 unserved)" = 0 ] && [ "$(vip_field fd00:92::111 unserved)" = 0 ] \
        && pass "the other VIPs counted no unserved packets" || fail "another VIP counted unserved packets"
    stop_rivorad
fi

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
