#!/bin/bash
#
# teardown_hosts.sh -- remove what setup_hosts.sh creates, on host0 and host1.
#
#   Safe to run at any point: every step tolerates resources that were never
#   created, were already removed, or are only half built.
#
#   For each host:
#     - stop stacd+stafd and restore the stock *.dnv-orig configs if present.
#       Stopping stacd with disconnect-scope=only-stas-connections drops only
#       the connections stacd itself made; unrelated nvme devices are left
#       alone (no `nvme disconnect-all`).
#     - optionally clear /etc/nvme/hostnqn + hostid.
#
# Usage: ./teardown_hosts.sh [host0|host1]    (default: both)
#
set -uo pipefail

SSH_USER="yupeng"
HOST0_IP="192.168.122.193"
HOST1_IP="192.168.122.197"
declare -A HOST_IP=( [host0]=$HOST0_IP [host1]=$HOST1_IP )
STAS_DIR=/etc/stas

# ===================== local helpers =========================================
_ssh() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
              -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

_emit_vars() {
    cat <<EOF
STAS_DIR='$STAS_DIR'
EOF
}

# Remote helpers (run ON the host; quoted heredoc -- nothing expanded locally).
_emit_common() {
    cat <<'EOF_COMMON'
SLOW_LIMIT_MS=3000
SLOW_LIST=/tmp/dnv-slow.list
: > "$SLOW_LIST"

_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_ok()   { echo "[ OK ] $*"; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

_t() {
    local desc="$1"; shift
    local t0 t1 ms rc=0
    t0=$(date +%s%N)
    "$@" || rc=$?
    t1=$(date +%s%N)
    ms=$(( (t1 - t0) / 1000000 ))
    if [ "$ms" -gt "$SLOW_LIMIT_MS" ]; then
        echo "[SLOW] ${ms}ms  $desc" >&2
        echo "${ms}ms  $desc" >> "$SLOW_LIST"
    fi
    return $rc
}

slow_summary() {
    if [ -s "$SLOW_LIST" ]; then
        _warn "commands exceeding ${SLOW_LIMIT_MS}ms:"
        cat "$SLOW_LIST" >&2
    else
        _info "timing: no command exceeded ${SLOW_LIMIT_MS}ms"
    fi
}

stas_stop_restore() {
    local f
    if ! dpkg -s nvme-stas >/dev/null 2>&1; then
        _info "nvme-stas not installed - nothing to stop"
        return 0
    fi
    _t "stop stafd/stacd" sudo systemctl stop stacd stafd >/dev/null 2>&1 || true
    for f in stafd.conf stacd.conf; do
        if [ -f "/etc/stas/$f.dnv-orig" ]; then
            sudo mv -f "/etc/stas/$f.dnv-orig" "/etc/stas/$f" && _info "restored /etc/stas/$f"
        fi
    done
    _info "nvme-stas stopped (stafd: $(systemctl is-active stafd 2>/dev/null), stacd: $(systemctl is-active stacd 2>/dev/null))"
}
EOF_COMMON
}

# ===================== per-host teardown ======================================
teardown_host() {   # <name>
    local name="$1"
    local ip="${HOST_IP[$name]:-}"
    [ -n "$ip" ]  || _fail "unknown host: $name"
    _info "=== tearing down $name ($ip) ==="
    {
        _emit_vars
        _emit_common
        cat <<'EOF_HOST'
set -uo pipefail
stas_stop_restore
sudo rm -f /etc/nvme/hostnqn /etc/nvme/hostid 2>/dev/null || true
_info "cleared /etc/nvme/hostnqn and hostid"
slow_summary
_info "host teardown complete"
EOF_HOST
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# ===================== main ==================================================
TARGETS=(host0 host1)
if [ $# -ge 1 ]; then
    case "$1" in
        host0|host1) TARGETS=("$1") ;;
        *) _fail "usage: $0 [host0|host1]" ;;
    esac
fi

for h in "${TARGETS[@]}"; do
    teardown_host "$h" || _warn "$h teardown reported errors"
done

_info "teardown_hosts complete: ${TARGETS[*]}"
exit 0