#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-edgecases.sh — the datapath's handling of traffic that is not a plain,
# untagged, unfragmented TCP/UDP packet:
#
#   1. VLAN: 802.1Q and QinQ (802.1ad + 802.1Q) tagged frames reach the VIP's
#      backend, in full-NAT and DSR, IPv4 and IPv6. The tags are in the frame
#      (offload is switched off so the XDP program really sees them).
#   2. IPv4 options: a packet with IP options (ihl > 5) is balanced.
#   3. IPv4 fragments: a UDP datagram larger than the MTU, fragmented by the
#      client, arrives whole at the backend and, in full-NAT, so does the
#      equally fragmented reply, in both NAT and DSR. Only the first fragment
#      carries the L4 header, so the later ones must follow it to the same backend
#      (and be un-NATed on the way back).
#   4. ICMP path-MTU discovery: a "fragmentation needed" (IPv4) or "packet too
#      big" (IPv6) addressed to a VIP is delivered to the backend that owns the
#      connection it quotes, so that backend learns the path MTU. Under NAT the
#      quoted packet is rewritten to the backend's address and the checksums are
#      fixed, or the backend's kernel would drop the message. An ICMP that quotes a
#      connection that does not exist is NOT steered.
#
# Known limits, tested as such where cheap: IPv6 extension headers and IPv6
# fragments are not handled (passed through untouched).
#
# Isolated veth/netns/bridge topology (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-edgecases.sh
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
    echo "selftest-edgecases.sh must run as root (netns + BPF attach)." >&2
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
HAVE_ETHTOOL=1; command -v ethtool >/dev/null 2>&1 || HAVE_ETHTOOL=0

# ---------------------------------------------------------------------------
#   client (.2) --+-- bridge --- lb (.1)
#   backend (.11) -/           untagged 10.83.0.0/24 + fd00:83::/64
#                              VLAN 42  10.84.0.0/24 + fd00:84::/64
#                              QinQ 100/42 10.85.0.0/24
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrec${SUFFIX}"
NS_LB="riv-elb-${SUFFIX}"
NS_CLIENT="riv-eclient-${SUFFIX}"
NS_BE="riv-ebe-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-selftest-ec.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9875"
RIVORAD_PID=""
BE_PIDS=()

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && ip netns exec "$NS_BE" kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

clear_stale_links() {
    local n
    for role in lb cl be; do
        ip link del "ecg-${role}-br" 2>/dev/null || true
        ip link del "ecg-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' ecg-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale ecg-* links did not go away" >&2; return 1
}

# in_ns <ns> <cmd...>
in_ns() { local ns="$1"; shift; ip netns exec "$ns" "$@"; }

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:1" "cl:$NS_CLIENT:2" "be:$NS_BE:11"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
        ip netns add "$ns"
        ip link add "ecg-${role}" type veth peer name "ecg-${role}-br"
        ip link set "ecg-${role}" netns "$ns"
        ip link set "ecg-${role}-br" master "$BR" up
        in_ns "$ns" ip link set lo up
        in_ns "$ns" sysctl -qw "net.ipv6.conf.ecg-${role}.accept_dad=0"
        if [ "$HAVE_ETHTOOL" = 1 ]; then
            # Keep VLAN tags in the frame: with offload on, a tag travels as skb metadata
            # and the XDP program would never see it.
            in_ns "$ns" ethtool -K "ecg-${role}" txvlan off rxvlan off \
                tx-vlan-stag-hw-insert off rx-vlan-stag-hw-parse off >/dev/null 2>&1 || true
            # And no checksum offload. With offload on, a veth carries a "partial" checksum that the
            # receiver never verifies, so a NAT that corrupts the checksum goes unnoticed; with it
            # off every packet carries a complete checksum and every receiver checks it.
            in_ns "$ns" ethtool -K "ecg-${role}" tx off rx off >/dev/null 2>&1 || true
        fi
        in_ns "$ns" ip link set "ecg-${role}" up
        in_ns "$ns" ip addr add "10.83.0.${n}/24" dev "ecg-${role}"
        in_ns "$ns" ip -6 addr add "fd00:83::${n}/64" dev "ecg-${role}"
        # VLAN 42
        in_ns "$ns" ip link add link "ecg-${role}" name "ecg-${role}.42" type vlan id 42
        in_ns "$ns" sysctl -qw "net.ipv6.conf.ecg-${role}/42.accept_dad=0" 2>/dev/null || true
        in_ns "$ns" ip link set "ecg-${role}.42" up
        in_ns "$ns" ip addr add "10.84.0.${n}/24" dev "ecg-${role}.42"
        in_ns "$ns" ip -6 addr add "fd00:84::${n}/64" dev "ecg-${role}.42" nodad
        # QinQ: 802.1ad outer 100, 802.1Q inner 42
        in_ns "$ns" ip link add link "ecg-${role}" name "ecg-${role}.100" type vlan proto 802.1ad id 100 2>/dev/null
        in_ns "$ns" ip link set "ecg-${role}.100" up 2>/dev/null
        in_ns "$ns" ip link add link "ecg-${role}.100" name "ecg-${role}.100.42" type vlan id 42 2>/dev/null
        in_ns "$ns" ip link set "ecg-${role}.100.42" up 2>/dev/null
        in_ns "$ns" ip addr add "10.85.0.${n}/24" dev "ecg-${role}.100.42" 2>/dev/null
        # Three tags: 802.1ad 100, 802.1Q 42, 802.1Q 7.
        in_ns "$ns" ip link add link "ecg-${role}.100.42" name "ecg-${role}.100.42.7" type vlan id 7 2>/dev/null
        in_ns "$ns" ip link set "ecg-${role}.100.42.7" up 2>/dev/null
        in_ns "$ns" ip addr add "10.86.0.${n}/24" dev "ecg-${role}.100.42.7" 2>/dev/null
    done
    # ...and on the bridge-side end of every pair. A veth marks a packet "checksum already verified"
    # when its receiver has rx offload on, and the bridge then carries that mark to the next hop,
    # so leaving these on would let a corrupted checksum through unchecked.
    if [ "$HAVE_ETHTOOL" = 1 ]; then
        for role in lb cl be; do ethtool -K "ecg-${role}-br" tx off rx off >/dev/null 2>&1; done
    fi
    in_ns "$NS_LB" sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
    # Replies to the client come back through the LB to be un-NATed.
    for net in 83 84 85 86; do
        in_ns "$NS_BE" ip route add "10.${net}.0.2/32" via "10.${net}.0.1" 2>/dev/null
    done
    in_ns "$NS_BE" ip -6 route add fd00:83::2/128 via fd00:83::1 dev ecg-be
    in_ns "$NS_BE" ip -6 route add fd00:84::2/128 via fd00:84::1 dev ecg-be.42

    # NAT VIPs are addresses of the LB itself; DSR VIPs are not: they live on the backend's
    # lo, and the client is pointed at the LB's MAC for them.
    in_ns "$NS_LB" ip addr add 10.83.0.100/32 dev ecg-lb
    in_ns "$NS_LB" ip -6 addr add fd00:83::100/128 dev ecg-lb nodad
    in_ns "$NS_LB" ip addr add 10.83.0.101/32 dev ecg-lb
    in_ns "$NS_LB" ip -6 addr add fd00:83::101/128 dev ecg-lb nodad
    in_ns "$NS_LB" ip addr add 10.83.0.105/32 dev ecg-lb
    in_ns "$NS_LB" ip -6 addr add fd00:83::105/128 dev ecg-lb nodad
    in_ns "$NS_LB" ip addr add 10.84.0.100/32 dev ecg-lb.42
    in_ns "$NS_LB" ip -6 addr add fd00:84::100/128 dev ecg-lb.42 nodad
    in_ns "$NS_LB" ip addr add 10.85.0.100/32 dev ecg-lb.100.42 2>/dev/null
    in_ns "$NS_LB" ip addr add 10.86.0.100/32 dev ecg-lb.100.42.7 2>/dev/null

    in_ns "$NS_BE" sysctl -qw net.ipv4.conf.all.arp_ignore=1 net.ipv4.conf.all.arp_announce=2 \
        net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0
    for dev in ecg-be ecg-be.42; do
        in_ns "$NS_BE" sysctl -qw "net.ipv4.conf.${dev//./\/}.rp_filter=0" 2>/dev/null || true
    done
    in_ns "$NS_BE" ip addr add 10.83.0.103/32 dev lo        # DSR v4, tcp
    in_ns "$NS_BE" ip addr add 10.83.0.104/32 dev lo        # DSR v4, udp
    in_ns "$NS_BE" ip -6 addr add fd00:83::103/128 dev lo nodad   # DSR v6, tcp
    in_ns "$NS_BE" ip -6 addr add fd00:83::104/128 dev lo nodad   # DSR v6, udp
    in_ns "$NS_BE" ip addr add 10.84.0.103/32 dev lo        # DSR v4 on VLAN 42
    in_ns "$NS_CLIENT" sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.default.rp_filter=0

    LB_MAC=$(in_ns "$NS_LB" cat /sys/class/net/ecg-lb/address)
    BE_MAC=$(in_ns "$NS_BE" cat /sys/class/net/ecg-be/address)
    for a in 10.83.0.103 10.83.0.104; do
        in_ns "$NS_CLIENT" ip neigh replace "$a" lladdr "$LB_MAC" dev ecg-cl nud permanent
    done
    in_ns "$NS_CLIENT" ip -6 neigh replace fd00:83::103 lladdr "$LB_MAC" dev ecg-cl nud permanent
    in_ns "$NS_CLIENT" ip -6 neigh replace fd00:83::104 lladdr "$LB_MAC" dev ecg-cl nud permanent
    in_ns "$NS_CLIENT" ip neigh replace 10.84.0.103 lladdr "$LB_MAC" dev ecg-cl.42 nud permanent

    tcp_server() { ip netns exec "$NS_BE" python3 -c "$TCP_SERVER" "$@" >/dev/null 2>&1 & BE_PIDS+=($!); }
    udp_server() { ip netns exec "$NS_BE" python3 -c "$UDP_SERVER" "$@" >/dev/null 2>&1 & BE_PIDS+=($!); }
    # ident addr port...: a greeting "<ident>:<port>" per connection.
    tcp_server BE 10.83.0.11 8080 9000 9001
    tcp_server BE fd00:83::11 8080 9000 9001
    tcp_server BE 10.83.0.103 8080
    tcp_server BE fd00:83::103 8080
    tcp_server BE 10.84.0.11 8080
    tcp_server BE fd00:84::11 8080
    tcp_server BE 10.84.0.103 8080
    tcp_server BE 10.85.0.11 8080
    tcp_server BE 10.86.0.11 8080
    udp_server 10.83.0.11 9000
    udp_server 10.83.0.11 9001
    udp_server fd00:83::11 9001
    udp_server 10.83.0.104 9000
    udp_server fd00:83::11 9000
    udp_server fd00:83::104 9000
    sleep 0.6
}

TCP_SERVER='
import socket, sys, threading
ident, addr, ports = sys.argv[1], sys.argv[2], [int(p) for p in sys.argv[3:]]
fam = socket.AF_INET6 if ":" in addr else socket.AF_INET
def serve(port):
    s = socket.socket(fam, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((addr, port)); s.listen(64)
    def handle(c):
        # Stay open until the client hangs up: an ICMP error is only acted on by the
        # endpoint whose socket it quotes, so the connection has to still exist.
        try:
            c.sendall(("%s:%d\n" % (ident, port)).encode())
            c.settimeout(6); c.recv(1)
        except OSError:
            pass
        finally:
            c.close()
    while True:
        c, _ = s.accept()
        threading.Thread(target=handle, args=(c,), daemon=True).start()
for p in ports:
    threading.Thread(target=serve, args=(p,), daemon=True).start()
threading.Event().wait()
'
# A UDP echo: replies with exactly what it received, from the address it was sent to.
UDP_SERVER='
import socket, sys
addr, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET6 if ":" in addr else socket.AF_INET, socket.SOCK_DGRAM)
s.bind((addr, port))
while True:
    d, peer = s.recvfrom(65535)
    try:
        s.sendto(d, peer)
    except OSError:
        pass
'

# probe <vip> <port> [ip-options]: the greeting, or "fail".
probe() {
    in_ns "$NS_CLIENT" python3 -c '
import socket, sys
vip, port, opts = sys.argv[1], int(sys.argv[2]), sys.argv[3] == "opts"
fam = socket.AF_INET6 if ":" in vip else socket.AF_INET
try:
    s = socket.socket(fam, socket.SOCK_STREAM); s.settimeout(1.5)
    if opts:
        s.setsockopt(socket.IPPROTO_IP, socket.IP_OPTIONS, b"\x01\x01\x01\x01")  # four NOPs: ihl becomes 6
    s.connect((vip, port))
    print(s.recv(32).decode().strip() or "fail")
except Exception:
    print("fail")
' "$1" "$2" "${3:-}"
}

expect() {   # expect <vip> <port> <want> <label> [opts]
    local got; got=$(probe "$1" "$2" "${5:-}")
    [ "$got" = "$3" ] && pass "$4 ($1:$2 -> $got)" || fail "$4: $1:$2 answered '$got', want '$3'"
}

# udp_echo <vip> <port> <bytes> <count>: sends <count> random datagrams of <bytes> bytes (which the
# client's kernel fragments to the MTU) and counts the ones echoed back intact.
udp_echo() {
    in_ns "$NS_CLIENT" python3 -c '
import os, socket, sys
vip, port, size, count = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
ok = 0
for _ in range(count):
    data = os.urandom(size)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM); s.settimeout(2.0)
    try:
        s.sendto(data, (vip, port))
        got, peer = s.recvfrom(65535)
        if got == data and peer == (vip, port):
            ok += 1
    except Exception:
        pass
    s.close()
print(ok)
' "$1" "$2" "$3" "$4"
}

# icmp_error <vip> <vport> <bogus|real>: sends the VIP a UDP datagram (so the load balancer knows the
# flow), then an ICMP "fragmentation needed" (IPv4, MTU 1000) or "packet too big" (IPv6, MTU 1400)
# that quotes the reply the backend "sent" on that flow. "bogus" quotes a client port no flow uses.
# UDP rather than TCP: a TCP endpoint ignores an ICMP error whose quoted sequence number is outside
# its send window, which a test that cannot see the backend's sequence numbers would trip over.
icmp_error() {
    in_ns "$NS_CLIENT" python3 -c '
import socket, struct, sys, time
vip, vport, which = sys.argv[1], int(sys.argv[2]), sys.argv[3]
v6 = ":" in vip
fam = socket.AF_INET6 if v6 else socket.AF_INET
c = socket.socket(fam, socket.SOCK_DGRAM); c.settimeout(2.0)
c.connect((vip, vport))   # a connected socket, so getsockname() gives the client address the ICMP must quote
c.send(b"hello"); c.recv(64)
me = c.getsockname()[0]
cport = c.getsockname()[1] if which == "real" else 45999

def csum(b):
    if len(b) % 2: b += b"\0"
    s = sum(struct.unpack("!%dH" % (len(b) // 2), b))
    s = (s >> 16) + (s & 0xffff); s += s >> 16
    return ~s & 0xffff

l4 = struct.pack("!HHHH", vport, cport, 1400, 0)   # the quoted UDP header of the backend reply: VIP:vport -> client:cport
if not v6:
    vb, mb = socket.inet_aton(vip), socket.inet_aton(me)
    ip = struct.pack("!BBHHHBBH4s4s", 0x45, 0, 1428, 0x1234, 0x4000, 64, 17, 0, vb, mb)
    ip = ip[:10] + struct.pack("!H", csum(ip)) + ip[12:]
    icmp = struct.pack("!BBHHH", 3, 4, 0, 0, 1000) + ip + l4      # dest unreachable, frag needed, MTU 1000
    icmp = icmp[:2] + struct.pack("!H", csum(icmp)) + icmp[4:]
    r = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
    r.sendto(icmp, (vip, 0))
else:
    vb, mb = socket.inet_pton(socket.AF_INET6, vip), socket.inet_pton(socket.AF_INET6, me)
    ip = struct.pack("!IHBB16s16s", 0x60000000, 1400, 17, 64, vb, mb)
    icmp = struct.pack("!BBHI", 2, 0, 0, 1400) + ip + l4           # packet too big, MTU 1400
    r = socket.socket(socket.AF_INET6, socket.SOCK_RAW, socket.IPPROTO_ICMPV6)   # the kernel fills in the checksum
    r.sendto(icmp, (vip, 0, 0, 0))
time.sleep(0.5)
' "$1" "$2" "$3"
}

# backend_pmtu <v4|v6> prints the path MTU the backend has cached towards the client, or "none".
backend_pmtu() {
    local out
    if [ "$1" = v4 ]; then out=$(in_ns "$NS_BE" ip route get 10.83.0.2 2>/dev/null); else out=$(in_ns "$NS_BE" ip -6 route get fd00:83::2 2>/dev/null); fi
    grep -o 'mtu [0-9]*' <<<"$out" | head -1 | cut -d' ' -f2 | grep . || echo none
}
flush_pmtu() { in_ns "$NS_BE" ip route flush cache 2>/dev/null; in_ns "$NS_BE" ip -6 route flush cache 2>/dev/null; }

setup_topology
{
    echo "interface: ecg-lb"
    echo "apiListen: ${API}"
    echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
    echo "vips:"
    echo "  - {address: 10.83.0.100, port: 8080, protocol: tcp, mode: nat, backends: [{address: 10.83.0.11, port: 8080}]}"
    echo "  - {address: 'fd00:83::100', port: 8080, protocol: tcp, mode: nat, backends: [{address: 'fd00:83::11', port: 8080}]}"
    echo "  - {address: 10.83.0.103, port: 8080, protocol: tcp, mode: dsr, backends: [{address: 10.83.0.11, port: 8080, mac: '${BE_MAC}'}]}"
    echo "  - {address: 'fd00:83::103', port: 8080, protocol: tcp, mode: dsr, backends: [{address: 'fd00:83::11', port: 8080, mac: '${BE_MAC}'}]}"
    echo "  - {address: 10.83.0.101, port: 9000, protocol: udp, mode: nat, backends: [{address: 10.83.0.11, port: 9000}]}"
    echo "  - {address: 10.83.0.104, port: 9000, protocol: udp, mode: dsr, backends: [{address: 10.83.0.11, port: 9000, mac: '${BE_MAC}'}]}"
    echo "  - {address: 'fd00:83::101', port: 9000, protocol: udp, mode: nat, backends: [{address: 'fd00:83::11', port: 9000}]}"
    echo "  - {address: 'fd00:83::104', port: 9000, protocol: udp, mode: dsr, backends: [{address: 'fd00:83::11', port: 9000, mac: '${BE_MAC}'}]}"
    echo "  - {address: 10.83.0.105, port: 9100, protocol: udp, mode: nat, backends: [{address: 10.83.0.11, port: 9001}]}"
    echo "  - {address: 'fd00:83::105', port: 9100, protocol: udp, mode: nat, backends: [{address: 'fd00:83::11', port: 9001}]}"
    echo "  - {address: 10.84.0.100, port: 8080, protocol: tcp, mode: nat, backends: [{address: 10.84.0.11, port: 8080}]}"
    echo "  - {address: 'fd00:84::100', port: 8080, protocol: tcp, mode: nat, backends: [{address: 'fd00:84::11', port: 8080}]}"
    echo "  - {address: 10.84.0.103, port: 8080, protocol: tcp, mode: dsr, backends: [{address: 10.84.0.11, port: 8080, mac: '${BE_MAC}'}]}"
    echo "  - {address: 10.85.0.100, port: 8080, protocol: tcp, mode: nat, backends: [{address: 10.85.0.11, port: 8080}]}"
    echo "  - {address: 10.86.0.100, port: 8080, protocol: tcp, mode: nat, backends: [{address: 10.86.0.11, port: 8080}]}"
} >"$CONFIG"

ip netns exec "$NS_LB" bash -c "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" >"$LOG" 2>&1 &
RIVORAD_PID=$!
up=0
for _ in $(seq 1 40); do
    in_ns "$NS_LB" curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1 && { up=1; break; }
    sleep 0.25
done
if [ "$up" != 1 ]; then
    fail "rivorad did not come up"; tail -n 30 "$LOG"; echo ""; echo "summary: pass=${PASS} fail=${FAIL}"; exit 1
fi
pass "rivorad is up"
sleep 3 # every backend probed healthy

# ---------------------------------------------------------------------------
section "Baseline: untagged, unoptioned traffic still works"
# ---------------------------------------------------------------------------
expect 10.83.0.100 8080 "BE:8080" "v4 full-NAT"
expect fd00:83::100 8080 "BE:8080" "v6 full-NAT"
expect 10.83.0.103 8080 "BE:8080" "v4 DSR"
expect fd00:83::103 8080 "BE:8080" "v6 DSR"

# ---------------------------------------------------------------------------
section "1. VLAN and QinQ"
# ---------------------------------------------------------------------------
if [ "$HAVE_ETHTOOL" = 1 ] && in_ns "$NS_LB" ethtool -k ecg-lb 2>/dev/null | grep -q "rx-vlan-offload: off"; then
    pass "VLAN offload is off, so tags are in the frame the XDP program sees"
    expect 10.84.0.100 8080 "BE:8080" "v4 full-NAT over 802.1Q"
    expect fd00:84::100 8080 "BE:8080" "v6 full-NAT over 802.1Q"
    expect 10.84.0.103 8080 "BE:8080" "v4 DSR over 802.1Q"
    # Generic XDP hands the program the frame with its OUTERMOST tag already removed, so a
    # single tag never reaches it; QinQ shows it one tag and three tags show it two. The last
    # two therefore exercise the tag skipping (one and two iterations).
    if in_ns "$NS_CLIENT" ip link show ecg-cl.100.42 >/dev/null 2>&1; then
        expect 10.85.0.100 8080 "BE:8080" "v4 full-NAT over QinQ (802.1ad + 802.1Q): one tag in the frame XDP sees"
    else
        skip "QinQ interfaces could not be created on this kernel"
    fi
    if in_ns "$NS_CLIENT" ip link show ecg-cl.100.42.7 >/dev/null 2>&1; then
        expect 10.86.0.100 8080 "BE:8080" "v4 full-NAT over three tags: two tags in the frame XDP sees"
    else
        skip "triple-tagged interfaces could not be created on this kernel"
    fi
else
    skip "cannot switch VLAN offload off here (no ethtool, or the veth refuses): tags would not be in the frame"
fi

# ---------------------------------------------------------------------------
section "2. IPv4 options"
# ---------------------------------------------------------------------------
expect 10.83.0.100 8080 "BE:8080" "v4 full-NAT with IP options (ihl 6)" opts
expect 10.83.0.103 8080 "BE:8080" "v4 DSR with IP options (ihl 6)" opts

# ---------------------------------------------------------------------------
section "3. IPv4 fragments (a 4000-byte UDP datagram is 3 fragments at MTU 1500)"
# ---------------------------------------------------------------------------
n=$(udp_echo 10.83.0.101 9000 4000 10)
[ "$n" = 10 ] && pass "v4 full-NAT: 10/10 fragmented datagrams echoed back intact (request and reply both fragmented)" \
    || fail "v4 full-NAT: only ${n}/10 fragmented datagrams came back intact"
n=$(udp_echo 10.83.0.104 9000 4000 10)
[ "$n" = 10 ] && pass "v4 DSR: 10/10 fragmented datagrams echoed back intact" \
    || fail "v4 DSR: only ${n}/10 fragmented datagrams came back intact"
n=$(udp_echo 10.83.0.101 9000 200 10)
[ "$n" = 10 ] && pass "v4 full-NAT: unfragmented datagrams unaffected" || fail "v4 full-NAT: unfragmented datagrams broke (${n}/10)"

# ---------------------------------------------------------------------------
section "4. ICMP path-MTU discovery is delivered to the owning backend"
# ---------------------------------------------------------------------------
# The last two use a VIP port (9100) that is not the backend's (9001): the quoted port must be rewritten
# too, and folded into the ICMP checksum, or the backend's kernel rejects the message.
for c in "v4 10.83.0.101 9000 nat 1000" "v4 10.83.0.104 9000 dsr 1000" "v6 fd00:83::101 9000 nat 1400" "v6 fd00:83::104 9000 dsr 1400" \
         "v4 10.83.0.105 9100 nat-with-port-rewrite 1000" "v6 fd00:83::105 9100 nat-with-port-rewrite 1400"; do
    set -- $c; fam="$1"; vip="$2"; vport="$3"; mode="$4"; want="$5"
    flush_pmtu
    icmp_error "$vip" "$vport" bogus
    got=$(backend_pmtu "$fam")
    [ "$got" = none ] && pass "$fam $mode: an ICMP error quoting a connection that does not exist is not delivered to the backend" \
        || fail "$fam $mode: a bogus ICMP error changed the backend's path MTU to $got"
    icmp_error "$vip" "$vport" real
    got=$(backend_pmtu "$fam")
    [ "$got" = "$want" ] && pass "$fam $mode: the backend learned the path MTU ($got) from an ICMP sent to the VIP" \
        || fail "$fam $mode: the backend's path MTU is '$got', want $want (ICMP not delivered, or its checksum wrong)"
done

echo ""
echo "summary: pass=${PASS} skip=${SKIP} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
