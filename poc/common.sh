#!/bin/bash
#
# common.sh -- shared library for the distributed block storage POC (t04).
#
# Sourced once by setup.sh / teardown.sh / failover.sh.  It defines:
#
#   Part A -- Resource API
#       High-level create_*/delete_* primitives.  Each takes a node name and IP
#       as its first arguments and SSHes to that node internally, so the
#       orchestrator reads like a recipe:
#           create_pd    dn0 192.168.122.48
#           create_vd    dn0 192.168.122.48 cn0 192.168.122.125 0 0 0   # vd0 leg0 grp0
#           create_grp   cn0 192.168.122.125 da0 0 0 <hnqn> <hid>
#           ...
#
#   Part B -- Infrastructure helpers
#       Low-level functions (dm_create, cfg_set, nvmet_add_subsys, rd_ok, ...)
#       that run ON the remote node.  They are uploaded to each node verbatim
#       by _emit_common() and invoked by the Part A functions.
#
# Sizes (SEC_LD, SEC_RMETA, ...) and NQN_PREFIX / NVME_PORT are constants
# defined once here, not per-call params.
#
# Terminology (t04):
#   pd   -- physical disk (loop bdev on a dn)
#   vd   -- virtual disk, dn->cn via nvmeof
#   grp  -- thin-pool underlying group (raid1 slice pair)
#   leg  -- raid0 underlying disk
#   side -- raid1 underlying vd
#   da   -- disk array (container, N legs, thin pools, snaps)
#   snap -- snapshot id in a da (shared across all legs)
#   exp  -- exporter (raid0 of snap thin-devs + nvmeof export)
#   dn   -- disk node   (linux server with physical disks)
#   cn   -- controller node (linux server exporting nvmet targets to nvme hosts)
#

# ===================== configuration ========================================
HOST0_IP="192.168.122.193"
DN0_IP="192.168.122.48"
DN1_IP="192.168.122.70"
CN0_IP="192.168.122.125"
CN1_IP="192.168.122.229"
REF0_IP="192.168.122.78"
SSH_USER="yupeng"

NQN_PREFIX="nqn.2026-07.org.dnv"
NQN_EXP="${NQN_PREFIX}:da:da0:snap0:exp0"
NQN_REF_ANCHOR="${NQN_PREFIX}:ref:ref0-anchor"
NQN_DISCOVERY="nqn.2014-08.org.nvmexpress.discovery"
HOSTNQN_CN0="${NQN_PREFIX}:host:cn0"
HOSTNQN_CN1="${NQN_PREFIX}:host:cn1"
HOSTNQN_HOST0="${NQN_PREFIX}:host:host0"

HOSTID_CN0="a0000000-0000-4000-8000-000000000000"
HOSTID_CN1="b0000000-0000-4000-8000-000000000000"
HOSTID_HOST0="c0000000-0000-4000-8000-000000000000"

NVME_PORT=4420
EXP_UUID="cafe0000-0000-4000-8000-000000000023"
EXP_SERIAL="DNVDA0SNAP0EXP0"
EXP_MODEL="dnv-da"
VD_MODEL="dnv-vd"

# All sizes in 512-byte sectors.
SEC_PD=1024000           # 500M  - one virtual disk on a dn
SEC_RMETA=8192           #   4M  - dm-raid1 metadata side
SEC_RDATA=1015808        # 496M  - dm-raid1 data side               (500M - 4M)
SEC_TMETA=16384          #   8M  - thin-pool metadata slice per group
SEC_TDATA=999424         # 488M  - thin-pool data slice per group   (500M - 4M - 8M)
SEC_POOL_META=32768      #  16M  - concat of 2 groups' thinmeta
SEC_POOL_DATA=1998848    # 976M  - concat of 2 groups' thindata
SEC_SNAP=2097152         #   1G  - dm-thin virtual size (per leg)
SEC_EXP=4194304          #   2G  - raid0 over leg0+leg1 snap0, and the
                         #         error/delay stack that mirrors its size

DELAY_MS=3600000         # 3600s, applied to read+write+flush
POOL_BLOCK_SECTORS=128   #  64K thin-pool allocation block
RAID_REGION_SECTORS=128
RAID0_CHUNK_SECTORS=128

LOOP_IMG_SIZE="4G"
STAS_DISCONNECT_WAIT=60

# ===================== local helpers =========================================
_ssh() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
              -o LogLevel=ERROR -o ConnectTimeout=10 "$@"; }
_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

# Variable block every remote script starts with (values substituted locally).
_emit_vars() {
    cat <<EOF
NQN_PREFIX='$NQN_PREFIX'
NQN_EXP='$NQN_EXP'
NQN_REF_ANCHOR='$NQN_REF_ANCHOR'
NQN_DISCOVERY='$NQN_DISCOVERY'
HOSTNQN_CN0='$HOSTNQN_CN0'
HOSTNQN_CN1='$HOSTNQN_CN1'
HOSTNQN_HOST0='$HOSTNQN_HOST0'
HOSTID_CN0='$HOSTID_CN0'
HOSTID_CN1='$HOSTID_CN1'
HOSTID_HOST0='$HOSTID_HOST0'
HOST0_IP='$HOST0_IP'
DN0_IP='$DN0_IP'
DN1_IP='$DN1_IP'
CN0_IP='$CN0_IP'
CN1_IP='$CN1_IP'
REF0_IP='$REF0_IP'
NVME_PORT='$NVME_PORT'
EXP_UUID='$EXP_UUID'
EXP_SERIAL='$EXP_SERIAL'
EXP_MODEL='$EXP_MODEL'
VD_MODEL='$VD_MODEL'
SEC_PD=$SEC_PD
SEC_RMETA=$SEC_RMETA
SEC_RDATA=$SEC_RDATA
SEC_TMETA=$SEC_TMETA
SEC_TDATA=$SEC_TDATA
SEC_POOL_META=$SEC_POOL_META
SEC_POOL_DATA=$SEC_POOL_DATA
SEC_SNAP=$SEC_SNAP
SEC_EXP=$SEC_EXP
DELAY_MS=$DELAY_MS
POOL_BLOCK_SECTORS=$POOL_BLOCK_SECTORS
RAID_REGION_SECTORS=$RAID_REGION_SECTORS
RAID0_CHUNK_SECTORS=$RAID0_CHUNK_SECTORS
LOOP_IMG_SIZE='$LOOP_IMG_SIZE'
STAS_DISCONNECT_WAIT=$STAS_DISCONNECT_WAIT
EOF
}

# =========================================================================
# Part B -- Infrastructure helpers (run ON the remote node; quoted heredoc)
# =========================================================================
# Uploaded verbatim to every node.  Nothing here is expanded locally.
_emit_common() {
    cat <<'EOF_COMMON'
NVMET=/sys/kernel/config/nvmet
SLOW_LIMIT_MS=3000
SLOW_LIST=/tmp/dnv-slow.list
VERIFY_FAIL=0
: > "$SLOW_LIST"

_info() { echo "[INFO] $*"; }
_warn() { echo "[WARN] $*" >&2; }
_ok()   { echo "[ OK ] $*"; }
_fail() { echo "[FAIL] $*" >&2; exit 1; }

# Time a command; flag anything slower than 3s (per the "no step over 3s" rule).
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

# --- node preparation -------------------------------------------------------
# The udev rule is essential.
#
#  * dm side: without it systemd-udevd workers block forever in D-state probing
#    dm-delay devices, hold them open and make removal impossible.  OPTIONS+=
#    "ignore_device" does NOT work on modern systemd; the DM_UDEV_* flags do.
#    It must sort after 55-dm.rules (which sets DM_NAME) and before
#    60-persistent-storage-dm.rules.
#  * nvme side: cn1's virtual disks are backed by a 3600s dm-delay on the DN,
#    so blkid on those namespaces would wedge a udev worker too.  Matching on
#    the parent controller's subsysnqn/model keeps the rule to our devices.
#
# NOTE: the rule alone is not enough for nvme.  The kernel itself runs
# nvme_partition_scan_work on every new namespace, which reads sector 0 without
# going through udev.  Setup therefore also disarms the DN delay devices while
# cn1 connects (see disarm_vd_delay / arm_vd_delay).
prep_node() {
    sudo modprobe -a dm-mod dm-delay dm-thin-pool dm-raid >/dev/null 2>&1 || true
    sudo modprobe -a nvmet nvmet-tcp nvme-tcp nvme-fabrics >/dev/null 2>&1 || true
    # lvm2 userland is needed on every node because the DN-side -real devices
    # are LVM LVs (note.md §6.1); prep_node runs on all 6 nodes, and a fresh VM
    # may not have lvm2 installed. Mirrors stas_install's dpkg-check pattern.
    if ! dpkg -s lvm2 >/dev/null 2>&1; then
        _info "installing lvm2"
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y lvm2 >/dev/null 2>&1 \
            || _warn "could not install lvm2 (need it for DN-side -real LVs)"
    fi
    mountpoint -q /sys/kernel/config 2>/dev/null || \
        sudo mount -t configfs none /sys/kernel/config >/dev/null 2>&1 || true
    sudo tee /etc/udev/rules.d/58-dnv-test.rules >/dev/null <<'RULE_EOF'
# distributed-nvme failover test: keep udev from probing our block devices.
# Probing a device backed by a 3600s dm-delay wedges the udev worker in D-state.
ACTION=="remove", GOTO="dnv_end"
SUBSYSTEM!="block", GOTO="dnv_end"
KERNEL=="dm-[0-9]*", ENV{DM_NAME}=="dnv-*", GOTO="dnv_hit"
KERNEL=="nvme[0-9]*", ATTRS{subsysnqn}=="nqn.2026-07.org.dnv:*", GOTO="dnv_hit"
KERNEL=="nvme[0-9]*", ATTRS{model}=="dnv-*", GOTO="dnv_hit"
GOTO="dnv_end"
LABEL="dnv_hit"
ENV{DM_UDEV_DISABLE_SUBSYSTEM_RULES_FLAG}="1"
ENV{DM_UDEV_DISABLE_DISK_RULES_FLAG}="1"
ENV{DM_UDEV_DISABLE_OTHER_RULES_FLAG}="1"
ENV{UDEV_DISABLE_PERSISTENT_STORAGE_RULES_FLAG}="1"
ENV{.DM_NOSCAN}="1"
ENV{SYSTEMD_READY}="0"
OPTIONS+="nowatch"
LABEL="dnv_end"
RULE_EOF
    sudo udevadm control --reload-rules >/dev/null 2>&1 || true
}

set_host_identity() {   # <hostnqn> <hostid>
    sudo mkdir -p /etc/nvme
    echo "$1" | sudo tee /etc/nvme/hostnqn >/dev/null
    echo "$2" | sudo tee /etc/nvme/hostid  >/dev/null
}

# --- device mapper ----------------------------------------------------------
dm_exists() { sudo dmsetup info "$1" >/dev/null 2>&1; }
dm_state()  { sudo dmsetup info "$1" 2>/dev/null | awk -F': *' '/^State:/{print $2}'; }
dm_size()   { sudo dmsetup table "$1" 2>/dev/null | awk '{s+=$2} END{print s+0}'; }
dm_target() { sudo dmsetup table "$1" 2>/dev/null | awk 'NR==1{print $3}'; }
dnv_list()  { sudo dmsetup ls 2>/dev/null | awk '{print $1}' | grep '^dnv-' || true; }
dm_count()  { sudo dmsetup ls 2>/dev/null | grep -v 'No devices found' | grep -c . || true; }

# --- lvm (logical volume manager) ------------------------------------------
# Only the DN-side -real devices (backed by the loop) are LVM-managed; every
# other dm device in this stack is plain dmsetup. These helpers mirror dm_exists
# etc. so create_*/delete_* stay no-op-on-match (note.md §13 idempotency rule).
#
# CRITICAL: every LVM command runs through _lvm(), which injects a device filter
# that accepts ONLY /dev/loop* (the PVs) and rejects everything else. Without
# this, pvs/vgs/lvs do a full block-device scan for PV labels; when that scan
# reaches a dm-delay (3600s) device created by create_vd itself, the read blocks
# for an hour and wedges every subsequent LVM call. The filter makes the scan
# touch only the loop PV, which carries the full VG+LV metadata -- the dm devices
# (LVs, dm-error, dm-delay, dm-linear) are never probed. Use loop-only devices
# for PVs; do not use a partition or md device as a PV under this filter.
_lvm() {
    local cmd="$1"; shift
    sudo "$cmd" --config 'devices { filter = ["a|^/dev/loop|", "r|.*|"] }' "$@"
}
pv_exists() { _lvm pvs  --noheadings "$1"            >/dev/null 2>&1; }   # <pv_dev>
vg_exists() { _lvm vgs  --noheadings "$1"            >/dev/null 2>&1; }   # <vg_name>
lv_exists() { _lvm lvs  --noheadings "/dev/$1/$2"    >/dev/null 2>&1; }   # <vg> <lv>
# Is a dm node (as listed by dnv_list) backed by LVM? Used by dnv_remove_all_dm
# to route LVM LVs to lvremove instead of dmsetup remove (which would leave the
# VG metadata stale). dmsetup info reports the owning VG name for LVM devices.
lv_is_lvm() { sudo dmsetup info -c --noheadings -o vg_name "$1" 2>/dev/null | grep -q .; }

# dm_create <name> <table>   -- table may be multi-line (concatenated targets)
dm_create() {
    local name="$1" table="$2"
    if dm_exists "$name"; then _info "exists : $name"; return 0; fi
    printf '%s\n' "$table" | _t "dmsetup create $name" \
        sudo dmsetup create --noudevsync "$name" || _fail "dmsetup create $name"
    sudo dmsetup mknodes "$name" >/dev/null 2>&1 || true
    _info "created: $name"
}

# dm_reload <name> <table>  -- suspend, swap the table, resume.
# --nolockfs/--noflush: there is never a filesystem on these devices and a
# flushing suspend would block forever if the old table points at a dm-delay.
dm_reload() {
    local name="$1" table="$2"
    dm_exists "$name" || { _warn "no such dm device: $name"; return 1; }
    if [ "$(dm_state "$name")" != "SUSPENDED" ]; then
        _t "suspend $name" sudo dmsetup suspend --nolockfs --noflush --noudevsync "$name" \
            || { _warn "suspend failed: $name"; return 1; }
    fi
    printf '%s\n' "$table" | _t "load $name" sudo dmsetup load "$name" \
        || { _warn "load failed: $name"; sudo dmsetup resume --noudevsync "$name"; return 1; }
    _t "resume $name" sudo dmsetup resume --noudevsync "$name" \
        || { _warn "resume failed: $name"; return 1; }
    _info "reloaded: $name -> $(sudo dmsetup table "$name" | head -1)"
}

# dm_remove <name> [force]
dm_remove() {
    local name="$1" mode="${2:-normal}"
    dm_exists "$name" || return 0
    dm_defuse_delay "$name"
    # A suspended device cannot be removed; make sure it is live first.
    if [ "$(dm_state "$name")" = "SUSPENDED" ]; then
        sudo dmsetup resume --noudevsync "$name" >/dev/null 2>&1 || true
    fi
    if _t "remove $name" sudo dmsetup remove --noudevsync "$name" >/dev/null 2>&1; then
        _info "removed: $name"; return 0
    fi
    [ "$mode" = "force" ] || return 1
    if _t "remove --force $name" sudo dmsetup remove --force --noudevsync "$name" >/dev/null 2>&1; then
        _warn "removed (forced): $name"; return 0
    fi
    if sudo dmsetup remove --deferred --noudevsync "$name" >/dev/null 2>&1; then
        _warn "deferred removal scheduled: $name"; return 0
    fi
    return 1
}

# Turn a dm-delay device into a dm-error device.
#
#   suspend --nolockfs --noflush runs delay_presuspend(), which issues every
#   pending delayed bio immediately instead of waiting out the delay -- this is
#   what releases readers stuck in D-state (including udev workers and, on a DN,
#   nvmet requests that would otherwise block controller teardown).
#   Loading an error table then drops the reference on the backing device, and
#   resume lets any newly queued I/O fail fast rather than block.
#
# Skipped when the device is already suspended, because suspending an
# already-suspended device is a no-op and would not flush anything.
dm_defuse_delay() {
    local name="$1" size
    [ "$(dm_target "$name")" = "delay" ] || return 0
    size=$(dm_size "$name")
    [ "$size" -gt 0 ] 2>/dev/null || return 0
    if [ "$(dm_state "$name")" != "SUSPENDED" ]; then
        _t "suspend $name" sudo dmsetup suspend --nolockfs --noflush --noudevsync "$name" \
            >/dev/null 2>&1 || true
    fi
    sudo dmsetup load "$name" --table "0 $size error" >/dev/null 2>&1 || true
    _t "resume $name" sudo dmsetup resume --noudevsync "$name" >/dev/null 2>&1 || true
    _info "defused dm-delay: $name"
}

# Defuse every dnv-* dm-delay on this node and resume anything left suspended,
# so no I/O is parked anywhere before the real teardown starts.
dnv_defuse_all() {
    local name n=0
    for name in $(dnv_list); do
        if [ "$(dm_target "$name")" = "delay" ]; then dm_defuse_delay "$name"; n=$((n+1)); fi
    done
    for name in $(dnv_list); do
        [ "$(dm_state "$name")" = "SUSPENDED" ] || continue
        _t "resume(suspended) $name" sudo dmsetup resume --noudevsync "$name" >/dev/null 2>&1 || true
        _info "resumed suspended device: $name"
    done
    _info "defused $n dm-delay device(s)"
}

# Remove every dnv-* dm device. Order-independent: defuse all delays first,
# then make repeated passes so stacked devices come off as their holders
# disappear, escalating to --force only once plain removal stops making progress.
#
# LVM LVs (the DN-side -real devices) are SKIPPED in the dm passes: removing an
# LV via `dmsetup remove` would drop its dm node but leave the VG metadata
# stale. They are torn down in a dedicated LVM sweep at the end (lvremove ->
# vgremove -> pvremove), after the dm stack holding them open is gone.
dnv_remove_all_dm() {
    local name pass before after mode vg
    dnv_defuse_all
    # Pass 1..N: plain dmsetup devices only. Skip anything LVM-managed.
    mode=normal
    for pass in 1 2 3 4 5 6 7 8; do
        before=$(dnv_list | wc -l)
        [ "$before" -eq 0 ] && break
        for name in $(dnv_list); do
            lv_is_lvm "$name" && continue
            dm_remove "$name" "$mode" >/dev/null 2>&1 || true
        done
        after=$(dnv_list | wc -l)
        _info "dm pass $pass ($mode): $before -> $after remaining"
        [ "$after" -eq 0 ] && break
        [ "$after" -eq "$before" ] && mode=force
    done
    # LVM sweep: remove any surviving dnv-* VGs (lvremove -f clears all their
    # LVs), then PVs on loop devices. This is the safety net; the happy path
    # (delete_vd per-slice + delete_pd per-DN) already did this.
    for vg in $(_lvm vgs --noheadings -o vg_name 2>/dev/null | grep '^dnv-'); do
        _lvm lvremove -y -f "$vg" >/dev/null 2>&1 && _info "lvremoved all LVs in $vg" || true
        _lvm vgremove -y "$vg" >/dev/null 2>&1 && _info "vgremoved $vg" || _warn "could not vgremove $vg"
    done
    for pv in $(_lvm pvs --noheadings -o pv_name 2>/dev/null | grep '^/dev/loop'); do
        _lvm pvremove -y "$pv" >/dev/null 2>&1 && _info "pvremoved $pv" || true
    done
    if [ "$(dnv_list | wc -l)" -ne 0 ]; then
        _warn "dm devices still present:"; dnv_list >&2
    else
        _ok "all dnv-* dm devices removed"
    fi
}

zero_head() {   # <devpath> <megabytes> -- buffered on purpose, dd's O_DIRECT is broken here
    local dev="$1" mb="${2:-4}"
    _t "zero $dev" sudo dd if=/dev/zero of="$dev" bs=1M count="$mb" conv=fsync status=none \
        || _warn "could not zero $dev"
}

# --- nvmet ------------------------------------------------------------------
# Idempotent configfs attribute write: skip if already at the wanted value,
# because re-writing an attribute of a live port/subsystem returns EBUSY.
# Trailing blanks are trimmed before comparing -- nvmet pads some attributes
# (attr_serial is space-padded to 20 chars), which would otherwise never compare
# equal and make every re-run attempt a doomed rewrite.
cfg_set() {
    local f="$1" v="$2" cur
    [ -e "$f" ] || return 0
    cur=$(sudo cat "$f" 2>/dev/null | head -1 | sed -e 's/[[:space:]]*$//' || true)
    [ "$cur" = "$(printf '%s' "$v" | sed -e 's/[[:space:]]*$//')" ] && return 0
    echo "$v" | sudo tee "$f" >/dev/null 2>&1 || _warn "could not set $f=$v"
}

# nvmet_add_subsys <nqn> <devpath> <uuid> <ana_grpid|""> [allowed host nqns...]
# Optional env: DNV_SERIAL DNV_MODEL DNV_CNTLID_MIN DNV_CNTLID_MAX
nvmet_add_subsys() {
    local nqn="$1" dev="$2" uuid="$3" ana="$4"; shift 4
    local s="$NVMET/subsystems/$nqn" h
    sudo mkdir -p "$s" || _fail "mkdir subsystem $nqn"
    [ -n "${DNV_SERIAL:-}" ]     && cfg_set "$s/attr_serial"     "$DNV_SERIAL"
    [ -n "${DNV_MODEL:-}" ]      && cfg_set "$s/attr_model"      "$DNV_MODEL"
    [ -n "${DNV_CNTLID_MIN:-}" ] && cfg_set "$s/attr_cntlid_min" "$DNV_CNTLID_MIN"
    [ -n "${DNV_CNTLID_MAX:-}" ] && cfg_set "$s/attr_cntlid_max" "$DNV_CNTLID_MAX"
    cfg_set "$s/attr_allow_any_host" "0"
    for h in "$@"; do
        sudo mkdir -p "$NVMET/hosts/$h" 2>/dev/null || true
        sudo ln -sfn "$NVMET/hosts/$h" "$s/allowed_hosts/$h" 2>/dev/null || true
    done
    sudo mkdir -p "$s/namespaces/1" || _fail "mkdir namespace for $nqn"
    if [ "$(sudo cat "$s/namespaces/1/enable" 2>/dev/null)" != "1" ]; then
        cfg_set "$s/namespaces/1/device_path"  "$dev"
        cfg_set "$s/namespaces/1/device_uuid"  "$uuid"
        cfg_set "$s/namespaces/1/device_nguid" "$uuid"
        [ -n "$ana" ] && cfg_set "$s/namespaces/1/ana_grpid" "$ana"
        _t "enable ns $nqn" sudo bash -c "echo 1 > '$s/namespaces/1/enable'" \
            || _fail "enable namespace for $nqn"
    fi
    _info "nvmet  : $nqn -> $dev"
}

nvmet_port() {   # <portnum> <ip> [ana_state]
    local p="$1" ip="$2" ana="${3:-}" d="$NVMET/ports/$1"
    sudo mkdir -p "$d" || _fail "mkdir port $p"
    cfg_set "$d/addr_trtype"  "tcp"
    cfg_set "$d/addr_adrfam"  "ipv4"
    cfg_set "$d/addr_traddr"  "$ip"
    cfg_set "$d/addr_trsvcid" "$NVME_PORT"
    [ -n "$ana" ] && cfg_set "$d/ana_groups/1/ana_state" "$ana"
    return 0
}

# Link a subsystem to a port. Must be a no-op when the link is already there:
# re-creating it on a live port tears the controllers down and stalls host I/O
# for as long as it takes the host to reconnect (observed: ~11s).
nvmet_link() {   # <nqn> <portnum>
    local l="$NVMET/ports/$2/subsystems/$1"
    { [ -L "$l" ] || [ -e "$l" ]; } && return 0
    sudo ln -s "$NVMET/subsystems/$1" "$l" 2>/dev/null || _warn "could not link $1 to port $2"
    return 0
}

# Add a referral to a port: a discovery log page entry of subtype "discovery
# subsystem referral" pointing a host at another discovery service.
#
# The address attributes must be written *before* enable=1: an enabled referral
# is a live port as far as nvmet is concerned and rejects attribute writes with
# EBUSY.  cfg_set skips writes whose value is already correct, so re-running this
# on an already-enabled referral touches nothing.
nvmet_referral() {   # <portnum> <name> <traddr> [remote portid, default 1]
    local port="$1" name="$2" ip="$3" pid="${4:-1}" d="$NVMET/ports/$1/referrals/$2"
    sudo mkdir -p "$d" || _fail "mkdir referral $name on port $port"
    cfg_set "$d/addr_trtype"  "tcp"
    cfg_set "$d/addr_adrfam"  "ipv4"
    cfg_set "$d/addr_traddr"  "$ip"
    cfg_set "$d/addr_trsvcid" "$NVME_PORT"
    cfg_set "$d/addr_portid"  "$pid"
    cfg_set "$d/enable"       "1"
    _info "referral: port $port/$name -> $ip:$NVME_PORT"
}

nvmet_remove_subsys() {   # <nqn>
    local nqn="$1" s n h p
    [ -d "$NVMET" ] || return 0
    for p in "$NVMET"/ports/*/subsystems/"$nqn"; do
        [ -L "$p" ] && { _t "unlink $p" sudo rm -f "$p" 2>/dev/null || true; }
    done
    s="$NVMET/subsystems/$nqn"
    [ -d "$s" ] || return 0
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
}

# Drop every referral a port publishes.  Writing 0 to enable (and dropping the
# config item) makes nvmet send a "discovery log changed" AEN to every host
# connected to that discovery service -- which is what makes host0 let go.
nvmet_remove_referrals() {   # <portnum>
    local d="$NVMET/ports/$1/referrals" r n=0
    [ -d "$d" ] || return 0
    for r in "$d"/*; do
        [ -d "$r" ] || continue
        sudo bash -c "echo 0 > '$r/enable'" >/dev/null 2>&1 || true
        if sudo rmdir "$r" >/dev/null 2>&1; then
            _info "removed referral: ${r##*/}"; n=$((n+1))
        else
            _warn "could not remove referral: ${r##*/}"
        fi
    done
    _info "port $1: $n referral(s) removed, $(ls "$d" 2>/dev/null | wc -l) left"
}

nvmet_remove_port() {     # <portnum>
    local d="$NVMET/ports/$1" g
    [ -d "$d" ] || return 0
    for g in "$d"/subsystems/*; do
        [ -L "$g" ] && { sudo rm -f "$g" 2>/dev/null || true; }
    done
    for g in "$d"/ana_groups/*; do
        [ -d "$g" ] || continue
        [ "$(basename "$g")" = "1" ] && continue   # group 1 is a default group
        sudo rmdir "$g" >/dev/null 2>&1 || true
    done
    if sudo rmdir "$d" >/dev/null 2>&1; then
        _info "removed nvmet port $1"
    else
        _warn "could not remove nvmet port $1"
    fi
}

nvmet_remove_host() {     # <hostnqn>
    [ -d "$NVMET/hosts/$1" ] || return 0
    sudo rmdir "$NVMET/hosts/$1" >/dev/null 2>&1 && _info "removed nvmet host: $1" || \
        _warn "could not remove nvmet host: $1"
}

# Remove every dnv VD subsystem on this node. Topology-agnostic safety net for
# teardown: scans $NVMET/subsystems/ for the dn:<dn>:da0-leg*-grp*-vd*-cn* prefix
# (which covers groups beyond the hardcoded grp0/grp1 base, e.g. grp2 added by
# grow.sh) and routes each to nvmet_remove_subsys. Called by delete_pd before
# nvmet_remove_port so the port's subsystem links are dropped first.
nvmet_remove_all_dnv_subsys() {
    local d nm cnt=0
    [ -d "$NVMET/subsystems" ] || return 0
    for d in "$NVMET"/subsystems/*; do
        [ -d "$d" ] || continue
        nm=$(basename "$d")
        case "$nm" in
            ${NQN_PREFIX}:dn:*) nvmet_remove_subsys "$nm"; cnt=$((cnt+1)) ;;
        esac
    done
    _info "removed $cnt dnv nvmet subsystem(s) via scan"
}

# --- nvme host side ---------------------------------------------------------
# Resolve a namespace block device from its subsystem NQN via sysfs. Deliberately
# does not rely on /dev/disk/by-id, whose symlinks come from udev (and which the
# 58-dnv-test.rules above suppresses on purpose).
nvme_dev_by_nqn() {
    local nqn="$1" s c n b
    for s in /sys/class/nvme-subsystem/nvme-subsys*; do
        [ -r "$s/subsysnqn" ] || continue
        [ "$(cat "$s/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        for n in "$s"/nvme*n*; do
            [ -e "$n" ] || continue
            b=${n##*/}
            [ -b "/dev/$b" ] && { echo "/dev/$b"; return 0; }
        done
    done
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        for n in "$c"/nvme*n*; do
            [ -e "$n" ] || continue
            b=${n##*/}
            [ -b "/dev/$b" ] && { echo "/dev/$b"; return 0; }
        done
    done
    return 1
}

nvme_wait_dev() {   # <nqn> [deciseconds, default 100 = 10s]
    local nqn="$1" tries="${2:-100}" i d
    for (( i = 0; i < tries; i++ )); do
        d=$(nvme_dev_by_nqn "$nqn") && { echo "$d"; return 0; }
        sleep 0.1
    done
    return 1
}

nvme_conn() {   # <ip> <nqn> <hostnqn> <hostid>
    local ip="$1" nqn="$2" hnqn="$3" hid="$4"
    if nvme_dev_by_nqn "$nqn" >/dev/null 2>&1; then
        _info "connected already: $nqn"; return 0
    fi
    _t "nvme connect $nqn @ $ip" sudo nvme connect -t tcp -a "$ip" -s "$NVME_PORT" \
        -n "$nqn" --hostnqn "$hnqn" --hostid "$hid" >/dev/null 2>&1 || true
}

nvme_disc() {             # <nqn>
    sudo nvme list-subsys 2>/dev/null | grep -q "NQN=$1" || return 0
    _t "nvme disconnect $1" sudo nvme disconnect -n "$1" >/dev/null 2>&1 || true
    _info "disconnected: $1"
}

# Disconnect every dnv VD connection this host holds. Topology-agnostic safety
# net for teardown: scans /sys/class/nvme/*/subsysnqn for NQNs matching
# ${NQN_PREFIX}:dn:* (which covers groups beyond the hardcoded grp0/grp1 base,
# e.g. grp2 added by grow.sh) and routes each to nvme_disc. Replaces the
# hardcoded for-leg/for-grp/for-d loop in teardown_cn's inline cleanup.
nvme_disconnect_all_dnv_vd() {
    local c nqn cnt=0
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        nqn=$(cat "$c/subsysnqn" 2>/dev/null) || continue
        [ -n "$nqn" ] || continue
        case "$nqn" in
            ${NQN_PREFIX}:dn:*) nvme_disc "$nqn"; cnt=$((cnt+1)) ;;
        esac
    done
    _info "disconnected $cnt dnv vd connection(s) via scan"
}

# How many controllers (paths) this host has for a given subsystem NQN.
nvme_path_count() {       # <nqn>
    local c n=0
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$1" ] || continue
        n=$(( n + 1 ))
    done
    echo "$n"
}

# How many controllers this host has for <nqn> reachable at <traddr>.
ctrl_count() {  # <nqn> <traddr|"">
    local nqn="$1" ip="${2:-}" c n=0
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        if [ -n "$ip" ]; then
            case "$(cat "$c/address" 2>/dev/null)" in *"traddr=$ip,"*) ;; *) continue ;; esac
        fi
        n=$(( n + 1 ))
    done
    echo "$n"
}

ana_set() {     # <state>
    local f="$NVMET/ports/1/ana_groups/1/ana_state"
    [ -e "$f" ] || { _warn "no ANA group at $f"; return 1; }
    _t "ana_state=$1" sudo bash -c "echo '$1' > '$f'" || { _warn "could not set ANA $1"; return 1; }
    _info "ANA group 1 -> $(sudo cat "$f")"
}

# --- nvme-stas (host side automation) ---------------------------------------
STAS_DIR=/etc/stas

stas_install() {
    if dpkg -s nvme-stas >/dev/null 2>&1; then
        _info "nvme-stas already installed: $(dpkg-query -W -f='${Version}' nvme-stas 2>/dev/null)"
        return 0
    fi
    _info "installing nvme-stas"
    sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nvme-stas >/dev/null 2>&1 \
        || _fail "could not install nvme-stas"
    _info "installed nvme-stas: $(dpkg-query -W -f='${Version}' nvme-stas 2>/dev/null)"
}

# Point stafd at ref0 and nothing else.  The stock config files are kept as
# *.dnv-orig on the first run so teardown.sh can put them back.
stas_write_config() {
    local f
    sudo mkdir -p "$STAS_DIR"
    for f in stafd.conf stacd.conf; do
        [ -f "$STAS_DIR/$f" ] && [ ! -f "$STAS_DIR/$f.dnv-orig" ] && \
            sudo cp -a "$STAS_DIR/$f" "$STAS_DIR/$f.dnv-orig"
    done

    sudo tee "$STAS_DIR/stafd.conf" >/dev/null <<STAFD_EOF
# distributed-nvme failover test (t04) -- written by setup.sh.
# ref0 is the only discovery controller; cn0 and cn1 arrive as referrals.
[Global]
tron=false
ip-family=ipv4
ignore-iface=true

[Service Discovery]
zeroconf=disabled

[Discovery controller connection management]
persistent-connections=false

[Controllers]
controller=transport=tcp;traddr=${REF0_IP};trsvcid=${NVME_PORT}
STAFD_EOF

    sudo tee "$STAS_DIR/stacd.conf" >/dev/null <<STACD_EOF
# distributed-nvme failover test (t04) -- written by setup.sh.
# Every I/O controller comes from a discovery log page entry; none is hard-coded,
# so removing a target from ref0 makes stacd disconnect it again.
[Global]
tron=false
ip-family=ipv4
ignore-iface=true

[I/O controller connection management]
disconnect-scope=only-stas-connections
disconnect-trtypes=tcp

[Controllers]
STACD_EOF
    _info "wrote $STAS_DIR/stafd.conf (discovery controller: ${REF0_IP}:${NVME_PORT}) and $STAS_DIR/stacd.conf"
}

# stafd/stacd read the host NQN/ID through sys.conf's file:// references at
# startup, so they have to be (re)started once set_host_identity has changed
# them.  On a re-run the identity is already right and a restart would drop and
# rebuild every connection -- including the live volume paths -- so the config is
# reloaded (SIGHUP) instead.
stas_start() {
    local want_nqn cur_nqn
    want_nqn=$(cat /etc/nvme/hostnqn 2>/dev/null)
    if [ -n "$want_nqn" ] && sudo systemctl is-active --quiet stafd \
                          && sudo systemctl is-active --quiet stacd; then
        cur_nqn=$(sudo stafctl status 2>/dev/null | sed -n "s/.*'hostnqn': '\([^']*\)'.*/\1/p" | head -1)
        if [ "$cur_nqn" = "$want_nqn" ]; then
            _t "reload stafd/stacd" sudo systemctl reload stafd stacd >/dev/null 2>&1 || true
            _info "stafd/stacd already running as $cur_nqn - config reloaded, connections kept"
            return 0
        fi
    fi
    _t "restart stafd/stacd" sudo systemctl restart stafd stacd \
        || _fail "could not start stafd/stacd"
    sudo systemctl is-active --quiet stafd || _fail "stafd is not running"
    sudo systemctl is-active --quiet stacd || _fail "stacd is not running"
    _info "stafd/stacd running with hostnqn $want_nqn"
}

# Stop nvme-stas and put its configuration back the way the package shipped it.
# persistent-connections=false means stopping stafd also drops its discovery
# connections, so nothing of ours is left on the host afterwards.
stas_stop_restore() {
    local f
    if ! dpkg -s nvme-stas >/dev/null 2>&1; then return 0; fi
    _t "stop stafd/stacd" sudo systemctl stop stacd stafd >/dev/null 2>&1 || true
    for f in stafd.conf stacd.conf; do
        if [ -f "/etc/stas/$f.dnv-orig" ]; then
            sudo mv -f "/etc/stas/$f.dnv-orig" "/etc/stas/$f" && _info "restored /etc/stas/$f"
        fi
    done
    _info "nvme-stas stopped (stafd: $(systemctl is-active stafd 2>/dev/null), stacd: $(systemctl is-active stacd 2>/dev/null))"
}

cleanup_node_common() {
    sudo rm -f /etc/udev/rules.d/58-dnv-test.rules 2>/dev/null || true
    sudo udevadm control --reload-rules >/dev/null 2>&1 || true
    sudo rm -f /etc/nvme/hostnqn /etc/nvme/hostid 2>/dev/null || true
    local stuck
    stuck=$(ps -eo pid,stat,comm 2>/dev/null | awk '$2 ~ /^D/ && $3 !~ /kworker/ {print $1":"$3}' | tr '\n' ' ')
    [ -n "$stuck" ] && _warn "processes still in uninterruptible sleep: $stuck"
    return 0
}

# --- read verification ------------------------------------------------------
# NOTE: dd here is uutils coreutils, whose iflag=direct/oflag=direct are broken
# (EINVAL on plain linear devices, silent zero-writes through raid0-over-thin).
# All verification therefore uses buffered I/O plus an explicit flushbufs.
rd_ok() {       # <path> [label] -- expect the read to succeed
    local p="$1" l="${2:-$1}"
    timeout 3 sudo blockdev --flushbufs "$p" >/dev/null 2>&1 || true
    if timeout 5 sudo dd if="$p" of=/dev/null bs=4096 count=1 status=none >/dev/null 2>&1; then
        _ok "read succeeds : $l"
    else
        _warn "read FAILED (expected success): $l"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
}

rd_eio() {      # <path> [label] -- expect the read to fail with EIO
    local p="$1" l="${2:-$1}"
    timeout 3 sudo blockdev --flushbufs "$p" >/dev/null 2>&1 || true
    if timeout 5 sudo dd if="$p" of=/dev/null bs=4096 count=1 status=none >/dev/null 2>&1; then
        _warn "read SUCCEEDED (expected EIO): $l"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    else
        _ok "read gives EIO: $l"
    fi
}

# rd_hang <read path> <dm-delay device name to release> [label]
# Starts a read that must block, confirms it is still blocked, then releases it
# and puts the delay device back exactly as it was.
#
# Releasing needs the error-table swap, not just suspend+resume: a --noflush
# suspend *requeues* the in-flight bios rather than completing them, so resuming
# onto the unchanged delay table simply re-delays them and the reader stays stuck.
# Swapping in an error table makes the requeued bios fail fast on resume.
rd_hang() {
    local p="$1" delay_dev="$2" l="${3:-$1}" pid orig size i
    orig=$(sudo dmsetup table "$delay_dev" 2>/dev/null)
    size=$(printf '%s\n' "$orig" | awk '{s+=$2} END{print s+0}')
    if [ -z "$orig" ] || [ "$size" -le 0 ] 2>/dev/null; then
        _warn "cannot read table of $delay_dev - skipping hang test"; VERIFY_FAIL=$((VERIFY_FAIL+1)); return 0
    fi

    sudo dd if="$p" of=/dev/null bs=4096 count=1 status=none >/dev/null 2>&1 &
    pid=$!
    sleep 1.5
    if kill -0 "$pid" 2>/dev/null; then
        _ok "read blocks   : $l"
    else
        _warn "read did NOT block (expected hang): $l"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi

    # release the blocked reader
    _t "defuse $delay_dev" sudo dmsetup suspend --nolockfs --noflush --noudevsync "$delay_dev" \
        >/dev/null 2>&1 || true
    sudo dmsetup load "$delay_dev" --table "0 $size error" >/dev/null 2>&1 || true
    _t "resume(error) $delay_dev" sudo dmsetup resume --noudevsync "$delay_dev" >/dev/null 2>&1 || true
    for (( i = 0; i < 50; i++ )); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        _warn "reader still blocked after release: $l"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
    wait "$pid" 2>/dev/null || true

    # put the delay table back so the device stays usable for failover testing
    sudo dmsetup suspend --nolockfs --noflush --noudevsync "$delay_dev" >/dev/null 2>&1 || true
    printf '%s\n' "$orig" | sudo dmsetup load "$delay_dev" >/dev/null 2>&1 || true
    _t "restore $delay_dev" sudo dmsetup resume --noudevsync "$delay_dev" >/dev/null 2>&1 || true
    if [ "$(sudo dmsetup table "$delay_dev" 2>/dev/null | awk 'NR==1{print $3}')" = "delay" ]; then
        _info "         restored dm-delay table on $delay_dev"
    else
        _warn "failed to restore delay table on $delay_dev"; VERIFY_FAIL=$((VERIFY_FAIL+1))
    fi
}

verify_summary() {
    if [ "$VERIFY_FAIL" -eq 0 ]; then
        _ok "all device checks passed"
    else
        _warn "$VERIFY_FAIL device check(s) did not behave as expected"
    fi
}
EOF_COMMON
}

# =========================================================================
# Part A -- Resource API (run LOCALLY; SSH to the target node internally)
# =========================================================================
#
# Naming convention (see plan.md section 3):
#   DN  dm:   dnv-<dn>-<da>-leg<leg>-grp<grp>-vd<vd>-cn<cn>
#             (+ -real, -err-<cn>, -delay-<cn>)
#   CN  dm:   dnv-<cn>-<da>-leg<leg>-grp<grp>-raid1-side<side>
#             (+ -meta-side<side>, -data-side<side>)
#   CN pool:  dnv-<cn>-<da>-leg<leg>-thinpool
#             (+ -thinmeta, -thindata, -grp<grp>-thinmeta, -grp<grp>-thindata)
#   CN exp:   dnv-<cn>-<da>-snap<id>-exp<id>
#             (+ -real, -error, -delay)
#   DN NQN:   nqn.2026-07.org.dnv:dn:<dn>:<da>-leg<leg>-grp<grp>-vd<vd>-cn<cn>
#   Host NQN: nqn.2026-07.org.dnv:da:<da>:snap<id>:exp<id>
#
# Disk array (da) defaults: da0, 2 legs, 2 grps per leg, 2 vds per grp (one per dn).
# vd_id encodes which dn it comes from: vd0 -> dn0, vd1 -> dn1.

# --- pd: physical disk (loop bdev on a dn) ----------------------------------
create_pd() {   # <dn_name> <dn_ip>
    local dn="$1" ip="$2"
    _info "=== create_pd $dn ($ip) ==="
    { _emit_vars; echo "DN='$dn'"; _emit_common; cat <<'EOF_PD'
set -uo pipefail
prep_node

# Backing file + loop device (the physical disk).
IMG="/var/tmp/dnv-${DN}-da0.img"
if [ ! -f "$IMG" ]; then
    _t "truncate $IMG" sudo truncate -s "$LOOP_IMG_SIZE" "$IMG" || _fail "create $IMG"
fi
LOOP=$(sudo losetup -j "$IMG" 2>/dev/null | awk -F: 'NR==1{print $1}')
if [ -z "$LOOP" ]; then
    LOOP=$(_t "losetup $IMG" sudo losetup -f --show "$IMG") || _fail "losetup $IMG"
fi
_info "loop device: $LOOP ($IMG, $LOOP_IMG_SIZE)"

# Volume group on the loop device. The DN-side -real devices (one per
# (leg,grp,vd) slice) are LVM LVs in this VG, not plain dm-linear on the loop.
# One loop -> one VG -> N LVs mirrors the old "one loop -> N dm-linear slices".
# Idempotent: pvcreate/vgcreate error if already present, so guard like dm_create.
VG="dnv-${DN}-da0-vg"
pv_exists "$LOOP" || { _t "pvcreate $LOOP" _lvm pvcreate "$LOOP" || _fail "pvcreate $LOOP"; }
vg_exists "$VG"   || { _t "vgcreate $VG" _lvm vgcreate "$VG" "$LOOP" || _fail "vgcreate $VG"; }
_info "VG: $VG on $LOOP ($LOOP_IMG_SIZE)"

# nvmet port for the DN's vd exports (the DN's own IP).
case "$DN" in
    dn0) MY_IP="$DN0_IP" ;;
    dn1) MY_IP="$DN1_IP" ;;
esac
nvmet_port 1 "$MY_IP"

_info "$(sudo dmsetup ls 2>/dev/null | wc -l) dm devices"
slow_summary
_info "PD ${DN} setup complete"
EOF_PD
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_pd() {   # <dn_name> <dn_ip>
    local dn="$1" ip="$2"
    _info "=== delete_pd $dn ($ip) ==="
    { _emit_vars; echo "DN='$dn'"; _emit_common; cat <<'EOF_DPD'
set -uo pipefail
dnv_remove_all_dm

# Remove the VG and PV on the loop. dnv_remove_all_dm above already swept
# strays, but run it here too so delete_pd is self-sufficient when run alone.
VG="dnv-${DN}-da0-vg"
vg_exists "$VG" && {
    _lvm lvremove -y -f "$VG" >/dev/null 2>&1 || true
    _t "vgremove $VG" _lvm vgremove -y "$VG" >/dev/null 2>&1 && _info "vgremoved $VG" || _warn "could not vgremove $VG"
}

# Detach the loop device and delete its backing file.
IMG="/var/tmp/dnv-${DN}-da0.img"
for LOOP in $(sudo losetup -j "$IMG" 2>/dev/null | awk -F: '{print $1}'); do
    pv_exists "$LOOP" && _lvm pvremove -y "$LOOP" >/dev/null 2>&1 || true
    _t "losetup -d $LOOP" sudo losetup -d "$LOOP" >/dev/null 2>&1 && _info "detached $LOOP" \
        || _warn "could not detach $LOOP"
done
sudo rm -f "$IMG" 2>/dev/null || true

# Drop the nvmet port + hosts on this DN.  nvmet_remove_all_dnv_subsys runs
# first so the port's subsystem links are unlinked before the port goes away;
# it is the topology-agnostic safety net that catches VD subsystems beyond the
# hardcoded grp0/grp1 base (e.g. grp2 added by grow.sh) that delete_vd's
# explicit per-(leg,grp) loop would otherwise miss.
nvmet_remove_all_dnv_subsys
nvmet_remove_port 1
nvmet_remove_host "$HOSTNQN_CN0"
nvmet_remove_host "$HOSTNQN_CN1"

cleanup_node_common
_info "remaining dm devices: $(dm_count)"
slow_summary
_info "PD ${DN} teardown complete"
EOF_DPD
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- vd: virtual disk (dn->cn via nvmeof, full symmetric fault-injection) ---
# One vd_id covers a (dn, cn) pair: vd_id is the dn index (0 or 1), cn is the
# cn index.  So create_vd dn0 ... cn0 ... 0 0 0  creates vd0 on dn0 exported to
# cn0, for leg 0 grp 0.  leg/grp are explicit params so grow.sh can add grp2 to
# a running pool without touching the existing (leg,grp) slices.
create_vd() {   # <dn_name> <dn_ip> <cn_name> <cn_ip> <vd_id> <leg> <grp>
    local dn="$1" ip="$2" cn="$3" cn_ip="$4" vid="$5" leg="$6" grp="$7"
    _info "=== create_vd $dn -> $cn (vd$vid leg$leg grp$grp) ==="
    { _emit_vars; echo "DN='$dn'; CN='$cn'; VD='$vid'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_VD'
set -uo pipefail
prep_node

# Derive this DN's own IP and the cn index (0/1) for naming.
case "$DN" in
    dn0) MY_IP="$DN0_IP" ;;
    dn1) MY_IP="$DN1_IP" ;;
esac
case "$CN" in
    cn0) C=0 ;;
    cn1) C=1 ;;
esac

nvmet_port 1 "$MY_IP"

# The DN-side -real devices are LVM LVs in dnv-<dn>-da0-vg (create_pd set up
# the loop/PV/VG in a prior call). Fail clearly if create_pd wasn't run.
VG="dnv-${DN}-da0-vg"
vg_exists "$VG" || _fail "no VG $VG on $DN -- run create_pd first"

# Create this (leg, grp) pair's full symmetric fault-injection stack.
# Names (t04):  dnv-<dn>-<da>-leg<leg>-grp<grp>-vd<vd>  (base, no -cn suffix)
#                + -real (LVM LV in dnv-<dn>-da0-vg, NOT a dmsetup device),
#                + -err-cn0, -err-cn1, -delay-cn0, -delay-cn1
#                + -cn0 (exported to cn0, on -real or -delay-cn0)
#                + -cn1 (exported to cn1, on -delay-cn1 or -real)
# NQN:  nqn.2026-07.org.dnv:dn:<dn>:<da>-leg<leg>-grp<grp>-vd<vd>-cn<cn>
#
# -real is the ONLY LVM-managed object in the whole stack; everything above it
# (-err/-delay/-cn) stays plain dmsetup, stacked on the LV path /dev/<vg>/<lv>.
base="dnv-${DN}-da0-leg${LEG}-grp${GRP}-vd${VD}"
LV="leg${LEG}-grp${GRP}-vd${VD}-real"
REAL_DEV="/dev/${VG}/${LV}"

# Backing store for this virtual disk slice: an LVM LV in the loop-backed VG.
# Idempotent: lvcreate errors if the LV exists, so guard like dm_create.
# Size in sectors (SEC_PD s suffix); SEC_PD=1024000 sectors = 500M = 125 PEs.
lv_exists "$VG" "$LV" || \
    { _t "lvcreate $LV" _lvm lvcreate -y -L "${SEC_PD}s" -n "$LV" "$VG" \
        || _fail "lvcreate $VG/$LV"; }

# Per-CN fault-injection pair (symmetric: both cn0 and cn1 get -err/-delay).
for cn in cn0 cn1; do
    dm_create "${base}-err-${cn}"   "0 $SEC_PD error"
    dm_create "${base}-delay-${cn}" "0 $SEC_PD delay /dev/mapper/${base}-err-${cn} 0 $DELAY_MS"
done

# The device exported to THIS cn -- live path on -real (active) or parked
# on -delay (standby).  cn0 (active) sees real data; cn1 (standby) sees the
# 3600s delay so it cannot touch the data until failover.
if [ "$C" = "0" ]; then
    dm_create "${base}-cn${C}" "0 $SEC_PD linear $REAL_DEV 0"
else
    dm_create "${base}-cn${C}" "0 $SEC_PD linear /dev/mapper/${base}-delay-cn${C} 0"
fi

# nvmet subsystem for this cn, visible only to that cn's hostnqn.
nqn="${NQN_PREFIX}:dn:${DN}:da0-leg${LEG}-grp${GRP}-vd${VD}-cn${C}"
# UUID: hex <vd><dn><leg><grp><cn> + "00" tail.
uuid=$(printf '0%s%s%s%s%s00-0000-4000-8000-000000000000' "$VD" "${DN#dn}" "$LEG" "$GRP" "$C")
eval "hnqn=\$HOSTNQN_CN${C}"
DNV_MODEL="$VD_MODEL" \
    nvmet_add_subsys "$nqn" "/dev/mapper/${base}-cn${C}" "$uuid" "" "$hnqn"
nvmet_link "$nqn" 1

_info "$(sudo dmsetup ls | wc -l) dm devices, $(ls "$NVMET/subsystems" 2>/dev/null | wc -l) nvmet subsystems"
slow_summary
_info "VD ${DN}->${CN} (vd${VD} leg${LEG} grp${GRP}) setup complete"
EOF_VD
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_vd() {   # <dn_name> <dn_ip> <cn_name> <cn_ip> <vd_id> <leg> <grp>
    # cn/cn_ip are vestigial: the function removes subsystems for both cns
    # regardless. Kept for signature symmetry with create_vd; used only in the
    # log message.
    local dn="$1" ip="$2" cn="$3" cn_ip="$4" vid="$5" leg="$6" grp="$7"
    _info "=== delete_vd $dn -> $cn (vd$vid leg$leg grp$grp) ==="
    { _emit_vars; echo "DN='$dn'; CN='$cn'; VD='$vid'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_DVD'
set -uo pipefail
prep_node

# Remove the nvmet subsystems for BOTH cns (symmetric stack).
for c in 0 1; do
    nqn="${NQN_PREFIX}:dn:${DN}:da0-leg${LEG}-grp${GRP}-vd${VD}-cn${c}"
    nvmet_remove_subsys "$nqn"
done

# Remove the dm stack for this vd.  dnv_remove_all_dm would nuke everything on
# the dn; instead, remove just this vd's devices so a coexisting vd is untouched.
# -real is an LVM LV, not a dmsetup device, so it is removed via lvremove (the
# dm devices above it go first via dm_remove, releasing the LV's open count).
VG="dnv-${DN}-da0-vg"
base="dnv-${DN}-da0-leg${LEG}-grp${GRP}-vd${VD}"
for n in "${base}-cn0" "${base}-cn1" \
         "${base}-delay-cn0" "${base}-delay-cn1" \
         "${base}-err-cn0" "${base}-err-cn1"; do
    dm_remove "$n" force >/dev/null 2>&1 || true
done
LV="leg${LEG}-grp${GRP}-vd${VD}-real"
lv_exists "$VG" "$LV" && _lvm lvremove -y "/dev/${VG}/${LV}" >/dev/null 2>&1 \
    && _info "lvremoved: $VG/$LV" || true
_info "VD ${DN}->${CN} (vd${VD} leg${LEG} grp${GRP}) dm devices removed"

nvmet_remove_host "$HOSTNQN_CN0"
nvmet_remove_host "$HOSTNQN_CN1"
_info "remaining dm devices: $(dm_count)"
slow_summary
_info "VD ${DN}->${CN} (vd${VD} leg${LEG} grp${GRP}) teardown complete"
EOF_DVD
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- connect_vd / disconnect_vd (cn-side nvme connect to a dn's vd) ----------
connect_vd() {   # <cn_name> <cn_ip> <dn_name> <dn_ip> <vd_id> <hostnqn> <hostid> <leg> <grp>
    local cn="$1" cn_ip="$2" dn="$3" dn_ip="$4" vid="$5" hnqn="$6" hid="$7" leg="$8" grp="$9"
    _info "=== connect_vd $cn <- $dn (vd$vid leg$leg grp$grp) ==="
    { _emit_vars; echo "CN='$cn'; DN='$dn'; DN_IP='$dn_ip'; VD='$vid'; HNQN='$hnqn'; HID='$hid'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_CV'
set -uo pipefail
nqn="${NQN_PREFIX}:dn:${DN}:da0-leg${LEG}-grp${GRP}-vd${VD}-${CN}"
nvme_conn "$DN_IP" "$nqn" "$HNQN" "$HID"
slow_summary
_info "connect_vd ${CN}<-${DN} (vd${VD} leg${LEG} grp${GRP}) complete"
EOF_CV
    } | _ssh "${SSH_USER}@${cn_ip}" bash -s
}

disconnect_vd() {   # <cn_name> <cn_ip> <dn_name> <dn_ip> <vd_id> <leg> <grp>
    local cn="$1" cn_ip="$2" dn="$3" dn_ip="$4" vid="$5" leg="$6" grp="$7"
    _info "=== disconnect_vd $cn <- $dn (vd$vid leg$leg grp$grp) ==="
    { _emit_vars; echo "CN='$cn'; DN='$dn'; VD='$vid'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_DV'
set -uo pipefail
nqn="${NQN_PREFIX}:dn:${DN}:da0-leg${LEG}-grp${GRP}-vd${VD}-${CN}"
nvme_disc "$nqn"
slow_summary
_info "disconnect_vd ${CN}<-${DN} (vd${VD} leg${LEG} grp${GRP}) complete"
EOF_DV
    } | _ssh "${SSH_USER}@${cn_ip}" bash -s
}

# --- grp: raid1 slice pair + thinmeta/thindata (cn-side, leaf) --------------
# create_grp connects both vds (vd0 from dn0, vd1 from dn1) internally, then
# builds the raid1 mirror + thinmeta/thindata slices.
create_grp() {   # <cn_name> <cn_ip> <da> <leg> <grp> <hostnqn> <hostid>
    local cn="$1" ip="$2" da="$3" leg="$4" grp="$5" hnqn="$6" hid="$7"
    _info "=== create_grp $cn $da leg$leg grp$grp ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; LEG='$leg'; GRP='$grp'; HNQN='$hnqn'; HID='$hid'"; _emit_common; cat <<'EOF_GRP'
set -uo pipefail
prep_node
# Host identity is set by create_cntlr_active; re-affirm in case this is called standalone.
set_host_identity "$HNQN" "$HID"

# Connect the two vds for this (leg,grp): vd0 from dn0, vd1 from dn1.
nvme_conn "$DN0_IP" "${NQN_PREFIX}:dn:dn0:${DA}-leg${LEG}-grp${GRP}-vd0-${CN}" "$HNQN" "$HID"
nvme_conn "$DN1_IP" "${NQN_PREFIX}:dn:dn1:${DA}-leg${LEG}-grp${GRP}-vd1-${CN}" "$HNQN" "$HID"

# Resolve the block devices.
declare -A VDDEV
for d in 0 1; do
    nqn="${NQN_PREFIX}:dn:dn${d}:${DA}-leg${LEG}-grp${GRP}-vd${d}-${CN}"
    dev=$(nvme_wait_dev "$nqn") || _fail "no block device for $nqn"
    VDDEV["$d"]="$dev"
    _info "nvme   : $nqn -> $dev"
done

p="dnv-${CN}-${DA}-leg${LEG}-grp${GRP}"
ld0="${VDDEV[0]}"
ld1="${VDDEV[1]}"

# Split each side into a raid1 metadata and data area.
dm_create "${p}-raid1-meta-side0" "0 $SEC_RMETA linear $ld0 0"
dm_create "${p}-raid1-data-side0" "0 $SEC_RDATA linear $ld0 $SEC_RMETA"
dm_create "${p}-raid1-meta-side1" "0 $SEC_RMETA linear $ld1 0"
dm_create "${p}-raid1-data-side1" "0 $SEC_RDATA linear $ld1 $SEC_RMETA"

# dm-raid1 across both sides. Metadata areas must be zeroed or dm-raid will
# try to interpret a stale superblock.
if ! dm_exists "${p}-raid1"; then
    zero_head "/dev/mapper/${p}-raid1-meta-side0" 4
    zero_head "/dev/mapper/${p}-raid1-meta-side1" 4
fi
dm_create "${p}-raid1" \
"0 $SEC_RDATA raid raid1 4 0 region_size $RAID_REGION_SECTORS nosync 2 \
/dev/mapper/${p}-raid1-meta-side0 /dev/mapper/${p}-raid1-data-side0 \
/dev/mapper/${p}-raid1-meta-side1 /dev/mapper/${p}-raid1-data-side1"

# Carve the mirror into thin-pool metadata and data slices for this group.
dm_create "${p}-thinmeta" "0 $SEC_TMETA linear /dev/mapper/${p}-raid1 0"
dm_create "${p}-thindata" "0 $SEC_TDATA linear /dev/mapper/${p}-raid1 $SEC_TMETA"

slow_summary
_info "GRP ${CN}/${DA}/leg${LEG}/grp${GRP} setup complete"
EOF_GRP
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_grp() {   # <cn_name> <cn_ip> <da> <leg> <grp>
    local cn="$1" ip="$2" da="$3" leg="$4" grp="$5"
    _info "=== delete_grp $cn $da leg$leg grp$grp ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_DGRP'
set -uo pipefail
p="dnv-${CN}-${DA}-leg${LEG}-grp${GRP}"
# Remove in reverse creation order.  Removing raid1 last drops the open count
# on the vd nvme namespaces so disconnect_vd can succeed afterwards.
dm_remove "${p}-thindata" force  >/dev/null 2>&1 || true
dm_remove "${p}-thinmeta" force  >/dev/null 2>&1 || true
dm_remove "${p}-raid1" force     >/dev/null 2>&1 || true
dm_remove "${p}-raid1-data-side1" force >/dev/null 2>&1 || true
dm_remove "${p}-raid1-meta-side1" force >/dev/null 2>&1 || true
dm_remove "${p}-raid1-data-side0" force >/dev/null 2>&1 || true
dm_remove "${p}-raid1-meta-side0" force >/dev/null 2>&1 || true

# Disconnect the two vds.
for d in 0 1; do
    nvme_disc "${NQN_PREFIX}:dn:dn${d}:${DA}-leg${LEG}-grp${GRP}-vd${d}-${CN}"
done

slow_summary
_info "GRP ${CN}/${DA}/leg${LEG}/grp${GRP} teardown complete"
EOF_DGRP
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- leg: concat grps -> thinpool -> create_snap 0  (cn-side) ---------------
create_leg() {   # <cn_name> <cn_ip> <da> <leg>
    local cn="$1" ip="$2" da="$3" leg="$4"
    _info "=== create_leg $cn $da leg$leg ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; LEG='$leg'"; _emit_common; cat <<'EOF_LEG'
set -uo pipefail
sp="dnv-${CN}-${DA}-leg${LEG}"

# Concatenate both groups' thinmeta and thindata.
dm_create "${sp}-thinmeta" \
"0 $SEC_TMETA linear /dev/mapper/${sp}-grp0-thinmeta 0
$SEC_TMETA $SEC_TMETA linear /dev/mapper/${sp}-grp1-thinmeta 0"
dm_create "${sp}-thindata" \
"0 $SEC_TDATA linear /dev/mapper/${sp}-grp0-thindata 0
$SEC_TDATA $SEC_TDATA linear /dev/mapper/${sp}-grp1-thindata 0"

# Thin pool. On a fresh setup the thinmeta starts as all-zeros (the raid1 was
# just built on fresh vd data). During failover rebuild, the thinmeta carries
# cn0's committed metadata and must NOT be zeroed -- it holds the mapping for
# thin 0 and therefore the data the host has already written.
if ! dm_exists "${sp}-thinpool"; then
    dm_create "${sp}-thinpool" \
"0 $SEC_POOL_DATA thin-pool /dev/mapper/${sp}-thinmeta /dev/mapper/${sp}-thindata \
$POOL_BLOCK_SECTORS 0 1 skip_block_zeroing"
    # Default snap 0 = the live writable origin (create_thin id 0). On a fresh
    # pool this succeeds; during failover it fails (inherited) -- which is fine.
    sudo dmsetup message "/dev/mapper/${sp}-thinpool" 0 "create_thin 0" >/dev/null 2>&1 \
        && _info "create_thin 0 succeeded in ${sp}-thinpool (fresh pool)" \
        || _info "thin 0 already present in ${sp}-thinpool (inherited metadata)"
else
    _info "exists : ${sp}-thinpool"
    sudo dmsetup message "/dev/mapper/${sp}-thinpool" 0 "create_thin 0" >/dev/null 2>&1 || true
fi

# The thin device for snap 0 on this leg.
dm_create "${sp}-snap0" "0 $SEC_SNAP thin /dev/mapper/${sp}-thinpool 0"

slow_summary
_info "LEG ${CN}/${DA}/leg${LEG} setup complete"
EOF_LEG
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_leg() {   # <cn_name> <cn_ip> <da> <leg>
    local cn="$1" ip="$2" da="$3" leg="$4"
    _info "=== delete_leg $cn $da leg$leg ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; LEG='$leg'"; _emit_common; cat <<'EOF_DLEG'
set -uo pipefail
sp="dnv-${CN}-${DA}-leg${LEG}"
# Reverse creation order. Removing thinpool commits its metadata (matters for failover).
dm_remove "${sp}-snap0"    force >/dev/null 2>&1 || true
dm_remove "${sp}-thinpool" force >/dev/null 2>&1 || true
dm_remove "${sp}-thindata" force >/dev/null 2>&1 || true
dm_remove "${sp}-thinmeta" force >/dev/null 2>&1 || true

slow_summary
_info "LEG ${CN}/${DA}/leg${LEG} teardown complete"
EOF_DLEG
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- cntlr_active / cntlr_standby (cn-side orchestrators) -------------------
create_cntlr_active() {   # <cn_name> <cn_ip> <da> <nlegs> <hostnqn> <hostid>
    local cn="$1" ip="$2" da="$3" nlegs="$4" hnqn="$5" hid="$6"
    _info "=== create_cntlr_active $cn $da ($nlegs legs) ==="
    local leg
    for leg in $(seq 0 $(( nlegs - 1 ))); do
        local grp
        for grp in 0 1; do
            create_grp "$cn" "$ip" "$da" "$leg" "$grp" "$hnqn" "$hid"
        done
        create_leg "$cn" "$ip" "$da" "$leg"
    done
}

delete_cntlr_active() {   # <cn_name> <cn_ip> <da>
    local cn="$1" ip="$2" da="$3"
    _info "=== delete_cntlr_active $cn $da ==="
    local nlegs="${NLEGS:-2}"
    local leg
    for leg in $(seq 0 $(( nlegs - 1 ))); do
        delete_leg "$cn" "$ip" "$da" "$leg"
        local grp
        for grp in 1 0; do
            delete_grp "$cn" "$ip" "$da" "$leg" "$grp"
        done
    done
}

create_cntlr_standby() {   # <cn_name> <cn_ip> <da> <nlegs> <hostnqn> <hostid>
    local cn="$1" ip="$2" da="$3" nlegs="$4" hnqn="$5" hid="$6"
    _info "=== create_cntlr_standby $cn $da ($nlegs legs) ==="
    local leg grp
    for leg in $(seq 0 $(( nlegs - 1 ))); do
        for grp in 0 1; do
            # vd0 from dn0, vd1 from dn1.
            connect_vd "$cn" "$ip" dn0 "$DN0_IP" 0 "$hnqn" "$hid" "$leg" "$grp"
            connect_vd "$cn" "$ip" dn1 "$DN1_IP" 1 "$hnqn" "$hid" "$leg" "$grp"
        done
    done
}

delete_cntlr_standby() {   # <cn_name> <cn_ip> <da>
    local cn="$1" ip="$2" da="$3"
    _info "=== delete_cntlr_standby $cn $da ==="
    local nlegs="${NLEGS:-2}"
    local leg
    for leg in $(seq 0 $(( nlegs - 1 ))); do
        local grp
        for grp in 0 1; do
            disconnect_vd "$cn" "$ip" dn0 "$DN0_IP" 0 "$leg" "$grp"
            disconnect_vd "$cn" "$ip" dn1 "$DN1_IP" 1 "$leg" "$grp"
        done
    done
}

# --- snap: create_thin (id 0) or create_snap (src>0) across ALL leg pools ---
create_snap() {   # <cn_name> <cn_ip> <da> <new_id> <src_id>
    local cn="$1" ip="$2" da="$3" new_id="$4" src_id="$5"
    _info "=== create_snap $cn $da id=$new_id src=$src_id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; NEW_ID='$new_id'; SRC_ID='$src_id'"; _emit_common; cat <<'EOF_SNAP'
set -uo pipefail
for leg in 0 1; do
    sp="dnv-${CN}-${DA}-leg${leg}-thinpool"
    dm_exists "$sp" || { _warn "no $sp on this cn"; continue; }
    if [ "$SRC_ID" = "0" ] && [ "$NEW_ID" = "0" ]; then
        # create_thin for the default origin.
        sudo dmsetup message "/dev/mapper/$sp" 0 "create_thin $NEW_ID" >/dev/null 2>&1 \
            && _info "create_thin $NEW_ID in $sp" \
            || _info "thin $NEW_ID already present in $sp (inherited)"
    else
        sudo dmsetup message "/dev/mapper/$sp" 0 "create_snap $NEW_ID $SRC_ID" >/dev/null 2>&1 \
            && _info "create_snap $NEW_ID src $SRC_ID in $sp" \
            || _warn "create_snap $NEW_ID src $SRC_ID failed in $sp"
    fi
done
slow_summary
_info "SNAP ${CN}/${DA} id=${NEW_ID} complete"
EOF_SNAP
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_snap() {   # <cn_name> <cn_ip> <da> <id>
    local cn="$1" ip="$2" da="$3" id="$4"
    _info "=== delete_snap $cn $da id=$id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; SNAP_ID='$id'"; _emit_common; cat <<'EOF_DSAP'
set -uo pipefail
for leg in 0 1; do
    sp="dnv-${CN}-${DA}-leg${leg}-thinpool"
    dm_exists "$sp" || continue
    sudo dmsetup message "/dev/mapper/$sp" 0 "delete $SNAP_ID" >/dev/null 2>&1 \
        && _info "deleted snap $SNAP_ID in $sp" \
        || _info "snap $SNAP_ID absent in $sp"
done
slow_summary
_info "SNAP ${CN}/${DA} id=${SNAP_ID} delete complete"
EOF_DSAP
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- exp_active / exp_standby (cn-side exporter stack + nvmet export) -----
# exp = raid0 of snap thin-devs (per leg) + nvmeof export to host.
#   active:  -real (raid0) -> exp (linear on -real), ANA optimized
#   standby: -error -> -delay -> exp (linear on -delay), ANA inaccessible
create_exp_active() {   # <cn_name> <cn_ip> <da> <snap_id> <host_nqn> <host_id> <cntlid_min> <cntlid_max>
    local cn="$1" ip="$2" da="$3" snap_id="$4" hnqn="$5" hid="$6" cmin="$7" cmax="$8"
    _info "=== create_exp_active $cn $da snap$snap_id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; SNAP_ID='$snap_id'; HNQN='$hnqn'; CMIN='$cmin'; CMAX='$cmax'"; _emit_common; cat <<'EOF_EA'
set -uo pipefail
prep_node

vp="dnv-${CN}-${DA}-snap${SNAP_ID}-exp0"

# raid0 of the per-leg snap thin devices.
dm_create "${vp}-real" \
"0 $SEC_EXP raid raid0 1 $RAID0_CHUNK_SECTORS 2 \
- /dev/mapper/dnv-${CN}-${DA}-leg0-snap${SNAP_ID} \
- /dev/mapper/dnv-${CN}-${DA}-leg1-snap${SNAP_ID}"

# Fault-injection devices, sized like the exp.
dm_create "${vp}-error" "0 $SEC_EXP error"
dm_create "${vp}-delay" "0 $SEC_EXP delay /dev/mapper/${vp}-error 0 $DELAY_MS"

# The exported device; on the active CN it points at the raid0.
dm_create "${vp}" "0 $SEC_EXP linear /dev/mapper/${vp}-real 0"

# Export to host0, ANA optimized. cntlid range must not overlap the standby cn's.
DNV_SERIAL="$EXP_SERIAL" DNV_MODEL="$EXP_MODEL" DNV_CNTLID_MIN="$CMIN" DNV_CNTLID_MAX="$CMAX" \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/${vp}" "$EXP_UUID" 1 "$HNQN"
case "$CN" in
    cn0) MY_IP="$CN0_IP" ;;
    cn1) MY_IP="$CN1_IP" ;;
esac
nvmet_port 1 "$MY_IP" "optimized"
nvmet_link "$NQN_EXP" 1

_info "$(sudo dmsetup ls | wc -l) dm devices created"
slow_summary
_info "EXP active ${CN}/${DA}/snap${SNAP_ID} setup complete"
EOF_EA
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

create_exp_standby() {   # <cn_name> <cn_ip> <da> <snap_id> <host_nqn> <host_id> <cntlid_min> <cntlid_max>
    local cn="$1" ip="$2" da="$3" snap_id="$4" hnqn="$5" hid="$6" cmin="$7" cmax="$8"
    _info "=== create_exp_standby $cn $da snap$snap_id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; SNAP_ID='$snap_id'; HNQN='$hnqn'; CMIN='$cmin'; CMAX='$cmax'"; _emit_common; cat <<'EOF_ES'
set -uo pipefail
prep_node

vp="dnv-${CN}-${DA}-snap${SNAP_ID}-exp0"

# Standby exporter: error -> delay -> exp stub (ANA inaccessible). Any I/O that
# reaches it stalls instead of silently succeeding.
dm_create "${vp}-error" "0 $SEC_EXP error"
dm_create "${vp}-delay" "0 $SEC_EXP delay /dev/mapper/${vp}-error 0 $DELAY_MS"
dm_create "${vp}"       "0 $SEC_EXP linear /dev/mapper/${vp}-delay 0"

# Identical NQN / namespace identity / size as the active cn so host0 aggregates
# both into one multipath device; disjoint cntlid range; ANA inaccessible.
DNV_SERIAL="$EXP_SERIAL" DNV_MODEL="$EXP_MODEL" DNV_CNTLID_MIN="$CMIN" DNV_CNTLID_MAX="$CMAX" \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/${vp}" "$EXP_UUID" 1 "$HNQN"
case "$CN" in
    cn0) MY_IP="$CN0_IP" ;;
    cn1) MY_IP="$CN1_IP" ;;
esac
nvmet_port 1 "$MY_IP" "inaccessible"
nvmet_link "$NQN_EXP" 1

slow_summary
_info "EXP standby ${CN}/${DA}/snap${SNAP_ID} setup complete"
EOF_ES
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_exp_active() {   # <cn_name> <cn_ip> <da> <snap_id>
    local cn="$1" ip="$2" da="$3" snap_id="$4"
    _info "=== delete_exp_active $cn $da snap$snap_id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; SNAP_ID='$snap_id'"; _emit_common; cat <<'EOF_DEA'
set -uo pipefail
vp="dnv-${CN}-${DA}-snap${SNAP_ID}-exp0"
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_port 1
nvmet_remove_host "$HOSTNQN_HOST0"

# Remove the full active stack (reverse creation order).
dm_remove "${vp}"       force >/dev/null 2>&1 || true
dm_remove "${vp}-delay"  force >/dev/null 2>&1 || true
dm_remove "${vp}-error" force >/dev/null 2>&1 || true
dm_remove "${vp}-real"   force >/dev/null 2>&1 || true

cleanup_node_common
_info "remaining dm devices: $(dm_count)"
slow_summary
_info "EXP active ${CN}/${DA}/snap${SNAP_ID} teardown complete"
EOF_DEA
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

delete_exp_standby() {   # <cn_name> <cn_ip> <da> <snap_id>
    local cn="$1" ip="$2" da="$3" snap_id="$4"
    _info "=== delete_exp_standby $cn $da snap$snap_id ==="
    { _emit_vars; echo "CN='$cn'; DA='$da'; SNAP_ID='$snap_id'"; _emit_common; cat <<'EOF_DES'
set -uo pipefail
vp="dnv-${CN}-${DA}-snap${SNAP_ID}-exp0"
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_port 1
nvmet_remove_host "$HOSTNQN_HOST0"

# Remove the standby stub stack (reverse creation order).
dm_remove "${vp}"       force >/dev/null 2>&1 || true
dm_remove "${vp}-delay"  force >/dev/null 2>&1 || true
dm_remove "${vp}-error" force >/dev/null 2>&1 || true

cleanup_node_common
_info "remaining dm devices: $(dm_count)"
slow_summary
_info "EXP standby ${CN}/${DA}/snap${SNAP_ID} teardown complete"
EOF_DES
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

# --- disarm_vd_delay / arm_vd_delay (dn-side delay-cn1 disarm/arm) ---------
# Why this exists: the kernel runs nvme_partition_scan_work on each namespace it
# discovers, which reads sector 0 outside udev's control.  If cn1's namespaces
# are backed by a 3600s dm-delay at connect time, that read never completes: it
# wedges an nvme-wq worker, wedges a udev worker behind the open_mutex, and --
# worst of all -- leaves a request outstanding on the DN, which then makes
# "echo 0 > namespaces/1/enable" and controller teardown hang at teardown time.
#
# So cn1 connects while these devices carry an error table (scan fails instantly)
# and the real delay table is put back afterwards.
#
# leg/grp are explicit params: disarming one group means disarming all its delay
# devices (the vid x cn loop still runs internally).  grow.sh disarms only the
# grp2 devices it just created; setup.sh disarms the base groups by looping.
disarm_vd_delay() {   # <dn_name> <dn_ip> <leg> <grp>
    local dn="$1" ip="$2" leg="$3" grp="$4"
    _info "=== disarm_vd_delay $dn (leg$leg grp$grp) ==="
    { _emit_vars; echo "DN='$dn'; MODE='disarm'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_DLY'
set -uo pipefail
for vid in 0 1; do
  for cn in cn0 cn1; do
    d="dnv-${DN}-da0-leg${LEG}-grp${GRP}-vd${vid}-delay-${cn}"
    dm_exists "$d" || continue
    [ "$(dm_target "$d")" = "delay" ] || continue
    dm_reload "$d" "0 $SEC_PD error" >/dev/null
  done
done
_info "DN ${DN} leg${LEG} grp${GRP}: -delay devices disarmed"
slow_summary
EOF_DLY
    } | _ssh "${SSH_USER}@${ip}" bash -s
}

arm_vd_delay() {   # <dn_name> <dn_ip> <leg> <grp>
    local dn="$1" ip="$2" leg="$3" grp="$4"
    _info "=== arm_vd_delay $dn (leg$leg grp$grp) ==="
    { _emit_vars; echo "DN='$dn'; MODE='arm'; LEG='$leg'; GRP='$grp'"; _emit_common; cat <<'EOF_ARM'
set -uo pipefail
for vid in 0 1; do
  for cn in cn0 cn1; do
    d="dnv-${DN}-da0-leg${LEG}-grp${GRP}-vd${vid}-delay-${cn}"
    dm_exists "$d" || continue
    [ "$(dm_target "$d")" = "delay" ] && continue
    dm_reload "$d" "0 $SEC_PD delay /dev/mapper/dnv-${DN}-da0-leg${LEG}-grp${GRP}-vd${vid}-err-${cn} 0 $DELAY_MS" >/dev/null
  done
done
_info "DN ${DN} leg${LEG} grp${GRP}: -delay devices armed"
slow_summary
EOF_ARM
    } | _ssh "${SSH_USER}@${ip}" bash -s
}
