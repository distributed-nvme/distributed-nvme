#!/bin/bash
#
# ana4.sh -- 4-node NVMe-oF ANA failover demo (host0/dn0/cn0/cn1).
#
# A standalone demonstration of Asymmetric Namespace Access (ANA) failover that
# FLIPS ana-group STATES (a live attribute write, namespace stays enabled) rather
# than moving the namespace between groups (which required disable/enable and
# produced an EIO window). Fault injection uses dm-error (no dm-delay, so none of
# the 6-node disarm/arm dance is needed).
#
# dn0 stack:
#   loop (1G) -> dm-error-cn0, dm-error-cn1 (same size, active => immediate EIO)
#             -> dm-linear-cn0  (on loop)            serves subsys-cn0
#             -> dm-linear-cn1  (on dm-error-cn1)    serves subsys-cn1 (EIO pre-failover)
#   TWO nvmet ports on the same IP, distinct trsvcid:
#     port A (1 / 4420): ana group 1 (state=optimized)       hosts subsys-cn0
#     port B (2 / 4421): ana group 1 (state=non-optimized)   hosts subsys-cn1
#   Each subsystem's single ns uses ana_grpid=1 (each on its OWN port's group 1).
#   A failover FLIPS each port's group-1 STATE -- the ns's ana_grpid never moves,
#   so it is never disabled (no EIO from a disappearing namespace).
# cn0 (active) / cn1 (standby):
#   Each connects to its own dn0 subsystem and re-exports ONE subsystem
#   (NQN_EXP) to host0. Each cn has ONE nvmet port with TWO ana groups
#   (1=optimized, 2=inaccessible); the export ns is bound to ONE group's id and
#   the STATE of that group is what flips. cn0's export ns = dm-linear on
#   subsys-cn0's disk (group 1, optimized); cn1's export ns = dm-linear on a
#   LOCAL dm-error (group 2, inaccessible). Same NQN/uuid/serial/model/size +
#   disjoint cntlid range -> host0 aggregates both into a single native nvme
#   multipath namespace. cn0 also carries an unused dm-error standby (per spec).
# host0:
#   Explicit `nvme connect` to both cn0 and cn1 (no stas/ref0), native multipath
#   (ANA steering), and O_DIRECT I/O via host0_io.sh.
#
# Failover sequence (do_failover): dn0 flips port A grp1 optimized->non-opt
# (ns stays enabled), suspends (parks) dm-linear-cn0, reloads dm-linear-cn1
# err-cn1->loop (real data), flips port B grp1 non-opt->opt; cn1 (re)connects to
# subsys-cn1, waits (locally, no dn0 query) for the ns's group state to read
# optimized, reloads its export dm-linear onto subsys-cn1's disk, takes over
# (export grp2 state inaccessible->optimized) and STAYS optimized (no step-down
# -- cn1 is the live path that keeps serving); cn0 waits (locally) for
# subsys-cn0 group state non-optimized and steps its export down by REMOVING
# the subsystem link from its nvmet port (host0's cn0 controllers tear down);
# dn0 sleeps 5, then reloads the parked dm-linear-cn0 onto dm-error-cn0 and
# resumes it so the queued I/O fails fast (EIO); cn0 then reloads its export
# dm-linear onto the local dm-error, flips the export ana group state
# optimized->inaccessible and re-links the subsystem to its port (host0
# reconnects as an inaccessible standby). The demo ends with cn1 optimized
# (serving) + cn0 inaccessible (standby), so host0 keeps serving via cn1 and the
# report shows the before/during/after latency split.
#
# Usage: ./ana4.sh prep|setup|host|failover|failover-dn0|failover-cn1|failover-cn0|failover-dn0final|inspect-cn0|diagnose|showreport|teardown|status
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

# ---- demo-specific constants (sizes in 512B sectors, per common.sh) ---------
SEC_DEMO=2097152            # 1G  : loop / dm-* / host namespace size
# dn0 dm stack (all dnv-ana4-dn0-* so dnv_remove_all_dm / teardown catch them).
#   lin-cn1 STARTS on err-cn1 (subsys-cn1 returns EIO pre-failover) and is
#   reloaded onto the loop during failover; lin-cn0 STARTS on the loop and is
#   reloaded onto err-cn0 after the failover window is over.
DM_ERR_CN0="dnv-ana4-dn0-err-cn0"  # dm-error backing for lin-cn0 (post-failover)
DM_ERR_CN1="dnv-ana4-dn0-err-cn1"  # dm-error backing for lin-cn1 (pre-failover)
DM_LIN_CN0="dnv-ana4-dn0-lin-cn0"  # dm-linear -> loop   (serves subsys-cn0)
DM_LIN_CN1="dnv-ana4-dn0-lin-cn1"  # dm-linear -> err-cn1 -> loop (serves subsys-cn1)
# cn0 dm stack: err0 is a standby created per spec (unused by the failover);
# lin0 (on the imported subsys-cn0 disk) is the namespace re-exported to host0.
DM_CN0_ERR="dnv-ana4-cn0-err0"
DM_CN0_LIN="dnv-ana4-cn0-lin0"
# cn1 dm stack: lin0 STARTS on err0 (export returns EIO pre-failover) and is
# reloaded onto the imported subsys-cn1 disk during failover.
DM_CN1_ERR="dnv-ana4-cn1-err0"
DM_CN1_LIN="dnv-ana4-cn1-lin0"
LOOP_IMG="/var/tmp/ana4-dn0.img"

NQN_DN_CN0="${NQN_PREFIX}:dn:dn0:subsys-cn0"
NQN_DN_CN1="${NQN_PREFIX}:dn:dn0:subsys-cn1"
# NQN_EXP is reused from common.sh = nqn.2026-07.org.dnv:sp:sp0:td0:exp0
# (host0_io.sh expects exactly this NQN for the multipath volume).

DEMO_UUID="cafe1234-0000-4000-8000-0000000000ab"
DEMO_SERIAL="ANA4DEMO0"
DEMO_MODEL="ana4-demo"

# ANA group ids. In the UPDATED design the namespace's ana_grpid is set ONCE and
# never changed; a failover FLIPS the ana_group STATE (a live attribute write,
# ns stays enabled) instead of moving the ns between groups (which required
# disable/enable and produced an EIO window).
#   dn0: TWO ports (A=1/trsvcid 4420, B=2/trsvcid 4421), each carrying ONE ana
#        group (group 1). subsys-cn0 lives on port A group 1 (state=optimized);
#        subsys-cn1 lives on port B group 1 (state=non-optimized). Both ns use
#        ana_grpid=1 (each on its own port's group). Failover flips each port's
#        group-1 STATE: A optimized->non-optimized, B non-optimized->optimized.
#   cn0/cn1: ONE port each, with TWO ana groups (1=optimized, 2=inaccessible).
#        cn0 export ns is in group 1 (optimized); cn1 export ns is in group 2
#        (inaccessible). Failover flips the STATE of the group holding the ns:
#        cn1 export group-2 inaccessible->optimized (take-over) ->inaccessible
#        (step-down); cn0 export group-1 optimized->inaccessible (step-down).
ANA_OPT=1          # group id 1: optimized (cn0/cn1 group 1; dn0 ports' group 1)
ANA_INAC=2        # group id 2: inaccessible (cn0/cn1 group 2; cn1 export lives here)

# dn0's second nvmet port (subsys-cn1 gets its own port so it can have its own
# single ANA group on a separate TCP socket). Two ports cannot share a (ip,svc).
DN0_PORT_A=1       # nvmet port-number for subsys-cn0  (trsvcid 4420 = NVME_PORT)
DN0_PORT_B=2       # nvmet port-number for subsys-cn1  (trsvcid 4421)
DN0_PORT_B_SVC=4421

# ---- per-node IP convenience -----------------------------------------------
ipof() { case "$1" in dn0) echo "$DN0_IP";; cn0) echo "$CN0_IP";; cn1) echo "$CN1_IP";; host0) echo "$HOST0_IP";; esac; }

# ===================== shared remote helper snippets ========================
# These are appended after _emit_common in every stage heredoc so the remote
# bash has them in scope. They build on the helpers common.sh already ships
# (prep_node, dm_create, dm_reload, dm_remove, cfg_set, nvmet_add_subsys,
#  nvmet_port, nvmet_link, nvmet_remove_*, nvme_conn, nvme_disc,
#  nvme_dev_by_nqn, nvme_wait_dev, ctrl_count, dnv_remove_all_dm, etc.).
emit_helpers() {
cat <<'EOF_HELP'
# Make sure a port's ana group <gid> exists with state <state> (optimized/
# inaccessible). Group 1 already exists by default; others must be mkdir'd.
ana_grp() {  # <portid> <gid> <state>
    local p="$1" g="$2" st="$3" d
    d="$NVMET/ports/$p/ana_groups/$g"
    sudo mkdir -p "$d" || _warn "mkdir $d"
    cfg_set "$d/ana_state" "$st"
    _info "ana group port $p / $g -> $st (cur=$(sudo cat "$d/ana_state" 2>/dev/null))"
}

# Create a nvmet port on an ARBITRARY trsvcid. common.sh's nvmet_port writes
# addr_trtype before addr_trsvcid; on this kernel writing trtype attaches the
# transport and locks further addr edits, SO a non-default trsvcid MUST be set
# FIRST. This helper sets adrfam/traddr/trsvcid, then trtype (last).
nvmet_port_svc() {  # <portnum> <ip> <trsvcid>
    local p="$1" ip="$2" svc="$3" d="$NVMET/ports/$1"
    sudo mkdir -p "$d" || _fail "mkdir port $p"
    cfg_set "$d/addr_adrfam"  "ipv4"
    cfg_set "$d/addr_traddr"  "$ip"
    cfg_set "$d/addr_trsvcid" "$svc"
    cfg_set "$d/addr_trtype"  "tcp"
    _info "nvmet port $p -> $ip:$svc"
}

# Bind (or move) a subsystem's single namespace to ana group <gid>. The
# namespace must be disabled to write ana_grpid, so disable (retrying around
# EBUSY from live controllers), set grpid, then re-enable.
# NOTE: the UPDATED demo never MOVES a ns -- it is created with the right
# ana_grpid by nvmet_add_subsys and only the group STATE is flipped (see
# ana_grp_state). Kept here for completeness / a future ns-move variant.
ns_ana() {  # <nqn> <gid>
    local nqn="$1" g="$2" s i
    s="$NVMET/subsystems/$nqn/namespaces/1"
    [ -e "$s" ] || { _warn "no namespace for $nqn"; return 1; }
    for (( i=0; i<20; i++ )); do
        sudo bash -c "echo 0 > '$s/enable'" 2>/dev/null && break
        [ "$(sudo cat "$s/enable" 2>/dev/null)" = "0" ] && break
        sleep 0.5
    done
    cfg_set "$s/ana_grpid" "$g"
    sudo bash -c "echo 1 > '$s/enable'" || _fail "enable ns $nqn (gid $g)"
    _info "ns $nqn -> ana_grpid $g"
}

# Live-flip a port's ana group <gid> STATE to <state>. The namespace STAYS
# ENABLED (unlike moving ana_grpid, which requires disable/enable and yields an
# EIO window): nvmet accepts a live ana_state write and pushes an ANA-change AEN
# to every connected host, which re-fetches the ANA log and re-steers I/O.
# Confirmed live-writable even with an enabled ns + linked port (probed on dn0).
ana_grp_state() {  # <portid> <gid> <state>   (optimized / non-optimized / inaccessible)
    local f="$NVMET/ports/$1/ana_groups/$2/ana_state"
    [ -e "$f" ] || { _warn "no ana_groups/$2 on port $1 ($f)"; return 1; }
    _t "ana_state port $1 grp $2 -> $3" sudo bash -c "echo '$3' > '$f'" \
        || { _warn "could not set ana_state '$3' on port $1 grp $2"; return 1; }
    _info "port $1 grp $2 -> $(sudo cat "$f")"
}

# Normalise ANA state strings (tolerant of full or short kernel spelling).
_norm_ana() { printf '%s' "$1" | tr 'A-Z' 'a-z' \
    | sed -e 's#non-opt.*#non-opt#' -e 's#persistent-loss.*#ploss#'; }

# LOCALLY observed ANA STATE of the namespace of <subsysnqn>, read from the
# per-namespace sysfs attribute nvmeXcYnZ/ana_state (refreshed by the nvme host
# driver from the ANA log on each ANA-change AEN). This kernel exposes ana_state
# on the NAMESPACE, not under a per-controller ana_groups/<gid>/ dir (which does
# not exist here) -- probed on cn0:  /sys/class/nvme/nvme0/nvme0c0n1/ana_state.
# So this reflects the target's group-STATE flip WITH NO QUERY to the exporting
# node -- exactly what the spec means by "check the state locally".
ns_ana_group_state_local() {  # <subsysnqn> -> echoes ana_state, or "" if unknown
    local nqn="$1" c ns
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        for ns in "$c"/nvme[0-9]*n[0-9]*; do
            [ -e "$ns/ana_state" ] || continue
            cat "$ns/ana_state" 2>/dev/null && return 0
        done
    done
    return 1
}

# Diagnostic: dump the local per-namespace ANA view for <subsysnqn>.
ana_state_of() {  # <subsysnqn>
    local nqn="$1" c ns
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        for ns in "$c"/nvme[0-9]*n[0-9]*; do
            [ -e "$ns/ana_state" ] || continue
            echo "ctrl=$(basename "$c") ns=$(basename "$ns") ana_grpid=$(cat "$ns/ana_grpid" 2>/dev/null) ana_state=$(cat "$ns/ana_state" 2>/dev/null)"
        done
    done
}

# Wait until this node LOCALLY observes the ns of <subsysnqn> in the wanted ANA
# group STATE, by polling the local sysfs that the driver refreshes from the ANA
# log on each AEN (NOT by querying the exporting node). Best-effort: continues
# after <tries>; prints the first observed value so a missing sysfs is visible.
wait_ana() {  # <subsysnqn> <want-state> [tries]
    local nqn="$1" want="$2" tries="${3:-160}" i cur w
    w=$(_norm_ana "$want")
    for (( i=0; i<tries; i++ )); do
        cur=$(ns_ana_group_state_local "$nqn")
        [ -n "$cur" ] && [ "$(_norm_ana "$cur")" = "$w" ] \
            && { _info "ANA-local($nqn): ns-group state=$cur == $want (after ${i}x)"; return 0; }
        [ "$i" = "0" ] && _info "wait_ana($nqn): want '$want', first cur=${cur:-<none>}"
        sleep 0.25
    done
    _warn "wait_ana($nqn): '$want' not confirmed (last=${cur:-<none>}); continuing"
    return 0
}

# Like ctrl_count but berths the subsystem-level multipath namespace device.
mp_ns_dev() {  # <subsysnqn> -> first /dev/nvmeXnY under the subsystem
    local nqn="$1" s n b
    for s in /sys/class/nvme-subsystem/nvme-subsys*; do
        [ -r "$s/subsysnqn" ] || continue
        [ "$(cat "$s/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        for n in "$s"/nvme*n*; do
            [ -e "$n" ] || continue
            b=${n##*/}
            [ -b "/dev/$b" ] && { echo "/dev/$b"; return 0; }
        done
    done
    return 1
}

# Idempotent multipath-aware connect: add a controller at <traddr>/<nqn> only if
# one isn't already there. Works for the FIRST and SUBSEQUENT paths of a
# multipath subsystem (nvme_dev_by_nqn would wrongly report "already connected"
# for the 2nd path, so we key on traddr). Optional 5th arg overrides trsvcid.
mp_connect() {  # <traddr> <nqn> <hostnqn> <hostid> [trsvcid, default $NVME_PORT]
    local ip="$1" nqn="$2" hnqn="$3" hid="$4" svc="${5:-$NVME_PORT}"
    [ "$(ctrl_count "$nqn" "$ip")" -ge 1 ] && { _info "already connected: $nqn @ $ip:$svc"; return 0; }
    _t "nvme connect $nqn @ $ip:$svc" sudo nvme connect -t tcp -a "$ip" -s "$svc" \
        -n "$nqn" --hostnqn "$hnqn" --hostid "$hid" || _warn "nvme connect $nqn @ $ip:$svc failed"
}
EOF_HELP
}

# Common header locals shipped to every remote bash (node identity + demo vars).
emit_demo_vars() {
cat <<EOF
SEC_DEMO=$SEC_DEMO
DM_ERR_CN0='$DM_ERR_CN0'
DM_ERR_CN1='$DM_ERR_CN1'
DM_LIN_CN0='$DM_LIN_CN0'
DM_LIN_CN1='$DM_LIN_CN1'
DM_CN0_ERR='$DM_CN0_ERR'
DM_CN0_LIN='$DM_CN0_LIN'
DM_CN1_ERR='$DM_CN1_ERR'
DM_CN1_LIN='$DM_CN1_LIN'
LOOP_IMG='$LOOP_IMG'
NQN_DN_CN0='$NQN_DN_CN0'
NQN_DN_CN1='$NQN_DN_CN1'
NQN_EXP='$NQN_EXP'
DEMO_UUID='$DEMO_UUID'
DEMO_SERIAL='$DEMO_SERIAL'
DEMO_MODEL='$DEMO_MODEL'
ANA_OPT=$ANA_OPT
ANA_INAC=$ANA_INAC
DN0_PORT_A=$DN0_PORT_A
DN0_PORT_B=$DN0_PORT_B
DN0_PORT_B_SVC=$DN0_PORT_B_SVC
EOF
}

# Run a remote stage script on <node> <ip>: prep + helpers + the inline body.
run_on() {  # <node> <ip>  ; stdin = stage body (quoted heredoc content)
    local node="$1" ip="$2"
    { _emit_vars; emit_demo_vars; echo "NODE='$node'"; _emit_common; emit_helpers; cat; } \
        | _ssh "${SSH_USER}@${ip}" "bash -s"
}

# ===================== stage: prep (modules/udev on all 4) ====================
do_prep() {
    local n ip
    for n in dn0 cn0 cn1 host0; do
        ip=$(ipof "$n")
        _info "=== prep $n ($ip) ==="
        run_on "$n" "$ip" <<'EOF_BODY'
set -uo pipefail
prep_node
_info "prepNode: $(uname -n) dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY
    done
}

# ===================== stage: setup (dn0 + cn0 + cn1) ========================
do_setup() {
    # ---- dn0: loop, err pair, lin-cn0->loop, lin-cn1->err-cn1, 2 ANA groups --
    _info "=== setup dn0 ($DN0_IP) ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
[ -f "$LOOP_IMG" ] || _t "truncate $LOOP_IMG" sudo truncate -s 1G "$LOOP_IMG" || _fail "truncate"
LOOP=$(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: 'NR==1{print $1}')
[ -n "$LOOP" ] || LOOP=$(_t "losetup $LOOP_IMG" sudo losetup -f --show "$LOOP_IMG") || _fail "losetup"
_info "loop: $LOOP ($LOOP_IMG)"

# dm-error pair (same size as the loop). ACTIVE dm-error => immediate EIO,
# not a hang; both must be resumed so reads through their lin wrappers fail fast.
dm_create "$DM_ERR_CN0" "0 $SEC_DEMO error"
dm_create "$DM_ERR_CN1" "0 $SEC_DEMO error"
sudo dmsetup resume --noudevsync "$DM_ERR_CN0" "$DM_ERR_CN1" 2>/dev/null || true
# lin-cn0 -> loop   (real data, serves subsys-cn0 / optimized)
dm_create "$DM_LIN_CN0" "0 $SEC_DEMO linear $LOOP 0"
# lin-cn1 -> err-cn1 (EIO pre-failover, serves subsys-cn1 / non-optimized)
dm_create "$DM_LIN_CN1" "0 $SEC_DEMO linear /dev/mapper/$DM_ERR_CN1 0"
sudo dmsetup resume --noudevsync "$DM_LIN_CN0" "$DM_LIN_CN1" 2>/dev/null || true
_info "lin-cn0: $(sudo dmsetup table "$DM_LIN_CN0") state=$(dm_state "$DM_LIN_CN0")"
_info "lin-cn1: $(sudo dmsetup table "$DM_LIN_CN1") state=$(dm_state "$DM_LIN_CN1")"

# nvmet: port A (4420, group 1 optimized) hosts subsys-cn0;
#        port B (4421, group 1 non-optimized) hosts subsys-cn1.
# Two ports on the same IP MUST carry different trsvcid -> port B uses 4421.
nvmet_port      "$DN0_PORT_A" "$DN0_IP"          # group 1 state set explicitly below
ana_grp "$DN0_PORT_A" 1 "optimized"
nvmet_port_svc  "$DN0_PORT_B" "$DN0_IP" "$DN0_PORT_B_SVC"   # trsvcid MUST precede trtype
ana_grp "$DN0_PORT_B" 1 "non-optimized"

# subsys-cn0 -> lin-cn0 (real data), ana group 1 (optimized) on port A; allow cn0
DNV_MODEL="$DEMO_MODEL" nvmet_add_subsys "$NQN_DN_CN0" "/dev/mapper/$DM_LIN_CN0" "$DEMO_UUID" 1 "$HOSTNQN_CN0"
nvmet_link "$NQN_DN_CN0" "$DN0_PORT_A"
# subsys-cn1 -> lin-cn1 (EIO pre-failover), ana group 1 (non-optimized) on port B; allow cn1
DNV_MODEL="$DEMO_MODEL" nvmet_add_subsys "$NQN_DN_CN1" "/dev/mapper/$DM_LIN_CN1" "$DEMO_UUID" 1 "$HOSTNQN_CN1"
nvmet_link "$NQN_DN_CN1" "$DN0_PORT_B"

_info "dn0 ready: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems"|wc -l) ports=$(ls "$NVMET/ports"|wc -l)"
_info "  port A($DN0_PORT_A/$NVME_PORT grp1)=optimized -> subsys-cn0=lin-cn0(loop)"
_info "  port B($DN0_PORT_B/$DN0_PORT_B_SVC grp1)=non-optimized -> subsys-cn1=lin-cn1(err-cn1)"
EOF_BODY

    # ---- cn0: connect subsys-cn0 (port A/4420); err standby + lin-on-subsys; export opt ----
    _info "=== setup cn0 ($CN0_IP) ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
mp_connect "$DN0_IP" "$NQN_DN_CN0" "$HOSTNQN_CN0" "$HOSTID_CN0"
DEV=$(nvme_wait_dev "$NQN_DN_CN0" 100) || _fail "no block dev for $NQN_DN_CN0"
_info "cn0 imported subsys-cn0 -> $DEV"

dm_create "$DM_CN0_ERR" "0 $SEC_DEMO error"
dm_create "$DM_CN0_LIN" "0 $SEC_DEMO linear $DEV 0"
sudo dmsetup resume --noudevsync "$DM_CN0_ERR" "$DM_CN0_LIN" 2>/dev/null || true
_info "cn0 lin: $(sudo dmsetup table "$DM_CN0_LIN")"

# cn0 export port: group 1 = optimized (export ns lives here), group 2 = inaccessible (unused standby)
nvmet_port 1 "$CN0_IP"
ana_grp 1 $ANA_OPT    "optimized"
ana_grp 1 $ANA_INAC   "inaccessible"
DNV_SERIAL="$DEMO_SERIAL" DNV_MODEL="$DEMO_MODEL" DNV_CNTLID_MIN=1 DNV_CNTLID_MAX=255 \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/$DM_CN0_LIN" "$DEMO_UUID" $ANA_OPT "$HOSTNQN_HOST0"
nvmet_link "$NQN_EXP" 1
_info "cn0 export ready: $NQN_EXP -> /dev/mapper/$DM_CN0_LIN (ns ana_grpid=$ANA_OPT=opt, cntlid 1-255); err0 standby"
EOF_BODY

    # ---- cn1: connect subsys-cn1 (port B/4421); lin-on-err; export SAME NQN (inac) --------
    _info "=== setup cn1 ($CN1_IP) ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
# connect to dn0's port B (trsvcid 4421), NOT the default 4420
mp_connect "$DN0_IP" "$NQN_DN_CN1" "$HOSTNQN_CN1" "$HOSTID_CN1" "$DN0_PORT_B_SVC"
# subsys-cn1's ns is backed (on dn0) by lin-cn1->err-cn1, so reads return EIO;
# the namespace block device still appears (EIO on partition scan is fine).
DEV=$(nvme_wait_dev "$NQN_DN_CN1" 100) || _warn "no block dev for $NQN_DN_CN1 yet (EIO backing)"
_info "cn1 imported subsys-cn1 -> ${DEV:-<not-yet>}"

dm_create "$DM_CN1_ERR" "0 $SEC_DEMO error"
# lin0 starts on the LOCAL dm-error (EIO); reload to subsys-cn1 disk in failover.
dm_create "$DM_CN1_LIN" "0 $SEC_DEMO linear /dev/mapper/$DM_CN1_ERR 0"
sudo dmsetup resume --noudevsync "$DM_CN1_ERR" "$DM_CN1_LIN" 2>/dev/null || true
_info "cn1 lin: $(sudo dmsetup table "$DM_CN1_LIN")"

# cn1 export port: group 1 = optimized (unused standby), group 2 = inaccessible (export ns here)
nvmet_port 1 "$CN1_IP"
ana_grp 1 $ANA_OPT    "optimized"
ana_grp 1 $ANA_INAC   "inaccessible"
DNV_SERIAL="$DEMO_SERIAL" DNV_MODEL="$DEMO_MODEL" DNV_CNTLID_MIN=256 DNV_CNTLID_MAX=511 \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/$DM_CN1_LIN" "$DEMO_UUID" $ANA_INAC "$HOSTNQN_HOST0"
nvmet_link "$NQN_EXP" 1
_info "cn1 export ready: $NQN_EXP -> /dev/mapper/$DM_CN1_LIN (ns ana_grpid=$ANA_INAC=inac, cntlid 256-511, same nqn/model/uuid/serial as cn0)"
EOF_BODY
}

# ===================== stage: host (connect multipath + I/O) =================
do_host() {
    _info "=== host0 ($HOST0_IP): connect both paths + start I/O ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
mp_connect "$CN0_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
mp_connect "$CN1_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
sleep 1
echo "--- list-subsys ---"; sudo nvme list-subsys 2>/dev/null
echo "--- /sys/class/nvme-subsystem ---"; ls /sys/class/nvme-subsystem/ 2>/dev/null
DEV=$(mp_ns_dev "$NQN_EXP") || _warn "no multipath ns for $NQN_EXP yet"
[ -n "$DEV" ] && _info "host0 multipath volume: $DEV ($(sudo blockdev --getsize64 "$DEV" 2>/dev/null) bytes)"
echo "ctrls for $NQN_EXP: $(ctrl_count "$NQN_EXP" '')  (cn0=$(ctrl_count "$NQN_EXP" "$CN0_IP") cn1=$(ctrl_count "$NQN_EXP" "$CN1_IP"))"
EOF_BODY

    _info "starting I/O generator (host0_io.sh)"
    "$(dirname "$0")/host0_io.sh" start
    sleep 2
    "$(dirname "$0")/host0_io.sh" status
    "$(dirname "$0")/host0_io.sh" mark before_failover
}

# ===================== stage: failover =======================================
# Sequence (goal spec): dn0 moves each subsys ns to the other ANA group and
# parks the now-inactive backing linear on a dm-error; cn1 takes the active data
# path via subsys-cn1 (then steps down); cn0 confirms its backend is
# non-optimized and steps its export down; finally dn0 resumes the parked
# lin-cn0 onto its dm-error so queued I/O fails fast. End state: BOTH host0
# paths (cn0 export + cn1 export) are inaccessible, so the post-failover window
# shows real I/O errors.

# dn0: flip subsys-cn0's port A group-1 state optimized->non-optimized; suspend
# lin-cn0 (park in-flight reads); reload lin-cn1 err-cn1->loop (real data);
# flip subsys-cn1's port B group-1 state non-optimized->optimized. The NAMESPACES
# stay enabled the whole time -- only the group STATE flips (no disable, no EIO
# from a disappearing ns). lin-cn0 stays suspended until the final dn0 stage.
do_failover_dn0() {
    _info "=== dn0 failover: flip ana group states, park lin-cn0, bring lin-cn1 online ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
# $LOOP is per-session; rediscover it (the setup stage ran in another shell).
LOOP=$(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: 'NR==1{print $1}')
[ -n "$LOOP" ] || _fail "dn0 failover: loop for $LOOP_IMG not found"
_info "dn0 failover: loop = $LOOP"
# 1. subsys-cn0 (port A) group-1 STATE: optimized -> non-optimized (ns stays enabled)
_info "dn0: port $DN0_PORT_A grp1 state optimized -> non-optimized"
ana_grp_state "$DN0_PORT_A" 1 "non-optimized"
# 2. park dm-linear-cn0 -- in-flight reads stay queued until the final resume
_info "dn0: suspend $DM_LIN_CN0 (parked; resumed in dn0-final)"
sudo dmsetup suspend --nolockfs --noflush --noudevsync "$DM_LIN_CN0" || _fail "suspend $DM_LIN_CN0"
# 3. reload dm-linear-cn1: err-cn1 backing -> loop (subsys-cn1 now serves real data)
_info "dn0: reload $DM_LIN_CN1 (err-cn1 -> loop)"
dm_reload "$DM_LIN_CN1" "0 $SEC_DEMO linear $LOOP 0"
# 4. subsys-cn1 (port B) group-1 STATE: non-optimized -> optimized (ns stays enabled)
_info "dn0: port $DN0_PORT_B grp1 state non-optimized -> optimized"
ana_grp_state "$DN0_PORT_B" 1 "optimized"
_info "dn0 failover step done: lin-cn0=$(dm_state "$DM_LIN_CN0") lin-cn1=$(dm_state "$DM_LIN_CN1")"
_info "  port A grp1 = $(sudo cat "$NVMET/ports/$DN0_PORT_A/ana_groups/1/ana_state")"
_info "  port B grp1 = $(sudo cat "$NVMET/ports/$DN0_PORT_B/ana_groups/1/ana_state")"
EOF_BODY
}

# cn1: (re)connect subsys-cn1; wait its group state becomes "optimized" LOCALLY
# (no query to dn0 -- the driver refreshes ns ana_state from the ANA log on the
# ANA-change AEN); reload lin0 onto the subsys-cn1 disk; take over (export group
# state inaccessible->optimized). Per the UPDATED plan cn1 STAYS optimized -- no
# step-down -- so host0 keeps serving real data via cn1 post-failover.
do_failover_cn1() {
    _info "=== cn1 failover: connect, wait optimized, reload onto subsys-cn1, take-over (STAYS optimized) ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
_info "cn1: (re)connect to $NQN_DN_CN1 (dn0 port B / $DN0_PORT_B_SVC)"
mp_connect "$DN0_IP" "$NQN_DN_CN1" "$HOSTNQN_CN1" "$HOSTID_CN1" "$DN0_PORT_B_SVC"
# wait subsys-cn1's ns-group state becomes optimized LOCALLY (AEN-driven; no dn0 query)
wait_ana "$NQN_DN_CN1" "optimized" 160
DEV=$(nvme_wait_dev "$NQN_DN_CN1" 80) || _fail "cn1: no block dev for subsys-cn1"
_info "cn1: subsys-cn1 disk = $DEV; reloading $DM_CN1_LIN (err -> subsys-cn1 disk)"
dm_reload "$DM_CN1_LIN" "0 $SEC_DEMO linear $DEV 0"
# take over: export group STATE inaccessible -> optimized and STAY there. host0
# sees cn1's path become optimized and routes I/O here (real data now). ns stays
# ENABLED; no step-down -- this is the live failover that actually keeps serving.
_info "cn1: export group $ANA_INAC state inaccessible -> optimized (take-over, STAYS)"
ana_grp_state 1 $ANA_INAC "optimized"
_info "cn1 failover step done: export grp state = $(sudo cat "$NVMET/ports/1/ana_groups/$ANA_INAC/ana_state")"
EOF_BODY
}

# cn0: wait subsys-cn0's group state is non-optimized (locally); step the export
# down by REMOVING the subsystem link from cn0's nvmet port (per spec) so host0
# tears down its cn0 controllers. The backend-release reload of cn0's dm-linear
# onto its local dm-error is done SEPARATELY in do_failover_cn0_release -- AFTER
# dn0-final resumes dn0's lin-cn0 -- because reloading cn0's lin0 (which dm_reload
# does via suspend->load->resume) DEADLOCKS while cn0's backing /dev/nvme0n1 has
# reads parked at dn0's SUSPENDED lin-cn0 (suspended dm targets above an nvme ns
# with outstanding stuck reads don't return from suspend). Resuming dn0's lin-cn0
# first drains those reads with EIO (or abort them), which lets cn0's lin0 suspend
# proceed. do_failover_cn0_release also flips the export ana group state
# optimized -> inaccessible and re-links the subsystem to the port, re-adding the
# cn0 path as an inaccessible standby.
do_failover_cn0() {
    _info "=== cn0 failover: confirm backend non-optimized, step export down ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
# 1. wait until subsys-cn0's ns-group state is non-optimized, checked LOCALLY
wait_ana "$NQN_DN_CN0" "non-optimized" 160
# 2. step the export down: REMOVE the subsystem link from cn0's nvmet port (per
#    spec) -- ns stays enabled; host0's cn0 controllers tear down. Group state
#    stays "optimized" here; flipped to "inaccessible" + re-linked in cn0-release.
_info "cn0: remove export $NQN_EXP from nvmet port 1 (host0 cn0 path disconnects)"
if [ -e "$NVMET/ports/1/subsystems/$NQN_EXP" ] || [ -L "$NVMET/ports/1/subsystems/$NQN_EXP" ]; then
    _t "unlink $NQN_EXP from port 1" sudo rm -f "$NVMET/ports/1/subsystems/$NQN_EXP" \
        || _warn "could not unlink $NQN_EXP from port 1"
fi
_info "cn0 step-down done: export link removed from port 1 (grp1 state stays $(sudo cat "$NVMET/ports/1/ana_groups/$ANA_OPT/ana_state"))"
EOF_BODY
}

# cn0 backend release (run AFTER dn0-final): reload cn0's export dm-linear onto
# the LOCAL dm-error standby, flip the export ana group state optimized ->
# inaccessible, and re-link the subsystem to cn0's nvmet port (per spec) so
# host0 reconnects to the cn0 path as an inaccessible standby. The reload also
# drops cn0's hold on /dev/nvmeXnY (the subsys-cn0 disk) -> the upstream dn0
# chain is free to be torn down. MUST run after dn0-final resumed lin-cn0, else
# the dmsetup suspend here deadlocks on the still-parked reads at dn0.
do_failover_cn0_release() {
    _info "=== cn0 backend release: reload export dm-linear onto local dm-error ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
_info "cn0-release: reload $DM_CN0_LIN onto $DM_CN0_ERR (release subsys-cn0 backend -> EIO standby)"
dm_reload "$DM_CN0_LIN" "0 $SEC_DEMO linear /dev/mapper/$DM_CN0_ERR 0"
# per spec: flip the export ana group state optimized -> inaccessible, then
# re-link the subsystem to cn0's port (host0 reconnects, path seen inaccessible -> steers to cn1)
ana_grp_state 1 $ANA_OPT "inaccessible"
nvmet_link "$NQN_EXP" 1
_info "cn0-release done: lin0=$(sudo dmsetup table "$DM_CN0_LIN") grp1=$(sudo cat "$NVMET/ports/1/ana_groups/$ANA_OPT/ana_state") link=$([ -e "$NVMET/ports/1/subsystems/$NQN_EXP" ] && echo present || echo ABSENT)"
EOF_BODY
}

# dn0 final: sleep 5; reload the parked (suspended) lin-cn0 onto err-cn0 and
# resume -- the bios queued in the suspend now fail with EIO instead of hanging
# past the host controller command timeout.
do_failover_dn0_final() {
    _info "=== dn0 final: sleep 5, reload lin-cn0 onto err-cn0 + resume ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
_info "dn0-final: sleeping 5s before bringing lin-cn0 back as dm-error ..."
sleep 5
_info "dn0-final: reload $DM_LIN_CN0 (loop -> err-cn0) and resume (parked bios -> EIO)"
dm_reload "$DM_LIN_CN0" "0 $SEC_DEMO linear /dev/mapper/$DM_ERR_CN0 0"
_info "dn0-final done: lin-cn0=$(dm_state "$DM_LIN_CN0") table=[ $(sudo dmsetup table "$DM_LIN_CN0") ]"
EOF_BODY
}

do_failover() {
    "$(dirname "$0")/host0_io.sh" mark failover_start
    do_failover_dn0
    do_failover_cn1
    do_failover_cn0
    do_failover_dn0_final
    do_failover_cn0_release
    "$(dirname "$0")/host0_io.sh" mark failover_end
    _info "failover complete: cn0 clean standby (inac export + EIO backend), cn1 serving (opt + real data) -> host0 keeps serving via cn1"
}

# ===================== stage: inspect cn0 ====================================
# Dump cn0's nvmet export subsystem + namespace attributes plus the initiator
# side (block devices / sysfs / nvme list-subsys) -- used after `failover-dn0`
# to see why cn0's backing /dev/nvme0n1 disappears once subsys-cn0 is ANA-inac.
do_inspect_cn0() {
    _info "=== inspect cn0 ($CN0_IP): nvmet subsystem + namespace status ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
SUB="$NVMET/subsystems/$NQN_EXP"
NS="$SUB/namespaces/1"
DN_SUB="$NVMET/subsystems/$NQN_DN_CN0"
echo "########## cn0 nvmet: EXPORT subsystem ($NQN_EXP) ##########"
if [ -d "$SUB" ]; then
    echo "subsys dir: $SUB"
    echo "  attr_allow_any_host = $(sudo cat "$SUB/attr_allow_any_host" 2>/dev/null)"
    echo "  attr_serial         = $(sudo cat "$SUB/attr_serial" 2>/dev/null)"
    echo "  attr_cntlid_min/max = $(sudo cat "$SUB/attr_cntlid_min" 2>/dev/null)/$(sudo cat "$SUB/attr_cntlid_max" 2>/dev/null)"
    echo "  allowed_hosts       = $(ls "$SUB/allowed_hosts/" 2>/dev/null | tr '\n' ' ')"
    echo "  --- namespaces ---"
    for n in "$SUB"/namespaces/*; do
        [ -d "$n" ] || continue
        echo "  ns $(basename "$n"):"
        echo "    enable      = $(sudo cat "$n/enable" 2>/dev/null)"
        echo "    device_path = $(sudo cat "$n/device_path" 2>/dev/null)"
        echo "    device_uuid = $(sudo cat "$n/device_uuid" 2>/dev/null)"
        echo "    ana_grpid   = $(sudo cat "$n/ana_grpid" 2>/dev/null)"
        echo "    device exists? -> $([ -b "$(sudo cat "$n/device_path" 2>/dev/null)" ] && echo yes || echo NO)"
    done
else
    echo "  (no export subsystem dir at $SUB)"
fi
echo
echo "########## cn0 nvmet: DOWNSTREAM subsys-cn0 ($NQN_DN_CN0) ##########"
if [ -d "$DN_SUB" ]; then
    echo "subsys dir: $DN_SUB"
    for n in "$DN_SUB"/namespaces/*; do
        [ -d "$n" ] || continue
        echo "  ns $(basename "$n"): enable=$(sudo cat "$n/enable" 2>/dev/null) grpc=$(sudo cat "$n/ana_grpid" 2>/dev/null) dev=$(sudo cat "$n/device_path" 2>/dev/null)"
    done
else
    echo "  (no downstream subsystem dir at $DN_SUB -- cn0 is a HOST of subsys-cn0, not its target)"
fi
echo "  port links to subsys-cn0: $(ls -l "$NVMET/ports/1/subsystems/" 2>/dev/null | grep -o "subsys-cn0" | head -1)"
echo
echo "########## cn0 port 1 ana groups ##########"
for g in "$NVMET"/ports/1/ana_groups/*; do
    [ -d "$g" ] || continue
    echo "  group $(basename "$g"): ana_state=$(sudo cat "$g/ana_state" 2>/dev/null)"
done
echo
echo "########## cn0 initiator side (subsys-cn0 consumer) ##########"
echo "-- /dev/nvme* --"; ls -l /dev/nvme* 2>/dev/null || echo "  (none)"
echo "-- /sys/class/nvme --"; ls /sys/class/nvme/ 2>/dev/null || echo "  (none)"
echo "-- /sys/class/nvme-subsystem --"; ls /sys/class/nvme-subsystem/ 2>/dev/null || echo "  (none)"
for s in /sys/class/nvme-subsystem/nvme-subsys*; do
    [ -r "$s/subsysnqn" ] || continue
    echo "  $(basename "$s"): NQN=$(cat "$s/subsysnqn" 2>/dev/null)"
    for c in "$s"/nvme*c*; do
        [ -e "$c" ] || continue
        cb=$(basename "$c")
        echo "    $cb state=$(cat "$c/state" 2>/dev/null) ana_states: $(for ag in "$c"/ana_groups/*; do [ -d "$ag" ] && printf '%s=%s ' "$(basename "$ag")" "$(cat "$ag/state" 2>/dev/null)"; done)"
    done
done
echo "-- nvme list-subsys --"; sudo nvme list-subsys 2>/dev/null
echo
echo "########## cn0 dm devices (err0 standby + lin0 on subsys-cn0 disk) ##########"
sudo dmsetup ls 2>/dev/null | grep -E 'dnv-ana4' || echo "  (none)"
EOF_BODY
}

# ===================== stage: diagnose (initiator ANA sysfs checkpoint) =======
# After setup+host: dump dn0's nvmet port group states and the cn0/cn1 INITIATOR
# side /sys/class/nvme/nvmeX/ana_groups/<gid>/state. If those sysfs files exist
# and reflect the target's group state, the state-flip wait_ana is sound. If they
# are absent, wait_ana will time out (and fall through), signalling the kernel
# does not expose per-group initiator state and a different local signal is needed.
do_diagnose() {
    _info "=== diagnose dn0 nvmet port group states ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
for p in "$NVMET"/ports/*; do
    [ -d "$p" ] || continue
    echo "port $(basename "$p") trsvcid=$(sudo cat "$p/addr_trsvcid" 2>/dev/null) traddr=$(sudo cat "$p/addr_traddr" 2>/dev/null)"
    for g in "$p"/ana_groups/*; do
        [ -d "$g" ] || continue
        echo "  group $(basename "$g"): ana_state=$(sudo cat "$g/ana_state" 2>/dev/null)"
    done
    echo "  linked subsys: $(ls "$p/subsystems" 2>/dev/null | tr '\n' ' ')"
done
echo "dn0 listening sockets:"; sudo ss -ltn 2>/dev/null | grep -E '4420|4421' || true
EOF_BODY
    _info "=== diagnose cn0 initiator ANA sysfs (for $NQN_DN_CN0) ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
echo "-- /sys/class/nvme --"; ls /sys/class/nvme/ 2>/dev/null
echo "-- per-namespace ana_state (the local signal wait_ana reads) --"
ana_state_of "$NQN_DN_CN0"
echo "-- nvme ana-log (per ctrl) --"
for d in /dev/nvme[0-9]*n[0-9]*; do [ -b "$d" ] && sudo nvme ana-log "$d" 2>/dev/null | head -16; done
EOF_BODY
    _info "=== diagnose cn1 initiator ANA sysfs (for $NQN_DN_CN1) ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
echo "-- /sys/class/nvme --"; ls /sys/class/nvme/ 2>/dev/null
echo "-- per-namespace ana_state (the local signal wait_ana reads) --"
ana_state_of "$NQN_DN_CN1"
EOF_BODY
    _info "=== diagnose host0 multipath ANA (NQN_EXP both paths) ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
sudo nvme list-subsys 2>/dev/null
echo "-- per-namespace ana_state on both paths --"
ana_state_of "$NQN_EXP"
EOF_BODY
}

# ===================== stage: showreport ====================================
# After `failover`, cn1's export STAYS optimized, so host0 keeps serving real
# data via cn1. Let the I/O generator run a few seconds more so the report's
# "after failover" phase has real successes on the cn1 path, then stop + print
# the stats. (The single failed write is the straddler caught in dn0's suspended
# lin-cn0, released with EIO at the final resume.)
do_showreport() {
    _info "letting host0 keep serving via cn1 for 8s post-failover ..."
    sleep 8
    "$(dirname "$0")/host0_io.sh" stop
    "$(dirname "$0")/host0_io.sh" report
}

# ===================== stage: status (all 4 nodes) ===========================
do_status() {
    local n ip
    for n in dn0 cn0 cn1 host0; do
        ip=$(ipof "$n")
        _info "=== status $n ($ip) ==="
        run_on "$n" "$ip" <<'EOF_BODY'
set -uo pipefail
echo "host=$(uname -n)"
echo "dm(dnv):"; sudo dmsetup ls 2>/dev/null | grep -E 'dnv-|ana4' || true
echo "nvmet subsys:"; ls "$NVMET/subsystems" 2>/dev/null || true
echo "nvmet ports:"; ls "$NVMET/ports" 2>/dev/null || true
echo "nvme ctrls:"; ls /sys/class/nvme/ 2>/dev/null || true
echo "nvme-subsys:"; ls /sys/class/nvme-subsystem/ 2>/dev/null || true
echo "loop(dnv/ana4):"; sudo losetup -a 2>/dev/null | grep -E 'dnv|ana4' || true
EOF_BODY
    done
    _info "host0 nvme list-subsys:"; _ssh "${SSH_USER}@${HOST0_IP}" "sudo nvme list-subsys 2>/dev/null"
}

# ===================== stage: teardown (all 4 nodes) =========================
do_teardown() {
    # Order: host0 disconnect -> cn0 export -> cn1 export(+dm) -> dn0 subsys -> dn0 dm/loop
    _info "=== teardown host0 ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
sudo nvme disconnect -n "$NQN_EXP" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
sudo rm -f /etc/udev/rules.d/58-dnv-test.rules 2>/dev/null || true
sudo udevadm control --reload-rules >/dev/null 2>&1 || true
_info "host0 teardown: nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l) subsys=$(ls /sys/class/nvme-subsystem/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown cn0 ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
# Remove the EXPORT subsystem (releases nvmet's hold on /dev/mapper/lin0),
# then the dm stack (lin0 holds the imported /dev/nvmeXnY open, so it MUST go
# before `nvme disconnect` or the disconnect waits on the lingering open).
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_port 1
dnv_remove_all_dm
sudo nvme disconnect -n "$NQN_DN_CN0" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
sudo rm -f /etc/udev/rules.d/58-dnv-test.rules 2>/dev/null || true
sudo udevadm control --reload-rules >/dev/null 2>&1 || true
_info "cn0 teardown: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown cn1 ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_port 1
dnv_remove_all_dm
sudo nvme disconnect -n "$NQN_DN_CN1" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
sudo rm -f /etc/udev/rules.d/58-dnv-test.rules 2>/dev/null || true
sudo udevadm control --reload-rules >/dev/null 2>&1 || true
_info "cn1 teardown: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown dn0 ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
nvmet_remove_subsys "$NQN_DN_CN0"
nvmet_remove_subsys "$NQN_DN_CN1"
nvmet_remove_port "$DN0_PORT_A"
nvmet_remove_port "$DN0_PORT_B"
nvmet_remove_host "$HOSTNQN_CN0"
nvmet_remove_host "$HOSTNQN_CN1"
# resume anything suspended before removing
dnv_defuse_all 2>/dev/null || true
dnv_remove_all_dm
for LOOP in $(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: '{print $1}'); do
    _t "losetup -d $LOOP" sudo losetup -d "$LOOP" >/dev/null 2>&1 || true
done
sudo rm -f "$LOOP_IMG" 2>/dev/null || true
sudo rm -f /etc/udev/rules.d/58-dnv-test.rules 2>/dev/null || true
sudo udevadm control --reload-rules >/dev/null 2>&1 || true
_info "dn0 teardown: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) ports=$(ls "$NVMET/ports" 2>/dev/null|wc -l) loop=$(sudo losetup -a 2>/dev/null|grep -c -E 'dnv|ana4')"
EOF_BODY

    _info "=== final verification ==="
    do_status
}

# ===================== dispatch =============================================
case "${1:-}" in
    prep)             do_prep ;;
    setup)            do_setup ;;
    host)             do_host ;;
    failover)         do_failover ;;
    failover-dn0)     "$(dirname "$0")/host0_io.sh" mark failover_start; do_failover_dn0 ;;
    failover-cn1)     do_failover_cn1 ;;
    failover-cn0)     do_failover_cn0 ;;
    failover-cn0release) do_failover_cn0_release ;;
    failover-dn0final) do_failover_dn0_final ;;
    inspect-cn0)      do_inspect_cn0 ;;
    diagnose)         do_diagnose ;;
    showreport)       do_showreport ;;
    teardown)         do_teardown ;;
    status)           do_status ;;
    *) echo "Usage: $0 prep|setup|host|failover|failover-dn0|failover-cn1|failover-cn0|failover-dn0final|failover-cn1|failover-cn0|failover-cn0release|inspect-cn0|diagnose|showreport|teardown|status" >&2; exit 1 ;;
esac