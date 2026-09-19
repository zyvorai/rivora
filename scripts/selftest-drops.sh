#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-drops.sh — per-VIP drop-reason counters, end to end through the kernel.
#
# The XDP program counts, per service, why a packet for a matched VIP did not
# reach a backend, and rivorad exports it as rivora_vip_dropped_packets_total
# {reason} / rivora_vip_unserved_packets_total. This drives real traffic through
# a full-NAT VIP with a tight rate limit and checks each reason fires for the
# right cause and only that cause:
#
#   1. rate_limited        a fast burst of SYNs against a healthy backend.
#   2. no_healthy_backend  the backend is killed and marked down, then SYNs
#                          (spaced past the rate limit) reach backend selection
#                          and have nowhere to go.
#   3. unserved            the backend's map entry is deleted behind rivorad's
#                          back, so the VIP matches but can't be served and the
#                          packet passes to the kernel stack instead.
#   4. invariant           the per-reason drops sum to exactly the node-wide
#                          rivora_dropped_packets_total (both are bumped at every
#                          drop), and "unserved" is not part of that sum.
#   5. reset               remove a VIP and add it back via SIGHUP: its counters
#                          start from zero, not from the previous VIP's counts
#                          (drop_stats_map is pinned and outlives VIPs).
#
# rivorad runs in one `ip netns exec` (a private mount namespace with its own
# bpffs), so it never touches the host's /sys/fs/bpf/rivora-lb. Must run as root.
# Usage: sudo ./scripts/selftest-drops.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-drops.sh must run as root (netns + BPF attach)." >&2; exit 1; }

# Prefer this checkout's build over anything installed system-wide, which may predate
# the feature under test (see selftest-ratelimit.sh).
RIVORAD="${RIVORAD:-${ROOT}/bin/rivorad}"
[ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
if [ -z "${BPF_DIR:-}" ]; then
    BPF_DIR="${ROOT}/bpf"
    [ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"
fi
for f in "$RIVORAD" "${BPF_DIR}/xdp_ingress.o" "${BPF_DIR}/tc_nat.o"; do
    [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done
command -v bpftool >/dev/null || { echo "bpftool is required (to delete a backend map entry)" >&2; exit 1; }

SUFFIX="$$"
BR="brdrp${SUFFIX}"
NS_LB="drp-lb-${SUFFIX}"; NS_CLIENT="drp-cl-${SUFFIX}"; NS_BE="drp-be-${SUFFIX}"
PORT="8080"
VIP1="10.81.0.100"; VIP2="10.81.0.101"; BE="10.81.0.11"
WORK="$(mktemp -d /tmp/rivora-drops-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"; LOG="${WORK}/rivorad.log"
INNER_PID=""; BE_PID=""; CMDN=0

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    stop_backend
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# Fixed device names + asynchronous veth teardown after `ip netns del`: clear any
# leftovers from a previous run and wait until they are really gone.
clear_stale_links() {
    local n
    for role in lb cl be; do
        ip link del "drp-${role}-br" 2>/dev/null || true
        ip link del "drp-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' drp-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale drp-* links did not go away" >&2; return 1
}

start_backend() {
    ip netns exec "$NS_BE" python3 -c '
import socket
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("10.81.0.11", 8080)); s.listen(64)
while True:
    c, _ = s.accept(); c.sendall(b"BACKEND\n"); c.close()
' >/dev/null 2>&1 &
    BE_PID=$!
}
stop_backend() { [ -n "$BE_PID" ] && ip netns exec "$NS_BE" kill "$BE_PID" 2>/dev/null; BE_PID=""; }

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.81.0.1" "cl:$NS_CLIENT:10.81.0.2" "be:$NS_BE:${BE}"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "drp-${role}" type veth peer name "drp-${role}-br"
        ip link set "drp-${role}" netns "$ns"
        ip link set "drp-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "drp-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "drp-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    for v in "$VIP1" "$VIP2"; do ip netns exec "$NS_LB" ip addr add "${v}/32" dev drp-lb; done
    ip netns exec "$NS_BE" ip route add 10.81.0.2/32 via 10.81.0.1 dev drp-be
    start_backend
    sleep 0.3
}

# Tight rate limit: 5/s and burst 5, divided per CPU and floored at 1, so on any
# real host a fast burst is mostly dropped while one SYN per ~second gets through.
write_config() {   # write_config <vip>...
    {
        echo "interface: drp-lb"
        echo "apiListen: 127.0.0.1:9870"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "rateLimit: {enabled: true, perSourcePacketsPerSecond: 5, burst: 5}"
        echo "vips:"
        for v in "$@"; do
            echo "  - {address: ${v}, port: ${PORT}, protocol: tcp, mode: nat, backends: [{address: ${BE}, port: ${PORT}}]}"
        done
    } > "$CONFIG"
}

write_inner() {
    cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# env: RIVORAD CONFIG BPF_DIR WORK LOG
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
start() { "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 80); do curl -sf http://127.0.0.1:9870/api/v1/vips >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    c="$(cat "${WORK}/cmd-$n")"
    case "$c" in
        start) start; wait_api ;;
        hup)   kill -HUP "$RPID"; sleep 1 ;;
        quit)  break ;;
        exec:*) bash -c "${c#exec:}" >"${WORK}/exec-$n.out" 2>&1 ;;   # runs in this mount ns (sees the private bpffs)
    esac
    touch "${WORK}/ack-$n"; n=$((n + 1))
done
INNER
    chmod +x "${WORK}/inner.sh"
}

lb() {
    CMDN=$((CMDN + 1))
    echo "$1" > "${WORK}/cmd-${CMDN}"
    for _ in $(seq 1 400); do [ -f "${WORK}/ack-${CMDN}" ] && return 0; sleep 0.05; done
    return 1
}

# One SYN from the client with a short timeout: prints ok / fail.
probe() {   # probe <vip> [timeout]
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
try:
    s = socket.create_connection((sys.argv[1], 8080), timeout=float(sys.argv[2])); s.recv(16); s.close(); print("ok")
except Exception:
    print("fail")
' "$1" "${2:-0.25}"
}

metrics() { ip netns exec "$NS_LB" curl -s http://127.0.0.1:9871/metrics; }
# vipmetric <metric> <vip> [reason]  -> value (0 if the series is absent)
vipmetric() {
    local v
    v=$(metrics | grep "^$1{" | grep "vip=\"$2:${PORT}\"" | { [ -n "${3:-}" ] && grep "reason=\"$3\"" || cat; } | awk '{print $NF}' | head -1)
    echo "${v:-0}"
}
nodedrops() { metrics | grep '^rivora_dropped_packets_total ' | awk '{print $NF}'; }
check_ge() { [ "$2" -ge "$3" ] && pass "$1 ($2)" || fail "$1: got $2, want >= $3"; }
check_eq() { [ "$2" = "$3" ] && pass "$1 ($2)" || fail "$1: got '$2', want '$3'"; }

setup_topology
write_config "$VIP1" "$VIP2"
write_inner
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS_LB" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!
lb start || { fail "rivorad did not start"; tail -20 "$LOG"; exit 1; }

D_RL() { vipmetric rivora_vip_dropped_packets_total "$1" rate_limited; }
D_NB() { vipmetric rivora_vip_dropped_packets_total "$1" no_healthy_backend; }
D_UN() { vipmetric rivora_vip_unserved_packets_total "$1"; }

section "0. baseline: counters exist and start at zero"
sleep 0.5
check_eq "rate_limited starts at 0" "$(D_RL $VIP1)" 0
check_eq "no_healthy_backend starts at 0" "$(D_NB $VIP1)" 0
check_eq "unserved starts at 0" "$(D_UN $VIP1)" 0

section "1. rate_limited: a fast burst of SYNs"
for _ in $(seq 1 40); do probe "$VIP1" 0.15 >/dev/null; done
RL1=$(D_RL "$VIP1")
check_ge "rate_limited counted the dropped SYNs" "$RL1" 5
check_eq "no other reason fired (no_healthy_backend)" "$(D_NB $VIP1)" 0
check_eq "no other reason fired (unserved)" "$(D_UN $VIP1)" 0
check_eq "the other VIP was untouched" "$(D_RL $VIP2)" 0

section "2. no_healthy_backend: backend down, SYNs spaced past the limiter"
stop_backend
for _ in $(seq 1 12); do
    sleep 0.5
    [ "$(ip netns exec "$NS_LB" curl -s http://127.0.0.1:9871/metrics | grep '^rivora_backend_healthy' | head -1 | awk '{print $NF}')" = "0" ] && break
done
sleep 1.2   # let the limiter's bucket refill so the next SYNs reach backend selection
for _ in 1 2 3 4; do probe "$VIP1" 0.3 >/dev/null; sleep 1.3; done
NB=$(D_NB "$VIP1")
check_ge "no_healthy_backend counted SYNs with nowhere to go" "$NB" 2

section "3. unserved: the backend's map entry deleted behind rivorad's back"
start_backend
for _ in $(seq 1 20); do
    sleep 0.5
    [ "$(metrics | grep '^rivora_backend_healthy' | head -1 | awk '{print $NF}')" = "1" ] && break
done
sleep 1.3
# Find the backend's real ID from the API rather than assuming it is 0: deleting a key
# that never existed also "succeeds" and also looks absent afterwards, which would make
# the precondition below pass vacuously.
# lbx runs a command in the LB namespace and leaves its output in LBX_OUT. It must be
# called directly, NOT inside $(...): lb bumps the command counter, and in a subshell
# that increment is lost, so the next command would reuse an already-acknowledged
# number and silently return stale output.
LBX_OUT=""
lbx() { lb "exec:$1"; LBX_OUT=$(cat "${WORK}/exec-${CMDN}.out" 2>/dev/null); }
BID=$(ip netns exec "$NS_LB" curl -s --max-time 3 http://127.0.0.1:9870/api/v1/backends | python3 -c 'import sys,json; b=json.load(sys.stdin); print(b[0]["id"] if b else "")' 2>/dev/null)
[ -n "$BID" ] || { fail "could not read the backend id from the API"; BID=0; }
KEYHEX=$(printf '%02x %02x %02x %02x' $((BID & 255)) $(((BID >> 8) & 255)) $(((BID >> 16) & 255)) $(((BID >> 24) & 255)))
lbx "bpftool map lookup pinned /sys/fs/bpf/rivora-lb/backend_map key hex ${KEYHEX}"; BEFORE_OUT="$LBX_OUT"
if echo "$BEFORE_OUT" | grep -qiE 'not found|no such|ENOENT|error'; then
    fail "precondition: backend id ${BID} is not in backend_map to begin with: '${BEFORE_OUT}'"
else
    pass "precondition: backend id ${BID} is present in backend_map before the delete"
fi
lbx "bpftool map delete pinned /sys/fs/bpf/rivora-lb/backend_map key hex ${KEYHEX}"; DEL_OUT="$LBX_OUT"
# The scenario rests on that delete having worked; prove it, and report it as a
# precondition failure (not a product bug) if it didn't.
lbx "bpftool map lookup pinned /sys/fs/bpf/rivora-lb/backend_map key hex ${KEYHEX}"; LOOK_OUT="$LBX_OUT"
if echo "$LOOK_OUT" | grep -qiE 'not found|no such|ENOENT|error'; then
    pass "precondition: backend id ${BID}'s map entry is really gone"
else
    fail "precondition: the backend_map entry still exists after the delete (delete said: '${DEL_OUT}'; lookup said: '${LOOK_OUT}')"
fi
UN0=$(D_UN "$VIP1")
for _ in 1 2 3; do probe "$VIP1" 0.3 >/dev/null; sleep 1.3; done
UN=$(D_UN "$VIP1")
check_ge "unserved counted the packets that bypassed the load balancer" "$((UN - UN0))" 1
if [ "$((UN - UN0))" -lt 1 ]; then
    echo "    diagnostics: $(bpftool version 2>&1 | grep -v WARNING | head -1); kernel $(uname -r); cpus $(nproc); backend id ${BID}"
    echo "    diagnostics: unserved before=${UN0} after=${UN}; vip metrics: $(metrics | grep -E '^rivora_vip_(unserved|dropped)' | grep "$VIP1" | sed -E 's/rivora_vip_//; s/\{[^}]*(reason="[a-z_]+")?[^}]*\}/ /' | tr '\n' ' ')"
    lbx 'bpftool map dump pinned /sys/fs/bpf/rivora-lb/backend_map';  echo "    diagnostics: backend_map after delete:      $(echo "$LBX_OUT" | tr '\n' ' ' | cut -c1-200)"
    lbx 'bpftool map lookup pinned /sys/fs/bpf/rivora-lb/service_config_map key hex 00 00 00 00'; echo "    diagnostics: service_config_map[0]:         $(echo "$LBX_OUT" | tr '\n' ' ' | cut -c1-200)"
    lbx 'bpftool map lookup pinned /sys/fs/bpf/rivora-lb/drop_stats_map key hex 00 00 00 00'; echo "    diagnostics: drop_stats_map[0] (per CPU):   $(echo "$LBX_OUT" | tr '\n' ' ' | cut -c1-240)"
fi

section "4. invariant: per-reason drops == node-wide drops; unserved excluded"
RLT=$(( $(D_RL $VIP1) + $(D_RL $VIP2) )); NBT=$(( $(D_NB $VIP1) + $(D_NB $VIP2) ))
NODE=$(nodedrops)
echo "    rate_limited=${RLT} no_healthy_backend=${NBT} node-wide dropped=${NODE} unserved=${UN}"
check_eq "sum of drop reasons equals rivora_dropped_packets_total" "$((RLT + NBT))" "$NODE"

section "5. reset: a VIP removed and re-added starts from zero"
BEFORE=$(D_RL "$VIP1")
check_ge "VIP1 has accumulated rate_limited drops to lose" "$BEFORE" 1
write_config "$VIP2"; lb hup
write_config "$VIP1" "$VIP2"; lb hup
sleep 1
check_eq "re-added VIP1: rate_limited reset" "$(D_RL $VIP1)" 0
check_eq "re-added VIP1: no_healthy_backend reset" "$(D_NB $VIP1)" 0
check_eq "re-added VIP1: unserved reset" "$(D_UN $VIP1)" 0

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
