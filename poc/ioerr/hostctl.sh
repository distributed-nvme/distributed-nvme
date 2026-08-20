#!/bin/bash
#
# hostctl.sh -- initiator side helper for the NVMe-oF IO-error test matrix.
#               Runs ON host0, always via sudo.
#
set -u
NRQ=2

die(){ echo "hostctl: $*" >&2; exit 1; }

# subsys sysfs dir for an NQN, or empty
sysdir(){    # <nqn>
    local d
    for d in /sys/class/nvme-subsystem/nvme-subsys*; do
        [ -e "$d/subsysnqn" ] || continue
        [ "$(cat "$d/subsysnqn")" = "$1" ] && { echo "$d"; return 0; }
    done
    return 1
}

# head block device for an NQN (the multipath device), e.g. /dev/nvme0n1
cmd_dev(){   # <nqn>
    local d n
    d=$(sysdir "$1") || return 1
    for n in "$d"/nvme*n*; do
        n=$(basename "$n")
        case "$n" in
            nvme*c*n*) continue ;;              # per-path device, not the head
            nvme[0-9]*n[0-9]*) [ -b "/dev/$n" ] && { echo "/dev/$n"; return 0; } ;;
        esac
    done
    return 1
}

cmd_connect(){   # <nqn> <ctrl_loss_tmo> <reconnect_delay> <kato> <ip:port> [ip:port ...]
    local nqn="$1" clt="$2" rd="$3" kato="$4"; shift 4
    local ep ip port
    for ep in "$@"; do
        ip="${ep%%:*}"; port="${ep##*:}"
        nvme connect -t tcp -a "$ip" -s "$port" -n "$nqn" \
             --nr-io-queues=$NRQ --ctrl-loss-tmo="$clt" \
             --reconnect-delay="$rd" --keep-alive-tmo="$kato" \
             >/dev/null 2>&1 || echo "hostctl: WARN connect $nqn $ep failed" >&2
    done
}

cmd_disconnect(){   # <nqn>
    nvme disconnect -n "$1" >/dev/null 2>&1 || true
}

# wait until the head device exists and is readable
cmd_wait_dev(){   # <nqn> <timeout_s>
    local nqn="$1" tmo="$2" i dev
    for ((i=0; i<tmo*4; i++)); do
        dev=$(cmd_dev "$nqn") && [ -b "$dev" ] && { echo "$dev"; return 0; }
        sleep 0.25
    done
    return 1
}

# one-line health summary: head device, path count, per-path ctrl state + ANA
cmd_state(){   # <nqn>
    local nqn="$1" d dev p n
    d=$(sysdir "$nqn") || { echo "nqn=$nqn subsys=absent"; return; }
    dev=$(cmd_dev "$nqn" || echo "-")
    local paths=""
    for p in "$d"/nvme[0-9]*; do
        n=$(basename "$p")
        case "$n" in nvme[0-9]*n[0-9]*) continue ;; esac
        [ -e "$p/state" ] || continue
        # the per-path block device is nvme<head>c<ctrl>n<nsid>, named after the
        # HEAD instance, not this controller's -- so glob on the c<ctrl> part.
        local ana="-" ns
        for ns in /sys/block/nvme*c${n#nvme}n*; do
            [ -e "$ns/ana_state" ] && { ana=$(cat "$ns/ana_state"); break; }
        done
        paths="$paths $n=$(cat "$p/state")/$ana@$(cat "$p/address" 2>/dev/null | tr -d '\n')"
    done
    echo "nqn=$nqn dev=$dev paths:$paths"
}

cmd_io_timeout(){   # <ms> -- apply to every nvme queue (head + per-path)
    local ms="$1" q
    for q in /sys/block/nvme*/queue/io_timeout; do
        [ -w "$q" ] && echo "$ms" > "$q" 2>/dev/null
    done
    echo "io_timeout=$ms"
}

cmd_cleanup(){
    local d nqn
    for d in /sys/class/nvme-subsystem/nvme-subsys*; do
        [ -e "$d/subsysnqn" ] || continue
        nqn=$(cat "$d/subsysnqn")
        case "$nqn" in *dnvio*) nvme disconnect -n "$nqn" >/dev/null 2>&1 ;; esac
    done
    pkill -f io_probe.py 2>/dev/null
    echo "host cleanup done"
}

# connect-batch: "<nqn> <clt> <rd> <kato> <ip:port> [ip:port]" lines on stdin,
# all fanned out at once -- 32 serial `nvme connect` calls per round would cost
# more wall clock than the observation window itself.
cmd_connect_batch(){
    local line
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        # shellcheck disable=SC2086
        ( cmd_connect $line ) &
    done
    wait
    echo "connect-batch done"
}

# disconnect-batch: one NQN per line, fanned out.
cmd_disconnect_batch(){
    local line
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        ( nvme disconnect -n "$line" >/dev/null 2>&1 ) &
    done
    wait
    echo "disconnect-batch done"
}

# state-batch / dev-batch: one NQN per line.
cmd_state_batch(){
    local line
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        cmd_state "$line"
    done
}
cmd_dev_batch(){
    local line d
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        d=$(cmd_dev "$line") || d="-"
        echo "$line $d"
    done
}

# probe-batch: "<nqn> <duration> <outfile>" lines; resolves the head device now
# and launches one io_probe.py per lane, detached.
cmd_probe_batch(){
    local line nqn dur out d
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        # shellcheck disable=SC2086
        set -- $line
        nqn="$1"; dur="$2"; out="$3"
        d=$(cmd_dev "$nqn") || d=""
        rm -f "$out"
        if [ -z "$d" ]; then
            echo "{\"dev\":null,\"nodev\":true,\"nqn\":\"$nqn\"}" > "$out"
            echo "probe $nqn NODEV"
            continue
        fi
        setsid nohup python3 /var/tmp/io_probe.py "$d" "$dur" "$out" \
            >/dev/null 2>&1 < /dev/null &
        echo "probe $nqn $d"
    done
    sleep 0.3
}

sub="${1:-}"; shift 2>/dev/null || true
case "$sub" in
    dev)              cmd_dev "$@" ;;
    connect-batch)    cmd_connect_batch ;;
    disconnect-batch) cmd_disconnect_batch ;;
    state-batch)      cmd_state_batch ;;
    dev-batch)        cmd_dev_batch ;;
    probe-batch)      cmd_probe_batch ;;
    connect)     cmd_connect "$@" ;;
    disconnect)  cmd_disconnect "$@" ;;
    wait-dev)    cmd_wait_dev "$@" ;;
    state)       cmd_state "$@" ;;
    io-timeout)  cmd_io_timeout "$@" ;;
    cleanup)     cmd_cleanup ;;
    *) die "usage: hostctl.sh {dev|connect|disconnect|wait-dev|state|io-timeout|cleanup}" ;;
esac
