#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-xdpmode.sh — xdpMode: generic | native | auto, on a real kernel.
#
# The XDP program used to attach in generic (SKB) mode only, which gives up most
# of the performance reason to use XDP on a real NIC. xdpMode selects the mode;
# generic stays the default so nothing changes unless it is asked for. This
# checks, against real interfaces and by asking the kernel which mode is really
# attached (`ip -d link`), that:
#
#   1. default            no xdpMode => generic ("xdpgeneric"), traffic flows.
#   2. native on veth     attaches natively ("xdp") and full-NAT traffic (which
#                         forwards with XDP_PASS) still flows.
#   3. native, no driver  a dummy device has no native XDP: rivorad must refuse
#                         to start, saying so, rather than quietly running slow.
#   4. auto, no driver    the same device with `auto` starts, in generic mode,
#                         with a warning that it fell back.
#   5. mode change        under -persist-datapath, switching generic -> native
#                         replaces the link (the kernel allows only one mode per
#                         interface) leaving exactly one pin, and a restart in
#                         the same mode then hot-swaps it in place.
#
# rivorad runs in one `ip netns exec` (private mount namespace, private bpffs),
# so this never touches the host's /sys/fs/bpf/rivora-lb. Must run as root.
# Usage: sudo ./scripts/selftest-xdpmode.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-xdpmode.sh must run as root (netns + BPF attach)." >&2; exit 1; }

# Prefer this checkout's build over anything installed system-wide, which may
# predate the feature under test.
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
BR="brxdp${SUFFIX}"
NS_LB="xdp-lb-${SUFFIX}"; NS_CLIENT="xdp-cl-${SUFFIX}"; NS_BE="xdp-be-${SUFFIX}"
PORT="8080"; VIP="10.82.0.100"; BE="10.82.0.11"
WORK="$(mktemp -d /tmp/rivora-xdpmode-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"; LOG="${WORK}/rivorad.log"
INNER_PID=""; BE_PID=""; CMDN=0

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    [ -n "$BE_PID" ] && ip netns exec "$NS_BE" kill "$BE_PID" 2>/dev/null
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
        ip link del "xdp-${role}-br" 2>/dev/null || true
        ip link del "xdp-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' xdp-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale xdp-* links did not go away" >&2; return 1
}

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.82.0.1" "cl:$NS_CLIENT:10.82.0.2" "be:$NS_BE:${BE}"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "xdp-${role}" type veth peer name "xdp-${role}-br"
        ip link set "xdp-${role}" netns "$ns"
        ip link set "xdp-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "xdp-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "xdp-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev xdp-lb
    ip netns exec "$NS_BE" ip route add 10.82.0.2/32 via 10.82.0.1 dev xdp-be
    # A dummy device: no native XDP support, so it stands in for "the driver
    # can't do it".
    ip netns exec "$NS_LB" ip link add xdpdummy type dummy
    ip netns exec "$NS_LB" ip link set xdpdummy up
    ip netns exec "$NS_BE" python3 -c '
import socket
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("10.82.0.11", 8080)); s.listen(64)
while True:
    c, _ = s.accept(); c.sendall(b"BACKEND\n"); c.close()
' >/dev/null 2>&1 &
    BE_PID=$!
    sleep 0.3
}

write_config() {   # write_config <interface> [xdpMode]
    {
        echo "interface: $1"
        [ -n "${2:-}" ] && echo "xdpMode: $2"
        echo "apiListen: 127.0.0.1:9870"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        echo "  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [{address: ${BE}, port: ${PORT}}]}"
    } > "$CONFIG"
}

write_inner() {
    cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# env: RIVORAD CONFIG BPF_DIR WORK LOG
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
start() { echo "=== start $n ($1)" >>"$LOG"; "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" $1 >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 80); do curl -sf http://127.0.0.1:9870/api/v1/vips >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    c="$(cat "${WORK}/cmd-$n")"
    case "$c" in
        start:*) start "${c#start:}"; wait_api ;;
        # Runs rivorad in the foreground and records its exit status: for a start that is expected to fail.
        try:*)   echo "=== try $n (${c#try:})" >>"$LOG"; timeout 10 "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" ${c#try:} >>"$LOG" 2>&1; echo $? > "${WORK}/exit-$n" ;;
        stop)    kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        quit)    break ;;
        exec:*)  bash -c "${c#exec:}" >"${WORK}/exec-$n.out" 2>&1 ;;   # in this mount ns: sees the private bpffs
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

probe() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket
try:
    s = socket.create_connection(("10.82.0.100", 8080), timeout=1.0)
    print("ok" if s.recv(16).strip() else "fail"); s.close()
except Exception:
    print("fail")
'
}
traffic_ok() { for _ in $(seq 1 20); do [ "$(probe)" = ok ] && return 0; sleep 0.2; done; return 1; }

# What the kernel says is attached to a device in the LB namespace: "xdpgeneric",
# "xdp" (native/driver) or "none". This is the ground truth, not rivorad's log.
# The mode is the flag token on the interface's first `ip link` line; the
# `prog/xdp` line below it appears for BOTH modes, so it cannot tell them apart.
kmode() {
    local d; d=$(ip netns exec "$NS_LB" ip link show "$1" 2>/dev/null | head -1)
    if grep -qE ' xdpgeneric ' <<<"$d"; then echo xdpgeneric
    elif grep -qE ' xdp ' <<<"$d"; then echo xdp
    else echo none; fi
}
lastlog() { grep -E "$1" "$LOG" | tail -1 | cut -c1-220; }
check_eq() { [ "$2" = "$3" ] && pass "$1 ($3)" || fail "$1: got '$2', want '$3'"; }

setup_topology
write_inner
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS_LB" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!

section "1. default (no xdpMode): generic"
write_config xdp-lb
lb "start:" || fail "rivorad did not start"
check_eq "kernel reports the attached mode" "$(kmode xdp-lb)" xdpgeneric
lastlog 'msg=attached' | grep -q 'xdp_mode=generic' && pass "log reports xdp_mode=generic" || fail "log: $(lastlog 'msg=attached')"
traffic_ok && pass "traffic flows through the VIP" || fail "no traffic in generic mode"
lb stop

section "2. native on veth"
write_config xdp-lb native
lb "start:"
NATIVE=$(kmode xdp-lb)
echo "    kernel reports: ${NATIVE}"
lastlog 'msg=attached|attach xdp|level=ERROR' | sed 's/^/    log: /'
check_eq "kernel reports native (driver-mode) XDP" "$NATIVE" xdp
traffic_ok && pass "full-NAT traffic (XDP_PASS) flows in native mode" || fail "no traffic in native mode"
lb stop

section "3. native where the driver can't: must refuse, not run slow"
write_config xdpdummy native
lb "try:"
EXITN=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null)
[ "$EXITN" = 1 ] && pass "rivorad refused to start (exit ${EXITN})" || fail "rivorad started, or exit status unknown ('${EXITN}')"
grep -qi 'native XDP' "$LOG" && pass "the error says native XDP is the problem" || fail "error doesn't mention native XDP: $(tail -3 "$LOG")"
check_eq "nothing was left attached to the device" "$(kmode xdpdummy)" none

section "4. auto where the driver can't: falls back, with a warning"
write_config xdpdummy auto
lb "start:"
check_eq "kernel reports generic after the fallback" "$(kmode xdpdummy)" xdpgeneric
lastlog 'msg=attached' | grep -q 'xdp_mode=generic' && pass "log reports xdp_mode=generic" || fail "log: $(lastlog 'msg=attached')"
grep -q 'native XDP is unavailable' "$LOG" && pass "a fallback warning was logged" || fail "no fallback warning"
lb stop

section "5. mode change under -persist-datapath"
write_config xdp-lb generic
lb "start:-persist-datapath"
lb 'exec:ls /sys/fs/bpf/rivora-lb/links | tr "\n" " "'
PINS=$(cat "${WORK}/exec-${CMDN}.out")
echo "    after generic start, pins: ${PINS}"
[[ "$PINS" == *xdp-generic-xdp-lb* ]] && pass "generic link pinned under a mode-specific name" || fail "pins: ${PINS}"
lb stop
write_config xdp-lb native
lb "start:-persist-datapath"
check_eq "kernel now reports native" "$(kmode xdp-lb)" xdp
lb 'exec:ls /sys/fs/bpf/rivora-lb/links | grep "^xdp-" | tr "\n" " "'
PINS=$(cat "${WORK}/exec-${CMDN}.out")
echo "    after switching to native, xdp pins: ${PINS}"
check_eq "exactly one XDP pin remains, the native one" "$PINS" "xdp-native-xdp-lb "
traffic_ok && pass "traffic flows after the mode change" || fail "no traffic after the mode change"
lb stop
lb "start:-persist-datapath"
lastlog 'persistence on' | grep -q 'hot_swapped=.*xdp-native-xdp-lb' && pass "restarting in the same mode hot-swaps the native link in place" \
    || fail "no hot swap: $(lastlog 'persistence on')"
check_eq "still native after the hot swap" "$(kmode xdp-lb)" xdp
lb stop

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
