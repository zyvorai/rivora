#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-adopt.sh — does a restart reconcile against what the pinned maps
# already hold, or program the new config on top of the old one's leftovers?
#
# rivorad's BPF maps are pinned and outlive it, but its ID allocators start
# empty. Without adoption a restart therefore:
#   - leaves a VIP that was removed from the config programmed, and
#   - reuses that VIP's service ID for the new config's first VIP, so the stale
#     VIP's traffic is forwarded to some *other* VIP's backends.
# and any VIP inserted ahead of existing ones shifts their IDs mid-apply.
#
# Three full-NAT VIPs, each backed by a server that answers with its own name
# (A, B, C), so a hijack is visible as a wrong answer, not just a failure:
#   1. [A B]     baseline: each VIP reaches its own backend.
#   2. [B]       restart with A removed: VIP A must stop answering, B must still
#                answer "B".
#   3. [C B A]   restart with C inserted first (every existing ID shifts): all
#                three answer correctly, and continuous traffic to B across the
#                restart never receives another VIP's answer.
#
# Set RIVORAD to test a specific binary (e.g. an older build, to watch scenario 2
# fail without adoption). Runs in private network + mount namespaces, so it never
# touches the host's /sys/fs/bpf/rivora-lb. Must run as root.
# Usage: sudo ./scripts/selftest-adopt.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-adopt.sh must run as root (netns + BPF attach)." >&2; exit 1; }

RIVORAD="${RIVORAD:-$(command -v rivorad || echo "${ROOT}/bin/rivorad")}"
if [ -z "${BPF_DIR:-}" ]; then
    BPF_DIR="/usr/local/share/rivora/bpf"
    [ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="${ROOT}/bpf"
fi
for f in "$RIVORAD" "${BPF_DIR}/xdp_ingress.o" "${BPF_DIR}/tc_nat.o"; do
    [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done

SUFFIX="$$"
BR="bradp${SUFFIX}"
NS_LB="adp-lb-${SUFFIX}"; NS_CLIENT="adp-cl-${SUFFIX}"; NS_BE="adp-be-${SUFFIX}"
PORT="8080"
declare -A VIPADDR=([A]=10.79.0.101 [B]=10.79.0.102 [C]=10.79.0.103)
declare -A BEPORT=([A]=9001 [B]=9002 [C]=9003)
WORK="$(mktemp -d /tmp/rivora-adopt-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"
LOG="${WORK}/rivorad.log"
INNER_PID=""; BE_PIDS=()
CMDN=0

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    for p in "${BE_PIDS[@]:-}"; do [ -n "$p" ] && ip netns exec "$NS_BE" kill "$p" 2>/dev/null; done
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# Device names are fixed (not suffixed), and deleting a network namespace tears
# its veths down asynchronously, so a run started right after another can find
# the previous run's halves still present and fail with "File exists". Remove any
# and wait until they are really gone before creating anything.
clear_stale_links() {
    local n
    for role in lb cl be; do
        ip link del "adp-${role}-br" 2>/dev/null || true
        ip link del "adp-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' adp-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale adp-* links did not go away" >&2; return 1
}

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.79.0.1" "cl:$NS_CLIENT:10.79.0.2" "be:$NS_BE:10.79.0.11"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "adp-${role}" type veth peer name "adp-${role}-br"
        ip link set "adp-${role}" netns "$ns"
        ip link set "adp-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "adp-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "adp-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    for v in A B C; do ip netns exec "$NS_LB" ip addr add "${VIPADDR[$v]}/32" dev adp-lb; done
    ip netns exec "$NS_BE" ip route add 10.79.0.2/32 via 10.79.0.1 dev adp-be

    for v in A B C; do
        ip netns exec "$NS_BE" python3 -c '
import socket, sys
port, ident = int(sys.argv[1]), sys.argv[2]
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("10.79.0.11", port)); s.listen(64)
while True:
    c, _ = s.accept(); c.sendall((ident + "\n").encode()); c.close()
' "${BEPORT[$v]}" "$v" >/dev/null 2>&1 &
        BE_PIDS+=($!)
    done
    sleep 0.3
}

# Writes the config listing the given VIP names, in that order — order is what
# decides which service ID each VIP gets in a process with no memory of the last.
write_config() {
    {
        echo "interface: adp-lb"
        echo "apiListen: 127.0.0.1:9870"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        for v in "$@"; do
            echo "  - {address: ${VIPADDR[$v]}, port: ${PORT}, protocol: tcp, mode: nat, backends: [{address: 10.79.0.11, port: ${BEPORT[$v]}}]}"
        done
    } > "$CONFIG"
}

# LB side: driven by numbered command files so every rivorad run shares one bpffs.
write_inner() {
    cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# env: RIVORAD CONFIG BPF_DIR WORK LOG
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
start() { "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" -persist-datapath >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 80); do curl -sf http://127.0.0.1:9870/api/v1/vips >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    case "$(cat "${WORK}/cmd-$n")" in
        start) echo "=== start $n" >>"$LOG"; start; wait_api ;;
        stop)  kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        detach) "$RIVORAD" -detach >>"$LOG" 2>&1 ;;
        quit)  break ;;
    esac
    touch "${WORK}/ack-$n"; n=$((n + 1))
done
INNER
    chmod +x "${WORK}/inner.sh"
}

lb() {   # lb start|stop|detach — runs it in the LB namespace and waits for it
    CMDN=$((CMDN + 1))
    echo "$1" > "${WORK}/cmd-${CMDN}"
    for _ in $(seq 1 400); do [ -f "${WORK}/ack-${CMDN}" ] && return 0; sleep 0.05; done
    return 1
}

probe() {   # probe <vip-name>  -> the backend's identity, or "fail"
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
try:
    s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1.0)
    d = s.recv(32).decode().strip(); s.close(); print(d or "fail")
except Exception:
    print("fail")
' "${VIPADDR[$1]}" "$PORT"
}

expect_answer() {   # expect_answer <vip-name> <want> <label>
    local got; got=$(probe "$1")
    [ "$got" = "$2" ] && pass "$3 (VIP $1 -> $got)" || fail "$3: VIP $1 answered '$got', want '$2'"
}

start_prober() {   # start_prober <vip-name> <seconds> <outfile>
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys, time
vip, port, dur, out = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), sys.argv[4]
end = time.time() + dur
with open(out, "w") as f:
    while time.time() < end:
        try:
            s = socket.create_connection((vip, port), timeout=0.4)
            d = s.recv(32).decode().strip() or "fail"; s.close()
        except Exception:
            d = "fail"
        f.write(d + "\n"); f.flush()
        time.sleep(0.02)
' "${VIPADDR[$1]}" "$PORT" "$2" "$3" >/dev/null 2>&1 &
    PROBER_PID=$!
}

setup_topology
write_inner
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS_LB" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!

section "1. baseline [A B]"
write_config A B
lb start || fail "rivorad did not start"
expect_answer A A "each VIP reaches its own backend"
expect_answer B B "each VIP reaches its own backend"

section "2. restart with A removed from the config"
lb stop
write_config B
lb start || fail "rivorad did not restart"
expect_answer B B "the kept VIP still reaches its own backend"
# Without adoption VIP A stays programmed, and its stale service ID now belongs
# to B, so it answers "B" — another VIP's backend. With adoption it is removed.
got=$(probe A)
if [ "$got" = "fail" ]; then
    pass "removed VIP A no longer forwards"
else
    fail "removed VIP A still forwards (answered '$got')"
fi
if grep -q "removed VIPs left programmed" "$LOG"; then
    pass "start-up reported removing the leftover VIP"
else
    fail "start-up did not report removing the leftover VIP"
fi

section "3. restart with C inserted first: every existing ID shifts [C B A]"
start_prober B 7 "${WORK}/trace-B"
sleep 1.5
lb stop
write_config C B A
lb start || fail "rivorad did not restart"
wait "$PROBER_PID" 2>/dev/null
expect_answer A A "re-added VIP"
expect_answer B B "existing VIP under a shifted ID"
expect_answer C C "newly inserted VIP"
total=$(wc -l < "${WORK}/trace-B"); wrong=$(grep -cvE '^(B|fail)$' "${WORK}/trace-B"); fails=$(grep -c '^fail$' "${WORK}/trace-B")
echo "    ${total} probes of VIP B across the restart: ${wrong} answered by another VIP's backend, ${fails} failed"
[ "$wrong" -eq 0 ] && pass "no probe of VIP B was ever answered by another VIP's backend" \
                   || fail "${wrong} probes of VIP B were answered by another VIP's backend (a hijack)"
[ "$fails" -eq 0 ] && pass "no probe of VIP B failed across the restart" || fail "${fails} probes of VIP B failed across the restart"

lb detach >/dev/null 2>&1

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
