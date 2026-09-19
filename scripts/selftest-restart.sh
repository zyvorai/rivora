#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-restart.sh — does restarting rivorad drop traffic?
#
# Two runs over the same isolated veth/netns topology (full-NAT, so both the
# XDP ingress and the TCX egress links are exercised), each SIGTERMing rivorad
# and starting it again while a client hammers the VIP:
#
#   control     no flag. Links are attached to rivorad's own fds, so they
#               detach on exit. Expected: connections fail during the restart.
#               This proves the test can see a gap at all — a "zero failures"
#               result means nothing if the control run also shows zero.
#   persisted   -persist-datapath. Links are pinned and hot-swapped on start.
#               Expected: zero failed connections, both links reported
#               hot-swapped, the datapath still forwarding while rivorad is
#               stopped, and `rivorad -detach` then really removing it.
#
# rivorad runs inside one `ip netns exec` invocation per run: that gives it a
# private mount namespace with a fresh bpffs (pins live only as long as it), so
# this never touches the host's /sys/fs/bpf/rivora-lb, and both rivorad
# instances of a run share that one bpffs, which the persisted path needs.
#
# Must run as root. Usage: sudo ./scripts/selftest-restart.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-restart.sh must run as root (netns + BPF attach)." >&2; exit 1; }

# Both overridable so the test can run a freshly built binary without
# installing it over a running rivorad service.
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
BR="brrst${SUFFIX}"
NS_LB="rst-lb-${SUFFIX}"; NS_CLIENT="rst-client-${SUFFIX}"; NS_BE1="rst-be1-${SUFFIX}"; NS_BE2="rst-be2-${SUFFIX}"
VIP="10.78.0.100"; PORT="8080"
WORK="$(mktemp -d /tmp/rivora-restart-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"
INNER_PID=""; BE1_PID=""; BE2_PID=""

cleanup() {
    touch "${WORK}/QUIT" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    [ -n "$BE1_PID" ] && ip netns exec "$NS_BE1" kill "$BE1_PID" 2>/dev/null
    [ -n "$BE2_PID" ] && ip netns exec "$NS_BE2" kill "$BE2_PID" 2>/dev/null
    for ns in "$NS_LB" "$NS_CLIENT" "$NS_BE1" "$NS_BE2"; do ip netns del "$ns" 2>/dev/null; done
    ip link del "$BR" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

# Fixed device names + asynchronous veth teardown after `ip netns del` means a run
# started right after another can hit "File exists". Clear any leftovers and wait
# until they are gone before creating anything.
clear_stale_links() {
    local n
    for role in lb client be1 be2; do
        ip link del "rst-${role}-br" 2>/dev/null || true
        ip link del "rst-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' rst-(lb|client|be1|be2)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale rst-* links did not go away" >&2; return 1
}

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.78.0.1" "client:$NS_CLIENT:10.78.0.2" "be1:$NS_BE1:10.78.0.11" "be2:$NS_BE2:10.78.0.12"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "rst-${role}" type veth peer name "rst-${role}-br"
        ip link set "rst-${role}" netns "$ns"
        ip link set "rst-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "rst-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "rst-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    ip netns exec "$NS_LB" ip addr add "${VIP}/32" dev rst-lb
    # Force backend->client replies back through the LB so its TCX egress
    # program un-NATs them, even though everything shares one L2 segment.
    ip netns exec "$NS_BE1" ip route add 10.78.0.2/32 via 10.78.0.1 dev rst-be1
    ip netns exec "$NS_BE2" ip route add 10.78.0.2/32 via 10.78.0.1 dev rst-be2

    cat > "$CONFIG" <<EOF
interface: rst-lb
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.78.0.11, port: ${PORT}},
      {address: 10.78.0.12, port: ${PORT}}]}
EOF
    backend() { ip netns exec "$1" python3 -c '
import socket, sys
a, p, ident = sys.argv[1], int(sys.argv[2]), sys.argv[3]
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((a, p)); s.listen(64)
while True:
    c, _ = s.accept(); c.sendall((ident + "\n").encode()); c.close()
' "$2" "$PORT" "$3" >/dev/null 2>&1 & }
    backend "$NS_BE1" 10.78.0.11 BACKEND-1; BE1_PID=$!
    backend "$NS_BE2" 10.78.0.12 BACKEND-2; BE2_PID=$!
    sleep 0.3
}

# The LB side of one run. Everything that must share a bpffs happens in here.
write_inner() {
    cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# args: <persist-flag or "-"> ; env: RIVORAD CONFIG BPF_DIR WORK
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
FLAG=""; [ "$1" != "-" ] && FLAG="$1"
LOG="${WORK}/rivorad-$2.log"; : > "$LOG"
start() { "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" $FLAG >>"$LOG" 2>&1 & RPID=$!; }
wait_api() { for _ in $(seq 1 60); do curl -sf http://127.0.0.1:9870/api/v1/status >/dev/null 2>&1 && return 0; sleep 0.1; done; return 1; }
wait_file() { while [ ! -f "$1" ]; do sleep 0.02; done; }

start; wait_api; touch "${WORK}/READY"
wait_file "${WORK}/GO"
kill -TERM "$RPID"; wait "$RPID" 2>/dev/null           # graceful stop
touch "${WORK}/STOPPED"                                # datapath state right now is what the client sees
start; wait_api; touch "${WORK}/RESTARTED"
wait_file "${WORK}/FINALSTOP"
kill -TERM "$RPID"; wait "$RPID" 2>/dev/null
ls /sys/fs/bpf/rivora-lb/links 2>/dev/null | tr '\n' ' ' > "${WORK}/links-after-stop"
touch "${WORK}/FINAL_STOPPED"
wait_file "${WORK}/DETACH"
"$RIVORAD" -detach >>"$LOG" 2>&1
ls /sys/fs/bpf/rivora-lb/links 2>/dev/null | tr '\n' ' ' > "${WORK}/links-after-detach"
touch "${WORK}/DETACHED"
wait_file "${WORK}/QUIT"
INNER
    chmod +x "${WORK}/inner.sh"
}

# One connection attempt from the client; prints "ok" or "fail".
probe_once() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
try:
    s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1.0)
    print("ok" if s.recv(32).strip() else "fail"); s.close()
except Exception:
    print("fail")
' "$VIP" "$PORT"
}

# Hammers the VIP for $1 seconds, one attempt every ~20ms; writes "<t> ok|fail".
start_prober() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys, time
vip, port, dur, out = sys.argv[1], int(sys.argv[2]), float(sys.argv[3]), sys.argv[4]
end = time.time() + dur
with open(out, "w") as f:
    while time.time() < end:
        t = time.time()
        try:
            s = socket.create_connection((vip, port), timeout=0.4)
            ok = bool(s.recv(32).strip()); s.close()
        except Exception:
            ok = False
        f.write("%.3f %s\n" % (t, "ok" if ok else "fail")); f.flush()
        time.sleep(0.02)
' "$VIP" "$PORT" "$1" "$2" >/dev/null 2>&1 &
    PROBER_PID=$!
}

# Traffic is "up" once a connection succeeds; retry, since the first attempts
# after rivorad starts can race neighbour (ARP) resolution and the health
# checker's first pass. A single unlucky probe must not abort the run.
wait_traffic() { for _ in $(seq 1 25); do [ "$(probe_once)" = ok ] && return 0; sleep 0.2; done; return 1; }

# Tear down a scenario's LB side (used when it aborts) so the next scenario
# doesn't collide with a leftover rivorad on the API port.
stop_inner() {
    touch "${WORK}/GO" "${WORK}/FINALSTOP" "${WORK}/DETACH" "${WORK}/QUIT"
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    INNER_PID=""; sleep 0.5
}

wait_for() { for _ in $(seq 1 "${2:-300}"); do [ -f "${WORK}/$1" ] && return 0; sleep 0.05; done; return 1; }

run_scenario() {
    local label="$1" flag="$2"
    LAST_FAILS=-1   # stays -1 if the scenario aborts early; callers treat that as already-failed
    section "${label}"
    rm -f "${WORK}"/{READY,GO,STOPPED,RESTARTED,FINALSTOP,FINAL_STOPPED,DETACH,DETACHED,QUIT,links-after-stop,links-after-detach}

    RIVORAD="$RIVORAD" CONFIG="$CONFIG" BPF_DIR="$BPF_DIR" WORK="$WORK" \
        ip netns exec "$NS_LB" "${WORK}/inner.sh" "${flag:--}" "$label" >/dev/null 2>&1 &
    INNER_PID=$!
    wait_for READY 200 || { fail "${label}: rivorad never became ready"; tail -20 "${WORK}/rivorad-${label}.log"; stop_inner; return 1; }
    wait_traffic && pass "${label}: traffic flows before the restart" || { fail "${label}: no traffic before the restart"; stop_inner; return 1; }

    local trace="${WORK}/trace-${label}"
    start_prober 9 "$trace"
    sleep 2
    touch "${WORK}/GO"
    wait_for RESTARTED 300 || fail "${label}: rivorad did not come back after restart"
    wait "$PROBER_PID" 2>/dev/null

    local total fails
    total=$(wc -l < "$trace"); fails=$(grep -c ' fail$' "$trace")
    # Longest run of consecutive failures, as wall-clock milliseconds.
    local gap_ms
    gap_ms=$(awk '$2=="fail"{ if(!s) s=$1; e=$1 } $2=="ok"{ if(s){ d=(e-s)*1000; if(d>m) m=d; s=0 } } END{ if(s){d=(e-s)*1000; if(d>m)m=d} printf "%d", m }' "$trace")
    echo "    ${total} connection attempts across the restart, ${fails} failed (longest failing stretch ~${gap_ms}ms)"
    LAST_FAILS="$fails"
    # Reading: the persisted run is judged by the caller.

    if [ -n "$flag" ]; then
        # rivorad is up again; now stop it for good and see what the datapath does.
        touch "${WORK}/FINALSTOP"
        wait_for FINAL_STOPPED 200 || fail "${label}: final stop did not complete"
        [ "$(probe_once)" = ok ] && pass "${label}: datapath still forwards while rivorad is stopped" \
                                  || fail "${label}: datapath stopped forwarding when rivorad exited"
        local links; links=$(cat "${WORK}/links-after-stop" 2>/dev/null)
        [[ "$links" == *xdp-generic-rst-lb* && "$links" == *tcx-egress-rst-lb* ]] \
            && pass "${label}: both links pinned after exit (${links})" || fail "${label}: expected xdp + tcx links pinned, got '${links}'"
        touch "${WORK}/DETACH"
        wait_for DETACHED 200 || fail "${label}: -detach did not complete"
        [ -z "$(cat "${WORK}/links-after-detach" 2>/dev/null | tr -d ' ')" ] \
            && pass "${label}: rivorad -detach removed every pinned link" || fail "${label}: links remain after -detach: $(cat "${WORK}/links-after-detach")"
        sleep 0.3
        [ "$(probe_once)" = fail ] && pass "${label}: after -detach the VIP no longer forwards" \
                                    || fail "${label}: VIP still forwards after -detach"
    else
        # Nothing was persisted; still walk the inner script through its steps.
        touch "${WORK}/FINALSTOP"; wait_for FINAL_STOPPED 200
        touch "${WORK}/DETACH"; wait_for DETACHED 200
    fi
    touch "${WORK}/QUIT"; wait "$INNER_PID" 2>/dev/null; INNER_PID=""
}

setup_topology
write_inner

run_scenario "control" ""
CONTROL_FAILS="$LAST_FAILS"
if [ "$CONTROL_FAILS" -lt 0 ]; then
    : # scenario aborted; the failure was already recorded above
elif [ "$CONTROL_FAILS" -gt 0 ]; then
    pass "control: an unpersisted restart drops connections (${CONTROL_FAILS}) — so the test can see a gap"
else
    fail "control: saw no failures across an unpersisted restart; the test is not sensitive enough to prove anything"
fi

run_scenario "persisted" "-persist-datapath"
if [ "$LAST_FAILS" -lt 0 ]; then
    : # scenario aborted; the failure was already recorded above
elif [ "$LAST_FAILS" -eq 0 ]; then
    pass "persisted: zero failed connections across the restart"
else
    fail "persisted: ${LAST_FAILS} connections failed across the restart (control: ${CONTROL_FAILS})"
fi
if grep -q 'hot_swapped=' "${WORK}/rivorad-persisted.log" && grep 'hot_swapped=' "${WORK}/rivorad-persisted.log" | tail -1 | grep -q 'xdp-generic-rst-lb' \
   && grep 'hot_swapped=' "${WORK}/rivorad-persisted.log" | tail -1 | grep -q 'tcx-egress-rst-lb'; then
    pass "persisted: the restart hot-swapped both the XDP and TCX programs in place"
else
    fail "persisted: restart did not hot-swap both links: $(grep 'hot_swapped=' "${WORK}/rivorad-persisted.log" | tail -1)"
fi

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
