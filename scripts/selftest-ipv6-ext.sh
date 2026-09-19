#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-ipv6-ext.sh — IPv6 extension headers and IPv6 fragments.
#
# Before this, a packet with any extension header ahead of the TCP/UDP header was passed
# through unbalanced, and a fragmented IPv6 datagram was never load-balanced at all. This
# sends real traffic through the real kernel stack (which validates every checksum and
# reassembles every fragmented datagram) and checks:
#
#   1. full-NAT, TCP:   with a Destination Options header, and with Hop-by-Hop + Destination
#                       Options chained, every connection completes on some backend, and both
#                       backends are used
#   2. full-NAT, UDP:   a 4000-byte datagram (three fragments each way) round-trips intact from
#                       the VIP's address. It only can if every fragment of a request reached the
#                       SAME backend and every fragment of the reply had its source rewritten
#   3. full-NAT, UDP with a Destination Options header as well as fragmentation
#   4. DSR (L2):        the same fragmented UDP round trip and an extension-header TCP connection;
#                       the replies come straight from the backend, bypassing the balancer
#
# Isolated veth/netns/bridge topology (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-ipv6-ext.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-ipv6-ext.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi
if ! command -v ethtool >/dev/null 2>&1; then
    echo "selftest-ipv6-ext.sh needs ethtool to switch checksum offload off." >&2
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
#   client (::2, and fd00:93:1::1 off-segment) --+
#   be1 (::11), be2 (::12) ----------------------+-- bridge --- lb (::1)
# The off-segment client address makes full-NAT replies return through the balancer (whose TCX
# program un-NATs them); the on-segment one lets a DSR backend answer the client directly.
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrx${SUFFIX}"
NS_LB="riv-xlb-${SUFFIX}"; NS_CLIENT="riv-xcl-${SUFFIX}"; NS_B1="riv-xb1-${SUFFIX}"; NS_B2="riv-xb2-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-selftest-x6.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9880"
RIVORAD_PID=""
BE_PIDS=()
NAT_VIP="fd00:93::100"
DSR_VIP="fd00:93::110"
NAT_CLIENT="fd00:93:1::1"
DSR_CLIENT="fd00:93::2"

cleanup() {
    [ -n "$RIVORAD_PID" ] && kill "$RIVORAD_PID" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_B1" "$NS_B2"; do ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_B1" "$NS_B2"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

for role in lb cl b1 b2; do ip link del "px-${role}-br" 2>/dev/null || true; ip link del "px-${role}" 2>/dev/null || true; done
for _ in $(seq 1 50); do ip -o link 2>/dev/null | grep -qE ' px-(lb|cl|b1|b2)(-br)?[:@]' || break; sleep 0.1; done

ip link add "$BR" type bridge; ip link set "$BR" up
for pair in "lb:$NS_LB:1" "cl:$NS_CLIENT:2" "b1:$NS_B1:11" "b2:$NS_B2:12"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
    ip netns add "$ns"
    ip link add "px-${role}" type veth peer name "px-${role}-br"
    ip link set "px-${role}" netns "$ns"
    ip link set "px-${role}-br" master "$BR" up
    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" sysctl -qw "net.ipv6.conf.px-${role}.accept_dad=0"
    ip netns exec "$ns" ip link set "px-${role}" up
    ip netns exec "$ns" ip -6 addr add "fd00:93::${n}/64" dev "px-${role}" nodad
    # A checksum a receiver never verifies would hide a NAT that corrupts it: switch offload off on
    # both ends of every pair (the bridge side too, since veth marks packets verified and the bridge
    # carries the mark).
    ip netns exec "$ns" ethtool -K "px-${role}" tx off rx off >/dev/null 2>&1
    ethtool -K "px-${role}-br" tx off rx off >/dev/null 2>&1
done
ip netns exec "$NS_LB" sysctl -qw net.ipv6.conf.all.forwarding=1
# preferred_lft 0 keeps the VIPs from being chosen as the source of the balancer's own traffic (its
# health probes): a DSR backend owns the DSR VIP, so a probe sourced from it would never leave the backend.
for v in "$NAT_VIP" "$DSR_VIP"; do ip netns exec "$NS_LB" ip -6 addr add "${v}/128" dev px-lb nodad preferred_lft 0; done
ip netns exec "$NS_CLIENT" ip -6 addr add "${NAT_CLIENT}/128" dev px-cl nodad
ip netns exec "$NS_LB" ip -6 route add fd00:93:1::/64 via fd00:93::2 dev px-lb
for ns_dev in "$NS_B1:px-b1" "$NS_B2:px-b2"; do
    ip netns exec "${ns_dev%%:*}" ip -6 route add fd00:93:1::/64 via fd00:93::1 dev "${ns_dev##*:}"
done
# A DSR backend answers as the VIP, so it must own the address.
for ns in "$NS_B1" "$NS_B2"; do ip netns exec "$ns" ip -6 addr add "${DSR_VIP}/128" dev lo nodad; done
MAC1=$(ip netns exec "$NS_B1" cat /sys/class/net/px-b1/address)
MAC2=$(ip netns exec "$NS_B2" cat /sys/class/net/px-b2/address)

# TCP: answers with its identity. UDP: echoes "ident|payload" from the address the datagram was
# sent to, so a DSR reply carries the VIP as its source.
SERVER='
import socket, struct, sys, threading
ident = sys.argv[1]
def tcp():
    s = socket.socket(socket.AF_INET6); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("::", 8080)); s.listen(128)
    while True:
        c, _ = s.accept()
        try: c.sendall((ident + "\n").encode())
        except OSError: pass
        finally: c.close()
def udp():
    s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM)
    s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_RECVPKTINFO, 1)
    s.bind(("::", 9000))
    while True:
        data, anc, _, addr = s.recvmsg(65535, 1024)
        pkt = [d for lvl, typ, d in anc if typ == socket.IPV6_PKTINFO]
        reply = ident.encode() + b"|" + data
        try:
            if pkt: s.sendmsg([reply], [(socket.IPPROTO_IPV6, socket.IPV6_PKTINFO, pkt[0])], 0, addr)
            else:   s.sendto(reply, addr)
        except OSError: pass
threading.Thread(target=tcp, daemon=True).start()
udp()
'
ip netns exec "$NS_B1" python3 -c "$SERVER" BACKEND-1 >/dev/null 2>&1 & BE_PIDS+=("$!")
ip netns exec "$NS_B2" python3 -c "$SERVER" BACKEND-2 >/dev/null 2>&1 & BE_PIDS+=("$!")
sleep 0.7

# The topology itself, before the balancer is involved.
if ip netns exec "$NS_LB" ping -6 -c1 -W2 fd00:93::11 >/dev/null 2>&1 && ip netns exec "$NS_LB" ping -6 -c1 -W2 fd00:93::12 >/dev/null 2>&1 \
    && ip netns exec "$NS_CLIENT" ping -6 -c1 -W2 fd00:93::1 >/dev/null 2>&1; then
    pass "the topology carries IPv6 (balancer reaches both backends and the client)"
else
    fail "the topology does not carry IPv6 before the balancer starts"
    if [ -n "${DEBUG:-}" ]; then
        ip netns exec "$NS_LB" ping -6 -c1 -W2 fd00:93::11 2>&1 | tail -3; ip netns exec "$NS_LB" ip -6 neigh; ip netns exec "$NS_LB" ip -6 addr show dev px-lb | head -12
        ip netns exec "$NS_B1" ip -6 addr show dev px-b1 | head -6; bridge link | grep -E "px-" ; ip -o link show master "$BR"
    fi
    exit 1
fi

cat >"$CONFIG" <<EOF
interface: px-lb
apiListen: ${API}
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: '${NAT_VIP}', port: 8080, protocol: tcp, mode: nat, backends: [{address: 'fd00:93::11', port: 8080}, {address: 'fd00:93::12', port: 8080}]}
  - {address: '${NAT_VIP}', port: 9000, protocol: udp, mode: nat, healthCheck: {port: 8080}, backends: [{address: 'fd00:93::11', port: 9000}, {address: 'fd00:93::12', port: 9000}]}
  - {address: '${DSR_VIP}', port: 8080, protocol: tcp, mode: dsr, backends: [{address: 'fd00:93::11', port: 8080, mac: '${MAC1}'}, {address: 'fd00:93::12', port: 8080, mac: '${MAC2}'}]}
  - {address: '${DSR_VIP}', port: 9000, protocol: udp, mode: dsr, healthCheck: {port: 8080}, backends: [{address: 'fd00:93::11', port: 9000, mac: '${MAC1}'}, {address: 'fd00:93::12', port: 9000, mac: '${MAC2}'}]}
EOF
ip netns exec "$NS_LB" bash -c "mount -t bpf bpf /sys/fs/bpf 2>/dev/null; exec '$RIVORAD' -config '$CONFIG' -bpf-dir '$BPF_DIR'" >"$LOG" 2>&1 &
RIVORAD_PID=$!
up=0
for _ in $(seq 1 40); do
    ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1 && { up=1; break; }
    sleep 0.25
done
sleep 3 # health checks settle
if [ "$up" = 1 ]; then pass "rivorad is up with a full-NAT and a DSR VIP, each over TCP and UDP"; else fail "rivorad did not come up"; tail -n 20 "$LOG"; exit 1; fi

# tcp <src> <vip> <n> <ext>: n connections, each carrying the given extension headers on every
# packet (none | dst | hbh | both). Prints "<answered> <backend-1 count> <backend-2 count>".
tcp() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
src, vip, n, ext = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
PAD = bytes([0, 0, 1, 4, 0, 0, 0, 0])   # an 8-byte header holding one PadN option
got = {"BACKEND-1": 0, "BACKEND-2": 0}; ok = 0
for _ in range(n):
    try:
        s = socket.socket(socket.AF_INET6); s.settimeout(2)
        if ext in ("dst", "both"): s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_DSTOPTS, PAD)
        if ext in ("hbh", "both"): s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_HOPOPTS, PAD)
        s.bind((src, 0)); s.connect((vip, 8080))
        who = s.recv(32).decode().strip()
        if who in got: got[who] += 1; ok += 1
    except Exception:
        pass
    finally:
        s.close()
print(ok, got["BACKEND-1"], got["BACKEND-2"])
' "$@"
}

# udp <src> <vip> <n> <size> <ext>: n datagrams of size bytes, each from its own source port, echoed
# back. Prints "<intact> <backend-1> <backend-2> <wrong-source> <corrupt>".
udp() {
    ip netns exec "$NS_CLIENT" python3 -c '
import os, socket, sys
src, vip, n, size, ext = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4]), sys.argv[5]
PAD = bytes([0, 0, 1, 4, 0, 0, 0, 0])
got = {"BACKEND-1": 0, "BACKEND-2": 0}; ok = wrong_src = corrupt = 0
for _ in range(n):
    s = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM); s.settimeout(2)
    try:
        if ext == "dst": s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_DSTOPTS, PAD)
        s.bind((src, 0))
        data = os.urandom(size)
        s.sendto(data, (vip, 9000))
        reply, frm = s.recvfrom(65535)
        if frm[0] != vip: wrong_src += 1; continue
        who, _, body = reply.partition(b"|")
        if body != data: corrupt += 1; continue
        if who.decode() in got: got[who.decode()] += 1; ok += 1
    except Exception:
        pass
    finally:
        s.close()
print(ok, got["BACKEND-1"], got["BACKEND-2"], wrong_src, corrupt)
' "$@"
}

reasm() {   # reasm <ns>: successfully reassembled IPv6 datagrams so far
    ip netns exec "$1" awk '$1 == "Ip6ReasmOKs" {print $2}' /proc/net/snmp6
}

both_used() { [ "$1" -gt 0 ] && [ "$2" -gt 0 ]; }

# ---------------------------------------------------------------------------
section "1. Full-NAT, TCP with extension headers"
# ---------------------------------------------------------------------------
read -r ok b1 b2 < <(tcp "$NAT_CLIENT" "$NAT_VIP" 20 none)
[ "$ok" -ge 18 ] && both_used "$b1" "$b2" && pass "control, no extension header: ${ok}/20 answered (${b1}/${b2})" \
    || { fail "control failed: ${ok}/20 answered (${b1}/${b2})"; [ -n "${DEBUG:-}" ] && { ip netns exec "$NS_LB" ping -6 -c1 -W1 fd00:93::11 2>&1 | tail -2; ip netns exec "$NS_B1" ss -lntu 2>&1 | head; ip netns pids "$NS_B1"; tail -n 20 "$LOG"; ip netns exec "$NS_LB" curl -s "http://${API}/api/v1/vips" | head -c 1500; echo; }; }
read -r ok b1 b2 < <(tcp "$NAT_CLIENT" "$NAT_VIP" 20 dst)
[ "$ok" -ge 18 ] && both_used "$b1" "$b2" && pass "Destination Options header on every packet: ${ok}/20 answered, both backends used (${b1}/${b2})" \
    || fail "Destination Options: ${ok}/20 answered (${b1}/${b2})"
read -r ok b1 b2 < <(tcp "$NAT_CLIENT" "$NAT_VIP" 20 both)
[ "$ok" -ge 18 ] && both_used "$b1" "$b2" && pass "Hop-by-Hop then Destination Options chained: ${ok}/20 answered, both backends used (${b1}/${b2})" \
    || fail "Hop-by-Hop + Destination Options: ${ok}/20 answered (${b1}/${b2})"

# ---------------------------------------------------------------------------
section "2. Full-NAT, fragmented UDP"
# ---------------------------------------------------------------------------
req0=$(( $(reasm "$NS_B1") + $(reasm "$NS_B2") )); rep0=$(reasm "$NS_CLIENT")
read -r ok b1 b2 ws bad < <(udp "$NAT_CLIENT" "$NAT_VIP" 30 4000 none)
[ "$ok" -ge 27 ] && pass "4000-byte datagrams round-trip intact: ${ok}/30" || fail "fragmented UDP: only ${ok}/30 intact (wrong source ${ws}, corrupt ${bad})"
both_used "$b1" "$b2" && pass "both backends took datagrams (${b1}/${b2}), so a datagram's fragments were kept together per datagram, not per source" \
    || fail "one backend took everything (${b1}/${b2})"
[ "$ok" -gt 0 ] && [ "$ws" = 0 ] && [ "$bad" = 0 ] && pass "every reply came from the VIP address with its payload intact" || fail "wrong-source ${ws}, corrupt ${bad}"
req1=$(( $(reasm "$NS_B1") + $(reasm "$NS_B2") )); rep1=$(reasm "$NS_CLIENT")
[ "$ok" -gt 0 ] && [ $((req1 - req0)) -ge "$ok" ] && pass "the backends reassembled $((req1 - req0)) fragmented requests (the fragments were really fragments, and all reached one backend)" \
    || fail "backends reassembled only $((req1 - req0)) requests"
[ "$ok" -gt 0 ] && [ $((rep1 - rep0)) -ge "$ok" ] && pass "the client reassembled $((rep1 - rep0)) fragmented replies (each fragment's source was rewritten back to the VIP)" \
    || fail "client reassembled only $((rep1 - rep0)) replies"

# ---------------------------------------------------------------------------
section "3. Full-NAT, fragmented UDP with a Destination Options header"
# ---------------------------------------------------------------------------
read -r ok b1 b2 ws bad < <(udp "$NAT_CLIENT" "$NAT_VIP" 30 4000 dst)
[ "$ok" -ge 27 ] && both_used "$b1" "$b2" && [ "$ws" = 0 ] && [ "$bad" = 0 ] \
    && pass "Destination Options + fragmentation: ${ok}/30 intact, both backends used (${b1}/${b2})" \
    || fail "Destination Options + fragmentation: ${ok}/30 intact (${b1}/${b2}, wrong source ${ws}, corrupt ${bad})"

# ---------------------------------------------------------------------------
section "4. DSR (L2)"
# ---------------------------------------------------------------------------
req0=$(( $(reasm "$NS_B1") + $(reasm "$NS_B2") ))
read -r ok b1 b2 ws bad < <(udp "$DSR_CLIENT" "$DSR_VIP" 30 4000 none)
[ "$ok" -ge 27 ] && both_used "$b1" "$b2" && [ "$ws" = 0 ] && [ "$bad" = 0 ] \
    && pass "fragmented UDP: ${ok}/30 intact, both backends used (${b1}/${b2}), replies straight from the backend as the VIP" \
    || fail "DSR fragmented UDP: ${ok}/30 intact (${b1}/${b2}, wrong source ${ws}, corrupt ${bad})"
req1=$(( $(reasm "$NS_B1") + $(reasm "$NS_B2") ))
[ "$ok" -gt 0 ] && [ $((req1 - req0)) -ge "$ok" ] && pass "the backends reassembled $((req1 - req0)) fragmented requests" || fail "backends reassembled only $((req1 - req0)) requests"
read -r ok b1 b2 < <(tcp "$DSR_CLIENT" "$DSR_VIP" 20 both)
[ "$ok" -ge 18 ] && both_used "$b1" "$b2" && pass "Hop-by-Hop + Destination Options TCP: ${ok}/20 answered, both backends used (${b1}/${b2})" \
    || fail "DSR extension-header TCP: ${ok}/20 answered (${b1}/${b2})"

# ---------------------------------------------------------------------------
section "5. The limits: a chain that is too long is left alone"
# ---------------------------------------------------------------------------
# A hand-built UDP packet with N Destination Options headers in a row, sent as a raw frame: the
# balancer walks up to four headers, so three are balanced and five are not. Unwalked, the packet
# goes to the balancer's own stack, which has no listener on the VIP and answers "port unreachable"
# instead of a backend echoing it.
LBMAC=$(ip netns exec "$NS_LB" cat /sys/class/net/px-lb/address)
chain() {   # chain <headers>: prints the backend that answered, or "none"
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, struct, sys
n, lbmac, src, dst = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4]
def mac(m): return bytes(int(x, 16) for x in m.split(":"))
rx = socket.socket(socket.AF_INET6, socket.SOCK_DGRAM); rx.settimeout(1.5); rx.bind((src, 40000))
payload = b"hello"
udp_len = 8 + len(payload)
s6, d6 = socket.inet_pton(socket.AF_INET6, src), socket.inet_pton(socket.AF_INET6, dst)
pseudo = s6 + d6 + struct.pack("!II", udp_len, 17)
udp0 = struct.pack("!HHHH", 40000, 9000, udp_len, 0) + payload
tot = pseudo + udp0
if len(tot) % 2: tot += b"\0"
c = sum(struct.unpack("!%dH" % (len(tot) // 2), tot))
while c >> 16: c = (c & 0xffff) + (c >> 16)
csum = (~c & 0xffff) or 0xffff
udp = struct.pack("!HHHH", 40000, 9000, udp_len, csum) + payload
PAD = bytes([0, 0, 1, 4, 0, 0, 0, 0])            # next header patched below, hdrlen 0, one PadN
exts = b""
for i in range(n):
    exts += bytes([60 if i < n - 1 else 17]) + PAD[1:]
plen = len(exts) + len(udp)
ip6 = struct.pack("!IHBB", 6 << 28, plen, 60, 64) + s6 + d6
tx = socket.socket(socket.AF_PACKET, socket.SOCK_RAW); tx.bind(("px-cl", 0))
cl = open("/sys/class/net/px-cl/address").read().strip()
tx.send(mac(lbmac) + mac(cl) + b"\x86\xdd" + ip6 + exts + udp)
try:
    data, frm = rx.recvfrom(2048)
    print(data.split(b"|")[0].decode())
except Exception:
    print("none")
' "$1" "$LBMAC" "$NAT_CLIENT" "$NAT_VIP"
}
r3=$(chain 3); r4=$(chain 4); r5=$(chain 5)
case "$r3" in BACKEND-*) pass "three Destination Options headers in a row are balanced (answered by ${r3})" ;; *) fail "three headers were not balanced: ${r3}" ;; esac
case "$r4" in BACKEND-*) pass "four headers, the walk's limit, are still balanced (${r4})" ;; *) fail "four headers were not balanced: ${r4}" ;; esac
[ "$r5" = none ] && pass "five headers are left alone: no backend answered (the balancer's own stack got the packet)" || fail "five headers were balanced anyway: ${r5}"

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
