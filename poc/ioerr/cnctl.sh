#!/bin/bash
#
# cnctl.sh -- controller-node (target) side helper for the NVMe-oF IO-error
#             test matrix.  Runs ON cn0 / cn1, always via sudo.
#
# A "lane" is one fully independent target stack:
#     dm-linear slice on a shared loop file      (so dm-error / suspend is
#                                                 per-lane)
#   + one nvmet subsystem (unique NQN)
#   + one nvmet port with its own TCP port       (so iptables and
#                                                 "port stops listening" are
#                                                 per-lane)
#   + one anchor subsystem, normally NOT linked  (used only by fault d)
#
# Lanes are independent, so many test cases can run concurrently on one node.
# Every mutation is idempotent so a lane can be rebuilt without a teardown.
#
set -u
CFG=/sys/kernel/config/nvmet
IMG=/var/tmp/dnv-ioerr.img
LOOPF=/var/tmp/dnv-ioerr.loop
HNQNF=/var/tmp/dnv-ioerr.hostnqn
SLICE_SECTORS=131072          # 64 MiB per lane
NLANES_MAX=64

die(){ echo "cnctl: $*" >&2; exit 1; }
now(){ date +%s.%N; }

# cfg_set -- write a configfs attribute only if it differs.  attr_serial reads
# back space-padded, so compare on the trimmed value.
cfg_set(){
    local f="$1" v="$2" cur
    cur=$(cat "$f" 2>/dev/null | sed 's/[[:space:]]*$//')
    [ "$cur" = "$v" ] && return 0
    echo "$v" > "$f" 2>/dev/null || echo "cnctl: WARN cannot set $f=$v" >&2
}

lane_dm(){   echo "dnvio-$1"; }
lane_port(){ echo "$((200 + $1))"; }          # nvmet port id

# ---------------------------------------------------------------- init ------
cmd_init(){    # <hostnqn>
    local hostnqn="$1"
    modprobe nvmet    2>/dev/null
    modprobe nvmet-tcp 2>/dev/null
    modprobe dm-mod   2>/dev/null
    modprobe loop     2>/dev/null

    # Keep udev out of the dm devices.  A suspended or error-target dm device
    # that udev tries to blkid wedges the udev worker for the whole test.
    # OPTIONS+="ignore_device" is obsolete; the DM_UDEV_DISABLE_* env flags are
    # the supported way and must land before 60-persistent-storage.rules.
    cat > /etc/udev/rules.d/58-dnv-ioerr.rules <<'EOF'
ACTION=="add|change", SUBSYSTEM=="block", KERNEL=="dm-*", \
  ENV{DM_UDEV_DISABLE_DISK_RULES_FLAG}="1", \
  ENV{DM_UDEV_DISABLE_OTHER_RULES_FLAG}="1", \
  ENV{DM_UDEV_DISABLE_SUBSYSTEM_RULES_FLAG}="1"
EOF
    udevadm control --reload >/dev/null 2>&1

    echo "$hostnqn" > "$HNQNF"
    mkdir -p "$CFG/hosts/$hostnqn" 2>/dev/null

    [ -f "$IMG" ] || truncate -s 8G "$IMG"
    local lo
    lo=$(losetup -j "$IMG" | head -1 | cut -d: -f1)
    if [ -z "$lo" ]; then
        losetup -f "$IMG" || die "losetup failed"
        lo=$(losetup -j "$IMG" | head -1 | cut -d: -f1)
    fi
    [ -n "$lo" ] || die "no loop device for $IMG"
    echo "$lo" > "$LOOPF"
    echo "init ok loop=$lo hostnqn=$hostnqn"
}

# ------------------------------------------------------------ lane-create ---
# lane-create <lane> <tcpport> <nqn> <uuid> <myip> <cntlid_min> <cntlid_max>
#             <ana_state> <serial> <model>
cmd_lane_create(){
    local lane="$1" tport="$2" nqn="$3" uuid="$4" myip="$5"
    local cmin="$6" cmax="$7" ana="$8" serial="$9" model="${10}"
    local lo off dm port s a
    lo=$(cat "$LOOPF") || die "run init first"
    off=$(( lane * SLICE_SECTORS ))
    dm=$(lane_dm "$lane"); port=$(lane_port "$lane")

    # --- backing dm-linear slice -------------------------------------------
    if dmsetup info "$dm" >/dev/null 2>&1; then
        dmsetup suspend --nolockfs --noflush "$dm" 2>/dev/null
        dmsetup load "$dm" --table "0 $SLICE_SECTORS linear $lo $off" 2>/dev/null
        dmsetup resume "$dm" 2>/dev/null
    else
        echo "0 $SLICE_SECTORS linear $lo $off" | \
            dmsetup create --noudevsync "$dm" || die "dmsetup create $dm"
    fi

    # --- subsystem ----------------------------------------------------------
    s="$CFG/subsystems/$nqn"
    mkdir -p "$s" 2>/dev/null || die "mkdir subsys $nqn"
    cfg_set "$s/attr_serial"      "$serial"
    cfg_set "$s/attr_model"       "$model"
    cfg_set "$s/attr_cntlid_min"  "$cmin"
    cfg_set "$s/attr_cntlid_max"  "$cmax"
    cfg_set "$s/attr_allow_any_host" "0"
    local hnqn; hnqn=$(cat "$HNQNF")
    [ -e "$s/allowed_hosts/$hnqn" ] || ln -s "$CFG/hosts/$hnqn" "$s/allowed_hosts/$hnqn" 2>/dev/null

    mkdir -p "$s/namespaces/1" 2>/dev/null
    if [ "$(cat "$s/namespaces/1/enable" 2>/dev/null)" != "1" ]; then
        cfg_set "$s/namespaces/1/device_path"  "/dev/mapper/$dm"
        cfg_set "$s/namespaces/1/device_uuid"  "$uuid"
        cfg_set "$s/namespaces/1/device_nguid" "$uuid"
        cfg_set "$s/namespaces/1/ana_grpid"    "1"
        echo 1 > "$s/namespaces/1/enable" || die "enable ns $nqn"
    fi

    # --- anchor subsystem (fault d only): no namespace, no allowed host ------
    a="$CFG/subsystems/${nqn}-anchor"
    mkdir -p "$a" 2>/dev/null
    cfg_set "$a/attr_allow_any_host" "0"

    # --- port ---------------------------------------------------------------
    mkdir -p "$CFG/ports/$port" 2>/dev/null
    cfg_set "$CFG/ports/$port/addr_adrfam"  "ipv4"
    cfg_set "$CFG/ports/$port/addr_trtype"  "tcp"
    cfg_set "$CFG/ports/$port/addr_traddr"  "$myip"
    cfg_set "$CFG/ports/$port/addr_trsvcid" "$tport"
    [ -e "$CFG/ports/$port/subsystems/$nqn" ] || \
        ln -s "$s" "$CFG/ports/$port/subsystems/$nqn" 2>/dev/null
    cfg_set "$CFG/ports/$port/ana_groups/1/ana_state" "$ana"
    echo "lane $lane ready: $nqn @ $myip:$tport ana=$ana dm=$dm port=$port"
}

# -------------------------------------------------------------- lane-ana ----
cmd_lane_ana(){    # <lane> <state>
    local port; port=$(lane_port "$1")
    echo "$2" > "$CFG/ports/$port/ana_groups/1/ana_state" || die "ana_state"
    echo "lane $1 ana=$2"
}

# ------------------------------------------------------------- lane-fault ---
# lane-fault <lane> <nqn> <case a..h> <apply|clear>
cmd_lane_fault(){
    local lane="$1" nqn="$2" c="$3" act="$4"
    local dm port s a hnqn tport t0
    dm=$(lane_dm "$lane"); port=$(lane_port "$lane")
    s="$CFG/subsystems/$nqn"; a="$CFG/subsystems/${nqn}-anchor"
    hnqn=$(cat "$HNQNF")
    tport=$(cat "$CFG/ports/$port/addr_trsvcid")
    t0=$(now)

    case "$c:$act" in
    # (a) drop every packet the host sends to the NVMe port.  Matching on
    #     --dport keeps ssh alive; an established connection still dies because
    #     nothing from the host reaches the target any more.
    a:apply) iptables -C INPUT -p tcp --dport "$tport" -j DROP 2>/dev/null || \
             iptables -I INPUT 1 -p tcp --dport "$tport" -j DROP ;;
    a:clear) while iptables -C INPUT -p tcp --dport "$tport" -j DROP 2>/dev/null; do
                 iptables -D INPUT -p tcp --dport "$tport" -j DROP; done ;;

    # (b) answer every packet from the host with a TCP RST.
    b:apply) iptables -C INPUT -p tcp --dport "$tport" -j REJECT --reject-with tcp-reset 2>/dev/null || \
             iptables -I INPUT 1 -p tcp --dport "$tport" -j REJECT --reject-with tcp-reset ;;
    b:clear) while iptables -C INPUT -p tcp --dport "$tport" -j REJECT --reject-with tcp-reset 2>/dev/null; do
                 iptables -D INPUT -p tcp --dport "$tport" -j REJECT --reject-with tcp-reset; done ;;

    # (c) subsystem off the port AND nothing else on the port -> nvmet tears the
    #     TCP listener down entirely, so the host gets ECONNREFUSED.
    c:apply) rm -f "$CFG/ports/$port/subsystems/$nqn" ;;
    c:clear) [ -e "$CFG/ports/$port/subsystems/$nqn" ] || ln -s "$s" "$CFG/ports/$port/subsystems/$nqn" ;;

    # (d) same, but an anchor subsystem keeps the listener up: the host now
    #     reaches a live target that does not know this NQN.
    d:apply) [ -e "$CFG/ports/$port/subsystems/${nqn}-anchor" ] || \
                 ln -s "$a" "$CFG/ports/$port/subsystems/${nqn}-anchor"
             rm -f "$CFG/ports/$port/subsystems/$nqn" ;;
    d:clear) [ -e "$CFG/ports/$port/subsystems/$nqn" ] || ln -s "$s" "$CFG/ports/$port/subsystems/$nqn"
             rm -f "$CFG/ports/$port/subsystems/${nqn}-anchor" ;;

    # (e) host no longer authorised.  nvmet checks allowed_hosts at connect
    #     time only, so this is a reconnect-time fault, not a live-IO fault.
    e:apply) rm -f "$s/allowed_hosts/$hnqn" ;;
    e:clear) [ -e "$s/allowed_hosts/$hnqn" ] || ln -s "$CFG/hosts/$hnqn" "$s/allowed_hosts/$hnqn" ;;

    # (f) namespace disabled.  Blocks until the ns percpu ref drains, so run it
    #     under a timeout and report if it wedged.
    f:apply) timeout 60 bash -c "echo 0 > '$s/namespaces/1/enable'" \
                 || echo "cnctl: WARN ns disable timed out" >&2 ;;
    f:clear) timeout 60 bash -c "echo 1 > '$s/namespaces/1/enable'" \
                 || echo "cnctl: WARN ns enable timed out" >&2 ;;

    # (g) backend returns EIO for every bio.
    g:apply) dmsetup suspend --nolockfs "$dm"
             dmsetup load "$dm" --table "0 $SLICE_SECTORS error"
             dmsetup resume "$dm" ;;
    g:clear) local lo off; lo=$(cat "$LOOPF"); off=$(( lane * SLICE_SECTORS ))
             dmsetup suspend --nolockfs --noflush "$dm"
             dmsetup load "$dm" --table "0 $SLICE_SECTORS linear $lo $off"
             dmsetup resume "$dm" ;;

    # (h) backend swallows every bio and never completes it.
    h:apply) dmsetup suspend --nolockfs --noflush "$dm" ;;
    h:clear) dmsetup resume "$dm" 2>/dev/null || true ;;

    none:apply|none:clear) : ;;
    *) die "bad fault $c:$act" ;;
    esac
    echo "FAULT lane=$lane case=$c act=$act t=$t0 done=$(now)"
}

# ---------------------------------------------------------------- status ----
cmd_lane_status(){   # <lane> <nqn>
    local lane="$1" nqn="$2" port dm
    port=$(lane_port "$lane"); dm=$(lane_dm "$lane")
    echo "lane=$lane port=$port listen_subsys=[$(ls "$CFG/ports/$port/subsystems" 2>/dev/null | tr '\n' ' ')]"
    echo "  ana=$(cat "$CFG/ports/$port/ana_groups/1/ana_state" 2>/dev/null)"
    echo "  ns_enable=$(cat "$CFG/subsystems/$nqn/namespaces/1/enable" 2>/dev/null)"
    echo "  allowed=[$(ls "$CFG/subsystems/$nqn/allowed_hosts" 2>/dev/null | tr '\n' ' ')]"
    echo "  dm=$(dmsetup table "$dm" 2>/dev/null) susp=$(dmsetup info -c --noheadings -o suspended "$dm" 2>/dev/null)"
}

# --------------------------------------------------------------- cleanup ----
cmd_cleanup(){
    iptables -S INPUT | grep -oP '(?<=-A INPUT ).*--dport 4[45]\d\d .*' | while read -r r; do
        eval "iptables -D INPUT $r" 2>/dev/null
    done
    iptables -F INPUT 2>/dev/null
    local p n
    for p in "$CFG"/ports/*; do
        [ -d "$p" ] || continue
        for n in "$p"/subsystems/*; do [ -e "$n" ] && rm -f "$n"; done
        for n in "$p"/referrals/*; do [ -d "$n" ] && rmdir "$n"; done
        rmdir "$p" 2>/dev/null
    done
    for n in "$CFG"/subsystems/*; do
        [ -d "$n" ] || continue
        [ -d "$n/namespaces/1" ] && { echo 0 > "$n/namespaces/1/enable" 2>/dev/null; rmdir "$n/namespaces/1" 2>/dev/null; }
        for h in "$n"/allowed_hosts/*; do [ -e "$h" ] && rm -f "$h"; done
        rmdir "$n" 2>/dev/null
    done
    for n in "$CFG"/hosts/*; do [ -d "$n" ] && rmdir "$n" 2>/dev/null; done
    for d in $(dmsetup ls 2>/dev/null | awk '/^dnvio-/{print $1}'); do
        dmsetup resume "$d" 2>/dev/null
        dmsetup remove --force "$d" 2>/dev/null
    done
    local lo; lo=$(cat "$LOOPF" 2>/dev/null)
    [ -n "$lo" ] && losetup -d "$lo" 2>/dev/null
    rm -f "$LOOPF"
    echo "cleanup done"
}

# ----------------------------------------------------------------- batch ----
# Reads "<subcmd> <args...>" lines on stdin and runs them in order, bracketing
# each with the epoch just before and just after.  One ssh round trip drives a
# whole round of lanes, and the T= line is the exact fault-injection timestamp
# that the report subtracts from the probe's first-error epoch.
cmd_batch(){
    local line sub
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        # shellcheck disable=SC2086
        set -- $line
        sub="$1"; shift
        echo "T=$(now) CMD=$sub ARGS=$*"
        case "$sub" in
            lane-create) cmd_lane_create "$@" ;;
            lane-ana)    cmd_lane_ana "$@" ;;
            lane-fault)  cmd_lane_fault "$@" ;;
            lane-status) cmd_lane_status "$@" ;;
            *) echo "cnctl: bad batch subcmd $sub" >&2 ;;
        esac
        echo "D=$(now)"
    done
}

sub="${1:-}"; shift 2>/dev/null || true
case "$sub" in
    init)        cmd_init "$@" ;;
    batch)       cmd_batch ;;
    lane-create) cmd_lane_create "$@" ;;
    lane-ana)    cmd_lane_ana "$@" ;;
    lane-fault)  cmd_lane_fault "$@" ;;
    lane-status) cmd_lane_status "$@" ;;
    cleanup)     cmd_cleanup ;;
    *) die "usage: cnctl.sh {init|lane-create|lane-ana|lane-fault|lane-status|cleanup}" ;;
esac
