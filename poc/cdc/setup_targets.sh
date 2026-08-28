#!/bin/bash
#
# setup_targets.sh -- provision NVMe-oF I/O targets on cn0/cn1/cn2 for the CDC
# (Centralized Discovery Controller) test.
#
# Reads the SAME JSON file the CDC watches (/etc/dnv-cdc/subsystems.json) ON
# the local box (the orchestrator, which also runs the CDC), determines which
# cn owns each port entry's traddr, and SSHes to that cn to do idempotent
# configfs writes + dmsetup. See cdc/plan.md sections 6, 9, 10, 12.
#
# Per-port target on the matching cn:
#   - dm-zero backend, 1 GiB:        dnv-cdc-<cn>-<tag>      (tag = sanitized nqn)
#   - nvmet subsystem <nqn>, namespace 1, fixed UUID/NGUID per subsys
#   - attr_serial/attr_model same on every cn (required for multipath fold-up)
#   - cntlid range per-cn (disjoint so the kernel folds paths into one ns)
#   - allowed_hosts = BOTH host0 and host1 NQNs (nvmet ACL; the CDC does the
#     per-host subsystem filtering via the discovery log)
#   - nvmet port on <cn ip>:4420, subsystem linked to it
#   - ana_groups/<grpid>/ana_state per the test seed. ana_grpid is per-subsys
#     (subsys-A=1, subsys-B=2) so two subsystems sharing one cn/port can hold
#     different ANA states (e.g. cn1: subsys-A inaccessible, subsys-B optimized).
#
# Conventions (mirroring poc/common.sh):
#   - set -uo pipefail (NO -e); errors handled explicitly with _fail/_warn.
#   - cfg_set is an idempotent configfs write (no-op-on-match, whitespace
#     trimmed) so re-running never hits EBUSY on a live port/subsystem.
#   - all create/remove is existenced-checked + no-op-on-match; dm-create is
#     idempotent (skip if the dm device already exists).
#   - SSH happens INSIDE the resource functions (create_target/remove_target/
#     verify_target); the orchestrator (do_all/do_sync/do_verify) reads like a
#     recipe. Remote helpers are uploaded verbatim via a quoted heredoc.
#
# Usage: ./setup_targets.sh [all|sync|verify]
#   all     provision every target present in the JSON file (default)
#   sync    reconcile: create targets present in file but missing on cn; remove
#           targets on cn whose (nqn, traddr) is absent from the file
#   verify  check each cn target: dm device linear read OK, nvmet
#           subsystem+ns+port present
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

dm_create_zero() {
    local name="$1" size="$2"
    if dm_exists "$name"; then _info "exists : $name"; return 0; fi
    printf '0 %s zero\n' "$size" | _t "dmsetup create $name" \
        sudo dmsetup create --noudevsync "$name" || _fail "dmsetup create $name"
    sudo dmsetup mknodes "$name" >/dev/null 2>&1 || true
    _info "created: $name"
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

nvmet_add_target() {
    local nqn="$1" dev="$2" uuid="$3" ana_grpid="$4"
    local serial="$5" model="$6" cmin="$7" cmax="$8" h0="$9" h1="${10}" h
    local s="$NVMET/subsystems/$nqn"
    sudo mkdir -p "$s" || _fail "mkdir subsystem $nqn"
    cfg_set "$s/attr_serial"         "$serial"
    cfg_set "$s/attr_model"          "$model"
    cfg_set "$s/attr_cntlid_min"     "$cmin"
    cfg_set "$s/attr_cntlid_max"     "$cmax"
    cfg_set "$s/attr_allow_any_host" "0"
    for h in "$h0" "$h1"; do
        sudo mkdir -p "$NVMET/hosts/$h" 2>/dev/null || true
        [ -e "$s/allowed_hosts/$h" ] || \
            sudo ln -sfn "$NVMET/hosts/$h" "$s/allowed_hosts/$h" 2>/dev/null || true
    done
    sudo mkdir -p "$s/namespaces/1" || _fail "mkdir namespace for $nqn"
    if [ "$(sudo cat "$s/namespaces/1/enable" 2>/dev/null)" != "1" ]; then
        cfg_set "$s/namespaces/1/device_path"  "$dev"
        cfg_set "$s/namespaces/1/device_uuid"  "$uuid"
        cfg_set "$s/namespaces/1/device_nguid" "$uuid"
        cfg_set "$s/namespaces/1/ana_grpid"    "$ana_grpid"
        _t "enable ns $nqn" sudo bash -c "echo 1 > '$s/namespaces/1/enable'" \
            || _fail "enable namespace for $nqn"
    fi
    _info "nvmet  : $nqn -> $dev (ana_grpid=$ana_grpid)"
}

nvmet_port_prep() {
    local portnum="$1" ip="$2"
    local d="$NVMET/ports/$portnum"
    sudo mkdir -p "$d" || _fail "mkdir port $portnum"
    cfg_set "$d/addr_trtype"  "tcp"
    cfg_set "$d/addr_adrfam"  "ipv4"
    cfg_set "$d/addr_traddr"  "$ip"
    cfg_set "$d/addr_trsvcid" "$NVME_IO_PORT"
}

nvmet_link() {
    local portnum="$1" nqn="$2" ana_grpid="$3"
    local l="$NVMET/ports/$portnum/subsystems/$nqn"
    { [ -L "$l" ] || [ -e "$l" ]; } || \
        sudo ln -s "$NVMET/subsystems/$nqn" "$l" 2>/dev/null \
        || _warn "could not link $nqn to port $portnum"
    sudo mkdir -p "$NVMET/ports/$portnum/ana_groups/$ana_grpid" 2>/dev/null || true
}

nvmet_set_ana() {
    cfg_set "$NVMET/ports/$1/ana_groups/$2/ana_state" "$3"
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

rd_ok() {
    local p="$1" l="${2:-$1}"
    timeout 3 sudo blockdev --flushbufs "$p" >/dev/null 2>&1 || true
    if timeout 5 sudo dd if="$p" of=/dev/null bs=4096 count=1 status=none >/dev/null 2>&1; then
        _ok "read succeeds : $l"
    else
        _warn "read FAILED (expected success): $l"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
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

_sanitized_tag() {
    local nqn="$1" tail
    tail="${nqn##*:}"
    printf '%s' "${tail//-/}"
}

create_target() {
    local cn="$1" ip="$2" nqn="$3" trsvcid="$4"
    local tag dm_name portnum uuid ana_grpid cmin cmax ana_state
    tag=$(_sanitized_tag "$nqn")
    dm_name="dnv-cdc-${cn}-${tag}"
    portnum=${CN_PORT[$cn]}
    uuid=${SUBSYS_UUID[$nqn]:-}
    [ -n "$uuid" ] || _fail "no UUID map entry for subsystem $nqn"
    ana_grpid=${SUBSYS_ANAGRPID[$nqn]:-}
    [ -n "$ana_grpid" ] || _fail "no ana_grpid map entry for subsystem $nqn"
    cmin=${CNTLID_MIN[$cn]}; cmax=${CNTLID_MAX[$cn]}
    ana_state=${ANA_STATE["$nqn:$cn"]:-optimized}
    _info "=== create_target $cn ($ip) $nqn (tag=$tag ana=$ana_state) ==="
    { _emit_vars; cat <<EOF
CN='$cn'
CN_IP='$ip'
NQN='$nqn'
TAG='$tag'
DM_NAME='$dm_name'
PORTNUM=$portnum
UUID='$uuid'
ANA_GRPID=$ana_grpid
ANA_STATE='$ana_state'
CNTLID_MIN=$cmin
CNTLID_MAX=$cmax
EOF
      _emit_remote; cat <<'EOF_CREATE'
set -uo pipefail
prep_cn
dm_create_zero "$DM_NAME" "$DM_SIZE_SECTORS"
nvmet_port_prep "$PORTNUM" "$CN_IP"
nvmet_add_target "$NQN" "/dev/mapper/$DM_NAME" "$UUID" "$ANA_GRPID" \
    "$DM_SERIAL" "$DM_MODEL" "$CNTLID_MIN" "$CNTLID_MAX" "$HOST0_NQN" "$HOST1_NQN"
nvmet_link "$PORTNUM" "$NQN" "$ANA_GRPID"
nvmet_set_ana "$PORTNUM" "$ANA_GRPID" "$ANA_STATE"
_info "$(dnv_cdc_list | wc -l) dnv-cdc dm devices, $(ls "$NVMET/subsystems" 2>/dev/null | grep -c '^nqn.2026-08.org.dnv:subsys-') nvmet subsystems"
slow_summary
_info "target $NQN on $CN ($CN_IP) setup complete"
EOF_CREATE
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

remove_target() {
    local cn="$1" ip="$2" nqn="$3"
    local tag dm_name portnum
    tag=$(_sanitized_tag "$nqn")
    dm_name="dnv-cdc-${cn}-${tag}"
    portnum=${CN_PORT[$cn]}
    _info "=== remove_target $cn ($ip) $nqn ==="
    { _emit_vars; cat <<EOF
CN='$cn'
NQN='$nqn'
DM_NAME='$dm_name'
PORTNUM=$portnum
EOF
      _emit_remote; cat <<'EOF_REMOVE'
set -uo pipefail
prep_cn
nvmet_remove_target "$NQN" "$PORTNUM" "$DM_NAME"
_info "$(dnv_cdc_list | wc -l) dnv-cdc dm devices left"
slow_summary
_info "target $NQN on $CN removed"
EOF_REMOVE
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

verify_target() {
    local cn="$1" ip="$2" nqn="$3" trsvcid="$4"
    local tag dm_name portnum
    tag=$(_sanitized_tag "$nqn")
    dm_name="dnv-cdc-${cn}-${tag}"
    portnum=${CN_PORT[$cn]}
    _info "=== verify_target $cn ($ip) $nqn ==="
    { _emit_vars; cat <<EOF
CN='$cn'
NQN='$nqn'
DM_NAME='$dm_name'
PORTNUM=$portnum
EOF
      _emit_remote; cat <<'EOF_VERIFY'
set -uo pipefail
prep_cn
s="$NVMET/subsystems/$NQN"
if ! dm_exists "$DM_NAME"; then
    _warn "no dm device: $DM_NAME"; VERIFY_FAIL=$((VERIFY_FAIL+1))
fi
if [ ! -d "$s" ]; then
    _warn "no nvmet subsystem: $NQN"; VERIFY_FAIL=$((VERIFY_FAIL+1))
else
    if [ "$(sudo cat "$s/namespaces/1/enable" 2>/dev/null)" != "1" ]; then
        _warn "namespace 1 not enabled for $NQN"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
    if [ ! -L "$NVMET/ports/$PORTNUM/subsystems/$NQN" ]; then
        _warn "$NQN not linked to port $PORTNUM"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
    dev="/dev/mapper/$DM_NAME"
    if [ -b "$dev" ]; then
        rd_ok "$dev" "$DM_NAME"
    else
        _warn "no block device: $dev"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
fi
verify_summary
slow_summary
[ "$VERIFY_FAIL" -eq 0 ] || exit 1
_info "target $NQN on $CN verified"
EOF_VERIFY
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

ip_to_cn() {
    case "$1" in
        "$CN0_IP") echo cn0 ;;
        "$CN1_IP") echo cn1 ;;
        "$CN2_IP") echo cn2 ;;
        *) echo "" ;;
    esac
}

_list_file_ports() {
    jq -r '.subsystems[] | .nqn as $nqn | .ports[] |
           [$nqn, .traddr, (.trsvcid|tostring)] | @tsv' "$CDC_FILE"
}

do_all() {
    _info "--- provisioning all cn targets from $CDC_FILE ---"
    local nqn traddr trsvcid cn
    while IFS=$'\t' read -r nqn traddr trsvcid; do
        [ -n "$nqn" ] || continue
        cn=$(ip_to_cn "$traddr")
        [ -n "$cn" ] || { _warn "no cn matches traddr $traddr (subsys $nqn) -- skipping"; continue; }
        create_target "$cn" "${CN_IP[$cn]}" "$nqn" "$trsvcid"
    done < <(_list_file_ports)
}

do_sync() {
    _info "--- reconciling cn targets with $CDC_FILE ---"
    declare -A wanted_trsvcid=()
    local all_nqns="$SUBSYS_A_NQN $SUBSYS_B_NQN"
    local nqn traddr trsvcid cn
    while IFS=$'\t' read -r nqn traddr trsvcid; do
        [ -n "$nqn" ] || continue
        cn=$(ip_to_cn "$traddr")
        [ -n "$cn" ] && { wanted_trsvcid["$cn:$nqn"]="$trsvcid"; \
            case " $all_nqns " in *" $nqn "*) ;; *) all_nqns="$all_nqns $nqn" ;; esac; }
    done < <(_list_file_ports)
    for cn in cn0 cn1 cn2; do
        for nqn in $all_nqns; do
            if [ -n "${wanted_trsvcid[$cn:$nqn]:-}" ]; then
                create_target "$cn" "${CN_IP[$cn]}" "$nqn" "${wanted_trsvcid[$cn:$nqn]}"
            else
                remove_target "$cn" "${CN_IP[$cn]}" "$nqn"
            fi
        done
    done
}

do_verify() {
    _info "--- verifying cn targets from $CDC_FILE ---"
    local nqn traddr trsvcid cn rc=0
    while IFS=$'\t' read -r nqn traddr trsvcid; do
        [ -n "$nqn" ] || continue
        cn=$(ip_to_cn "$traddr")
        [ -n "$cn" ] || { _warn "no cn matches traddr $traddr -- skipping"; continue; }
        verify_target "$cn" "${CN_IP[$cn]}" "$nqn" "$trsvcid" || rc=1
    done < <(_list_file_ports)
    [ "$rc" -eq 0 ] || exit 1
}

main() {
    local action="${1:-all}"
    command -v jq >/dev/null 2>&1 || _fail "jq is required (not found in PATH)"
    [ -f "$CDC_FILE" ] || _fail "CDC file not found: $CDC_FILE"
    case "$action" in
        all)    do_all ;;
        sync)   do_sync ;;
        verify) do_verify ;;
        *) echo "Usage: $0 [all|sync|verify]" >&2; exit 1 ;;
    esac
    _info "setup_targets.sh ($action) finished"
}

main "$@"