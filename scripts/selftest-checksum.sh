#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-checksum.sh — full-NAT must leave TCP and UDP checksums VALID.
#
# Why this exists: on a veth pair with checksum offload on (the default), a packet
# carries a "partial" checksum that the receiving end never verifies, so a NAT that
# corrupts the checksum field goes unnoticed. That is exactly what happened: the
# 16-bit port update went through bpf_csum_diff(), which rejects a size of 2 with
# -EINVAL, and the unchecked error silently subtracted 22 from the checksum of every
# NAT'd packet. Every earlier selftest passed regardless; a real NIC would have
# discarded the traffic.
#
# So here checksum offload is switched OFF on every interface: each packet carries a
# complete checksum, and each receiver (the backend for the request, the client for the
# reply) verifies it. The VIP port differs from the backend port so the port-rewrite
# path (the one that used to corrupt the checksum) runs in both directions.
#
# Covers TCP and UDP, IPv4 and IPv6, full-NAT. Isolated veth/netns/bridge topology
# (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-checksum.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-checksum.sh must run as root (netns + BPF attach)." >&2
    exit 1
fi
if ! command -v ethtool >/dev/null 2>&1; then
    echo "selftest-checksum.sh needs ethtool to switch checksum offload off." >&2
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
#   client (.2) --+-- bridge --- lb (.1)      10.86.0.0/24 and fd00:86::/64
#   backend (.11) -/
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrck${SUFFIX}"
NS_LB="riv-klb-${SUFFIX}"
NS_CLIENT="riv-kclient-${SUFFIX}"
NS_BE="riv-kbe-${SUFFIX}"
VIP_PORT=8080
BE_PORT=9090
WORK="$(mktemp -d /tmp/rivora-selftest-ck.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9876"
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
        ip link del "cks-${role}-br" 2>/dev/null || true
        ip link del "cks-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' cks-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale cks-* links did not go away" >&2; return 1
}

in_ns() { local ns="$1"; shift; ip netns exec "$ns" "$@"; }

clear_stale_links || exit 1
ip link add "$BR" type bridge; ip link set "$BR" up
for pair in "lb:$NS_LB:1" "cl:$NS_CLIENT:2" "be:$NS_BE:11"; do
    role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
    ip netns add "$ns"
    ip link add "cks-${role}" type veth peer name "cks-${role}-br"
    ip link set "cks-${role}" netns "$ns"
    ip link set "cks-${role}-br" master "$BR" up
    in_ns "$ns" ip link set lo up
    in_ns "$ns" sysctl -qw "net.ipv6.conf.cks-${role}.accept_dad=0"
    # No checksum offload, on this end of every pair.
    in_ns "$ns" ethtool -K "cks-${role}" tx off rx off >/dev/null 2>&1
    in_ns "$ns" ip link set "cks-${role}" up
    in_ns "$ns" ip addr add "10.86.0.${n}/24" dev "cks-${role}"
    in_ns "$ns" ip -6 addr add "fd00:86::${n}/64" dev "cks-${role}"
done
# ...and on the bridge side of every pair, so no hop can hand over a partial checksum.
for role in lb cl be; do ethtool -K "cks-${role}-br" tx off rx off >/dev/null 2>&1; done
if in_ns "$NS_CLIENT" ethtool -k cks-cl 2>/dev/null | grep -q "tx-checksumming: off"; then
    pass "checksum offload is off: every packet carries a full checksum that its receiver verifies"
else
    fail "could not switch checksum offload off; this test would prove nothing"
    echo ""; echo "summary: pass=${PASS} fail=${FAIL}"; exit 1
fi
in_ns "$NS_LB" sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
in_ns "$NS_BE" ip route add 10.86.0.2/32 via 10.86.0.1 dev cks-be
in_ns "$NS_BE" ip -6 route add fd00:86::2/128 via fd00:86::1 dev cks-be
in_ns "$NS_LB" ip addr add 10.86.0.100/32 dev cks-lb
in_ns "$NS_LB" ip -6 addr add fd00:86::100/128 dev cks-lb nodad

TCP_SERVER='
import socket, sys, threading
addr, port = sys.argv[1], int(sys.argv[2])
s = socket.socket(socket.AF_INET6 if ":" in addr else socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, port)); s.listen(64)
def handle(c):
    try:
        c.sendall(b"BE\n")
    except OSError:
        pass
    finally:
        c.close()
while True:
    c, _ = s.accept()
    threading.Thread(target=handle, args=(c,), daemon=True).start()
'
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
for a in 10.86.0.11 fd00:86::11; do
    ip netns exec "$NS_BE" python3 -c "$TCP_SERVER" "$a" "$BE_PORT" >/dev/null 2>&1 & BE_PIDS+=($!)
    ip netns exec "$NS_BE" python3 -c "$UDP_SERVER" "$a" "$BE_PORT" >/dev/null 2>&1 & BE_PIDS+=($!)
done
sleep 0.6

{
    echo "interface: cks-lb"
    echo "apiListen: ${API}"
    echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
    echo "vips:"
    for proto in tcp udp; do
        echo "  - {address: 10.86.0.100, port: ${VIP_PORT}, protocol: ${proto}, mode: nat, backends: [{address: 10.86.0.11, port: ${BE_PORT}}]}"
        echo "  - {address: 'fd00:86::100', port: ${VIP_PORT}, protocol: ${proto}, mode: nat, backends: [{address: 'fd00:86::11', port: ${BE_PORT}}]}"
    done
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
sleep 3

# tcp_ok / udp_ok <vip> <count>: how many of <count> exchanges completed. The UDP one sends a
# payload big enough that a wrong checksum cannot hide behind a lucky value.
tcp_ok() {
    in_ns "$NS_CLIENT" python3 -c '
import socket, sys
vip, port, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
fam = socket.AF_INET6 if ":" in vip else socket.AF_INET
ok = 0
for _ in range(n):
    try:
        s = socket.socket(fam, socket.SOCK_STREAM); s.settimeout(1.5)
        s.connect((vip, port))
        if s.recv(8).strip() == b"BE": ok += 1
        s.close()
    except Exception:
        pass
print(ok)
' "$1" "$VIP_PORT" "$2"
}
udp_ok() {
    in_ns "$NS_CLIENT" python3 -c '
import os, socket, sys
vip, port, n = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
fam = socket.AF_INET6 if ":" in vip else socket.AF_INET
ok = 0
for _ in range(n):
    data = os.urandom(600)
    try:
        s = socket.socket(fam, socket.SOCK_DGRAM); s.settimeout(1.0)
        s.sendto(data, (vip, port))
        got, peer = s.recvfrom(2048)
        if got == data and peer[1] == port: ok += 1
        s.close()
    except Exception:
        pass
print(ok)
' "$1" "$VIP_PORT" "$2"
}

section "Full-NAT keeps checksums valid (VIP :${VIP_PORT} -> backend :${BE_PORT})"
for vip in 10.86.0.100 fd00:86::100; do
    fam=v4; [[ "$vip" == *:* ]] && fam=v6
    n=$(tcp_ok "$vip" 20)
    [ "$n" -ge 19 ] && pass "$fam TCP: ${n}/20 connections completed (SYN, data and reply checksums all verified)" \
        || fail "$fam TCP: only ${n}/20 completed: a packet's checksum was corrupted by the NAT"
    n=$(udp_ok "$vip" 20)
    [ "$n" -ge 19 ] && pass "$fam UDP: ${n}/20 datagrams echoed back intact (request and reply checksums verified)" \
        || fail "$fam UDP: only ${n}/20 came back: a packet's checksum was corrupted by the NAT"
done

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
