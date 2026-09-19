#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-l3dsr.sh — L3 DSR: `mode: dsr-ipip` and `mode: dsr-gre`.
#
# The load balancer wraps each packet for a VIP in an IP-in-IP or GRE tunnel to the
# backend, which unwraps it and answers the client from the VIP. Unlike plain DSR the
# backend is NOT on the load balancer's L2 segment: here it is one routed hop away, on
# another subnet behind a second interface, so the tunnelled frame has to be routed out
# of a different interface than it arrived on (bpf_fib_lookup + redirect).
#
#   client --- (10.87.0.0/24, fd00:87::/64) --- LB --- (10.88.0.0/24, fd00:88::/64) --- backend
#
# Checked, for IPv4 (IP-in-IP, GRE) and IPv6 (IPv6-in-IPv6, GRE over IPv6):
#   1. a request reaches the backend through the tunnel and is answered from the VIP,
#      including a 1300-byte one (the outer length arithmetic, and GRE's extra 4 bytes)
#   2. the outer header names the configured tunnelSource, and, when it is left unset,
#      the attached interface's own address
#   3. the outer header's checksum is valid (checksum offload is off, so every receiver
#      verifies it; a bad one is dropped and the request fails)
#   4. a bad config is refused: a MAC on a tunnel backend
#
# Isolated veth/netns topology (never a host interface). Must run as root. Needs the
# ipip, ip_gre, ip6_tunnel and ip6_gre kernel modules; skipped where they are missing.
# Usage: sudo ./scripts/selftest-l3dsr.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
SKIP=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
skip() { SKIP=$((SKIP + 1)); echo "  [skip] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-l3dsr.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi

RIVORAD="${ROOT}/bin/rivorad"
[ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
RIVORACTL="${ROOT}/bin/rivoractl"
[ -x "$RIVORACTL" ] || RIVORACTL="$(command -v rivoractl || echo "$RIVORACTL")"
BPF_DIR="${ROOT}/bpf"
[ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"

section "Binaries"
[ -x "$RIVORAD" ] && pass "rivorad: $RIVORAD" || fail "rivorad not found"
[ -f "${BPF_DIR}/xdp_ingress.o" ] && pass "xdp_ingress.o: ${BPF_DIR}/xdp_ingress.o" || fail "xdp_ingress.o missing"
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "summary: pass=${PASS} fail=${FAIL} — skipping functional test (missing binaries)"
    exit 1
fi

for m in ipip ip_gre ip6_tunnel ip6_gre; do modprobe "$m" 2>/dev/null; done
missing=""
for m in ipip ip_gre ip6_tunnel ip6_gre; do
    [ -d "/sys/module/$m" ] || grep -qE "^$m " /proc/modules 2>/dev/null || missing="$missing $m"
done
if [ -n "$missing" ]; then
    echo ""
    skip "kernel tunnel modules not available:${missing}"
    echo ""; echo "summary: pass=${PASS} skip=${SKIP} fail=${FAIL}"
    exit 0
fi

SUFFIX="$$"
NS_LB="riv-tlb-${SUFFIX}"
NS_CLIENT="riv-tclient-${SUFFIX}"
NS_BE="riv-tbe-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-selftest-l3.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9877"
RIVORAD_PID=""
BE_PIDS=()

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && ip netns exec "$NS_BE" kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns del "$ns" 2>/dev/null; done
    rm -rf "$WORK"
}
trap cleanup EXIT

in_ns() { local ns="$1"; shift; ip netns exec "$ns" "$@"; }

# Device names are fixed, and a namespace's veths are torn down asynchronously: clear any left by
# a previous run and wait until they are gone.
for d in ldr-cl ldr-lb ldr-lb2 ldr-be; do ip link del "$d" 2>/dev/null || true; done
for _ in $(seq 1 50); do
    ip -o link 2>/dev/null | grep -qE ' ldr-(cl|lb|lb2|be)[:@]' || break
    sleep 0.1
done

for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns add "$ns"; in_ns "$ns" ip link set lo up; done
ip link add ldr-cl type veth peer name ldr-lb
ip link add ldr-lb2 type veth peer name ldr-be
ip link set ldr-cl netns "$NS_CLIENT"; ip link set ldr-lb netns "$NS_LB"
ip link set ldr-lb2 netns "$NS_LB"; ip link set ldr-be netns "$NS_BE"

# No checksum offload: every packet, including the outer header we build, is verified.
for pair in "$NS_CLIENT:ldr-cl" "$NS_LB:ldr-lb" "$NS_LB:ldr-lb2" "$NS_BE:ldr-be"; do
    ns="${pair%%:*}"; dev="${pair#*:}"
    in_ns "$ns" sysctl -qw "net.ipv6.conf.${dev}.accept_dad=0"
    command -v ethtool >/dev/null 2>&1 && in_ns "$ns" ethtool -K "$dev" tx off rx off >/dev/null 2>&1
    in_ns "$ns" ip link set "$dev" up
done
in_ns "$NS_CLIENT" ip addr add 10.87.0.2/24 dev ldr-cl;  in_ns "$NS_CLIENT" ip -6 addr add fd00:87::2/64 dev ldr-cl
in_ns "$NS_LB" ip addr add 10.87.0.1/24 dev ldr-lb;      in_ns "$NS_LB" ip -6 addr add fd00:87::1/64 dev ldr-lb
in_ns "$NS_LB" ip addr add 10.88.0.1/24 dev ldr-lb2;     in_ns "$NS_LB" ip -6 addr add fd00:88::1/64 dev ldr-lb2
in_ns "$NS_BE" ip addr add 10.88.0.11/24 dev ldr-be;     in_ns "$NS_BE" ip -6 addr add fd00:88::11/64 dev ldr-be
in_ns "$NS_LB" sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1 net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
in_ns "$NS_BE" sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
in_ns "$NS_CLIENT" sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
# The backend's way back to the client is through the load balancer (replies still leave from the VIP).
in_ns "$NS_BE" ip route add 10.87.0.0/24 via 10.88.0.1
in_ns "$NS_BE" ip -6 route add fd00:87::/64 via fd00:88::1

# The VIPs are not on the load balancer. They live on the backend's lo, and the client is pointed
# at the load balancer's MAC for them (in a real network a router or BGP does that).
LB_MAC=$(in_ns "$NS_LB" cat /sys/class/net/ldr-lb/address)
for v in 10.87.0.103 10.87.0.104; do
    in_ns "$NS_BE" ip addr add "${v}/32" dev lo
    in_ns "$NS_CLIENT" ip neigh replace "$v" lladdr "$LB_MAC" dev ldr-cl nud permanent
done
for v in fd00:87::103 fd00:87::104; do
    in_ns "$NS_BE" ip -6 addr add "${v}/128" dev lo nodad
    in_ns "$NS_CLIENT" ip -6 neigh replace "$v" lladdr "$LB_MAC" dev ldr-cl nud permanent
done

# The backend's tunnel endpoints: any remote may send to its address.
in_ns "$NS_BE" ip tunnel add tv4i mode ipip local 10.88.0.11
in_ns "$NS_BE" ip tunnel add tv4g mode gre local 10.88.0.11
in_ns "$NS_BE" ip -6 tunnel add tv6i mode ip6ip6 local fd00:88::11
in_ns "$NS_BE" ip -6 tunnel add tv6g mode ip6gre local fd00:88::11
for t in tv4i tv4g tv6i tv6g; do
    in_ns "$NS_BE" sysctl -qw "net.ipv4.conf.${t}.rp_filter=0" 2>/dev/null
    in_ns "$NS_BE" ip link set "$t" up || fail "could not bring up tunnel endpoint $t"
done

# ident addr: answers "<ident>:<bytes received>" after reading what the client sent.
SERVER='
import socket, sys, threading
ident, addr = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_INET6 if ":" in addr else socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, 8080)); s.listen(64)
def handle(c):
    try:
        c.settimeout(1.0)
        n = 0
        try:
            while True:
                d = c.recv(4096)
                if not d: break
                n += len(d)
                if n >= 1300 or d.endswith(b"\n"): break
        except OSError:
            pass
        c.sendall(("%s:%d\n" % (ident, n)).encode())
    except OSError:
        pass
    finally:
        c.close()
while True:
    c, _ = s.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
'
serve() { ip netns exec "$NS_BE" python3 -c "$SERVER" "$1" "$2" >/dev/null 2>&1 & BE_PIDS+=($!); }
serve ipip 10.87.0.103; serve gre 10.87.0.104
serve ipip6 fd00:87::103; serve gre6 fd00:87::104
serve health 10.88.0.11; serve health6 fd00:88::11 # the health checker probes the backend's real address
sleep 0.6

write_config() {   # write_config <explicit-source: yes|no>
    {
        echo "interface: ldr-lb"
        echo "apiListen: ${API}"
        if [ "$1" = yes ]; then echo "tunnelSource: 10.88.0.1"; echo "tunnelSource6: 'fd00:88::1'"; fi
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        echo "  - {address: 10.87.0.103, port: 8080, protocol: tcp, mode: dsr-ipip, backends: [{address: 10.88.0.11, port: 8080}]}"
        echo "  - {address: 10.87.0.104, port: 8080, protocol: tcp, mode: dsr-gre, backends: [{address: 10.88.0.11, port: 8080}]}"
        echo "  - {address: 'fd00:87::103', port: 8080, protocol: tcp, mode: dsr-ipip, backends: [{address: 'fd00:88::11', port: 8080}]}"
        echo "  - {address: 'fd00:87::104', port: 8080, protocol: tcp, mode: dsr-gre, backends: [{address: 'fd00:88::11', port: 8080}]}"
    } >"$CONFIG"
}

start_rivorad() {
    ip netns exec "$NS_LB" bash -c "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" >"$LOG" 2>&1 &
    RIVORAD_PID=$!
    for _ in $(seq 1 40); do
        in_ns "$NS_LB" curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1 && return 0
        sleep 0.25
    done
    return 1
}
stop_rivorad() { kill "$RIVORAD_PID" 2>/dev/null; wait "$RIVORAD_PID" 2>/dev/null; RIVORAD_PID=""; sleep 0.5; }

# request <vip> <bytes>: what the backend answered.
request() {
    in_ns "$NS_CLIENT" python3 -c '
import socket, sys
vip, n = sys.argv[1], int(sys.argv[2])
fam = socket.AF_INET6 if ":" in vip else socket.AF_INET
try:
    s = socket.socket(fam, socket.SOCK_STREAM); s.settimeout(2.0)
    s.connect((vip, 8080))
    s.sendall(b"x" * (n - 1) + b"\n")
    print(s.recv(64).decode().strip() or "fail")
except Exception:
    print("fail")
' "$1" "$2"
}
expect() {
    local got; got=$(request "$2" "$3")
    [ "$got" = "$4" ] && pass "$1 ($2, ${3}-byte request -> $got)" || fail "$1: $2 answered '$got', want '$4'"
}

run_checks() {   # run_checks <expected v4 outer source> <expected v6 outer source>
    local want4="$1" want6="$2"
    # Neighbour entries for the backend must exist on the load balancer before the first tunnelled
    # packet: the fast path needs a resolved next hop. Health probes keep them warm in practice.
    sleep 3
    expect "IPv4 IP-in-IP" 10.87.0.103 100 "ipip:100"
    expect "IPv4 IP-in-IP" 10.87.0.103 1300 "ipip:1300"
    expect "IPv4 GRE" 10.87.0.104 100 "gre:100"
    expect "IPv4 GRE" 10.87.0.104 1300 "gre:1300"
    expect "IPv6 IPv6-in-IPv6" fd00:87::103 100 "ipip6:100"
    expect "IPv6 IPv6-in-IPv6" fd00:87::103 1300 "ipip6:1300"
    expect "IPv6 GRE over IPv6" fd00:87::104 100 "gre6:100"
    expect "IPv6 GRE over IPv6" fd00:87::104 1300 "gre6:1300"

    # Each mode must use its OWN encapsulation on the wire: the backend accepts both IP-in-IP and GRE,
    # so a VIP encapsulated the wrong way would still be answered.
    local gre4 gre6 ipip4
    in_ns "$NS_BE" timeout -s INT 4 tcpdump -nn -i ldr-be -c 1 'ip proto 47' >"${WORK}/gre4.txt" 2>&1 &
    local q1=$!
    in_ns "$NS_BE" timeout -s INT 4 tcpdump -nn -i ldr-be -c 1 'ip6 proto 47' >"${WORK}/gre6.txt" 2>&1 &
    local q2=$!
    in_ns "$NS_BE" timeout -s INT 4 tcpdump -nn -i ldr-be -c 1 'ip proto 4 and dst 10.88.0.11' >"${WORK}/ipip4.txt" 2>&1 &
    local q3=$!
    sleep 1; request 10.87.0.104 100 >/dev/null; request fd00:87::104 100 >/dev/null
    wait "$q1" "$q2" "$q3" 2>/dev/null
    gre4=$(grep -c "10.87.0.104.8080" "${WORK}/gre4.txt"); gre6=$(grep -c "fd00:87::104.8080" "${WORK}/gre6.txt")
    [ "$gre4" -ge 1 ] && pass "the dsr-gre VIP is carried in GRE (IP protocol 47) over IPv4" || fail "no GRE packet for the dsr-gre IPv4 VIP was seen on the wire"
    [ "$gre6" -ge 1 ] && pass "the dsr-gre VIP is carried in GRE (next header 47) over IPv6" || fail "no GRE packet for the dsr-gre IPv6 VIP was seen on the wire"
    request 10.87.0.103 100 >/dev/null
    # (the IP-in-IP capture above is started before the GRE requests, so it must have stayed empty for them)
    if grep -q "10.87.0.104" "${WORK}/ipip4.txt"; then
        fail "the dsr-gre VIP also appeared as IP-in-IP"
    else
        pass "the dsr-gre VIP never appears as IP-in-IP"
    fi

    # What rivorad says it uses, then what is actually on the wire.
    if grep -q "L3 DSR tunnel sources.*ipv4=${want4}.*ipv6=${want6}" "$LOG"; then
        pass "rivorad reports tunnel sources ${want4} and ${want6}"
    else
        fail "rivorad's tunnel sources are not ${want4} / ${want6}: $(grep 'tunnel sources' "$LOG" | tail -1)"
    fi
    # The outer header's source. Captured on the backend-facing interface.
    local cap4 cap6
    in_ns "$NS_BE" timeout -s INT 4 tcpdump -nn -i ldr-be -c 3 'ip proto 4' >"${WORK}/cap4.txt" 2>&1 &
    local p1=$!
    in_ns "$NS_BE" timeout -s INT 4 tcpdump -nn -i ldr-be -c 3 'ip6 proto 41' >"${WORK}/cap6.txt" 2>&1 &
    local p2=$!
    sleep 1; request 10.87.0.103 100 >/dev/null; request fd00:87::103 100 >/dev/null
    wait "$p1" "$p2" 2>/dev/null
    cap4=$(grep -m1 "10.87.0.2" "${WORK}/cap4.txt" | head -1)
    cap6=$(grep -m1 "fd00:87::2" "${WORK}/cap6.txt" | head -1)
    if grep -q "IP ${want4} > 10.88.0.11" <<<"$(grep -m1 ' > 10.88.0.11' "${WORK}/cap4.txt")"; then
        pass "IPv4 outer header is ${want4} > 10.88.0.11 carrying the client's packet"
    else
        fail "IPv4 outer header: wanted source ${want4}; saw: $(grep -m1 ' > 10.88.0.11' "${WORK}/cap4.txt")"
    fi
    if grep -q "IP6 ${want6} > fd00:88::11" <<<"$(grep -m1 ' > fd00:88::11' "${WORK}/cap6.txt")"; then
        pass "IPv6 outer header is ${want6} > fd00:88::11 carrying the client's packet"
    else
        fail "IPv6 outer header: wanted source ${want6}; saw: $(grep -m1 ' > fd00:88::11' "${WORK}/cap6.txt")"
    fi
}

# ---------------------------------------------------------------------------
section "1-3. Tunnelling with an explicit tunnelSource"
# ---------------------------------------------------------------------------
write_config yes
if ! start_rivorad; then
    fail "rivorad did not come up"; tail -n 30 "$LOG"
else
    pass "rivorad is up with dsr-ipip and dsr-gre VIPs"
    run_checks 10.88.0.1 fd00:88::1
    stop_rivorad
fi

# ---------------------------------------------------------------------------
section "2. With tunnelSource unset the attached interface's own address is used"
# ---------------------------------------------------------------------------
write_config no
in_ns "$NS_LB" rm -rf /sys/fs/bpf/rivora-lb 2>/dev/null
if ! start_rivorad; then
    fail "rivorad did not come up without tunnelSource"; tail -n 30 "$LOG"
else
    pass "rivorad is up with the tunnel source auto-detected"
    run_checks 10.87.0.1 fd00:87::1
    stop_rivorad
fi

# ---------------------------------------------------------------------------
section "4. A bad config is refused"
# ---------------------------------------------------------------------------
cat >"${WORK}/bad.yaml" <<EOF
interface: ldr-lb
vips:
  - {address: 10.87.0.103, port: 8080, protocol: tcp, mode: dsr-ipip, backends: [{address: 10.88.0.11, port: 8080, mac: 'aa:bb:cc:dd:ee:01'}]}
EOF
if out=$("$RIVORACTL" validate "${WORK}/bad.yaml" 2>&1); then
    fail "a MAC on a tunnel backend was accepted: $out"
elif grep -q "only applies to mode dsr" <<<"$out"; then
    pass "a MAC on a tunnel backend is refused ($out)"
else
    fail "refused, but not for the right reason: $out"
fi

echo ""
echo "summary: pass=${PASS} skip=${SKIP} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
