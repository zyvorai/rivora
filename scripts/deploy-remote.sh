#!/usr/bin/env bash
# Copyright 2026 Zyvor AI Labs · https://zyvor.dev
# SPDX-License-Identifier: Apache-2.0
# ─────────────────────────────────────────────────────────────
# Rivora — Remote deployment (SSH + rsync)
#
# Same shape as guestkit/scripts/deploy-remote.sh in the sibling repo.
#
# Profiles:
#   default     Sync source -> install deps -> build on remote -> verify
#   --quick     Rsync + remote build only (skip system dep install)
#
# Auth: SSH keys (recommended). Password via sshpass is supported but deprecated.
#
# Post-deploy: remote scripts/selftest.sh
# ─────────────────────────────────────────────────────────────
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION="0.1.0"
REMOTE_DIR=""
DEPLOY_PROFILE="full"
DEPLOY_LOG="${RIVORA_DEPLOY_LOG:-${HOME}/.rivora/deploy-$(date +%Y%m%d-%H%M%S).log}"

UNINSTALL=false
FLEET_FILE=""
KEY_AUTH=false
DRY_RUN=false
SKIP_SYNC=false
SKIP_VERIFY=false
VERIFY_ONLY=false
PREFLIGHT_ONLY=false
VERBOSE=false
SSH_RETRIES="${RIVORA_SSH_RETRIES:-3}"
POSITIONAL=()

usage() {
    cat <<EOF
Rivora remote deploy v${VERSION}

Usage:
  $0 <host> <user> [options]
  $0 user@host [options]
  $0 --fleet hosts.txt

Profiles:
  (default)     Full remote build + system deps (clang, bpftool, linux headers)
  --quick       Rsync + build on remote (skip dep install)

Options:
  --help              Show this help
  --dry-run           Print steps without SSH/rsync/build
  --preflight-only    SSH + disk/sudo checks, then exit
  --verify-only       Run remote selftest only (no deploy)
  --skip-sync         Skip rsync (sources already on host)
  --skip-verify       Skip remote selftest
  --key               SSH key auth (clear password)
  --uninstall         Remove rivora from host
  -v, --verbose       Verbose rsync

Environment:
  RIVORA_DEPLOY_LOG      Log file path
  RIVORA_SSH_RETRIES     SSH retry count (default: 3)
  DEPLOY_DIR             Override remote staging dir (default: ~/.deployments/rivora)

Examples:
  $0 80.79.5.173 sus --key
  $0 sus@80.79.5.173 --quick
  $0 80.79.5.173 sus --verify-only
  make deploy-remote H=80.79.5.173 U=sus

Fleet file (one host per line):
  host user [password] [options]
  user@host root --quick
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)        usage; exit 0 ;;
        --quick)          DEPLOY_PROFILE="quick"; shift ;;
        --uninstall)      UNINSTALL=true; shift ;;
        --key)            KEY_AUTH=true; shift ;;
        --dry-run)        DRY_RUN=true; shift ;;
        --skip-sync)      SKIP_SYNC=true; shift ;;
        --skip-verify)    SKIP_VERIFY=true; shift ;;
        --verify-only)    VERIFY_ONLY=true; shift ;;
        --preflight-only) PREFLIGHT_ONLY=true; shift ;;
        -v|--verbose)     VERBOSE=true; shift ;;
        --fleet)
            shift
            FLEET_FILE="${1:?--fleet requires a hosts file path}"
            shift
            ;;
        *)
            POSITIONAL+=("$1")
            shift
            ;;
    esac
done

TARGET_HOST="${POSITIONAL[0]:-}"
TARGET_USER="${POSITIONAL[1]:-root}"
TARGET_PASS="${POSITIONAL[2]:-}"

if [ "$KEY_AUTH" = true ]; then
    TARGET_PASS=""
fi

if [[ -n "${TARGET_HOST}" && "${TARGET_HOST}" == *"@"* ]]; then
    TARGET_USER="${TARGET_HOST%%@*}"
    TARGET_HOST="${TARGET_HOST#*@}"
fi

_use_color() { [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; }
if _use_color; then
    C_OK=$'\033[32m'; C_FAIL=$'\033[31m'; C_INFO=$'\033[36m'; C_WARN=$'\033[33m'
    C_DIM=$'\033[2m'; C_BOLD=$'\033[1m'; C_CYAN=$'\033[96m'; C_RST=$'\033[0m'
else
    C_OK= C_FAIL= C_INFO= C_WARN= C_DIM= C_BOLD= C_CYAN= C_RST=
fi

_log_file() { mkdir -p "$(dirname "$DEPLOY_LOG")" 2>/dev/null || true; echo "[$(date -Iseconds)] $*" >>"$DEPLOY_LOG" 2>/dev/null || true; }
ok()   { echo "${C_OK}  [ok] $*${C_RST}"; _log_file "OK $*"; }
fail() { echo "${C_FAIL}  [fail] $*${C_RST}" >&2; _log_file "FAIL $*"; exit 1; }
info() { echo "${C_INFO}  [info] $*${C_RST}"; _log_file "INFO $*"; }
warn() { echo "${C_WARN}  [warn] $*${C_RST}"; _log_file "WARN $*"; }
dry()  { echo "${C_DIM}  [dry-run] $*${C_RST}"; _log_file "DRY $*"; }

print_banner() {
    local target
    target="${TARGET_USER}@${TARGET_HOST}"
    [ -z "${TARGET_HOST}" ] && target="(fleet mode)"
    echo ""
    echo "${C_CYAN}${C_BOLD}  Rivora Remote Deploy v${VERSION}${C_RST}"
    echo "${C_CYAN}  target: ${target}  profile: ${DEPLOY_PROFILE}${C_RST}"
    [ "$DRY_RUN" = true ] && echo "${C_DIM}  dry-run — no remote changes${C_RST}"
    echo ""
}

STEP_T0=0
STEP_IDX=0
step_begin() {
    STEP_IDX=$((STEP_IDX + 1))
    STEP_T0=$(date +%s)
    echo ""
    echo "${C_BOLD}${C_CYAN}  Step ${STEP_IDX}: $*${C_RST}"
    _log_file "STEP ${STEP_IDX}: $*"
}
step_end() { echo "${C_DIM}  done in $(( $(date +%s) - STEP_T0 ))s${C_RST}"; }
run_step() {
    step_begin "$1"; shift
    if [ "$DRY_RUN" = true ]; then dry "would run: $*"; step_end; return 0; fi
    "$@"; step_end
}

SSH_OPTS="-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=15 -o ServerAliveInterval=30"
if [ -z "${TARGET_PASS}" ]; then
    SSH_OPTS+=" -o BatchMode=yes -o PreferredAuthentications=publickey"
fi

_ssh_once() {
    if [ -n "${TARGET_PASS}" ] && command -v sshpass &>/dev/null; then
        export SSHPASS="${TARGET_PASS}"
        sshpass -e ssh ${SSH_OPTS} "${TARGET_USER}@${TARGET_HOST}" "$@"
    else
        ssh ${SSH_OPTS} "${TARGET_USER}@${TARGET_HOST}" "$@"
    fi
}

_ssh() {
    local attempt=1 max="${SSH_RETRIES}"
    while [ "$attempt" -le "$max" ]; do
        if _ssh_once "$@"; then return 0; fi
        attempt=$((attempt + 1))
        if [ "$attempt" -le "$max" ]; then
            local _d=$(( 2 * (attempt - 1) )); _d=$(( _d < 2 ? 2 : _d > 30 ? 30 : _d ))
            warn "SSH retry ${attempt}/${max}" && sleep "${_d}"
        fi
    done
    return 1
}

_rsync() {
    local opts="-az --delete"
    [ "$VERBOSE" = true ] && opts+=" --progress"
    if [ -n "${TARGET_PASS}" ] && command -v sshpass &>/dev/null; then
        export SSHPASS="${TARGET_PASS}"
        rsync ${opts} -e "sshpass -e ssh ${SSH_OPTS}" "$@"
    else
        rsync ${opts} -e "ssh ${SSH_OPTS}" "$@"
    fi
}

validate() {
    [ -n "${TARGET_HOST}" ] || { usage; exit 1; }
    [ -f "${PROJECT_DIR}/go.mod" ] || fail "Not in rivora repo: ${PROJECT_DIR}"
    if [ -n "${TARGET_PASS}" ]; then
        warn "Password auth is deprecated. Prefer: ssh-copy-id ${TARGET_USER}@${TARGET_HOST}"
        command -v sshpass &>/dev/null || fail "sshpass required for password auth"
    fi
}

check_connectivity() {
    info "SSH -> ${TARGET_USER}@${TARGET_HOST}  log: ${DEPLOY_LOG}"
    if [ "$DRY_RUN" = true ]; then
        REMOTE_DIR="${DEPLOY_DIR:-${HOME}/.deployments/rivora}"
        return 0
    fi
    _ssh "echo ok" &>/dev/null || fail "SSH failed — try: ssh-copy-id ${TARGET_USER}@${TARGET_HOST}"
    ok "SSH connected"
    local remote_home
    remote_home=$(_ssh "echo \$HOME" 2>/dev/null | tr -d '\r')
    remote_home="${remote_home:-/home/${TARGET_USER}}"
    REMOTE_DIR="${DEPLOY_DIR:-${remote_home}/.deployments/rivora}"
    info "Remote path: ${REMOTE_DIR}"
}

preflight_remote() {
    info "Preflight on ${TARGET_HOST}..."
    if [ "$DRY_RUN" = true ]; then return 0; fi
    _ssh bash <<'REMOTE' || fail "Preflight failed"
set -e
echo "  host: $(hostname -f 2>/dev/null || hostname)"
echo "  os:   $(. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME" || uname -s)"
echo "  arch: $(uname -m)"
echo "  kernel: $(uname -r)"
echo "  mem:  $(free -h 2>/dev/null | awk '/^Mem:/{print $2}' || echo n/a)"
echo "  disk: $(df -h / 2>/dev/null | awk 'NR==2{print $4 " free on " $1}' || echo n/a)"
if [ "$(id -u)" -ne 0 ]; then
    if ! sudo -n true 2>/dev/null; then
        echo "  FAIL: non-root user needs passwordless sudo for package install"
        exit 1
    fi
    echo "  OK: passwordless sudo"
else
    echo "  OK: running as root"
fi
command -v go >/dev/null && echo "  OK: go $(go version | awk '{print $3}')" || echo "  WARN: go missing (will be installed)"
command -v clang >/dev/null && echo "  OK: clang $(clang --version | head -1)" || echo "  WARN: clang missing (will be installed)"
REMOTE
    ok "Preflight passed"
}

sync_files() {
    if [ "$SKIP_SYNC" = true ]; then
        info "Skipping rsync (--skip-sync)"
        return 0
    fi
    _ssh "mkdir -p '${REMOTE_DIR}'"
    _rsync --exclude '.git' --exclude 'bin' --exclude '*.o' \
        "${PROJECT_DIR}/" "${TARGET_USER}@${TARGET_HOST}:${REMOTE_DIR}/"
    ok "Source synced to ${REMOTE_DIR}"
}

install_system_deps() {
    _ssh bash <<'REMOTE'
set -euo pipefail
SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"

. /etc/os-release 2>/dev/null || true
_ID="${ID:-}" _ID_LIKE="${ID_LIKE:-}"
if [[ "$_ID" == "debian" || "$_ID" == "ubuntu" || "$_ID_LIKE" == *"debian"* ]]; then
    $SUDO apt-get update -qq
    $SUDO apt-get install -y -qq clang llvm libelf-dev build-essential \
        linux-tools-common "linux-tools-$(uname -r)" curl git rsync || \
        $SUDO apt-get install -y -qq clang llvm libelf-dev build-essential curl git rsync
elif command -v dnf &>/dev/null; then
    $SUDO dnf install -y clang llvm elfutils-libelf-devel gcc make bpftool curl git rsync
else
    echo "ERROR: unsupported package manager" >&2
    exit 1
fi

if ! command -v go &>/dev/null; then
    echo "Installing Go toolchain..."
    GOARCH_="amd64"; [ "$(uname -m)" = "aarch64" ] && GOARCH_="arm64"
    curl -fsSL "https://go.dev/dl/$(curl -fsSL https://go.dev/VERSION?m=text | head -1).linux-${GOARCH_}.tar.gz" -o /tmp/go.tar.gz
    $SUDO rm -rf /usr/local/go
    $SUDO tar -C /usr/local -xzf /tmp/go.tar.gz
    echo 'export PATH=$PATH:/usr/local/go/bin' | $SUDO tee /etc/profile.d/rivora-go.sh >/dev/null
fi
echo "System dependencies installed"
REMOTE
}

build_install_remote() {
    _ssh env REMOTE_STAGING="${REMOTE_DIR}" bash <<'REMOTE'
set -e
SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"
export PATH="$PATH:/usr/local/go/bin"
cd "${REMOTE_STAGING}"
bash scripts/build-linux-release.sh
$SUDO install -m755 bin/rivorad /usr/local/bin/rivorad
$SUDO install -m755 bin/rivoractl /usr/local/bin/rivoractl
$SUDO install -m755 bin/rivora-doctor /usr/local/bin/rivora-doctor
$SUDO mkdir -p /usr/local/share/rivora/bpf /etc/rivora
$SUDO install -m644 bpf/xdp_ingress.o bpf/tc_nat.o /usr/local/share/rivora/bpf/
if [ ! -f /etc/rivora/config.yaml ]; then
    $SUDO install -m644 config/examples/single-vip.yaml /etc/rivora/config.yaml.example
fi
$SUDO install -m644 deploy/systemd/rivorad.service /etc/systemd/system/rivorad.service 2>/dev/null || true
$SUDO systemctl daemon-reload 2>/dev/null || true
echo "Installed: $(rivorad -version 2>/dev/null || echo ok)"
REMOTE
}

verify_remote() {
    info "Running selftest on ${TARGET_HOST}..."
    if [ "$DRY_RUN" = true ]; then
        dry "would run: bash ${REMOTE_DIR}/scripts/selftest.sh && bash ${REMOTE_DIR}/scripts/selftest-multivip.sh"
        return 0
    fi
    # Selftest failures are warnings, not fatal to the deploy — matches the
    # original single-script behavior; rivora is installed either way.
    if _ssh_once "cd '${REMOTE_DIR}' && sudo bash scripts/selftest.sh"; then
        ok "single-VIP selftest passed"
    else
        warn "single-VIP selftest reported failures (see above)"
    fi
    if _ssh_once "cd '${REMOTE_DIR}' && sudo bash scripts/selftest-multivip.sh"; then
        ok "multi-VIP selftest passed"
    else
        warn "multi-VIP selftest reported failures (see above)"
    fi
    return 0
}

do_uninstall() {
    _ssh env REMOTE_STAGING="${REMOTE_DIR}" bash <<'REMOTE'
set -e
SUDO=""
[ "$(id -u)" -ne 0 ] && SUDO="sudo"
$SUDO systemctl stop rivorad 2>/dev/null || true
$SUDO systemctl disable rivorad 2>/dev/null || true
$SUDO rm -f /usr/local/bin/rivorad /usr/local/bin/rivoractl /usr/local/bin/rivora-doctor
$SUDO rm -f /etc/systemd/system/rivorad.service
$SUDO rm -rf /usr/local/share/rivora
$SUDO rm -rf /sys/fs/bpf/rivora-lb 2>/dev/null || true
rm -rf "${REMOTE_STAGING}"
echo "rivora removed"
REMOTE
    ok "Uninstalled on ${TARGET_HOST}"
}

deploy_profile_full() {
    run_step "Sync sources" sync_files
    run_step "System dependencies" install_system_deps
    run_step "Build and install" build_install_remote
}

deploy_profile_quick() {
    run_step "Sync sources" sync_files
    run_step "Build and install" build_install_remote
}

print_deployment_summary() {
    echo ""
    echo "${C_OK}${C_BOLD}  Deploy complete — ${TARGET_USER}@${TARGET_HOST}${C_RST}"
    echo "  log:    ${DEPLOY_LOG}"
    echo "  remote: ${REMOTE_DIR}"
    echo ""
    echo "  ssh ${TARGET_USER}@${TARGET_HOST}"
    echo "  rivora-doctor"
    echo "  sudo bash ${REMOTE_DIR}/scripts/selftest.sh"
    echo ""
}

deploy_fleet() {
    local hosts_file="$1"
    [ -f "$hosts_file" ] || fail "Fleet file not found: $hosts_file"
    chmod 600 "$hosts_file" 2>/dev/null || true
    local count=0
    while IFS=' ' read -r host user pass opts; do
        [ -z "$host" ] && continue
        [[ "$host" =~ ^# ]] && continue
        count=$((count + 1))
        TARGET_HOST="$host"; TARGET_USER="${user:-root}"; TARGET_PASS="${pass:-}"
        if [[ "$host" == *"@"* ]]; then
            TARGET_USER="${host%%@*}"; TARGET_HOST="${host#*@}"
        fi
        STEP_IDX=0
        print_banner
        check_connectivity
        preflight_remote
        if [[ "${opts:-}" == *"--uninstall"* ]]; then
            run_step "Uninstall" do_uninstall
        elif [[ "${opts:-}" == *"--quick"* ]]; then
            deploy_profile_quick
        else
            deploy_profile_full
        fi
        [ "$SKIP_VERIFY" != true ] && verify_remote
        print_deployment_summary
    done < "$hosts_file"
    ok "Fleet complete — ${count} host(s)"
}

main() {
    print_banner
    if [ -n "${FLEET_FILE}" ]; then
        validate
        deploy_fleet "${FLEET_FILE}"
        exit 0
    fi
    validate
    check_connectivity
    preflight_remote

    if [ "$PREFLIGHT_ONLY" = true ]; then ok "Preflight-only complete"; exit 0; fi
    if [ "$UNINSTALL" = true ]; then run_step "Uninstall rivora" do_uninstall; exit 0; fi
    if [ "$VERIFY_ONLY" = true ]; then
        [ "$SKIP_VERIFY" != true ] && run_step "Verify" verify_remote
        print_deployment_summary
        exit 0
    fi

    case "${DEPLOY_PROFILE}" in
        quick) deploy_profile_quick ;;
        *)     deploy_profile_full ;;
    esac

    [ "$SKIP_VERIFY" != true ] && run_step "Verify (selftest)" verify_remote
    print_deployment_summary
}

main "$@"
