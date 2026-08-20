#!/bin/bash
#
# ana5.sh -- 4-node NVMe-oF ANA failover demo driven by ANA-GROUP MEMBERSHIP
#            MOVES (host0/dn0/cn0/cn1).
#
# Where ana4.sh flips the STATE of a fixed ana group, this demo keeps every
# group's state constant and MOVES the namespace between groups
# (disable -> write ana_grpid -> enable), which is what the design doc calls for:
# every node has ONE nvmet port carrying TWO ana groups, one permanently
# "optimized" (the A group) and one permanently "inaccessible" (the B group).
# A failover is then "put the namespace in the other group", plus an unlink /
# relink of the subsystem from the port around it (a namespace cannot be
# disabled while a live controller is using it).
#
# Because a port with no subsystem is not listened on at all, each port also
# carries an anchor subsystem (subsys-fake-*): no namespace, no allowed host,
# it exists only to keep the kernel's TCP listener up while the real subsystems
# come and go.
#
# Topology
# --------
# dn0   loop(1G) --+-- dm-linear-cn0 -> loop            (real data)
#                  +-- dm-error-cn0                     (parking lot for lin-cn0)
#                  +-- dm-error-cn1 <- dm-linear-cn1    (EIO before the failover)
#       port 1 (dn0:4420): ana group 1 = dn0A optimized, 2 = dn0B inaccessible
#         subsys-fake-dn0   anchor, no ns
#         subsys-cn0        ns1 = dm-linear-cn0, group dn0A  (optimized)
#         subsys-cn1        ns1 = dm-linear-cn1, group dn0B  (inaccessible)
#
# cn0   connects subsys-cn0 -> /dev/nvmeXn1
#       dm-linear-cn0 -> that namespace ; dm-error-cn0 standby
#       port 1 (cn0:4420): group 1 = cn0A optimized, 2 = cn0B inaccessible
#         subsys-fake-cn0    anchor
#         subsys-host0-cn0   ns1 = dm-linear, group cn0A (optimized)
#
# cn1   connects subsys-cn1 -> /dev/nvmeXn1 (inaccessible until the failover)
#       dm-linear-cn1 -> dm-error-cn1 (local) ; reloaded onto the namespace later
#       port 1 (cn1:4420): group 1 = cn1A optimized, 2 = cn1B inaccessible
#         subsys-fake-cn1    anchor
#         subsys-host0-cn1   ns1 = dm-linear, group cn1B (inaccessible)
#
# host0 connects to cn0 and cn1 and gets ONE native-multipath namespace.
#
# NOTE on the two host0 exports: NVMe native multipath aggregates CONTROLLERS OF
# THE SAME SUBSYSTEM, and a subsystem is identified by its NQN.  "subsys-host0-cn0"
# and "subsys-host0-cn1" therefore cannot be two different NQNs and still be one
# multipath device -- they are the cn0-side and the cn1-side instance of ONE
# subsystem, $NQN_EXP, with the same ns uuid/nguid, serial and model and disjoint
# cntlid ranges.  Everything else follows the spec literally.
#
# Failover (do_failover), in the order the spec gives:
#   dn0  unlink subsys-cn0 + subsys-cn1 from the port; move subsys-cn0's ns to
#        dn0B (inaccessible) and subsys-cn1's ns to dn0A (optimized); suspend
#        dm-linear-cn0 (park it); reload dm-linear-cn1 from dm-error-cn1 onto the
#        loop; relink both subsystems.
#   cn1  wait LOCALLY (sysfs, no query to dn0) until subsys-cn1 reads Optimized;
#        reload its dm-linear onto that namespace; unlink subsys-host0-cn1; move
#        its ns to cn1A (optimized); relink -> cn1 is now the serving path.
#   cn0  wait LOCALLY until subsys-cn0 reads Inaccessible; unlink
#        subsys-host0-cn0 from its port (host0's cn0 controllers tear down).
#   dn0  sleep 3, reload the parked dm-linear-cn0 onto dm-error-cn0 and resume,
#        so anything still queued there fails fast instead of hanging.
#   cn0  reload its dm-linear onto the local dm-error, move subsys-host0-cn0's ns
#        to cn0B (inaccessible), relink -> cn0 is back as a standby path.
#
# Usage: ./ana5.sh prep|setup|host|failover|showreport|status|diagnose|teardown
#        (the failover stages can also be run one at a time:
#         failover-dn0 / failover-cn1 / failover-cn0 / failover-dn0final /
#         failover-cn0final)
#
set -uo pipefail
source "$(dirname "$0")/common.sh"

# ---- sizes / names ---------------------------------------------------------
SEC_DEMO=2097152                      # 1G in 512B sectors: loop and every dm dev

DM_DN0_ERR_CN0="dnv-ana5-dn0-err-cn0"   # parking lot for lin-cn0 (post-failover)
DM_DN0_ERR_CN1="dnv-ana5-dn0-err-cn1"   # backing of lin-cn1 before the failover
DM_DN0_LIN_CN0="dnv-ana5-dn0-lin-cn0"   # -> loop        (serves subsys-cn0)
DM_DN0_LIN_CN1="dnv-ana5-dn0-lin-cn1"   # -> err-cn1     (serves subsys-cn1)
DM_CN0_ERR="dnv-ana5-cn0-err0"
DM_CN0_LIN="dnv-ana5-cn0-lin0"
DM_CN1_ERR="dnv-ana5-cn1-err0"
DM_CN1_LIN="dnv-ana5-cn1-lin0"
LOOP_IMG="/var/tmp/ana5-dn0.img"

NQN_FAKE_DN0="${NQN_PREFIX}:dn:dn0:subsys-fake-dn0"
NQN_DN_CN0="${NQN_PREFIX}:dn:dn0:subsys-cn0"
NQN_DN_CN1="${NQN_PREFIX}:dn:dn0:subsys-cn1"
NQN_FAKE_CN0="${NQN_PREFIX}:cn:cn0:subsys-fake-cn0"
NQN_FAKE_CN1="${NQN_PREFIX}:cn:cn1:subsys-fake-cn1"
# NQN_EXP (common.sh) = nqn.2026-07.org.dnv:sp:sp0:td0:exp0 -- the ONE subsystem
# that cn0 and cn1 both export (see the note above); host0_io.sh expects this NQN.

DEMO_UUID="cafe1234-0000-4000-8000-0000000000a5"
DEMO_SERIAL="ANA5DEMO0"
DEMO_MODEL="ana5-demo"

# ANA group ids. Group STATES never change in this demo; only the namespace's
# ana_grpid moves. Group 1 exists by default on every port; group 2 is mkdir'd.
GRP_A=1            # dn0A / cn0A / cn1A  -- always "optimized"
GRP_B=2            # dn0B / cn0B / cn1B  -- always "inaccessible"

# Short fabrics timers so a subsystem that is unlinked and relinked comes back in
# ~1s instead of the 10s default; the failover window is dominated by this.
RECONNECT_DELAY=1
CTRL_LOSS_TMO=600

# ---- failover timing instrumentation ---------------------------------------
# FO_DELAY seconds are slept after each key step of the failover, to answer "how
# long may we take between these two actions before host0 sees I/O errors?".
# FO_ONLY, if set to a step id (see _pause callers), restricts the stall to that
# one step so a threshold can be attributed to a single window.
FO_DELAY="${FO_DELAY:-0}"
FO_ONLY="${FO_ONLY:-}"

ipof() { case "$1" in dn0) echo "$DN0_IP";; cn0) echo "$CN0_IP";; cn1) echo "$CN1_IP";; host0) echo "$HOST0_IP";; esac; }

# ===================== remote helper snippets ===============================
emit_helpers() {
cat <<'EOF_HELP'
# --- failover step instrumentation ------------------------------------------
# Stall for FO_DELAY seconds after a key step. <id> names the WINDOW that this
# step opens, i.e. _pause dn0-unlink measures "how long may dn0 sit with the
# subsystems off its port". Timestamped so the per-node logs of a parallel run
# can be interleaved afterwards.
_pause() {  # <step-id>
    local id="$1"
    [ "$FO_DELAY" = "0" ] && return 0
    [ -n "$FO_ONLY" ] && [ "$FO_ONLY" != "$id" ] && return 0
    _stamp "PAUSE[$id] ${FO_DELAY}s"
    sleep "$FO_DELAY"
}
_stamp() { echo "[$(date +%H:%M:%S.%3N)] [$NODE] $*"; }

# --- nvmet: ana groups, anchors, membership moves ---------------------------

# Make sure port <p> has ana group <g> in state <st>. Group 1 always exists;
# anything else has to be mkdir'd first.
ana_grp() {  # <portid> <gid> <state>
    local p="$1" g="$2" st="$3" d="$NVMET/ports/$1/ana_groups/$2"
    sudo mkdir -p "$d" || _warn "mkdir $d"
    cfg_set "$d/ana_state" "$st"
    _info "port $p ana group $g -> $(sudo cat "$d/ana_state" 2>/dev/null)"
}

# An anchor subsystem: no namespace, no allowed host. Its only job is to be
# linked to the port -- nvmet only creates the TCP listener for a port that has
# at least one subsystem, so without it the port would go away every time the
# real subsystems are unlinked.
nvmet_add_anchor() {  # <nqn> <portnum>
    local nqn="$1" port="$2" s="$NVMET/subsystems/$1"
    sudo mkdir -p "$s" || _fail "mkdir anchor subsystem $nqn"
    cfg_set "$s/attr_allow_any_host" "0"
    nvmet_link "$nqn" "$port"
    _info "anchor : $nqn linked to port $port (no ns, no allowed host)"
}

# Unlink a subsystem from a port. nvmet tears down every controller of that
# subsystem on that port; connected hosts drop into their reconnect loop.
nvmet_unlink() {  # <nqn> <portnum>
    local l="$NVMET/ports/$2/subsystems/$1"
    { [ -L "$l" ] || [ -e "$l" ]; } || { _info "not linked: $1 on port $2"; return 0; }
    _t "unlink $1 from port $2" sudo rm -f "$l" || { _warn "could not unlink $1"; return 1; }
    _info "unlinked: $1 from port $2"
}

# Move a subsystem's single namespace into ana group <gid>: ana_grpid is only
# writable on a DISABLED namespace, so disable -> set -> enable. The disable can
# transiently return EBUSY while the last controller is being torn down, hence
# the retry loop.
ns_move_ana() {  # <nqn> <gid>
    local nqn="$1" g="$2" s i
    s="$NVMET/subsystems/$nqn/namespaces/1"
    [ -e "$s" ] || { _warn "no namespace 1 for $nqn"; return 1; }
    for (( i=0; i<40; i++ )); do
        sudo bash -c "echo 0 > '$s/enable'" 2>/dev/null && break
        [ "$(sudo cat "$s/enable" 2>/dev/null)" = "0" ] && break
        sleep 0.25
    done
    [ "$(sudo cat "$s/enable" 2>/dev/null)" = "0" ] || { _warn "could not disable ns of $nqn"; return 1; }
    _info "ns $nqn: disabled (was grp $(sudo cat "$s/ana_grpid" 2>/dev/null))"
    cfg_set "$s/ana_grpid" "$g"
    _t "enable ns $nqn" sudo bash -c "echo 1 > '$s/enable'" || { _warn "could not re-enable ns of $nqn"; return 1; }
    _info "ns $nqn: ana_grpid=$(sudo cat "$s/ana_grpid" 2>/dev/null), enabled"
}

# --- local (no-query) view of a namespace's ANA state ------------------------
# The nvme host driver refreshes /sys/class/nvme/nvmeX/nvmeXcYnZ/ana_state from
# the ANA log every time the target sends an ANA-change AEN, so reading it is a
# purely local check -- no command is sent to the exporting node.
_norm_ana() { printf '%s' "$1" | tr 'A-Z' 'a-z' \
    | sed -e 's#non-opt.*#non-opt#' -e 's#persistent-loss.*#ploss#'; }

ns_ana_state_local() {  # <subsysnqn> -> echoes ana_state
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

ana_state_of() {  # <subsysnqn> -- diagnostic dump
    local nqn="$1" c ns
    for c in /sys/class/nvme/nvme*; do
        [ -r "$c/subsysnqn" ] || continue
        [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
        echo "  ctrl=$(basename "$c") state=$(cat "$c/state" 2>/dev/null) addr=$(cat "$c/address" 2>/dev/null)"
        for ns in "$c"/nvme[0-9]*n[0-9]*; do
            [ -e "$ns/ana_state" ] || continue
            echo "    ns=$(basename "$ns") ana_grpid=$(cat "$ns/ana_grpid" 2>/dev/null) ana_state=$(cat "$ns/ana_state" 2>/dev/null)"
        done
    done
}

# Poll the local sysfs above until the wanted state shows up.
wait_ana() {  # <subsysnqn> <want-state> [tries, 0.25s each]
    local nqn="$1" want="$2" tries="${3:-240}" i cur w t0 t1
    w=$(_norm_ana "$want"); t0=$(date +%s%N)
    for (( i=0; i<tries; i++ )); do
        cur=$(ns_ana_state_local "$nqn")
        if [ -n "$cur" ] && [ "$(_norm_ana "$cur")" = "$w" ]; then
            t1=$(date +%s%N)
            _info "ANA-local($nqn): '$cur' == '$want' after $(( (t1-t0)/1000000 ))ms"
            return 0
        fi
        [ "$i" = "0" ] && _info "wait_ana($nqn): want '$want', now '${cur:-<none>}'"
        sleep 0.25
    done
    _warn "wait_ana($nqn): '$want' never observed (last='${cur:-<none>}'); continuing"
    return 1
}

# --- dm suspend with a deadline ---------------------------------------------
# A dm suspend waits for the bios already handed to the underlying device. If
# that device is an nvme namespace whose only path went ANA-inaccessible, the
# host driver has requeued them with nowhere to send them and the suspend never
# returns. <release-cmd> is the escape hatch: it is only run if the deadline is
# missed, and must be something that makes those bios fail.
dm_suspend_deadline() {  # <name> <secs> [release-cmd...]
    local name="$1" secs="$2"; shift 2
    dm_exists "$name" || { _warn "no such dm device: $name"; return 1; }
    [ "$(dm_state "$name")" = "SUSPENDED" ] && return 0
    sudo dmsetup suspend --nolockfs --noflush --noudevsync "$name" &
    local sp=$! i
    for (( i=0; i<secs*10; i++ )); do
        kill -0 "$sp" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$sp" 2>/dev/null; then
        if [ "$#" -gt 0 ]; then
            _warn "suspend($name) still blocked after ${secs}s -- backend I/O is stuck; releasing with: $*"
            "$@" || _warn "release command failed"
        else
            _warn "suspend($name) still blocked after ${secs}s and no release command given"
        fi
        for (( i=0; i<900; i++ )); do kill -0 "$sp" 2>/dev/null || break; sleep 0.1; done
    fi
    wait "$sp" 2>/dev/null || true
    [ "$(dm_state "$name")" = "SUSPENDED" ] || { _warn "suspend($name) did not take"; return 1; }
    _info "suspended: $name"
}

# Load a new table onto an already-suspended device and resume it.
dm_load_resume() {  # <name> <table>
    local name="$1" table="$2"
    printf '%s\n' "$table" | _t "load $name" sudo dmsetup load "$name" \
        || { _warn "load failed: $name"; sudo dmsetup resume --noudevsync "$name"; return 1; }
    _t "resume $name" sudo dmsetup resume --noudevsync "$name" || { _warn "resume failed: $name"; return 1; }
    _info "reloaded: $name -> $(sudo dmsetup table "$name" | head -1)"
}

# --- nvme host side ---------------------------------------------------------
mp_ns_dev() {  # <subsysnqn> -> the subsystem-level (multipath) block device
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

# Idempotent, multipath-aware connect: keyed on traddr so the SECOND path of a
# multipath subsystem is not mistaken for "already connected".
mp_connect() {  # <traddr> <nqn> <hostnqn> <hostid>
    local ip="$1" nqn="$2" hnqn="$3" hid="$4"
    [ "$(ctrl_count "$nqn" "$ip")" -ge 1 ] && { _info "already connected: $nqn @ $ip"; return 0; }
    _t "nvme connect $nqn @ $ip" sudo nvme connect -t tcp -a "$ip" -s "$NVME_PORT" \
        -n "$nqn" --hostnqn "$hnqn" --hostid "$hid" \
        --reconnect-delay "$RECONNECT_DELAY" --ctrl-loss-tmo "$CTRL_LOSS_TMO" \
        || _warn "nvme connect $nqn @ $ip failed"
}

# Wait for a controller of <nqn> at <traddr> to be back in state "live".
wait_ctrl_live() {  # <nqn> <traddr> [tries]
    local nqn="$1" ip="$2" tries="${3:-120}" i c
    for (( i=0; i<tries; i++ )); do
        for c in /sys/class/nvme/nvme*; do
            [ -r "$c/subsysnqn" ] || continue
            [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$nqn" ] || continue
            case "$(cat "$c/address" 2>/dev/null)" in *"traddr=$ip,"*) ;; *) continue ;; esac
            [ "$(cat "$c/state" 2>/dev/null)" = "live" ] && { _info "ctrl live: $nqn @ $ip ($(basename "$c"))"; return 0; }
        done
        sleep 0.25
    done
    _warn "no live controller for $nqn @ $ip after $((tries/4))s"
    return 1
}
EOF_HELP
}

emit_demo_vars() {
cat <<EOF
SEC_DEMO=$SEC_DEMO
DM_DN0_ERR_CN0='$DM_DN0_ERR_CN0'
DM_DN0_ERR_CN1='$DM_DN0_ERR_CN1'
DM_DN0_LIN_CN0='$DM_DN0_LIN_CN0'
DM_DN0_LIN_CN1='$DM_DN0_LIN_CN1'
DM_CN0_ERR='$DM_CN0_ERR'
DM_CN0_LIN='$DM_CN0_LIN'
DM_CN1_ERR='$DM_CN1_ERR'
DM_CN1_LIN='$DM_CN1_LIN'
LOOP_IMG='$LOOP_IMG'
NQN_FAKE_DN0='$NQN_FAKE_DN0'
NQN_DN_CN0='$NQN_DN_CN0'
NQN_DN_CN1='$NQN_DN_CN1'
NQN_FAKE_CN0='$NQN_FAKE_CN0'
NQN_FAKE_CN1='$NQN_FAKE_CN1'
DEMO_UUID='$DEMO_UUID'
DEMO_SERIAL='$DEMO_SERIAL'
DEMO_MODEL='$DEMO_MODEL'
GRP_A=$GRP_A
GRP_B=$GRP_B
RECONNECT_DELAY=$RECONNECT_DELAY
CTRL_LOSS_TMO=$CTRL_LOSS_TMO
FO_DELAY='$FO_DELAY'
FO_ONLY='$FO_ONLY'
EOF
}

run_on() {  # <node> <ip>  ; stdin = the stage body
    local node="$1" ip="$2"
    { _emit_vars; emit_demo_vars; echo "NODE='$node'"; _emit_common; emit_helpers; cat; } \
        | _ssh "${SSH_USER}@${ip}" "bash -s"
}

# ===================== stage: prep ==========================================
do_prep() {
    local n ip
    for n in dn0 cn0 cn1 host0; do
        ip=$(ipof "$n")
        _info "=== prep $n ($ip) ==="
        run_on "$n" "$ip" <<'EOF_BODY'
set -uo pipefail
prep_node
_info "prep: $(uname -n) dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY
    done
}

# ===================== stage: setup =========================================
do_setup() {
    _info "=== setup dn0 ($DN0_IP): loop, dm stack, port with dn0A/dn0B, 3 subsystems ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node

# 1. a 1G file and a loop device on top of it
[ -f "$LOOP_IMG" ] || _t "truncate $LOOP_IMG" sudo truncate -s 1G "$LOOP_IMG" || _fail "truncate $LOOP_IMG"
LOOP=$(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: 'NR==1{print $1}')
[ -n "$LOOP" ] || LOOP=$(_t "losetup $LOOP_IMG" sudo losetup -f --show "$LOOP_IMG") || _fail "losetup"
_info "loop: $LOOP ($LOOP_IMG, $((SEC_DEMO/2048))M)"

# 2. the dm stack. dm-error is ACTIVE, so anything reaching it gets EIO at once
#    (as opposed to dm-delay, which would hang and wedge udev workers).
dm_create "$DM_DN0_ERR_CN0" "0 $SEC_DEMO error"
dm_create "$DM_DN0_ERR_CN1" "0 $SEC_DEMO error"
dm_create "$DM_DN0_LIN_CN0" "0 $SEC_DEMO linear $LOOP 0"
dm_create "$DM_DN0_LIN_CN1" "0 $SEC_DEMO linear /dev/mapper/$DM_DN0_ERR_CN1 0"
sudo dmsetup resume --noudevsync "$DM_DN0_ERR_CN0" "$DM_DN0_ERR_CN1" \
                                 "$DM_DN0_LIN_CN0" "$DM_DN0_LIN_CN1" 2>/dev/null || true
_info "lin-cn0: [$(sudo dmsetup table "$DM_DN0_LIN_CN0")] state=$(dm_state "$DM_DN0_LIN_CN0")"
_info "lin-cn1: [$(sudo dmsetup table "$DM_DN0_LIN_CN1")] state=$(dm_state "$DM_DN0_LIN_CN1")"

# 3. one nvmet port, two ana groups: dn0A optimized, dn0B inaccessible
nvmet_port 1 "$DN0_IP"
ana_grp 1 $GRP_A "optimized"        # dn0A
ana_grp 1 $GRP_B "inaccessible"     # dn0B

# 3.3 anchor subsystem, so the port keeps its listener when the real
#     subsystems are unlinked during the failover
nvmet_add_anchor "$NQN_FAKE_DN0" 1

# 3.4/3.5 the two real subsystems
DNV_MODEL="$DEMO_MODEL" nvmet_add_subsys "$NQN_DN_CN0" "/dev/mapper/$DM_DN0_LIN_CN0" \
    "$DEMO_UUID" $GRP_A "$HOSTNQN_CN0"          # dn0A: optimized, real data
nvmet_link "$NQN_DN_CN0" 1
DNV_MODEL="$DEMO_MODEL" nvmet_add_subsys "$NQN_DN_CN1" "/dev/mapper/$DM_DN0_LIN_CN1" \
    "$DEMO_UUID" $GRP_B "$HOSTNQN_CN1"          # dn0B: inaccessible, EIO backing
nvmet_link "$NQN_DN_CN1" 1

_info "dn0 ready: port 1 grpA($GRP_A)=$(sudo cat "$NVMET/ports/1/ana_groups/$GRP_A/ana_state") grpB($GRP_B)=$(sudo cat "$NVMET/ports/1/ana_groups/$GRP_B/ana_state")"
_info "  linked: $(ls "$NVMET/ports/1/subsystems" | tr '\n' ' ')"
EOF_BODY

    _info "=== setup cn0 ($CN0_IP): import subsys-cn0, dm stack, port with cn0A/cn0B, export ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
mp_connect "$DN0_IP" "$NQN_DN_CN0" "$HOSTNQN_CN0" "$HOSTID_CN0"
DEV=$(nvme_wait_dev "$NQN_DN_CN0" 150) || _fail "cn0: no block device for $NQN_DN_CN0"
_info "cn0: subsys-cn0 -> $DEV ($(sudo blockdev --getsize64 "$DEV" 2>/dev/null) bytes)"

dm_create "$DM_CN0_ERR" "0 $SEC_DEMO error"
dm_create "$DM_CN0_LIN" "0 $SEC_DEMO linear $DEV 0"
sudo dmsetup resume --noudevsync "$DM_CN0_ERR" "$DM_CN0_LIN" 2>/dev/null || true
_info "cn0 lin: [$(sudo dmsetup table "$DM_CN0_LIN")]"

nvmet_port 1 "$CN0_IP"
ana_grp 1 $GRP_A "optimized"        # cn0A
ana_grp 1 $GRP_B "inaccessible"     # cn0B
nvmet_add_anchor "$NQN_FAKE_CN0" 1
# subsys-host0-cn0: the cn0-side instance of the exported subsystem. Same NQN /
# uuid / serial / model as cn1's, disjoint cntlid range -> host0 sees one
# subsystem with two controllers.
DNV_SERIAL="$DEMO_SERIAL" DNV_MODEL="$DEMO_MODEL" DNV_CNTLID_MIN=1 DNV_CNTLID_MAX=255 \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/$DM_CN0_LIN" "$DEMO_UUID" $GRP_A "$HOSTNQN_HOST0"
nvmet_link "$NQN_EXP" 1
_info "cn0 ready: subsys-host0-cn0=$NQN_EXP ns->/dev/mapper/$DM_CN0_LIN grp=$GRP_A(cn0A,optimized)"
EOF_BODY

    _info "=== setup cn1 ($CN1_IP): import subsys-cn1, dm stack, port with cn1A/cn1B, export ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
mp_connect "$DN0_IP" "$NQN_DN_CN1" "$HOSTNQN_CN1" "$HOSTID_CN1"
# subsys-cn1's namespace is in dn0B (inaccessible), so the host driver has no
# usable path for it: the block device appears, but any read of it is requeued.
# Nothing here reads it -- cn1's dm-linear sits on a LOCAL dm-error until the
# failover -- so that is fine.
DEV=$(nvme_wait_dev "$NQN_DN_CN1" 150) || _warn "cn1: no block device for $NQN_DN_CN1 yet"
_info "cn1: subsys-cn1 -> ${DEV:-<none yet>}"
ana_state_of "$NQN_DN_CN1"

dm_create "$DM_CN1_ERR" "0 $SEC_DEMO error"
dm_create "$DM_CN1_LIN" "0 $SEC_DEMO linear /dev/mapper/$DM_CN1_ERR 0"
sudo dmsetup resume --noudevsync "$DM_CN1_ERR" "$DM_CN1_LIN" 2>/dev/null || true
_info "cn1 lin: [$(sudo dmsetup table "$DM_CN1_LIN")]"

nvmet_port 1 "$CN1_IP"
ana_grp 1 $GRP_A "optimized"        # cn1A
ana_grp 1 $GRP_B "inaccessible"     # cn1B
nvmet_add_anchor "$NQN_FAKE_CN1" 1
DNV_SERIAL="$DEMO_SERIAL" DNV_MODEL="$DEMO_MODEL" DNV_CNTLID_MIN=256 DNV_CNTLID_MAX=511 \
    nvmet_add_subsys "$NQN_EXP" "/dev/mapper/$DM_CN1_LIN" "$DEMO_UUID" $GRP_B "$HOSTNQN_HOST0"
nvmet_link "$NQN_EXP" 1
_info "cn1 ready: subsys-host0-cn1=$NQN_EXP ns->/dev/mapper/$DM_CN1_LIN grp=$GRP_B(cn1B,inaccessible)"
EOF_BODY
}

# ===================== stage: host ==========================================
do_host() {
    _info "=== host0 ($HOST0_IP): connect both paths, verify multipath ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
prep_node
mp_connect "$CN0_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
mp_connect "$CN1_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
sleep 1
echo "--- nvme list-subsys ---"; sudo nvme list-subsys 2>/dev/null
DEV=$(mp_ns_dev "$NQN_EXP") || _fail "host0: no multipath namespace for $NQN_EXP"
_info "host0 volume: $DEV ($(sudo blockdev --getsize64 "$DEV") bytes)"
_info "paths: total=$(ctrl_count "$NQN_EXP" '') cn0=$(ctrl_count "$NQN_EXP" "$CN0_IP") cn1=$(ctrl_count "$NQN_EXP" "$CN1_IP")"
ana_state_of "$NQN_EXP"
EOF_BODY

    _info "starting the O_DIRECT I/O generator (host0_io.sh)"
    "$(dirname "$0")/host0_io.sh" start
    sleep 3
    "$(dirname "$0")/host0_io.sh" status
    "$(dirname "$0")/host0_io.sh" mark steady_state
}

# ===================== failover =============================================
# The three nodes run their own sequences CONCURRENTLY. Nothing coordinates them
# except what a node can observe locally: cn0 and cn1 each wait on the ANA state
# of their dn0 namespace (sysfs, no query to dn0), and dn0 has only its own
# "sleep 3" before releasing the parked lin-cn0. Within a node the order is
# exactly the one specified.
#
# Key steps and the WINDOW each one opens (the id passed to _pause):
#   dn0-unlink    subsystems off dn0's port  -> until dn0-relink, cn0/cn1 cannot
#                 reach dn0 and their reconnects are refused with DNR
#   dn0-ns-cn0    subsys-cn0's ns in dn0B
#   dn0-ns-cn1    subsys-cn1's ns in dn0A
#   dn0-suspend   lin-cn0 parked (I/O that reaches it stops there)
#   dn0-lin-cn1   lin-cn1 on the loop, subsystems still off the port
#   dn0-relink    subsystems back on the port, before the final lin-cn0 swap
#   cn1-wait      cn1 knows it is optimized but has not taken the data path yet
#   cn1-reload    cn1's dm-linear is on real data but its export is still cn1B
#   cn1-unlink    cn1's export OFF its port  <-- host0 loses this path if it
#                 reconnects during the window
#   cn1-ns        export ns in cn1A, still off the port
#   cn0-wait      cn0 knows it is inaccessible but still exports as optimized
#   cn0-unlink    cn0's export off its port
#   cn0-reload    cn0's dm-linear on its local dm-error, export still cn0A
#   cn0-ns        export ns in cn0B, still off the port

# dn0: unlink both subsystems, swap the two namespaces between dn0A and dn0B,
# park lin-cn0 (suspended), move lin-cn1 onto the loop, relink both, then after
# its own sleep 3 release the parked lin-cn0 onto dm-error-cn0.
do_failover_dn0() {
    _info "=== dn0: swap ana groups, park lin-cn0, bring lin-cn1 onto the loop ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
_stamp "failover start"
LOOP=$(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: 'NR==1{print $1}')
[ -n "$LOOP" ] || _fail "dn0: no loop for $LOOP_IMG"

# 1. remove both subsystems from the port (the anchor keeps the port listening)
nvmet_unlink "$NQN_DN_CN0" 1
nvmet_unlink "$NQN_DN_CN1" 1
_stamp "step 1 done: port 1 carries $(ls "$NVMET/ports/1/subsystems" | tr '\n' ' ')"
_pause dn0-unlink

# 2. subsys-cn0's namespace: dn0A (optimized) -> dn0B (inaccessible)
ns_move_ana "$NQN_DN_CN0" $GRP_B
_stamp "step 2 done: subsys-cn0 ns -> dn0B"
_pause dn0-ns-cn0

# 3. subsys-cn1's namespace: dn0B (inaccessible) -> dn0A (optimized)
ns_move_ana "$NQN_DN_CN1" $GRP_A
_stamp "step 3 done: subsys-cn1 ns -> dn0A"
_pause dn0-ns-cn1

# 4. park dm-linear-cn0: it stays SUSPENDED until step 7, so anything still
#    queued for it waits there instead of failing immediately.
dm_suspend_deadline "$DM_DN0_LIN_CN0" 15
_stamp "step 4 done: $DM_DN0_LIN_CN0 suspended"
_pause dn0-suspend

# 5. dm-linear-cn1: dm-error-cn1 -> the loop, i.e. subsys-cn1 now serves the
#    real data. (lin-cn1 is live, so this is a full suspend/load/resume.)
dm_reload "$DM_DN0_LIN_CN1" "0 $SEC_DEMO linear $LOOP 0"
_stamp "step 5 done: lin-cn1 -> loop"
_pause dn0-lin-cn1

# 6. put both subsystems back on the port; cn0 and cn1 reconnect on their own
nvmet_link "$NQN_DN_CN0" 1
nvmet_link "$NQN_DN_CN1" 1
_stamp "step 6 done: port 1 carries $(ls "$NVMET/ports/1/subsystems" | tr '\n' ' ')"
_pause dn0-relink

# 7. dn0's only synchronisation with cn0 is this sleep: release the parked
#    lin-cn0 onto dm-error-cn0 so anything queued there fails fast.
sleep 3
dm_load_resume "$DM_DN0_LIN_CN0" "0 $SEC_DEMO linear /dev/mapper/$DM_DN0_ERR_CN0 0"
_stamp "step 7 done: lin-cn0 -> err-cn0, state=$(dm_state "$DM_DN0_LIN_CN0")"
_info "dn0 done: subsys-cn0 ns grp=$(sudo cat "$NVMET/subsystems/$NQN_DN_CN0/namespaces/1/ana_grpid") subsys-cn1 ns grp=$(sudo cat "$NVMET/subsystems/$NQN_DN_CN1/namespaces/1/ana_grpid")"
EOF_BODY
}

# cn1: wait (locally) for subsys-cn1 to read Optimized, move its dm-linear onto
# that namespace, then promote its own export from cn1B to cn1A.
do_failover_cn1() {
    _info "=== cn1: wait Optimized, reload onto subsys-cn1, promote export to cn1A ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
_stamp "failover start"
# 1. purely local check: sysfs ana_state, refreshed from the ANA log on the AEN
wait_ana "$NQN_DN_CN1" "optimized" 200 || _stamp "WARNING: cn1 never saw optimized"
_stamp "step 1 done: subsys-cn1 reads optimized"
_pause cn1-wait

# 2. reload the dm-linear from the local dm-error onto subsys-cn1's namespace
DEV=$(nvme_wait_dev "$NQN_DN_CN1" 150) || _fail "cn1: no block device for $NQN_DN_CN1 -- cannot take over"
dm_reload "$DM_CN1_LIN" "0 $SEC_DEMO linear $DEV 0"
_stamp "step 2 done: cn1 dm-linear -> $DEV"
_pause cn1-reload

# 3-5. move the export's namespace from cn1B (inaccessible) to cn1A (optimized):
#      unlink from the port, move, relink. host0's cn1 controllers reconnect and
#      see the path go optimized, so it starts routing I/O here.
nvmet_unlink "$NQN_EXP" 1
_stamp "step 3 done: subsys-host0-cn1 off port 1"
_pause cn1-unlink

ns_move_ana "$NQN_EXP" $GRP_A
_stamp "step 4 done: export ns -> cn1A"
_pause cn1-ns

nvmet_link "$NQN_EXP" 1
_stamp "step 5 done: subsys-host0-cn1 back on port 1"
EOF_BODY
}

# cn0: wait (locally) for subsys-cn0 to read Inaccessible, take its export off
# the port, release the backend onto the local dm-error, demote the export to
# cn0B and put it back on the port as a standby path.
do_failover_cn0() {
    _info "=== cn0: wait Inaccessible, step the export down, come back as cn0B standby ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
_stamp "failover start"
wait_ana "$NQN_DN_CN0" "inaccessible" 200 || _stamp "WARNING: cn0 never saw inaccessible"
_stamp "step 1 done: subsys-cn0 reads inaccessible"
_pause cn0-wait

nvmet_unlink "$NQN_EXP" 1
_stamp "step 2 done: subsys-host0-cn0 off port 1"
_pause cn0-unlink

# The suspend below blocks on the bio the nvme driver requeued the moment
# subsys-cn0 went inaccessible ("block nvme0n1: no usable path - requeuing I/O").
# That bio was never sent to dn0, so dn0 resuming lin-cn0 onto its dm-error does
# NOT free it -- it would sit there until a path went optimized again, which
# never happens. Deleting cn0's controller for subsys-cn0 is the escape hatch:
# the namespace is marked dead and every requeued bio fails at once. cn0 has no
# further use for that namespace -- its export is moving to the local dm-error.
dm_suspend_deadline "$DM_CN0_LIN" 5 sudo nvme disconnect -n "$NQN_DN_CN0"
dm_load_resume "$DM_CN0_LIN" "0 $SEC_DEMO linear /dev/mapper/$DM_CN0_ERR 0"
_stamp "step 3 done: cn0 dm-linear -> local dm-error"
_pause cn0-reload

ns_move_ana "$NQN_EXP" $GRP_B
_stamp "step 4 done: export ns -> cn0B"
_pause cn0-ns

nvmet_link "$NQN_EXP" 1
_stamp "step 5 done: subsys-host0-cn0 back on port 1"
EOF_BODY
}

# All three nodes at once. Each runs its own ordered sequence; the only
# synchronisation is what each can observe locally.
do_failover() {
    local tmp d0 d1 d2
    tmp=$(mktemp -d)
    _info "=== parallel failover (FO_DELAY=${FO_DELAY}s${FO_ONLY:+, only step $FO_ONLY}) ==="
    "$(dirname "$0")/host0_io.sh" mark failover_start >/dev/null
    do_failover_dn0 >"$tmp/dn0.log" 2>&1 & d0=$!
    do_failover_cn1 >"$tmp/cn1.log" 2>&1 & d1=$!
    do_failover_cn0 >"$tmp/cn0.log" 2>&1 & d2=$!
    wait $d0 $d1 $d2
    "$(dirname "$0")/host0_io.sh" mark failover_end >/dev/null
    # interleave the three logs by their [HH:MM:SS.mmm] stamps; unstamped lines
    # keep their position by inheriting the previous stamp.
    awk '{ if ($1 ~ /^\[[0-9][0-9]:/) last=$1; printf "%s\t%s\n", last, $0 }' \
        "$tmp/dn0.log" "$tmp/cn1.log" "$tmp/cn0.log" | sort -s -k1,1 | cut -f2-
    rm -rf "$tmp"
    _info "=== parallel failover finished ==="
}

# ===================== stage: verify host0 ==================================
# A path whose subsystem is taken off the exporting node's port does NOT come
# back by itself: while it is unlinked, the host's reconnect attempt is answered
# with "Connect Invalid Data Parameter" (invalid subsystem), which carries DNR,
# so the driver stops retrying and removes the controller for good:
#     nvme nvme0: Connect Invalid Data Parameter, subsysnqn "...:exp0"
#     nvme nvme0: Failed reconnect attempt 1/600
#     nvme nvme0: Removing controller (16770)...
# Whether it happens depends on the race between the host's reconnect timer and
# the unlink/relink window, so this stage waits a moment and then re-issues the
# connect for whichever path is missing. (In a real deployment that is what the
# discovery controller + nvme-stas would do on the "discovery log changed" AEN.)
do_verify_host() {
    _info "=== host0: verify both cn0 and cn1 paths are back ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
for i in $(seq 20); do
    c0=$(ctrl_count "$NQN_EXP" "$CN0_IP"); c1=$(ctrl_count "$NQN_EXP" "$CN1_IP")
    [ "$c0" -ge 1 ] && [ "$c1" -ge 1 ] && break
    sleep 0.5
done
if [ "$c0" -lt 1 ] || [ "$c1" -lt 1 ]; then
    _warn "path(s) removed by the DNR reconnect refusal (cn0=$c0 cn1=$c1) -- re-connecting"
    [ "$c0" -lt 1 ] && mp_connect "$CN0_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
    [ "$c1" -lt 1 ] && mp_connect "$CN1_IP" "$NQN_EXP" "$HOSTNQN_HOST0" "$HOSTID_HOST0"
    sleep 1
    c0=$(ctrl_count "$NQN_EXP" "$CN0_IP"); c1=$(ctrl_count "$NQN_EXP" "$CN1_IP")
fi
_info "controllers: cn0=$c0 cn1=$c1 (total $(ctrl_count "$NQN_EXP" ''))"
wait_ctrl_live "$NQN_EXP" "$CN0_IP" 120
wait_ctrl_live "$NQN_EXP" "$CN1_IP" 120
echo "--- nvme list-subsys ---"; sudo nvme list-subsys 2>/dev/null
echo "--- per-path ANA state ---"; ana_state_of "$NQN_EXP"
DEV=$(mp_ns_dev "$NQN_EXP") && _info "volume still present: $DEV"
[ "$c0" -ge 1 ] && [ "$c1" -ge 1 ] && _ok "host0 is connected to BOTH cn0 and cn1" \
                                     || _warn "host0 is NOT connected to both cn0 and cn1"
EOF_BODY
}

# ===================== timing experiment ====================================
# One run = fresh stack, steady-state I/O, one parallel failover at the current
# FO_DELAY/FO_ONLY, a soak, then a one-line verdict. Everything is torn down and
# rebuilt between runs because the failover is one-directional.
SOAK="${SOAK:-6}"
RESULTS="${RESULTS:-/tmp/ana5-results.tsv}"

# host0's path state WITHOUT repairing anything -- the experiment is about what
# survives on its own.
probe_paths() {
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
s=""
for c in /sys/class/nvme/nvme*; do
    [ -r "$c/subsysnqn" ] || continue
    [ "$(cat "$c/subsysnqn" 2>/dev/null)" = "$NQN_EXP" ] || continue
    a=$(cat "$c/address" 2>/dev/null); ip=${a#*traddr=}; ip=${ip%%,*}
    an="-"
    for ns in "$c"/nvme[0-9]*n[0-9]*; do [ -e "$ns/ana_state" ] && an=$(cat "$ns/ana_state"); done
    case "$ip" in "$CN0_IP") nm=cn0 ;; "$CN1_IP") nm=cn1 ;; *) nm=$ip ;; esac
    s="$s $nm=$(cat "$c/state" 2>/dev/null)/$an"
done
echo "PATHS n=$(ctrl_count "$NQN_EXP" '')$s"
EOF_BODY
}

# Condense the whole I/O log into one line, split on the failover marks.
summarize_run() {  # <label> <paths>
    local label="$1" paths="$2" tmp
    tmp=$(mktemp)
    _ssh "${SSH_USER}@${HOST0_IP}" "sudo cat /tmp/dnv-io.jsonl" > "$tmp" 2>/dev/null
    python3 - "$tmp" "$label" "$paths" "$RESULTS" <<'EOF_PY'
import json, sys
path, label, paths, out = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]
recs, marks = [], {}
for line in open(path):
    try:    d = json.loads(line)
    except ValueError: continue
    if d.get('op') == 'mark': marks[d['label']] = d['start_ts']
    elif d.get('op') != 'event': recs.append(d)
t0 = marks.get('failover_start'); t1 = marks.get('failover_end')
after = [r for r in recs if t0 and r['stop_ts'] > t0]
fails = [r for r in after if not r['ok']]
# The stall the application sees: the longest single I/O, plus the longest gap
# with no completion at all (which is what a hung path looks like).
maxlat = max((r['lat_ms'] for r in after), default=0.0)
gap, prev = 0.0, t0
for r in sorted(after, key=lambda r: r['stop_ts']):
    gap = max(gap, r['stop_ts'] - prev); prev = r['stop_ts']
tail = (max(r['stop_ts'] for r in recs) if recs else t0)
first = ('+%.1fs' % (min(r['stop_ts'] for r in fails) - t0)) if fails else '-'
errs = sorted({r['err'] for r in fails})[:2]
row = ('%-22s IOs=%-6d fail=%-5d first_fail=%-8s max_lat=%8.0fms max_gap=%6.1fs  %s  %s'
       % (label, len(after), len(fails), first, maxlat, max(gap, tail - prev),
          paths, ('errs=' + '; '.join(errs)) if errs else ''))
print(row)
open(out, 'a').write(row + '\n')
EOF_PY
    rm -f "$tmp"
}

do_run_once() {  # <label>
    local label="$1" log
    log=$(mktemp)
    _info "######## RUN: $label ########"
    { do_teardown; do_prep; do_setup; do_host; } >"$log" 2>&1 \
        || { _warn "setup failed, see $log"; tail -20 "$log"; return 1; }
    rm -f "$log"
    do_failover
    _info "soaking ${SOAK}s"
    sleep "$SOAK"
    local paths
    paths=$(probe_paths | grep '^PATHS' || echo "PATHS ?")
    "$(dirname "$0")/host0_io.sh" stop >/dev/null 2>&1
    summarize_run "$label" "$paths"
}

# Uniform sweep: the same stall after EVERY key step, to find the point at which
# the failover stops being transparent.
do_sweep() {  # <delay> ...
    local d
    for d in "$@"; do FO_DELAY="$d"; FO_ONLY=""; do_run_once "D=${d}s all-steps"; done
    _info "=== sweep results ($RESULTS) ==="; cat "$RESULTS"
}

# Attribution: the same stall applied to exactly ONE step, so a threshold can be
# blamed on a specific window.
do_attrib() {  # <delay> <step-id> ...
    local d="$1" s; shift
    for s in "$@"; do FO_DELAY="$d"; FO_ONLY="$s"; do_run_once "D=${d}s only=$s"; done
    _info "=== attribution results ($RESULTS) ==="; cat "$RESULTS"
}

# ===================== stage: report ========================================
do_showreport() {
    _info "letting the workload run 8s past the failover so the 'after' phase has data"
    sleep 8
    "$(dirname "$0")/host0_io.sh" stop
    "$(dirname "$0")/host0_io.sh" report
}

# ===================== stage: diagnose ======================================
do_diagnose() {
    local n ip
    for n in dn0 cn0 cn1 host0; do
        ip=$(ipof "$n")
        _info "=== diagnose $n ($ip) ==="
        run_on "$n" "$ip" <<'EOF_BODY'
set -uo pipefail
echo "== nvmet ports =="
for p in "$NVMET"/ports/*; do
    [ -d "$p" ] || continue
    echo "port $(basename "$p") $(sudo cat "$p/addr_traddr" 2>/dev/null):$(sudo cat "$p/addr_trsvcid" 2>/dev/null)"
    for g in "$p"/ana_groups/*; do
        [ -d "$g" ] || continue
        echo "   ana group $(basename "$g") = $(sudo cat "$g/ana_state" 2>/dev/null)"
    done
    echo "   subsystems: $(ls "$p/subsystems" 2>/dev/null | tr '\n' ' ')"
done
echo "== nvmet subsystems =="
for s in "$NVMET"/subsystems/*; do
    [ -d "$s" ] || continue
    echo "$(basename "$s")"
    for n in "$s"/namespaces/*; do
        [ -d "$n" ] || continue
        echo "   ns $(basename "$n"): enable=$(sudo cat "$n/enable" 2>/dev/null) grp=$(sudo cat "$n/ana_grpid" 2>/dev/null) dev=$(sudo cat "$n/device_path" 2>/dev/null)"
    done
done
echo "== dm =="; sudo dmsetup ls 2>/dev/null | grep dnv- | while read -r nm _; do
    echo "   $nm state=$(dm_state "$nm") table=[$(sudo dmsetup table "$nm")]"
done
echo "== nvme host side =="
for c in /sys/class/nvme/nvme*; do
    [ -r "$c/subsysnqn" ] || continue
    echo "   $(basename "$c") state=$(cat "$c/state" 2>/dev/null) nqn=$(cat "$c/subsysnqn")"
    for ns in "$c"/nvme[0-9]*n[0-9]*; do
        [ -e "$ns/ana_state" ] || continue
        echo "      $(basename "$ns") grp=$(cat "$ns/ana_grpid" 2>/dev/null) state=$(cat "$ns/ana_state" 2>/dev/null)"
    done
done
EOF_BODY
    done
}

# ===================== stage: status ========================================
do_status() {
    local n ip
    for n in dn0 cn0 cn1 host0; do
        ip=$(ipof "$n")
        _info "=== status $n ($ip) ==="
        run_on "$n" "$ip" <<'EOF_BODY'
set -uo pipefail
echo "host=$(uname -n)"
echo "  dm:          $(sudo dmsetup ls 2>/dev/null | awk '{print $1}' | grep dnv- | tr '\n' ' ')"
echo "  nvmet subsys:$(ls "$NVMET/subsystems" 2>/dev/null | tr '\n' ' ')"
echo "  nvmet ports: $(ls "$NVMET/ports" 2>/dev/null | tr '\n' ' ')"
echo "  nvme ctrls:  $(ls /sys/class/nvme/ 2>/dev/null | tr '\n' ' ')"
echo "  nvme subsys: $(ls /sys/class/nvme-subsystem/ 2>/dev/null | tr '\n' ' ')"
echo "  loops:       $(sudo losetup -a 2>/dev/null | grep -E 'ana5|dnv' | tr '\n' ' ')"
EOF_BODY
    done
}

# ===================== stage: teardown ======================================
do_teardown() {
    _info "=== teardown host0 ==="
    run_on host0 "$HOST0_IP" <<'EOF_BODY'
set -uo pipefail
sudo pkill -f dnv-io-runner.py 2>/dev/null; true
sudo nvme disconnect -n "$NQN_EXP" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
cleanup_node_common
_info "host0: nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l) subsys=$(ls /sys/class/nvme-subsystem/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown cn0 ==="
    run_on cn0 "$CN0_IP" <<'EOF_BODY'
set -uo pipefail
# order: drop the export (releases nvmet's hold on the dm device), then the dm
# stack (it holds the imported namespace open), only then disconnect.
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_subsys "$NQN_FAKE_CN0"
nvmet_remove_port 1
dnv_remove_all_dm
sudo nvme disconnect -n "$NQN_DN_CN0" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
cleanup_node_common
_info "cn0: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown cn1 ==="
    run_on cn1 "$CN1_IP" <<'EOF_BODY'
set -uo pipefail
nvmet_remove_subsys "$NQN_EXP"
nvmet_remove_subsys "$NQN_FAKE_CN1"
nvmet_remove_port 1
dnv_remove_all_dm
sudo nvme disconnect -n "$NQN_DN_CN1" 2>/dev/null || true
nvme_disconnect_all_dnv_ld 2>/dev/null || true
cleanup_node_common
_info "cn1: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) nvme=$(ls /sys/class/nvme/ 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== teardown dn0 ==="
    run_on dn0 "$DN0_IP" <<'EOF_BODY'
set -uo pipefail
nvmet_remove_subsys "$NQN_DN_CN0"
nvmet_remove_subsys "$NQN_DN_CN1"
nvmet_remove_subsys "$NQN_FAKE_DN0"
nvmet_remove_port 1
nvmet_remove_host "$HOSTNQN_CN0"
nvmet_remove_host "$HOSTNQN_CN1"
dnv_defuse_all 2>/dev/null || true
dnv_remove_all_dm
for LOOP in $(sudo losetup -j "$LOOP_IMG" 2>/dev/null | awk -F: '{print $1}'); do
    _t "losetup -d $LOOP" sudo losetup -d "$LOOP" >/dev/null 2>&1 && _info "detached $LOOP" || _warn "could not detach $LOOP"
done
sudo rm -f "$LOOP_IMG" 2>/dev/null || true
cleanup_node_common
_info "dn0: dm=$(dm_count) nvmet=$(ls "$NVMET/subsystems" 2>/dev/null|wc -l) ports=$(ls "$NVMET/ports" 2>/dev/null|wc -l)"
EOF_BODY

    _info "=== post-teardown status ==="
    do_status
}

# ===================== dispatch =============================================
case "${1:-}" in
    prep)               do_prep ;;
    setup)              do_setup ;;
    host)               do_host ;;
    failover)           do_failover ;;
    failover-dn0)       do_failover_dn0 ;;
    failover-cn1)       do_failover_cn1 ;;
    failover-cn0)       do_failover_cn0 ;;
    run-once)           do_run_once "${2:-D=${FO_DELAY}s${FO_ONLY:+ only=$FO_ONLY}}" ;;
    sweep)              shift; do_sweep "$@" ;;
    attrib)             shift; do_attrib "$@" ;;
    verify)             do_verify_host ;;
    showreport)         do_showreport ;;
    diagnose)           do_diagnose ;;
    status)             do_status ;;
    teardown)           do_teardown ;;
    *) echo "Usage: $0 prep|setup|host|failover|verify|showreport|diagnose|status|teardown" >&2
       echo "       $0 run-once [label]        one instrumented run (FO_DELAY/FO_ONLY env)" >&2
       echo "       $0 sweep <delay> ...       same stall after every key step" >&2
       echo "       $0 attrib <delay> <step-id> ...   stall one step only" >&2
       exit 1 ;;
esac
