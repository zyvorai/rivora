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
#                          back (through the bpf() syscall directly, not bpftool,
#                          which does not work on every CI runner kernel), so the
#                          VIP matches but can't be served and the packet passes to
#                          the kernel stack instead.
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

# A minimal bpf() map client, so the test can inspect and delete a pinned map entry
# WITHOUT bpftool. On some CI runner kernels bpftool is a wrapper that prints "not
# found for kernel X" and does nothing; worse, that message contains "not found", so
# grepping its output for "entry not found" passes for the wrong reason. This prints
# an explicit PRESENT / ABSENT / DELETED / ENOENT instead.
write_bpfmap_helper() {
    cat > "${WORK}/bpfmap.py" <<'PYEOF'
import ctypes, os, platform, struct, sys
NR = {"x86_64": 321, "aarch64": 280}.get(platform.machine())
if NR is None:
    print("UNSUPPORTED-ARCH " + platform.machine()); sys.exit(3)
libc = ctypes.CDLL(None, use_errno=True)
def bpf(cmd, attr):
    buf = ctypes.create_string_buffer(attr, 128)
    r = libc.syscall(NR, cmd, buf, 128)
    return r, ctypes.get_errno()
def addr(b): return ctypes.addressof(b)
def obj_get(path):
    p = ctypes.create_string_buffer(path.encode())
    r, e = bpf(7, struct.pack("=QII", addr(p), 0, 0))       # BPF_OBJ_GET
    if r < 0: print("OBJ_GET-FAILED " + os.strerror(e)); sys.exit(2)
    return r
def lookup(fd, key, vsize):
    k = ctypes.create_string_buffer(struct.pack("=I", key)); v = ctypes.create_string_buffer(vsize)
    r, e = bpf(1, struct.pack("=IIQQQ", fd, 0, addr(k), addr(v), 0))   # BPF_MAP_LOOKUP_ELEM
    return (v.raw if r == 0 else None), e
def delete(fd, key):
    k = ctypes.create_string_buffer(struct.pack("=I", key))
    return bpf(3, struct.pack("=IIQ", fd, 0, addr(k)))                  # BPF_MAP_DELETE_ELEM
def possible_cpus():
    lo, _, hi = open("/sys/devices/system/cpu/possible").read().strip().partition("-")
    return int(hi or lo) + 1
cmd, path = sys.argv[1], sys.argv[2]
fd = obj_get(path)
if cmd in ("present", "delete"):
    key = int(sys.argv[3])
    if cmd == "present":
        v, e = lookup(fd, key, 12)                 # backend_map value is 12 bytes
        print("PRESENT" if v is not None else "ABSENT")
    else:
        r, e = delete(fd, key)
        print("DELETED" if r == 0 else ("ENOENT" if e == 2 else "ERROR " + os.strerror(e)))
elif cmd == "keys":
    # every u32 key in a hash map, via BPF_MAP_GET_NEXT_KEY
    keys, cur = [], None
    while True:
        nk = ctypes.create_string_buffer(4)
        if cur is None: attr = struct.pack("=IIQQQ", fd, 0, 0, addr(nk), 0)
        else:
            ck = ctypes.create_string_buffer(struct.pack("=I", cur)); attr = struct.pack("=IIQQQ", fd, 0, addr(ck), addr(nk), 0)
        r, e = bpf(4, attr)
        if r != 0: break
        cur = struct.unpack("=I", nk.raw)[0]; keys.append(cur)
        if len(keys) > 100000: break
    print("KEYS " + " ".join(map(str, keys)))
elif cmd == "dropsum":
    # drop_stats_map[key] is a per-CPU array of {rate_limited, no_backend, unserved}
    n = possible_cpus(); v, e = lookup(fd, int(sys.argv[3]), 24 * n)
    if v is None: print("LOOKUP-FAILED " + os.strerror(e)); sys.exit(2)
    tot = [0, 0, 0]
    for c in range(n):
        for i, x in enumerate(struct.unpack_from("=QQQ", v, 24 * c)): tot[i] += x
    print("DROPS rate_limited=%d no_backend=%d unserved=%d (cpus=%d)" % (tot[0], tot[1], tot[2], n))
PYEOF
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
write_bpfmap_helper
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
# Find the backend's real ID from the API rather than assuming it is 0 (deleting a key
# that never existed "succeeds" and looks absent afterwards). /api/v1/backends refuses to
# answer with more than one VIP configured, so read it from /api/v1/vips.
lbx() { lb "exec:$1"; LBX_OUT=$(cat "${WORK}/exec-${CMDN}.out" 2>/dev/null); }   # see the note above: call directly
BM=/sys/fs/bpf/rivora-lb/backend_map
BID=$(ip netns exec "$NS_LB" curl -s --max-time 3 http://127.0.0.1:9870/api/v1/vips | python3 -c 'import sys,json; v=json.load(sys.stdin); print(v[0]["backends"][0]["id"])' 2>/dev/null)
if [ -z "$BID" ]; then fail "could not read the backend id from /api/v1/vips"; BID=0; fi
lbx "python3 ${WORK}/bpfmap.py present ${BM} ${BID}"
[ "$LBX_OUT" = PRESENT ] && pass "precondition: backend id ${BID} is in backend_map before the delete" \
    || fail "precondition: backend id ${BID} should be in backend_map before the delete, helper said '${LBX_OUT}'"
lbx "python3 ${WORK}/bpfmap.py delete ${BM} ${BID}"; DEL_OUT="$LBX_OUT"
[ "$DEL_OUT" = DELETED ] && pass "the delete succeeded" || fail "the delete did not succeed: '${DEL_OUT}'"
lbx "python3 ${WORK}/bpfmap.py present ${BM} ${BID}"
[ "$LBX_OUT" = ABSENT ] && pass "precondition: backend id ${BID}'s map entry is really gone" \
    || fail "precondition: the entry is still present after the delete: '${LBX_OUT}'"
UN0=$(D_UN "$VIP1")
for _ in 1 2 3; do probe "$VIP1" 0.3 >/dev/null; sleep 1.3; done
UN=$(D_UN "$VIP1")
check_ge "unserved counted the packets that bypassed the load balancer" "$((UN - UN0))" 1
if [ "$((UN - UN0))" -lt 1 ]; then
    echo "    diagnostics: kernel $(uname -r); cpus $(nproc); backend id ${BID}; unserved before=${UN0} after=${UN}"
    echo "    diagnostics: vip metrics: $(metrics | grep -E '^rivora_vip_(unserved|dropped)' | grep "$VIP1" | tr '\n' ' ')"
    lbx "python3 ${WORK}/bpfmap.py keys ${BM}";                       echo "    diagnostics: backend_map ${LBX_OUT}"
    lbx "python3 ${WORK}/bpfmap.py dropsum /sys/fs/bpf/rivora-lb/drop_stats_map 0"; echo "    diagnostics: drop_stats_map[0] ${LBX_OUT}"
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
