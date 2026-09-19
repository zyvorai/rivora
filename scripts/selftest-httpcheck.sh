#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-httpcheck.sh — HTTP health checks, end to end.
#
# A TCP-connect probe only proves a port accepts connections, so a backend whose
# app is failing but whose socket is open stays "healthy" and keeps taking
# traffic. healthCheck: {type: http, ...} probes an HTTP endpoint and judges its
# status instead. Here the backend's *service* port (9002, raw TCP) answers
# traffic and never stops accepting, while a separate *health* port (9102, HTTP)
# can be told to fail; VIP 2 probes that health port. VIP 1 keeps the default TCP
# probe as the untouched control. Checks:
#
#   1. baseline           both VIPs answer and both backends are healthy.
#   2. failing health     health starts returning 503 while the service port is
#                         still open and accepting: the backend goes down and VIP 2
#                         stops answering, with a TCP connect to it still working
#                         (so it was the HTTP probe that decided). VIP 1 unaffected.
#   3. recovery           health returns 200 again: the backend comes back.
#   4. reload             health fails again; a SIGHUP that changes only the probe's
#                         path (to an endpoint that is healthy) brings it back, so a
#                         reload really reaches the checker.
#   5. bad config         two VIPs sharing a backend with different probes, and a path
#                         without a leading slash, are both refused at start-up.
#
# rivorad runs in one `ip netns exec` (private mount namespace, private bpffs), so
# this never touches the host's /sys/fs/bpf/rivora-lb. Must run as root.
# Usage: sudo ./scripts/selftest-httpcheck.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-httpcheck.sh must run as root (netns + BPF attach)." >&2; exit 1; }

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
BR="brhc${SUFFIX}"
NS_LB="hc-lb-${SUFFIX}"; NS_CLIENT="hc-cl-${SUFFIX}"; NS_BE="hc-be-${SUFFIX}"
PORT="8080"; VIP1="10.83.0.100"; VIP2="10.83.0.101"; BE="10.83.0.11"
WORK="$(mktemp -d /tmp/rivora-httpcheck-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"; LOG="${WORK}/rivorad.log"; FAILFLAG="${WORK}/health-fails"
BE_PIDS=(); INNER_PID=""; CMDN=0

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

# Fixed device names + asynchronous veth teardown after `ip netns del`: clear any
# leftovers from a previous run and wait until they are really gone.
clear_stale_links() {
    local n
    for role in lb cl be; do
        ip link del "hc-${role}-br" 2>/dev/null || true
        ip link del "hc-${role}" 2>/dev/null || true
    done
    for n in $(seq 1 50); do
        ip -o link 2>/dev/null | grep -qE ' hc-(lb|cl|be)(-br)?[:@]' || return 0
        sleep 0.1
    done
    echo "stale hc-* links did not go away" >&2; return 1
}

setup_topology() {
    clear_stale_links || exit 1
    ip link add "$BR" type bridge; ip link set "$BR" up
    for pair in "lb:$NS_LB:10.83.0.1" "cl:$NS_CLIENT:10.83.0.2" "be:$NS_BE:${BE}"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "hc-${role}" type veth peer name "hc-${role}-br"
        ip link set "hc-${role}" netns "$ns"
        ip link set "hc-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "hc-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "hc-${role}"
    done
    ip netns exec "$NS_LB" sysctl -qw net.ipv4.ip_forward=1
    for v in "$VIP1" "$VIP2"; do ip netns exec "$NS_LB" ip addr add "${v}/32" dev hc-lb; done
    ip netns exec "$NS_BE" ip route add 10.83.0.2/32 via 10.83.0.1 dev hc-be

    # Two raw-TCP "apps": they answer with their name and never stop accepting.
    for spec in "9001 A" "9002 B"; do
        set -- $spec
        ip netns exec "$NS_BE" python3 -c '
import socket, sys
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("10.83.0.11", int(sys.argv[1]))); s.listen(64)
while True:
    c, _ = s.accept(); c.sendall((sys.argv[2] + "\n").encode()); c.close()
' "$1" "$2" >/dev/null 2>&1 &
        BE_PIDS+=($!)
    done
    # The HTTP health endpoint for VIP 2's backend, on its own port. /healthz
    # fails (503) while the flag file exists; /ready always passes.
    ip netns exec "$NS_BE" python3 -c '
import http.server, os, sys
FLAG = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        code = 503 if (self.path == "/healthz" and os.path.exists(FLAG)) else 200
        self.send_response(code); self.send_header("Content-Length", "0"); self.end_headers()
    def log_message(self, *a): pass
http.server.ThreadingHTTPServer(("10.83.0.11", 9102), H).serve_forever()
' "$FAILFLAG" >/dev/null 2>&1 &
    BE_PIDS+=($!)
    sleep 0.5
}

# write_config <vip2 healthCheck path>
write_config() {
    {
        echo "interface: hc-lb"
        echo "apiListen: 127.0.0.1:9870"
        echo "healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}"
        echo "vips:"
        echo "  - {address: ${VIP1}, port: ${PORT}, protocol: tcp, mode: nat, backends: [{address: ${BE}, port: 9001}]}"
        echo "  - {address: ${VIP2}, port: ${PORT}, protocol: tcp, mode: nat, healthCheck: {type: http, port: 9102, path: ${1:-/healthz}}, backends: [{address: ${BE}, port: 9002}]}"
    } > "$CONFIG"
}

write_inner() {
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
        hup)   kill -HUP "$RPID"; sleep 1.2 ;;
        stop)  kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        # Runs rivorad in the foreground and records its exit status, for a start expected to fail.
        try)   echo "=== try $n" >>"$LOG"; timeout 10 "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" >>"$LOG" 2>&1; echo $? > "${WORK}/exit-$n" ;;
        quit)  break ;;
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

# probe <ip> <port>  -> what the server answers, or "fail"
probe() {
    ip netns exec "$NS_CLIENT" python3 -c '
import socket, sys
try:
    s = socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=1.0)
    d = s.recv(16).decode().strip(); s.close(); print(d or "fail")
except Exception:
    print("fail")
' "$1" "$2"
}

metrics() { ip netns exec "$NS_LB" curl -s --max-time 3 http://127.0.0.1:9871/metrics; }
# healthy <vip> -> 1|0 for that VIP's backend
healthy() { metrics | grep '^rivora_backend_healthy{' | grep "vip=\"$1:${PORT}\"" | awk '{print $NF}' | head -1; }
wait_healthy() {   # wait_healthy <vip> <want 0|1> [seconds]
    for _ in $(seq 1 $(( ${3:-12} * 2 ))); do [ "$(healthy "$1")" = "$2" ] && return 0; sleep 0.5; done
    return 1
}
check_eq() { [ "$2" = "$3" ] && pass "$1 ($3)" || fail "$1: got '$2', want '$3'"; }

setup_topology
write_config /healthz
write_inner
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS_LB" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!
lb start || { fail "rivorad did not start"; tail -20 "$LOG"; exit 1; }

section "1. baseline"
wait_healthy "$VIP1" 1 && pass "VIP 1's backend is healthy (tcp probe)" || fail "VIP 1's backend never became healthy"
wait_healthy "$VIP2" 1 && pass "VIP 2's backend is healthy (http probe on its health port)" || fail "VIP 2's backend never became healthy"
check_eq "VIP 1 answers" "$(probe $VIP1 $PORT)" A
check_eq "VIP 2 answers" "$(probe $VIP2 $PORT)" B

section "2. health endpoint fails while the service port stays open"
touch "$FAILFLAG"
wait_healthy "$VIP2" 0 && pass "VIP 2's backend was marked down by the HTTP probe" || fail "VIP 2's backend stayed healthy though its health endpoint returns 503"
check_eq "VIP 2 stops answering (nowhere to send it)" "$(probe $VIP2 $PORT)" fail
# The whole point: a TCP connect to that backend still succeeds, so a TCP probe
# would have kept it in rotation.
check_eq "the backend's service port still accepts TCP directly" "$(probe $BE 9002)" B
check_eq "VIP 1 (tcp probe, different backend) is unaffected" "$(probe $VIP1 $PORT)" A
check_eq "VIP 1's backend is still healthy" "$(healthy $VIP1)" 1

section "3. recovery"
rm -f "$FAILFLAG"
wait_healthy "$VIP2" 1 && pass "VIP 2's backend came back when /healthz returned 200" || fail "VIP 2's backend never recovered"
sleep 1.2
check_eq "VIP 2 answers again" "$(probe $VIP2 $PORT)" B

section "4. a reload that changes only the probe path"
touch "$FAILFLAG"
wait_healthy "$VIP2" 0 && pass "down again while /healthz fails" || fail "VIP 2's backend was not marked down"
write_config /ready       # /ready always returns 200
lb hup
wait_healthy "$VIP2" 1 && pass "SIGHUP moved the probe to /ready and the backend recovered" || fail "the reload did not reach the checker"
grep -q 'config reloaded' "$LOG" && pass "the reload was logged as applied" || fail "no reload log line"
sleep 1.2
check_eq "VIP 2 answers under the new probe" "$(probe $VIP2 $PORT)" B
lb stop

section "5. bad config is refused at start-up"
# Two VIPs sharing a backend but disagreeing on how to probe it.
{
    echo "interface: hc-lb"
    echo "vips:"
    echo "  - {address: ${VIP1}, port: ${PORT}, protocol: tcp, mode: nat, healthCheck: {type: http}, backends: [{address: ${BE}, port: 9002}]}"
    echo "  - {address: ${VIP2}, port: ${PORT}, protocol: tcp, mode: nat, backends: [{address: ${BE}, port: 9002}]}"
} > "$CONFIG"
lb try
X=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null)
[ "$X" = 1 ] && pass "conflicting probes on a shared backend refused (exit ${X})" || fail "conflicting probes were accepted (exit '${X}')"
grep -q 'must agree' "$LOG" && pass "the error explains the conflict" || fail "unclear error: $(tail -2 "$LOG")"

{
    echo "interface: hc-lb"
    echo "vips:"
    echo "  - {address: ${VIP1}, port: ${PORT}, protocol: tcp, mode: nat, healthCheck: {type: http, path: healthz}, backends: [{address: ${BE}, port: 9001}]}"
} > "$CONFIG"
lb try
X=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null)
[ "$X" = 1 ] && pass "an http path without a leading slash refused (exit ${X})" || fail "a bad path was accepted (exit '${X}')"

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
