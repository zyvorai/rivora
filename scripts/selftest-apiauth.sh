#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ============================================================================
# selftest-apiauth.sh — API authentication: roles, rotation, visibility, TLS trust.
#
# The API used to have one all-powerful shared token, so a dashboard that only
# needed to look held a key that could drain backends, and rotating the key meant
# an outage. Against a real rivorad with a real certificate file this checks:
#
#   1. rejection       no token / a wrong token is 401.
#   2. read-only key   may read; is refused (403) on a mutation, which then did NOT
#                      happen.
#   3. rotation        two admin keys both work (drain / undrain); after the old one
#                      is dropped and rivorad restarted it gets 401.
#   4. visibility      rivora_api_auth_failures_total counts each reason; the log of
#                      rejections is throttled, never contains a key, and never the
#                      caller's ephemeral source port.
#   5. rivoractl       --ca-file verifies the certificate (a good one passes, a
#                      different one and no CA both fail with advice); a read-only key
#                      gets a clear refusal; https is implied for a bare address.
#   8. named keys      "id:alice=KEY" keys are audited by name: every state change logs who made
#                      it (never the key), and a refused change names the caller.
#   9. client certs    RIVORA_TLS_CLIENT_CA: a certificate signed by that CA authenticates by
#                      common name with no key (listed names are admins, others read-only), one
#                      from another CA is refused, rivoractl --cert/--key works, and
#                      RIVORA_TLS_CLIENT_REQUIRED turns away a caller with no certificate.
#   6. refusals        rivorad won't start with a read-only key but no admin key, a
#                      key in both roles, or a key setting that holds no usable key
#                      (which would otherwise silently switch auth OFF).
#
# rivorad runs in a private network + mount namespace on a dummy interface, so this
# never touches the host's /sys/fs/bpf/rivora-lb or its real service. Root only.
# Usage: sudo ./scripts/selftest-apiauth.sh
# ============================================================================
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PASS=0; FAIL=0
pass() { PASS=$((PASS + 1)); echo "  [pass] $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  [FAIL] $1"; }
section() { echo ""; echo "=== $1 ==="; }

[ "$(id -u)" -eq 0 ] || { echo "selftest-apiauth.sh must run as root (netns + BPF attach)." >&2; exit 1; }
command -v openssl >/dev/null || { echo "openssl is required (to make a certificate file)" >&2; exit 1; }

# Prefer this checkout's build over anything installed system-wide.
RIVORAD="${RIVORAD:-${ROOT}/bin/rivorad}";     [ -x "$RIVORAD" ] || RIVORAD="$(command -v rivorad || echo "$RIVORAD")"
RIVORACTL="${RIVORACTL:-${ROOT}/bin/rivoractl}"; [ -x "$RIVORACTL" ] || RIVORACTL="$(command -v rivoractl || echo "$RIVORACTL")"
if [ -z "${BPF_DIR:-}" ]; then
    BPF_DIR="${ROOT}/bpf"
    [ -f "${BPF_DIR}/xdp_ingress.o" ] || BPF_DIR="/usr/local/share/rivora/bpf"
fi
for f in "$RIVORAD" "$RIVORACTL" "${BPF_DIR}/xdp_ingress.o" "${BPF_DIR}/tc_nat.o"; do
    [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done

SUFFIX="$$"
NS="apiauth-${SUFFIX}"
WORK="$(mktemp -d /tmp/rivora-apiauth-${SUFFIX}.XXXX)"
CONFIG="${WORK}/config.yaml"; LOG="${WORK}/rivorad.log"
CERT="${WORK}/cert.pem"; KEY="${WORK}/key.pem"; OTHERCERT="${WORK}/other.pem"; OTHERKEY="${WORK}/other.key"
INNER_PID=""; CMDN=0

# Distinct, long-enough keys (a short one would trigger the weak-key warning).
NEW="new-admin-key-0123456789abcdef"; OLD="old-admin-key-0123456789abcdef"; RO="reader-key-0123456789abcdef"

cleanup() {
    echo quit > "${WORK}/cmd-$((CMDN + 1))" 2>/dev/null
    [ -n "$INNER_PID" ] && kill "$INNER_PID" 2>/dev/null
    pkill -f "rivorad -config ${CONFIG}" 2>/dev/null
    sleep 0.5; ip netns del "$NS" 2>/dev/null; rm -rf "$WORK"
}
trap cleanup EXIT

ip netns add "$NS"; ip netns exec "$NS" ip link set lo up
ip netns exec "$NS" ip link add authdummy type dummy; ip netns exec "$NS" ip link set authdummy up

# A real certificate file for 127.0.0.1, plus a second unrelated one.
mkcert() { openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$2" -out "$1" -days 2 \
    -subj "/CN=rivorad" -addext "subjectAltName=IP:127.0.0.1,DNS:localhost" >/dev/null 2>&1; }
mkcert "$CERT" "$KEY"; mkcert "$OTHERCERT" "$OTHERKEY"
[ -s "$CERT" ] && [ -s "$OTHERCERT" ] || { echo "could not generate certificates" >&2; exit 1; }

cat > "$CONFIG" <<EOF
interface: authdummy
apiListen: 127.0.0.1:9870
healthCheck: {interval: 1s, timeout: 500ms, failThreshold: 2, successThreshold: 1}
vips:
  - {address: 10.84.0.100, port: 80, protocol: tcp, mode: nat, backends: [{address: 127.0.0.1, port: 9}]}
EOF

# LB side: numbered command files, so every run shares one private bpffs.
cat > "${WORK}/inner.sh" <<'INNER'
#!/usr/bin/env bash
# env: RIVORAD CONFIG BPF_DIR WORK LOG
mount -t bpf bpf /sys/fs/bpf 2>/dev/null
wait_api() { for _ in $(seq 1 80); do curl -sk --max-time 2 -o /dev/null http://127.0.0.1:9871/healthz 2>/dev/null && return 0; sleep 0.1; done; return 1; }
n=1
while :; do
    while [ ! -f "${WORK}/cmd-$n" ]; do sleep 0.02; done
    c="$(cat "${WORK}/cmd-$n")"
    case "$c" in
        # start:<VAR=value ...>  -> rivorad with those environment variables, in the background
        start:*) echo "=== start $n" >>"$LOG"; env ${c#start:} "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" >>"$LOG" 2>&1 & RPID=$!; wait_api ;;
        # try:<VAR=value ...>    -> rivorad in the foreground, recording its exit status (for a start that must fail)
        try:*)   echo "=== try $n" >>"$LOG"; env ${c#try:} timeout 10 "$RIVORAD" -config "$CONFIG" -bpf-dir "$BPF_DIR" >>"$LOG" 2>&1; echo $? > "${WORK}/exit-$n" ;;
        stop)    kill -TERM "$RPID"; wait "$RPID" 2>/dev/null ;;
        quit)    break ;;
    esac
    touch "${WORK}/ack-$n"; n=$((n + 1))
done
INNER
chmod +x "${WORK}/inner.sh"
export RIVORAD CONFIG BPF_DIR WORK LOG
ip netns exec "$NS" "${WORK}/inner.sh" >/dev/null 2>&1 &
INNER_PID=$!

lb() {
    CMDN=$((CMDN + 1)); echo "$1" > "${WORK}/cmd-${CMDN}"
    for _ in $(seq 1 400); do [ -f "${WORK}/ack-${CMDN}" ] && return 0; sleep 0.05; done
    return 1
}
api() { ip netns exec "$NS" curl -s --max-time 5 --cacert "$CERT" -o /dev/null -w '%{http_code}' "$@"; }   # api <curl args...> -> HTTP status
apibody() { ip netns exec "$NS" curl -s --max-time 5 --cacert "$CERT" "$@"; }
U=https://127.0.0.1:9870
metrics() { ip netns exec "$NS" curl -s --max-time 5 http://127.0.0.1:9871/metrics; }
ctl() { ip netns exec "$NS" "$RIVORACTL" "$@" 2>&1; }
check_eq() { [ "$2" = "$3" ] && pass "$1 ($3)" || fail "$1: got '$2', want '$3'"; }
JSON='Content-Type: application/json'
adrain() { api -X POST -H "$JSON" -H "Authorization: Bearer $1" "$U/api/v1/backends/0/$2"; }
draining() { apibody -H "Authorization: Bearer $NEW" "$U/api/v1/backends" | python3 -c "import sys,json; print(json.load(sys.stdin)[0].get('adminDraining', False))"; }

lb "start:RIVORA_API_KEY=${NEW},${OLD} RIVORA_API_READONLY_KEY=${RO} RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY}" || { fail "rivorad did not start"; tail -10 "$LOG"; exit 1; }

section "1. rejection"
check_eq "no token is refused" "$(api "$U/api/v1/vips")" 401
check_eq "a wrong token is refused" "$(api -H 'Authorization: Bearer nope' "$U/api/v1/vips")" 401
check_eq "a token without the Bearer scheme is refused" "$(api -H "Authorization: $NEW" "$U/api/v1/vips")" 401

section "2. the read-only key reads but cannot change anything"
check_eq "read-only GET" "$(api -H "Authorization: Bearer $RO" "$U/api/v1/vips")" 200
check_eq "read-only POST drain is forbidden" "$(adrain "$RO" drain)" 403
check_eq "the refused drain did not happen" "$(draining)" False
apibody -X POST -H "$JSON" -H "Authorization: Bearer $RO" "$U/api/v1/backends/0/drain" | grep -q "read-only" \
    && pass "the 403 explains that the key is read-only" || fail "403 body doesn't say read-only"

section "3. rotation: two admin keys, then only the new one"
check_eq "admin key #1 (new) drains" "$(adrain "$NEW" drain)" 200
check_eq "the drain took effect" "$(draining)" True
check_eq "admin key #2 (old) undrains" "$(adrain "$OLD" undrain)" 200
check_eq "the undrain took effect" "$(draining)" False
lb stop
lb "start:RIVORA_API_KEY=${NEW} RIVORA_API_READONLY_KEY=${RO} RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY}"
check_eq "the retired key is now refused" "$(api -H "Authorization: Bearer $OLD" "$U/api/v1/vips")" 401
check_eq "the current key still works" "$(api -H "Authorization: Bearer $NEW" "$U/api/v1/vips")" 200

section "4. visibility"
# The rejection log is throttled to one line per 10s, so wait out any window left
# open by the requests above; the burst below then starts with one line and swallows
# the rest, and one more request after the window reports how many were swallowed.
sleep 11
BEFORE_LOG=$(grep -c 'api request rejected' "$LOG")
for _ in $(seq 1 25); do api -H "Authorization: Bearer scanner-guess-$RANDOM" "$U/api/v1/vips" >/dev/null; done
adrain "$RO" drain >/dev/null
M=$(metrics)
UNAUTH=$(echo "$M" | grep 'rivora_api_auth_failures_total{reason="unauthenticated"}' | awk '{print $NF}')
FORB=$(echo "$M" | grep 'rivora_api_auth_failures_total{reason="forbidden"}' | awk '{print $NF}')
[ "${UNAUTH:-0}" -ge 25 ] && pass "unauthenticated failures counted (${UNAUTH})" || fail "unauthenticated counter = '${UNAUTH}', want >= 25"
[ "${FORB:-0}" -ge 1 ] && pass "forbidden attempts counted (${FORB})" || fail "forbidden counter = '${FORB}', want >= 1"
LOGGED=$(( $(grep -c 'api request rejected' "$LOG") - BEFORE_LOG ))
[ "$LOGGED" -ge 1 ] && [ "$LOGGED" -lt 10 ] && pass "26 rejections produced ${LOGGED} log line(s): throttled, not a flood" || fail "26 rejections produced ${LOGGED} log lines"
sleep 11
api -H 'Authorization: Bearer one-more-guess' "$U/api/v1/vips" >/dev/null
SUPP=$(grep 'api request rejected' "$LOG" | tail -1 | grep -oE 'similar_suppressed=[0-9]+' | cut -d= -f2)
[ "${SUPP:-0}" -ge 20 ] && pass "the next line after the window reports ${SUPP} suppressed" || fail "similar_suppressed = '${SUPP}', want >= 20 (the burst was swallowed silently)"
if grep -qE "$NEW|$OLD|$RO|scanner-guess" "$LOG"; then fail "a credential appears in the log"; else pass "no credential appears in the log"; fi
grep 'api request rejected' "$LOG" | grep -qE 'remote=127\.0\.0\.1:[0-9]+' && fail "the ephemeral source port is logged" || pass "only the source address is logged, not its port"

section "5. rivoractl and certificate trust"
check_eq "--ca-file with the right certificate (https implied for a bare address)" "$(ctl vips --api 127.0.0.1:9870 --ca-file "$CERT" --api-key "$NEW" >/dev/null && echo ok)" ok
OUT=$(ctl vips --api 127.0.0.1:9870 --ca-file "$OTHERCERT" --api-key "$NEW")
echo "$OUT" | grep -q "not trusted" && pass "a different certificate is rejected, with advice" || fail "wrong CA: $OUT"
OUT=$(ctl vips --api https://127.0.0.1:9870 --api-key "$NEW")
echo "$OUT" | grep -q "not trusted" && pass "no CA and no --tls-insecure is rejected, with advice" || fail "no CA: $OUT"
check_eq "--tls-insecure still works, and implies https for a bare address" "$(ctl vips --api 127.0.0.1:9870 --tls-insecure --api-key "$NEW" >/dev/null && echo ok)" ok
OUT=$(ctl vips --api http://127.0.0.1:9870 --api-key "$NEW")
echo "$OUT" | grep -q "speaks HTTPS" && pass "plain http to the TLS listener explains itself (not a bare 400)" || fail "http->tls: $OUT"
OUT=$(ctl vips --ca-file "$CERT" --tls-insecure --api 127.0.0.1:9870)
echo "$OUT" | grep -q "contradict" && pass "--ca-file with --tls-insecure is refused as contradictory" || fail "both flags: $OUT"
OUT=$(ctl drain 0 --api 127.0.0.1:9870 --ca-file "$CERT" --api-key "$RO")
echo "$OUT" | grep -q "read-only" && pass "a read-only key running drain is told why" || fail "ro drain: $OUT"
check_eq "an admin key running drain works" "$(ctl drain 0 --api 127.0.0.1:9870 --ca-file "$CERT" --api-key "$NEW" >/dev/null && echo ok)" ok
ctl undrain 0 --api 127.0.0.1:9870 --ca-file "$CERT" --api-key "$NEW" >/dev/null
lb stop

section "6. configurations rivorad must refuse"
# A refusal is rivorad exiting with ITS error status (1) and logging the key error.
# "Any non-zero exit" is not enough: a rivorad that started fine and was killed by
# the `timeout` wrapper exits 124, and would pass for a refusal it never made.
refuse() {   # refuse <label> <env...>
    local label="$1"; shift
    local before; before=$(grep -c 'api key configuration' "$LOG")
    lb "try:$*"; local x; x=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null)
    local after; after=$(grep -c 'api key configuration' "$LOG")
    if [ "$x" = 1 ] && [ "$after" -gt "$before" ]; then pass "$label refused (exit 1, error logged)"
    else fail "$label was not refused: exit '${x}' (124 = it started and was killed), key-config errors logged: $((after - before))"; fi
}
refuse "a read-only key with no admin key" "RIVORA_API_READONLY_KEY=${RO}"
grep -q "read-only key would protect nothing" "$LOG" && pass "…and the error says why" || fail "unclear error: $(tail -2 "$LOG")"
refuse "a key in both roles" "RIVORA_API_KEY=${NEW},${RO} RIVORA_API_READONLY_KEY=${RO}"
refuse "an admin setting with no usable key (would switch auth off)" "RIVORA_API_KEY=,"
refuse "a read-only setting with no usable key" "RIVORA_API_KEY=${NEW} RIVORA_API_READONLY_KEY=,"

section "7. a short key is accepted but warned about, without printing it"
lb "start:RIVORA_API_KEY=short-key RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY}"
grep -q "shorter than recommended" "$LOG" && pass "a short-key warning was logged" || fail "no short-key warning"
grep -q "short-key" "$LOG" && fail "the short key itself was logged" || pass "the key value was not logged"
lb stop

section "8. named keys are audited by name"
ALICE="alice-secret-key-0123456789abcdef"; BOB="bob-secret-key-0123456789abcdef"; VIEW="viewer-secret-key-0123456789abcdef"
lb "start:RIVORA_API_KEY=id:alice=${ALICE},id:bob=${BOB} RIVORA_API_READONLY_KEY=id:viewer=${VIEW} RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY}" || { fail "rivorad did not start with named keys"; tail -10 "$LOG"; }
check_eq "alice's drain" "$(adrain "$ALICE" drain)" 200
check_eq "bob's undrain" "$(adrain "$BOB" undrain)" 200
check_eq "the read-only key's drain is forbidden" "$(adrain "$VIEW" drain)" 403
check_eq "a read still works with the read-only key" "$(api -H "Authorization: Bearer ${VIEW}" "$U/api/v1/vips")" 200
grep -q 'msg="api change" user=alice role=admin method=POST path=/api/v1/backends/0/drain status=200' "$LOG" \
    && pass "alice's change is in the audit log by name" || fail "no audit line for alice: $(grep 'api change' "$LOG" | tail -2)"
grep -q 'msg="api change" user=bob role=admin method=POST path=/api/v1/backends/0/undrain status=200' "$LOG" \
    && pass "bob's change is in the audit log by name" || fail "no audit line for bob"
grep -q 'reason=forbidden user=viewer' "$LOG" && pass "the refused change names the read-only caller" || fail "the refusal did not name the caller"
grep -q 'method=GET' <(grep 'api change' "$LOG") && fail "a read was audited as a change" || pass "reads are not audited as changes"
for k in "$ALICE" "$BOB" "$VIEW"; do grep -q "$k" "$LOG" && fail "a key was written to the log"; done; pass "no key appears in the log"
metrics | grep -q 'rivora_api_changes_total{code="200",user="alice"} 1' && pass "rivora_api_changes_total counts alice's change" || fail "no metric for alice's change: $(metrics | grep changes_total)"
refuse_named() { local before; before=$(grep -c 'api key configuration' "$LOG"); lb "try:$1"; local x; x=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null); local after; after=$(grep -c 'api key configuration' "$LOG")
    [ "$x" = 1 ] && [ "$after" -gt "$before" ] && pass "$2 refused" || fail "$2 was not refused (exit '${x}')"; }
lb stop
refuse_named "RIVORA_API_KEY=id:same=${ALICE},id:same=${BOB}" "two keys with one name"
refuse_named "RIVORA_API_KEY=id:bad name=${ALICE}" "a malformed key name"

section "9. client certificates"
CA="${WORK}/ca.pem"; CAKEY="${WORK}/ca.key"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$CAKEY" -out "$CA" -days 2 -subj "/CN=rivora selftest CA" >/dev/null 2>&1
issue() {   # issue <name> <CN> [ca cert] [ca key]  -> ${WORK}/<name>.pem and .key
    local name="$1" cn="$2" cacert="${3:-$CA}" cakey="${4:-$CAKEY}"
    openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "${WORK}/${name}.key" -out "${WORK}/${name}.csr" -subj "/CN=${cn}" >/dev/null 2>&1
    printf 'extendedKeyUsage=clientAuth\n' > "${WORK}/${name}.ext"
    openssl x509 -req -in "${WORK}/${name}.csr" -CA "$cacert" -CAkey "$cakey" -CAcreateserial -out "${WORK}/${name}.pem" -days 2 -extfile "${WORK}/${name}.ext" >/dev/null 2>&1
}
issue ops ops; issue viewer viewer
OTHERCA="${WORK}/otherca.pem"; OTHERCAKEY="${WORK}/otherca.key"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -keyout "$OTHERCAKEY" -out "$OTHERCA" -days 2 -subj "/CN=some other CA" >/dev/null 2>&1
issue stranger ops "$OTHERCA" "$OTHERCAKEY"
[ -s "${WORK}/ops.pem" ] && [ -s "${WORK}/viewer.pem" ] && [ -s "${WORK}/stranger.pem" ] || fail "could not issue the test certificates"

# Certificates only: no bearer keys at all.
lb "start:RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY} RIVORA_TLS_CLIENT_CA=${CA} RIVORA_API_CERT_ADMIN_CNS=ops" || { fail "rivorad did not start with a client CA"; tail -10 "$LOG"; }
grep -q "client certificates accepted" "$LOG" && pass "rivorad reports accepting client certificates" || fail "no client-certificate log line"
cert_api() { local c="$1"; shift; api --cert "${WORK}/${c}.pem" --key "${WORK}/${c}.key" "$@"; }
check_eq "no certificate and no key: refused" "$(api "$U/api/v1/vips")" 401
check_eq "an admin certificate can read" "$(cert_api ops "$U/api/v1/vips")" 200
check_eq "an admin certificate can drain (no key sent)" "$(cert_api ops -X POST -H "$JSON" "$U/api/v1/backends/0/drain")" 200
check_eq "another verified certificate can read" "$(cert_api viewer "$U/api/v1/vips")" 200
check_eq "another verified certificate cannot change anything" "$(cert_api viewer -X POST -H "$JSON" "$U/api/v1/backends/0/undrain")" 403
st=$(cert_api stranger -X POST -H "$JSON" "$U/api/v1/backends/0/undrain")
[ "$st" = 401 ] || [ "$st" = 000 ] && pass "a certificate from another CA is refused (${st})" || fail "a certificate from another CA got ${st}"
grep -q 'msg="api change" user=cert:ops role=admin method=POST path=/api/v1/backends/0/drain status=200' "$LOG" \
    && pass "the certificate's change is audited by common name" || fail "no audit line for cert:ops"
grep -q 'reason=forbidden user=cert:viewer' "$LOG" && pass "the refused certificate change names its holder" || fail "the refusal did not name cert:viewer"
ctl_out=$(ctl --api 127.0.0.1:9870 --ca-file "$CERT" --cert "${WORK}/ops.pem" --key "${WORK}/ops.key" undrain 0)
echo "$ctl_out" | grep -qi "error" && fail "rivoractl --cert/--key failed: $ctl_out" || pass "rivoractl --cert/--key can undrain with no api key"
ctl --api 127.0.0.1:9870 --ca-file "$CERT" --cert "${WORK}/ops.pem" vips >/dev/null 2>&1 && fail "--cert without --key was accepted" || pass "--cert without --key is refused"
lb stop

# The listener can demand a certificate outright.
lb "start:RIVORA_API_KEY=${NEW} RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY} RIVORA_TLS_CLIENT_CA=${CA} RIVORA_TLS_CLIENT_REQUIRED=1 RIVORA_API_CERT_ADMIN_CNS=ops" || fail "rivorad did not start with client certificates required"
st=$(api -H "Authorization: Bearer ${NEW}" "$U/api/v1/vips"); [ "$st" = 000 ] && pass "with a certificate required, a valid key alone cannot even connect" || fail "a caller with no certificate got ${st}"
check_eq "a certificate holder connects" "$(cert_api ops "$U/api/v1/vips")" 200
lb stop
refuse_tls() { lb "try:$1"; local x; x=$(cat "${WORK}/exit-${CMDN}" 2>/dev/null); [ "$x" = 1 ] && pass "$2 refused" || fail "$2 was not refused (exit '${x}')"; }
refuse_tls "RIVORA_TLS_CLIENT_CA=${CA}" "a client CA without TLS"
refuse_tls "RIVORA_TLS_CERT=${CERT} RIVORA_TLS_KEY=${KEY} RIVORA_TLS_CLIENT_CA=${WORK}/does-not-exist" "a client CA file that cannot be read"

echo ""
echo "summary: pass=${PASS} fail=${FAIL}"
[ "$FAIL" -eq 0 ]
