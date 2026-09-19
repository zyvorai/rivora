#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-portrange.sh — port-range VIPs (`portRange: "9100-9109"`) and the
# `ports: [..]` shorthand, IPv4 and IPv6, TCP and UDP, full-NAT and DSR.
#
# A range VIP owns every port in it and forwards to the SAME port on the
# backend (the port is not rewritten). This checks:
#   1. every port in a range reaches the backend on that port (the reply names
#      the port the backend saw), and ports just outside are not served
#   2. an exact-port VIP inside a range takes precedence for its one port
#   3. UDP ranges, and the un-NAT of their replies back to VIP:port
#   4. DSR ranges (the destination port must be left alone there too)
#   5. `ports: [a, b]` expands to one VIP per port
#   6. the API and the metrics label say "first-last"
#   7. a reload that shrinks or removes a range takes the dropped ports out of
#      the datapath (the trie blocks are really deleted)
#   8. with -persist-datapath a restart adopts the ranges left in the pinned
#      maps, keeps serving while rivorad is down, survives VIP reordering
#      (service IDs shift), and drops a range that left the config while down
#
# Isolated veth/netns/bridge topology (never a host interface). Must run as root.
# Usage: sudo ./scripts/selftest-portrange.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

if [ "$(id -u)" -ne 0 ]; then
    echo "selftest-portrange.sh must run as root (netns + BPF attach)." >&2
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
#   client (.2) --+-- bridge --- lb (.1)      10.82.0.0/24 and fd00:82::/64
#   backend (.11 .12 and the DSR VIP on lo) -/
# ---------------------------------------------------------------------------
SUFFIX="$$"
BR="rbrpr${SUFFIX}"
NS_LB="riv-plb-${SUFFIX}"
NS_CLIENT="riv-pclient-${SUFFIX}"
NS_BE="riv-pbe-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-selftest-pr.XXXXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
API="127.0.0.1:9874"
INNER_PID=""; BE_PIDS=()
CMDN=0

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
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
        ip link del "prg-${role}-br" 2>/dev/null || true
        ip link del "prg-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' prg-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale prg-* links did not go away" >&2; return 1
}

DSR_VIP="10.82.0.103"

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:1" "cl:$NS_CLIENT:2" "be:$NS_BE:11"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; n="${rest#*:}"
        ip netns add "$ns"
        ip link add "prg-${role}" type veth peer name "prg-${role}-br"
        ip link set "prg-${role}" netns "$ns"
        ip link set "prg-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" sysctl -qw "net.ipv6.conf.prg-${role}.accept_dad=0"
        ip netns exec "$ns" ip link set "prg-${role}" up
        ip netns exec "$ns" ip addr add "10.82.0.${n}/24" dev "prg-${role}"
        ip netns exec "$ns" ip -6 addr add "fd00:82::${n}/64" dev "prg-${role}"
    done
    ip netns exec "$NS_BE" ip addr add 10.82.0.12/24 dev prg-be
    ip netns exec "$NS_BE" ip -6 addr add fd00:82::12/64 dev prg-be
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.forwarding=1
    # Replies to the client must return through the LB to be un-NATed.
    ip netns exec "$NS_BE" ip route add 10.82.0.2/32 via 10.82.0.1 dev prg-be src 10.82.0.11 2>/dev/null \
        || ip netns exec "$NS_BE" ip route add 10.82.0.2/32 via 10.82.0.1 dev prg-be
    ip netns exec "$NS_BE" ip -6 route add fd00:82::2/128 via fd00:82::1 dev prg-be
    # NAT VIPs live on the LB so the client can resolve them.
    for n in 100 101 102; do
        ip netns exec "$NS_LB" ip addr add "10.82.0.${n}/32" dev prg-lb
    done
    for n in 100 101; do
        ip netns exec "$NS_LB" ip -6 addr add "fd00:82::${n}/128" dev prg-lb nodad
    done
    # DSR VIP: not on the LB. The client is pointed at the LB's MAC for it; the backend
    # owns the address on lo and must not answer ARP for it.
    ip netns exec "$NS_BE" ip addr add "${DSR_VIP}/32" dev lo
    ip netns exec "$NS_BE" sysctl -qw net.ipv4.conf.all.arp_ignore=1 net.ipv4.conf.all.arp_announce=2 \
        net.ipv4.conf.prg-be.arp_ignore=1 net.ipv4.conf.prg-be.arp_announce=2 \
        net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.prg-be.rp_filter=0
    LB_MAC=$(ip netns exec "$NS_LB" cat /sys/class/net/prg-lb/address)
    BE_MAC=$(ip netns exec "$NS_BE" cat /sys/class/net/prg-be/address)
    ip netns exec "$NS_CLIENT" ip neigh replace "$DSR_VIP" lladdr "$LB_MAC" dev prg-cl nud permanent
    ip netns exec "$NS_CLIENT" sysctl -qw net.ipv4.conf.all.rp_filter=0 net.ipv4.conf.prg-cl.rp_filter=0

    tcp_server() { ip netns exec "$NS_BE" python3 -c "$TCP_SERVER" "$@" >/dev/null 2>&1 & BE_PIDS+=($!); }
    udp_server() { ip netns exec "$NS_BE" python3 -c "$UDP_SERVER" "$@" >/dev/null 2>&1 & BE_PIDS+=($!); }
    # ident addr port...: answers "<ident>:<port the backend saw>".
    tcp_server R 10.82.0.11 9100 9101 9102 9103 9104 9105 9106 9107 9108 9109 9199
    tcp_server R fd00:82::11 9100 9101 9102 9103 9104 9105 9106 9107 9108 9109 9199
    tcp_server E 10.82.0.12 9105
    tcp_server E fd00:82::12 9105
    tcp_server M 10.82.0.11 9198
    tcp_server D "$DSR_VIP" 9300 9301 9302
    tcp_server R 10.82.0.11 9099 9110 # ports just outside the ranges: answering would mean a leak
    udp_server U 10.82.0.11 9200 9201 9202 9203 9204
    udp_server U fd00:82::11 9200 9201 9202 9203 9204
    sleep 0.5
}

TCP_SERVER='
import socket, sys, threading
ident, addr, ports = sys.argv[1], sys.argv[2], [int(p) for p in sys.argv[3:]]
fam = socket.AF_INET6 if ":" in addr else socket.AF_INET
def serve(port):
    s = socket.socket(fam, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind((addr, port)); s.listen(64)
    while True:
        c, _ = s.accept()
        try:
            c.sendall(("%s:%d\n" % (ident, port)).encode())
        except OSError:
            pass  # a health probe that hangs up early must not stop this port being served
        finally:
            c.close()
for p in ports:
    threading.Thread(target=serve, args=(p,), daemon=True).start()
threading.Event().wait()
'
UDP_SERVER='
import socket, sys, threading
ident, addr, ports = sys.argv[1], sys.argv[2], [int(p) for p in sys.argv[3:]]
fam = socket.AF_INET6 if ":" in addr else socket.AF_INET
def serve(port):
    s = socket.socket(fam, socket.SOCK_DGRAM)
    s.bind((addr, port))
    while True:
        _, peer = s.recvfrom(64)
        try:
            s.sendto(("%s:%d" % (ident, port)).encode(), peer)
        except OSError:
            pass
for p in ports:
    threading.Thread(target=serve, args=(p,), daemon=True).start()
threading.Event().wait()
'

# probe <tcp|udp> <vip> <port>: the reply, or "fail". A UDP reply must come from the VIP
# and port that was addressed: that is the un-NAT (or, under DSR, the backend's own source).
probe() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
proto, vip, port = sys.argv[1], sys.argv[2], int(sys.argv[3])
fam = socket.AF_INET6 if ":" in vip else socket.AF_INET
try:
    if proto == "tcp":
        s = socket.create_connection((vip, port), timeout=1.0)
        print(s.recv(32).decode().strip() or "fail")
    else:
        s = socket.socket(fam, socket.SOCK_DGRAM); s.settimeout(1.0)
        s.sendto(b"x", (vip, port))
        d, peer = s.recvfrom(64)
        print(d.decode() if (peer[0], peer[1]) == (vip, port) else "wrong-source:%s:%d" % (peer[0], peer[1]))
except Exception:
    print("fail")
' "$1" "$2" "$3"
}

expect() {   # expect <proto> <vip> <port> <want> <label>
    local got; got=$(probe "$1" "$2" "$3")
    [ "$got" = "$4" ] && pass "$5 ($2:$3 -> $got)" || fail "$5: $2:$3 answered '$got', want '$4'"
}
expect_not_served() {   # expect_not_served <proto> <vip> <port> <label>
    local got; got=$(probe "$1" "$2" "$3")
    [ "$got" = "fail" ] && pass "$4 ($2:$3 is not served)" || fail "$4: $2:$3 answered '$got', want it unserved"
}

# The LB side runs one long-lived shell so every rivorad run shares one bpffs.
write_inner() {
    cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
start() { "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" -persist-datapath >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 80); do curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    case "$(cat "${WORK}/cmd-$n")" in
        start)  echo "=== start $n" >>"$LOG"; start; wait_api ;;
        stop)   kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        reload) kill -HUP "$RPID"; sleep 1.5 ;;
        detach) "$RIVORAD" -detach >>"$LOG" 2>&1 ;;
        quit)   break ;;
    esac
    touch "${WORK}/ack-$n"; n=$((n + 1))
done
INNER
    chmod +x "${WORK}/inner.sh"
}

lb() {   # lb start|stop|reload|detach — runs it in the LB namespace and waits
    CMDN=$((CMDN + 1))
    echo "$1" > "${WORK}/cmd-${CMDN}"
    for _ in $(seq 1 400); do [ -f "${WORK}/ack-${CMDN}" ] && return 0; sleep 0.05; done
    return 1
}

# vips_yaml <order>: the VIP entries. Order decides which service IDs a fresh process hands
# out, so reordering across a restart is what shifts them. Each letter is one entry.
vip_entry() {
    case "$1" in
    R4) echo "  - {address: 10.82.0.100, portRange: '9100-9109', protocol: tcp, mode: nat, healthCheck: {port: 9199}, backends: [{address: 10.82.0.11}]}" ;;
    R4s) echo "  - {address: 10.82.0.100, portRange: '9100-9104', protocol: tcp, mode: nat, healthCheck: {port: 9199}, backends: [{address: 10.82.0.11}]}" ;;
    E4) echo "  - {address: 10.82.0.100, port: 9105, protocol: tcp, mode: nat, backends: [{address: 10.82.0.12, port: 9105}]}" ;;
    U4) echo "  - {address: 10.82.0.101, portRange: '9200-9203', protocol: udp, mode: nat, healthCheck: {port: 9199}, backends: [{address: 10.82.0.11}]}" ;;
    M4) echo "  - {address: 10.82.0.102, ports: [8081, 8082], protocol: tcp, mode: nat, backends: [{address: 10.82.0.11, port: 9198}]}" ;;
    R6) echo "  - {address: 'fd00:82::100', portRange: '9100-9109', protocol: tcp, mode: nat, healthCheck: {port: 9199}, backends: [{address: 'fd00:82::11'}]}" ;;
    E6) echo "  - {address: 'fd00:82::100', port: 9105, protocol: tcp, mode: nat, backends: [{address: 'fd00:82::12', port: 9105}]}" ;;
    U6) echo "  - {address: 'fd00:82::101', portRange: '9200-9203', protocol: udp, mode: nat, healthCheck: {port: 9199}, backends: [{address: 'fd00:82::11'}]}" ;;
    D4) echo "  - {address: ${DSR_VIP}, portRange: '9300-9302', protocol: tcp, mode: dsr, healthCheck: {port: 9199}, backends: [{address: 10.82.0.11, mac: '${BE_MAC}'}]}" ;;
    esac
}
write_config() {
    {
        echo "interface: prg-lb"
        echo "apiListen: ${API}"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        for v in "$@"; do vip_entry "$v"; done
    } > "$CONFIG"
}

setup_topology
write_inner
ip netns exec "$NS_LB" env RIVORAD="$RIVORAD" CONFIG="$CONFIG" BPF_DIR="$BPF_DIR" WORK="$WORK" LOG="$LOG" API="$API" \
    bash "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!

ALL="R4 E4 U4 M4 R6 E6 U6 D4"
write_config $ALL
lb start
if ! ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips" >/dev/null 2>&1; then
    fail "rivorad did not come up"; tail -n 30 "$LOG"; echo ""; echo "summary: pass=${PASS} fail=${FAIL}"; exit 1
fi
pass "rivorad is up with port-range VIPs"
sleep 3 # health checks: every backend must have been probed healthy

# ---------------------------------------------------------------------------
section "1-2. TCP ranges: every port reaches the backend on that port; exact port wins"
# ---------------------------------------------------------------------------
for fam in "4 10.82.0.100" "6 fd00:82::100"; do
    set -- $fam; v="$2"
    for p in 9100 9101 9102 9103 9104 9106 9107 9108 9109; do
        expect tcp "$v" "$p" "R:$p" "v$1 range port reaches the backend on the same port"
    done
    expect tcp "$v" 9105 "E:9105" "v$1 exact-port VIP inside the range takes precedence"
    expect_not_served tcp "$v" 9099 "v$1 port just below the range"
    expect_not_served tcp "$v" 9110 "v$1 port just above the range"
done

section "3. UDP ranges (replies must come back from the VIP and port addressed)"
for v in 10.82.0.101 fd00:82::101; do
    for p in 9200 9201 9202 9203; do expect udp "$v" "$p" "U:$p" "udp range port"; done
    expect_not_served udp "$v" 9204 "udp port just above the range"
done

section "4. DSR range (the destination port must be left alone)"
for p in 9300 9301 9302; do expect tcp "$DSR_VIP" "$p" "D:$p" "dsr range port"; done
expect_not_served tcp "$DSR_VIP" 9303 "dsr port just above the range"

section "5. ports: [a, b] shorthand"
for p in 8081 8082; do expect tcp 10.82.0.102 "$p" "M:9198" "ports-list VIP :$p -> backend :9198"; done

# ---------------------------------------------------------------------------
section "6. API and metrics say first-last"
# ---------------------------------------------------------------------------
json=$(ip netns exec "$NS_LB" curl -sf "http://${API}/api/v1/vips")
if python3 -c "
import json, sys
v = [x for x in json.load(sys.stdin) if x['vipAddress'] == '10.82.0.100' and x.get('vipPortEnd')]
sys.exit(0 if len(v) == 1 and v[0]['vipPort'] == 9100 and v[0]['vipPortEnd'] == 9109 else 1)" <<<"$json"; then
    pass "the API reports the range as vipPort 9100 / vipPortEnd 9109"
else
    fail "the API does not describe the range VIP: $json"
fi
metric_ok=0
for _ in $(seq 1 10); do
    if ip netns exec "$NS_LB" curl -sf "http://127.0.0.1:9871/metrics" 2>/dev/null | grep -q 'vip="10.82.0.100:9100-9109"'; then metric_ok=1; break; fi
    sleep 0.5
done
if [ "$metric_ok" = 1 ]; then
    pass "metrics label the range VIP 10.82.0.100:9100-9109"
else
    fail "no metric labelled 10.82.0.100:9100-9109 (metrics said: $(ip netns exec "$NS_LB" curl -s "http://127.0.0.1:9871/metrics" 2>&1 | grep -m3 'rivora_vip_packets' | tr '\n' ' '))"
fi

# ---------------------------------------------------------------------------
section "7. Reload: shrinking then removing a range takes the ports out of the datapath"
# ---------------------------------------------------------------------------
write_config R4s E4 U4 M4 R6 E6 U6 D4
lb reload
expect tcp 10.82.0.100 9102 "R:9102" "v4 port kept by the shrunk range"
expect_not_served tcp 10.82.0.100 9107 "v4 port dropped by the shrink"
expect tcp 10.82.0.100 9105 "E:9105" "v4 exact port still served after the range shrank below it"
expect tcp fd00:82::100 9107 "R:9107" "v6 range untouched by the v4 change"
write_config E4 U4 M4 R6 E6 U6 D4
lb reload
expect_not_served tcp 10.82.0.100 9102 "v4 range removed"
expect tcp 10.82.0.100 9105 "E:9105" "v4 exact port survives its range's removal"
expect tcp fd00:82::100 9107 "R:9107" "v6 range still served"
write_config $ALL
lb reload
expect tcp 10.82.0.100 9107 "R:9107" "v4 range served again once re-added"

# ---------------------------------------------------------------------------
section "8. Restart with -persist-datapath: adoption of ranges"
# ---------------------------------------------------------------------------
lb stop
expect tcp 10.82.0.100 9103 "R:9103" "the datapath keeps serving the range while rivorad is down"
expect tcp fd00:82::100 9108 "R:9108" "...and the v6 range"
# Reorder the VIPs: a fresh process would hand out shifted service IDs, so a range that
# was not adopted would now point at another VIP's service.
write_config D4 U6 R6 M4 U4 E6 E4 R4
lb start
if grep -q "recovered existing datapath state" "$LOG"; then
    pass "start-up recovered the pinned state ($(grep -o 'adopted=[0-9]*' "$LOG" | tail -1))"
else
    fail "start-up did not report recovering the pinned state"
fi
sleep 3
for p in 9100 9104 9109; do expect tcp 10.82.0.100 "$p" "R:$p" "v4 range after a restart that reordered the VIPs"; done
expect tcp 10.82.0.100 9105 "E:9105" "v4 exact port after the reorder"
expect tcp fd00:82::100 9103 "R:9103" "v6 range after the reorder"
expect udp 10.82.0.101 9202 "U:9202" "udp range after the reorder"
expect tcp "$DSR_VIP" 9301 "D:9301" "dsr range after the reorder"
expect tcp 10.82.0.102 8082 "M:9198" "ports-list VIP after the reorder"

# A range that leaves the config while rivorad is down must not survive the restart.
lb stop
write_config R4 E4 U4 M4 E6 U6 D4
lb start
sleep 3
expect_not_served tcp fd00:82::100 9103 "v6 range removed from the config while down"
expect tcp fd00:82::100 9105 "E:9105" "v6 exact port kept"
expect tcp 10.82.0.100 9103 "R:9103" "v4 range kept"
if grep -q "dropped pinned VIP entries" "$LOG"; then
    fail "adoption dropped entries as inconsistent: $(grep 'dropped pinned' "$LOG" | tail -1)"
else
    pass "adoption found every pinned range consistent"
fi

lb stop
lb detach
echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
