#!/bin/bash
#
# teardown_targets.sh -- remove every NVMe-oF I/O target that setup_targets.sh
# provisions on cn0/cn1/cn2 for the CDC test.
#
# For each cn, remove ALL dnv-cdc-* nvmet subsystems/ports and dm-zero backends
# in the safe removal order to avoid EBUSY: unlink subsystem from port first,
# then disable+remove namespace(s), then drop allowed_hosts links, then rmdir
# subsystem, then rmdir the port (and its non-default ana_groups) only when no
# other subsystem is linked to it, then dmsetup remove the dm-zero backend.
# Idempotent and safe on partial or already-clean systems; leaves the box with
# no dnv-cdc dm devices and no dnv-cdc nvmet subsystems. See cdc/plan.md
# sections 6, 9, 10, 12.
#
# Conventions mirror poc/common.sh: set -uo pipefail (NO -e); errors handled
# explicitly with _fail/_warn; cfg_set/dm ops no-op-on-match; SSH happens INSIDE
# the resource functions and the orchestrator reads like a recipe.
#
# Usage: ./teardown_targets.sh [all]
#
set -uo pipefail

SSH_USER="yupeng"
CDC0_IP="192.168.122.78"
HOST0_IP="192.168.122.193"
HOST1_IP="192.168.122.197"
CN0_IP="192.168.122.125"
CN1_IP="192.168.122.229"
CN2_IP="192.168.122.77"
NVME_IO_PORT=4420
DISCOVERY_NQN="nqn.2014-08.org.nvmexpress.discovery"
HOST0_NQN="nqn.2026-08.org.dnv:host:host0"
HOST1_NQN="nqn.2026-08.org.dnv:host:host1"
CDC_FILE="/etc/dnv-cdc/subsystems.json"

SUBSYS_A_NQN="nqn.2026-08.org.dnv:subsys-A"
SUBSYS_B_NQN="nqn.2026-08.org.dnv:subsys-B"
SUBSYS_A_UUID="11111111-1111-1111-1111-111111111111"
SUBSYS_B_UUID="22222222-2222-2222-2222-222222222222"
DM_SIZE_SECTORS=2097152
DM_MODEL="DNV_CDC"
DM_SERIAL="DNV_CDC_POC"

declare -A CNTLID_MIN=( [cn0]=1   [cn1]=256 [cn2]=512 )
declare -A CNTLID_MAX=( [cn0]=255 [cn1]=511 [cn2]=767 )
declare -A CN_IP=(      [cn0]=$CN0_IP [cn1]=$CN1_IP [cn2]=$CN2_IP )
declare -A CN_PORT=(    [cn0]=1   [cn1]=2   [cn2]=3 )
declare -A SUBSYS_UUID=( [$SUBSYS_A_NQN]=$SUBSYS_A_UUID [$SUBSYS_B_NQN]=$SUBSYS_B_UUID )
declare -A SUBSYS_ANAGRPID=( [$SUBSYS_A_NQN]=1 [$SUBSYS_B_NQN]=2 )
declare -A ANA_STATE=(
    ["$SUBSYS_A_NQN:cn0"]="optimized"
    ["$SUBSYS_A_NQN:cn1"]="inaccessible"
    ["$SUBSYS_A_NQN:cn2"]="optimized"
    ["$SUBSYS_B_NQN:cn0"]="optimized"
    ["$SUBSYS_B_NQN:cn1"]="optimized"
    ["$SUBSYS_B_NQN:cn2"]="inaccessible"
)

_ssh() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
               -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

_emit_vars() {
    cat <<EOF
NVMET=/sys/kernel/config/nvmet
NVME_IO_PORT=$NVME_IO_PORT
DM_SIZE_SECTORS=$DM_SIZE_SECTORS
DM_MODEL='$DM_MODEL'
DM_SERIAL='$DM_SERIAL'
HOST0_NQN='$HOST0_NQN'
HOST1_NQN='$HOST1_NQN'
SUBSYS_A_NQN='$SUBSYS_A_NQN'
SUBSYS_B_NQN='$SUBSYS_B_NQN'
SUBSYS_A_UUID='$SUBSYS_A_UUID'
SUBSYS_B_UUID='$SUBSYS_B_UUID'
CN0_IP='$CN0_IP'
CN1_IP='$CN1_IP'
CN2_IP='$CN2_IP'
EOF
}

_emit_remote() {
    cat <<'EOF_REMOTE'
SLOW_LIMIT_MS=3000
SLOW_LIST=/tmp/dnv-cdc-slow.list
VERIFY_FAIL=0
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

prep_cn() {
    sudo modprobe -a nvmet nvmet-tcp >/dev/null 2>&1 || true
    mountpoint -q /sys/kernel/config 2>/dev/null || \
        sudo mount -t configfs none /sys/kernel/config >/dev/null 2>&1 || true
}

dm_exists() { sudo dmsetup info "$1" >/dev/null 2>&1; }
dnv_cdc_list() { sudo dmsetup ls 2>/dev/null | awk '{print $1}' | grep '^dnv-cdc-' || true; }

cfg_set() {
    local f="$1" v="$2" cur
    [ -e "$f" ] || return 0
    cur=$(sudo cat "$f" 2>/dev/null | head -1 | sed -e 's/[[:space:]]*$//' || true)
    [ "$cur" = "$(printf '%s' "$v" | sed -e 's/[[:space:]]*$//')" ] && return 0
    echo "$v" | sudo tee "$f" >/dev/null 2>&1 || _warn "could not set $f=$v"
}

dm_remove() {
    local name="$1"
    dm_exists "$name" || return 0
    if _t "remove $name" sudo dmsetup remove --noudevsync "$name" >/dev/null 2>&1; then
        _info "removed: $name"; return 0
    fi
    if _t "remove --force $name" sudo dmsetup remove --force --noudevsync "$name" >/dev/null 2>&1; then
        _warn "removed (forced): $name"; return 0
    fi
    _warn "could not remove dm device $name"
}

nvmet_remove_target() {
    local nqn="$1" portnum="$2" dm_name="$3"
    local s="$NVMET/subsystems/$nqn" n h l p g linked=0
    l="$NVMET/ports/$portnum/subsystems/$nqn"
    [ -L "$l" ] && { _t "unlink $l" sudo rm -f "$l" 2>/dev/null || true; }
    [ -d "$s" ] || { dm_remove "$dm_name"; return 0; }
    for n in "$s"/namespaces/*; do
        [ -d "$n" ] || continue
        _t "disable $n" sudo bash -c "echo 0 > '$n/enable'" >/dev/null 2>&1 || true
        sudo rmdir "$n" >/dev/null 2>&1 || _warn "could not rmdir $n"
    done
    for h in "$s"/allowed_hosts/*; do
        [ -L "$h" ] && { sudo rm -f "$h" 2>/dev/null || true; }
    done
    if sudo rmdir "$s" >/dev/null 2>&1; then
        _info "removed nvmet subsystem: $nqn"
    else
        _warn "could not remove nvmet subsystem: $nqn"
    fi
    p="$NVMET/ports/$portnum"
    if [ -d "$p" ]; then
        for l in "$p"/subsystems/*; do [ -L "$l" ] && linked=$((linked+1)); done
        if [ "$linked" -eq 0 ]; then
            for g in "$p"/ana_groups/*; do
                [ -d "$g" ] || continue
                [ "$(basename "$g")" = "1" ] && continue
                sudo rmdir "$g" >/dev/null 2>&1 || true
            done
            sudo rmdir "$p" >/dev/null 2>&1 && _info "removed nvmet port $portnum" \
                || _warn "could not remove nvmet port $portnum"
        fi
    fi
    dm_remove "$dm_name"
}

verify_summary() {
    if [ "$VERIFY_FAIL" -eq 0 ]; then
        _ok "all device checks passed"
    else
        _warn "$VERIFY_FAIL device check(s) did not behave as expected"
    fi
}
EOF_REMOTE
}

teardown_cn() {
    local cn="$1" ip="$2"
    local portnum=${CN_PORT[$cn]}
    _info "=== teardown $cn ($ip) ==="
    { _emit_vars; cat <<EOF
CN='$cn'
PORTNUM=$portnum
EOF
      _emit_remote; cat <<'EOF_TDN'
set -uo pipefail
prep_cn
n=0
if [ -d "$NVMET/subsystems" ]; then
    for d in "$NVMET"/subsystems/*; do
        [ -d "$d" ] || continue
        nqn=$(basename "$d")
        case "$nqn" in
            nqn.2026-08.org.dnv:subsys-*)
                tag="${nqn##*:}"; tag="${tag//-/}"
                dm_name="dnv-cdc-${CN}-${tag}"
                nvmet_remove_target "$nqn" "$PORTNUM" "$dm_name"
                n=$((n+1))
                ;;
        esac
    done
fi
_info "$CN: removed $n dnv-cdc subsystem(s)"
for name in $(dnv_cdc_list); do
    dm_remove "$name"
done
ndm=$(dnv_cdc_list | wc -l)
nsub=$(ls "$NVMET/subsystems" 2>/dev/null | grep -c '^nqn.2026-08.org.dnv:subsys-')
_info "$CN: $ndm dnv-cdc dm device(s) left, $nsub nvmet subsystem(s) left"
slow_summary
_info "$CN teardown complete"
EOF_TDN
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

main() {
    local action="${1:-all}"
    case "$action" in
        all)
            teardown_cn cn0 "$CN0_IP" || _warn "cn0 teardown reported errors"
            teardown_cn cn1 "$CN1_IP" || _warn "cn1 teardown reported errors"
            teardown_cn cn2 "$CN2_IP" || _warn "cn2 teardown reported errors"
            ;;
        *) echo "Usage: $0 [all]" >&2; exit 1 ;;
    esac
    _info "teardown_targets.sh ($action) finished"
}

main "$@"