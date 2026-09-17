#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# demo-traffic.sh — Stand up a real VIP+backends and drive live traffic
# through it, so rivorad's web console (https://<host>:9870/) shows actual
# moving numbers instead of the placeholder example config.
#
# Topology mirrors scripts/selftest.sh's proven bridge+netns setup (NAT
# mode), except veth-lb stays in the *root* network namespace instead of a
# throwaway one, so the host's real systemd-managed rivorad — the one the
# dashboard is already pointed at — attaches to it directly.
#
# Usage:
#   sudo ./scripts/demo-traffic.sh start [--duration SECONDS] [--clients N]
#   sudo ./scripts/demo-traffic.sh status
#   sudo ./scripts/demo-traffic.sh stop
#
# start with no --duration runs the live dashboard until Ctrl-C; traffic and
# the demo VIP keep running in the background after that — `stop` tears
# everything down and restores the original /etc/rivora/config.yaml.
# ============================================================================
set -uo pipefail

BR="br-demo0"
NS_CLIENT="riv-demo-client"
NS_BE1="riv-demo-be1"
NS_BE2="riv-demo-be2"
VIP="10.77.9.100"
PORT="8080"
CLIENTS=8
DURATION=0 # 0 = run the live view until Ctrl-C
STATE_DIR="/run/rivora-demo"
CONFIG="/etc/rivora/config.yaml"
CONFIG_BACKUP="/etc/rivora/config.yaml.demo-backup"

# ---- macOS-Terminal-style palette: black ground, bright ANSI foregrounds --
BG=$'\033[40m'
CLR=$'\033[2J\033[H'
RST=$'\033[0m'
BOLD=$'\033[1m'
DIM=$'\033[2m'
GREEN=$'\033[92m'
CYAN=$'\033[96m'
YELLOW=$'\033[93m'
MAGENTA=$'\033[95m'
RED=$'\033[91m'
WHITE=$'\033[97m'

need_root() { [ "$(id -u)" -eq 0 ] || { echo "must run as root" >&2; exit 1; }; }

teardown_topology() {
    kill "$(cat "${STATE_DIR}/client.pids" 2>/dev/null)" 2>/dev/null || true
    if [ -f "${STATE_DIR}/client.pids" ]; then
        while read -r pid; do kill "$pid" 2>/dev/null || true; done < "${STATE_DIR}/client.pids"
    fi
    ip netns exec "$NS_BE1" pkill -f demo_backend_server 2>/dev/null || true
    ip netns exec "$NS_BE2" pkill -f demo_backend_server 2>/dev/null || true
    for ns in "$NS_CLIENT" "$NS_BE1" "$NS_BE2"; do ip netns del "$ns" 2>/dev/null || true; done
    ip link del veth-lb 2>/dev/null || true
    ip link del "$BR" 2>/dev/null || true
}

backend_server_py() {
    cat <<'PYEOF'
import http.server, socket, sys
addr, port, ident = sys.argv[1], int(sys.argv[2]), sys.argv[3]

class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = (ident + "\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass  # demo_backend_server marker for pkill; keep stdout quiet

class Server(http.server.HTTPServer):
    address_family = socket.AF_INET

Server((addr, port), Handler).serve_forever()
PYEOF
}

setup_topology() {
    mkdir -p "$STATE_DIR"
    teardown_topology >/dev/null 2>&1 || true

    ip link add "$BR" type bridge
    ip link set "$BR" up

    ip link add veth-lb type veth peer name veth-lb-br
    ip link set veth-lb-br master "$BR" up
    ip link set veth-lb up
    ip addr add "10.77.9.1/24" dev veth-lb
    ip addr add "${VIP}/32" dev veth-lb

    for pair in "client:$NS_CLIENT:10.77.9.2" "be1:$NS_BE1:10.77.9.11" "be2:$NS_BE2:10.77.9.12"; do
        role="${pair%%:*}"; rest="${pair#*:}"; ns="${rest%%:*}"; ip_="${rest#*:}"
        ip netns add "$ns"
        ip link add "veth-${role}" type veth peer name "veth-${role}-br"
        ip link set "veth-${role}" netns "$ns"
        ip link set "veth-${role}-br" master "$BR" up
        ip netns exec "$ns" ip link set lo up
        ip netns exec "$ns" ip link set "veth-${role}" up
        ip netns exec "$ns" ip addr add "${ip_}/24" dev "veth-${role}"
    done

    # Force backend->client return traffic back through the LB so it un-NATs
    # it, even though everything shares one L2 segment here (see
    # selftest.sh's identical comment for why this matters in NAT mode).
    ip netns exec "$NS_BE1" ip route add 10.77.9.2/32 via 10.77.9.1 dev veth-be1
    ip netns exec "$NS_BE2" ip route add 10.77.9.2/32 via 10.77.9.1 dev veth-be2

    ip netns exec "$NS_BE1" python3 -c "$(backend_server_py)" 10.77.9.11 "$PORT" "BACKEND-1  (demo_backend_server)" \
        >"${STATE_DIR}/be1.log" 2>&1 &
    ip netns exec "$NS_BE2" python3 -c "$(backend_server_py)" 10.77.9.12 "$PORT" "BACKEND-2  (demo_backend_server)" \
        >"${STATE_DIR}/be2.log" 2>&1 &
    sleep 0.3
}

apply_config() {
    [ -f "$CONFIG_BACKUP" ] || cp -f "$CONFIG" "$CONFIG_BACKUP" 2>/dev/null || true
    cat > "$CONFIG" <<EOF
# Written by demo-traffic.sh — restore with: sudo ./scripts/demo-traffic.sh stop
interface: veth-lb
apiListen: 0.0.0.0:9870
healthCheck: {interval: 2s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: ${VIP}, port: ${PORT}, protocol: tcp, mode: nat, backends: [
      {address: 10.77.9.11, port: ${PORT}},
      {address: 10.77.9.12, port: ${PORT}}]}
EOF
    systemctl restart rivorad
    # Port 9871 (added for Prometheus scraping — see internal/api) is always
    # plain HTTP regardless of the main API's TLS setting, so it's the
    # simplest thing to poll here without caring whether RIVORA_TLS_* is set.
    for _ in $(seq 1 20); do
        curl -sf "http://127.0.0.1:9871/readyz" >/dev/null 2>&1 && return 0
        sleep 0.5
    done
    echo "${RED}rivorad did not become ready after restart — check: journalctl -u rivorad -n 50${RST}" >&2
    return 1
}

start_clients() {
    : > "${STATE_DIR}/requests.count"
    : > "${STATE_DIR}/client.pids"
    for _ in $(seq 1 "$CLIENTS"); do
        (
            while true; do
                ip netns exec "$NS_CLIENT" curl -s -o /dev/null "http://${VIP}:${PORT}/" \
                    && flock "${STATE_DIR}/requests.count" -c "echo -n . >> '${STATE_DIR}/requests.count'"
                sleep 0.05
            done
        ) &
        echo $! >> "${STATE_DIR}/client.pids"
    done
}

render_dashboard() {
    local elapsed="$1"
    local status vips_json err=""
    # install-systemd.sh always forces RIVORA_TLS_SELF_SIGNED=1 for this
    # lab-remote-access deployment shape, so :9870 is HTTPS with a
    # self-signed cert — hence -k, same as the deploy summary already tells
    # operators ("curl -k for API").
    status=$(curl -sfk "https://127.0.0.1:9870/api/v1/vips" 2>/dev/null) || err="rivorad API unreachable on :9870"
    vips_json="$status"

    printf '%s%s' "$CLR" "$BG"
    echo "${BOLD}${CYAN}  ● RIVORA — LIVE TRAFFIC DEMO${RST}${BG}"
    echo "${DIM}    VIP ${WHITE}${VIP}:${PORT}${DIM}  ·  mode ${WHITE}NAT${DIM}  ·  elapsed ${WHITE}${elapsed}s${DIM}  ·  Ctrl-C to detach (traffic keeps running)${RST}${BG}"
    echo ""

    if [ -n "$err" ]; then
        echo "  ${RED}${BOLD}${err}${RST}${BG}"
        return
    fi

    python3 - "$vips_json" <<'PYEOF'
import json, sys
try:
    vips = json.loads(sys.argv[1])
except Exception:
    print("  \033[91mcould not parse rivorad status\033[0m")
    sys.exit(0)
if not vips:
    print("  \033[93mno VIPs programmed yet\033[0m")
    sys.exit(0)
v = vips[0]
backends = v.get("backends") or []
maxp = max([b.get("packets", 0) for b in backends] + [1])
print(f"  \033[97m\033[1mpackets\033[0m {v.get('packets',0):>10,}    \033[95m\033[1mbytes\033[0m {v.get('bytes',0):>12,}    \033[91mdropped\033[0m {v.get('dropped',0):>6,}")
print()
print("  \033[96m\033[1mBACKENDS\033[0m")
for b in backends:
    dot = "\033[92m●\033[0m" if b.get("healthy") else "\033[91m●\033[0m"
    pct = (b.get("packets", 0) / maxp) if maxp else 0
    filled = int(pct * 30 + 0.5)
    barstr = "\033[96m" + ("█" * filled) + "\033[2m" + ("░" * (30 - filled)) + "\033[0m"
    print(f"   {dot} {b.get('address',''):<14}:{b.get('port',''):<6} {barstr} {b.get('packets',0):>8,} pkt")
PYEOF

    local n
    n=$(wc -c < "${STATE_DIR}/requests.count" 2>/dev/null || echo 0)
    echo ""
    echo "  ${MAGENTA}${BOLD}CLIENT${RST}${BG}  ${CLIENTS} concurrent loops, ${WHITE}${n}${RST}${BG} requests sent this run"
}

live_view() {
    local start now elapsed
    start=$(date +%s)
    trap 'printf "%s\n" "$RST"; echo "detached — demo keeps running. Check again: sudo $0 status   Tear down: sudo $0 stop"; exit 0' INT TERM
    while true; do
        now=$(date +%s)
        elapsed=$((now - start))
        render_dashboard "$elapsed"
        if [ "$DURATION" -gt 0 ] && [ "$elapsed" -ge "$DURATION" ]; then
            printf '%s\n' "$RST"
            echo "duration elapsed — demo keeps running. Check again: sudo $0 status   Tear down: sudo $0 stop"
            break
        fi
        sleep 1
    done
}

cmd_start() {
    need_root
    echo "${CYAN}setting up demo topology + backends...${RST}"
    setup_topology
    echo "${CYAN}applying rivorad config + restarting service...${RST}"
    apply_config || exit 1
    echo "${CYAN}starting ${CLIENTS} concurrent client loops...${RST}"
    start_clients
    live_view
}

cmd_status() {
    render_dashboard "n/a"
    printf '%s\n' "$RST"
}

cmd_stop() {
    need_root
    echo "${YELLOW}stopping traffic and tearing down demo topology...${RST}"
    teardown_topology
    if [ -f "$CONFIG_BACKUP" ]; then
        mv -f "$CONFIG_BACKUP" "$CONFIG"
        echo "${GREEN}restored original ${CONFIG}${RST}"
    fi
    systemctl restart rivorad 2>/dev/null || true
    rm -rf "$STATE_DIR"
    echo "${GREEN}done${RST}"
}

while [ $# -gt 0 ]; do
    case "$1" in
        --duration) DURATION="$2"; shift 2 ;;
        --clients)  CLIENTS="$2"; shift 2 ;;
        start|status|stop) CMD="$1"; shift ;;
        *) echo "unknown argument: $1" >&2; exit 1 ;;
    esac
done

case "${CMD:-}" in
    start)  cmd_start ;;
    status) cmd_status ;;
    stop)   cmd_stop ;;
    *) echo "usage: $0 {start|status|stop} [--duration SECONDS] [--clients N]" >&2; exit 1 ;;
esac
