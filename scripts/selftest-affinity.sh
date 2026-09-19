#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-affinity.sh — sessionAffinity: clientIP, through a real kernel.
#
# By default the Maglev slot is chosen from the whole 5-tuple, so one client's
# connections spread across backends. sessionAffinity: clientIP (Kubernetes'
# Service.spec.sessionAffinity: ClientIP) hashes the SOURCE ADDRESS only, so every
# connection from a client lands on the same backend. This drives many connections
# from many distinct client addresses through a full-NAT VIP with two backends
# (each answering with its own name) and checks:
#
#   1. control         no affinity: ONE client's connections reach BOTH backends
#                      (so "sticky" below is a property of the feature, not luck).
#   2. reload          affinity turned on with SIGHUP (no restart) is accepted.
#   3. clientIP        every one of 24 clients sees exactly ONE backend across 12
#                      connections each, and the clients as a whole use both backends.
#   4. restart         the client -> backend mapping is unchanged across a rivorad
#                      restart (it is a pure function of the source address).
#   5. failover        when a backend dies its clients move to the survivor, and the
#                      clients already on the survivor do not move.
#
# rivorad runs in one `ip netns exec` (private mount namespace, private bpffs), so
# this never touches the host's /sys/fs/bpf/rivora-lb. Must run as root.
# Usage: sudo ./scripts/selftest-affinity.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-affinity.sh must run as root (netns + BPF attach)." >&2; exit 1; }

# Prefer this checkout's build over anything installed system-wide, which may
# predate the feature under test (this one changes the BPF program too).
RIVORAD="${RIVORAD:-${ROOT}/bin/rivorad}"
[ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
if [ -z "${BPF_DIR:-}" ]; then
    BPF_DIR="${ROOT}/bpf"
    [ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"
fi
for f in "$RIVORAD" "${BPF_DIR}/xdp_ingress.o" "${BPF_DIR}/tc_nat.o"; do
    [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done

SUFFIX="$$"
BR="braff${SUFFIX}"
NS_LB="aff-lb-${SUFFIX}"; NS_CLIENT="aff-cl-${SUFFIX}"; NS_BE1="aff-b1-${SUFFIX}"; NS_BE2="aff-b2-${SUFFIX}"
PORT="8080"; VIP="10.85.0.100"
WORK="$(mktemp -d /tmp/rivora-affinity-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"; LOG="${WORK}/rivorad.log"
BE_PIDS=(); INNER_PID=""; CMDN=0
CLIENTS=24; CONNS=12

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE1" "$NS_BE2"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# Fixed device names + asynchronous veth teardown after `ip netns del`: clear any
# leftovers from a previous run and wait until they are really gone.
clear_stale_links() {
    local n
    for role in lb cl b1 b2; do
        ip link del "aff-${role}-br" 2>/dev/null || true
        ip link del "aff-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' aff-(lb|cl|b1|b2)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale aff-* links did not go away" >&2; return 1
}

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.85.0.1" "cl:$NS_CLIENT:10.85.0.2" "b1:$NS_BE1:10.85.0.11" "b2:$NS_BE2:10.85.0.12"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "aff-${role}" type veth peer name "aff-${role}-br"
        ip link set "aff-${role}" netns "$ns"
        ip link set "aff-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "aff-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "aff-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev aff-lb

    # Many client source addresses, all in 10.85.1.0/24, which is NOT on the backends'
    # segment: their replies must return through the LB (so its TCX program un-NATs
    # them) rather than straight back over the shared bridge.
    for i in $(seq 1 "$CLIENTS"); do ip netns exec "$NS_CLIENT" ip addr add "10.85.1.${i}/32" dev aff-cl; done
    ip netns exec "$NS_LB" ip route add 10.85.1.0/24 via 10.85.0.2 dev aff-lb
    ip netns exec "$NS_BE1" ip route add 10.85.1.0/24 via 10.85.0.1 dev aff-b1
    ip netns exec "$NS_BE2" ip route add 10.85.1.0/24 via 10.85.0.1 dev aff-b2

    for spec in "$NS_BE1 10.85.0.11 BACKEND-1" "$NS_BE2 10.85.0.12 BACKEND-2"; do
        set -- $spec
        ip netns exec "$1" python3 -c '
import socket, sys
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((sys.argv[1], 8080)); s.listen(128)
while True:
    c, _ = s.accept(); c.sendall((sys.argv[2] + "\n").encode()); c.close()
' "$2" "$3" >/dev/null 2>&1 &
        BE_PIDS+=($!)
    done
    sleep 0.4
}

write_config() {   # write_config [affinity]
    {
        echo "interface: aff-lb"
        echo "apiListen: 127.0.0.1:9870"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        echo "  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat${1:+, sessionAffinity: $1}, backends: [{address: 10.85.0.11, port: ${PORT}}, {address: 10.85.0.12, port: ${PORT}}]}"
    } > "$CONFIG"
}

cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# env: RIVORAD CONFIG BPF_DIR WORK LOG
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
start() { echo "=== start $n" >>"$LOG"; "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 80); do curl -sf http://127.0.0.1:9870/api/v1/vips >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    case "$(cat "${WORK}/cmd-$n")" in
        start) start; wait_api ;;
        hup)   kill -HUP "$RPID"; sleep 1.5 ;;
        stop)  kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        quit)  break ;;
    esac
    touch "${WORK}/ack-$n"; n=$((n + 1))
done
INNER
chmod +x "${WORK}/inner.sh"

lb() {
    CMDN=$((CMDN + 1)); echo "$1" > "${WORK}/cmd-${CMDN}"
    for _ in $(seq 1 400); do [ -f "${WORK}/ack-${CMDN}" ] && return 0; sleep 0.05; done
    return 1
}

# fan_out <first-client> <last-client> <conns-per-client> -> "<client> <identity>" per connection,
# each connection from its own ephemeral source port but a fixed source ADDRESS.
fan_out() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
first, last, n = int(sys.argv[1]), int(sys.argv[2]), int(sys.argv[3])
for i in range(first, last + 1):
    for _ in range(n):
        try:
            s = socket.socket(); s.settimeout(1.5)
            s.bind(("10.85.1.%d" % i, 0)); s.connect(("10.85.0.100", 8080))
            print(i, s.recv(32).decode().strip() or "fail"); s.close()
        except Exception:
            print(i, "fail")
' "$1" "$2" "$3"
}

# distinct backends per client, from fan_out output
per_client() { awk '$2 != "fail" {seen[$1 " " $2]=1} END {for (k in seen) {split(k,a," "); c[a[1]]++} for (i in c) print i, c[i]}' | sort -n; }
# the single backend each client used, "<client> <backend>" (only meaningful when sticky)
mapping() { awk '$2 != "fail" {m[$1]=$2} END {for (i in m) print i, m[i]}' | sort -n; }

setup_topology
write_config
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS_LB" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!
lb start || { fail "rivorad did not start"; tail -20 "$LOG"; exit 1; }
sleep 1.2   # let the first health pass finish so both backends are in rotation

section "1. control: no affinity, one client's connections spread across backends"
C=$(fan_out 1 1 40)
DIST=$(echo "$C" | awk '$2 != "fail" {print $2}' | sort -u | wc -l | tr -d ' ')
FAILS=$(echo "$C" | grep -c ' fail$')
[ "$FAILS" -eq 0 ] && pass "all 40 connections succeeded" || fail "${FAILS} of 40 connections failed"
[ "$DIST" = 2 ] && pass "one client reached BOTH backends (5-tuple hashing spreads its connections)" || fail "one client reached ${DIST} backend(s); the control needs 2 or 'sticky' proves nothing"

section "2. reload: turn clientIP on with SIGHUP, no restart"
PID_BEFORE=$(pgrep -f "rivorad -config ${CONFIG}" | head -1)
write_config clientIP
lb hup
PID_AFTER=$(pgrep -f "rivorad -config ${CONFIG}" | head -1)
[ "$PID_BEFORE" = "$PID_AFTER" ] && pass "rivorad was not restarted" || fail "rivorad restarted (pid ${PID_BEFORE} -> ${PID_AFTER})"
grep -q "config reloaded" "$LOG" && pass "the reload was logged as applied" || fail "no reload log line"

section "3. clientIP: every client sticks to one backend"
ALL=$(fan_out 1 "$CLIENTS" "$CONNS")
FAILS=$(echo "$ALL" | grep -c ' fail$')
[ "$FAILS" -eq 0 ] && pass "all $((CLIENTS * CONNS)) connections succeeded" || fail "${FAILS} connections failed"
MULTI=$(echo "$ALL" | per_client | awk '$2 > 1' | wc -l | tr -d ' ')
[ "$MULTI" -eq 0 ] && pass "each of ${CLIENTS} clients reached exactly one backend across ${CONNS} connections" || fail "${MULTI} client(s) were spread over more than one backend"
USED=$(echo "$ALL" | mapping | awk '{print $2}' | sort -u | wc -l | tr -d ' ')
[ "$USED" = 2 ] && pass "and the clients as a whole used both backends ($(echo "$ALL" | mapping | awk '{print $2}' | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}'))" || fail "clients used ${USED} backend(s): affinity collapsed everything onto one"
echo "$ALL" | mapping > "${WORK}/map-before"

section "4. restart: the mapping is unchanged"
lb stop; lb start; sleep 1.2
AFTER=$(fan_out 1 "$CLIENTS" 3 | mapping)
if [ "$AFTER" = "$(cat "${WORK}/map-before")" ]; then
    pass "all ${CLIENTS} clients map to the same backend after a restart"
else
    fail "the client->backend mapping changed across a restart: $(diff <(cat "${WORK}/map-before") <(echo "$AFTER") | head -4 | tr '\n' ' ')"
fi

section "5. failover: a dead backend's clients move; the survivor's clients stay"
DEAD_PID="${BE_PIDS[1]}"   # BACKEND-2
kill "$DEAD_PID" 2>/dev/null
for _ in $(seq 1 24); do sleep 0.5; [ "$(ip netns exec "$NS_LB" curl -s --max-time 3 http://127.0.0.1:9871/metrics | grep -c 'rivora_backend_healthy.* 0$')" -ge 1 ] && break; done
sleep 1
FO=$(fan_out 1 "$CLIENTS" 3)
FAILS=$(echo "$FO" | grep -c ' fail$')
[ "$FAILS" -eq 0 ] && pass "no connection failed after the backend died (sticky flows failed over, not blackholed)" || fail "${FAILS} connections failed after a backend died"
ONLY=$(echo "$FO" | mapping | awk '{print $2}' | sort -u | tr '\n' ' ')
[ "$ONLY" = "BACKEND-1 " ] && pass "every client is now on the surviving backend" || fail "clients are on: ${ONLY}"
MOVED=$(join <(sort -k1,1 "${WORK}/map-before") <(echo "$FO" | mapping | sort -k1,1) | awk '$2 == "BACKEND-1" && $3 != "BACKEND-1"' | wc -l | tr -d ' ')
[ "$MOVED" -eq 0 ] && pass "no client that was already on the survivor moved" || fail "${MOVED} client(s) on the survivor were moved"

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
