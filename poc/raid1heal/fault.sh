#!/bin/bash
# fault.sh — inject / clear one fault on a disk node. Run on dn0 or dn1.
#
#   net-drop    4.1  iptables DROP every packet from cn0
#   net-reset   4.2  iptables REJECT --reject-with tcp-reset
#   net-clear        remove both
#   suspend     4.3  dmsetup suspend the dm-linear leg
#   resume           resume it
#   delay MS    4.4  swap in a dm-delay table (MS >= nvme io_timeout)
#   error            swap in a dm-error table (hard media failure)
#   flakey MODE 4.5  drop_writes | error_writes | error_reads | random_read_corrupt
#   heal             restore the plain dm-linear table
#   status           show everything
set -u
CN=192.168.122.125
LEG=dnv-leg
CHAIN=DNVFAULT
SAVED=/var/tmp/dnv/leg.table

sudo mkdir -p /var/tmp/dnv
# The linear table is saved once, the first time we touch it, so `heal` always
# has the real backing device to restore even after several swaps.
save_table() {
    [ -s "$SAVED" ] && return 0
    sudo dmsetup table $LEG | grep -q ' linear ' && sudo dmsetup table $LEG | sudo tee $SAVED >/dev/null
}

sz()   { sudo blockdev --getsz /dev/mapper/$LEG; }
# Both VMs answer `hostname -s` with the same name, so the backing device is
# taken from the saved linear table ("0 <sz> linear <major:minor> 0") instead.
base() { save_table; awk '{print $4}' "$SAVED"; }

# Swap the leg's target. --nolockfs --noflush matters: a plain suspend tries to
# flush pending IO through a leg that is about to start erroring, and a --noflush
# suspend left on an unchanged table just re-delays requeued bios on resume.
swap() {
    save_table
    sudo dmsetup suspend --nolockfs --noflush --noudevsync $LEG
    if ! echo "$1" | sudo dmsetup load $LEG; then
        sudo dmsetup resume --noudevsync $LEG
        echo "LOAD FAILED for table: $1" >&2
        exit 1
    fi
    sudo dmsetup resume --noudevsync $LEG
    echo "leg table -> $(sudo dmsetup table $LEG)"
}

fw_chain() {
    sudo iptables -N $CHAIN 2>/dev/null
    sudo iptables -C INPUT -j $CHAIN 2>/dev/null || sudo iptables -I INPUT 1 -j $CHAIN
}

case "${1:-status}" in
net-drop)  fw_chain; sudo iptables -F $CHAIN; sudo iptables -A $CHAIN -s $CN -j DROP
           echo "iptables: DROP from $CN" ;;
net-reset) fw_chain; sudo iptables -F $CHAIN
           sudo iptables -A $CHAIN -s $CN -p tcp -j REJECT --reject-with tcp-reset
           sudo iptables -A $CHAIN -s $CN -j DROP
           echo "iptables: tcp-reset to $CN" ;;
net-clear) sudo iptables -F $CHAIN 2>/dev/null; echo "iptables: cleared" ;;

suspend)   sudo dmsetup suspend --nolockfs --noflush --noudevsync $LEG
           echo "leg suspended: $(sudo dmsetup info $LEG | grep State)" ;;
resume)    sudo dmsetup resume --noudevsync $LEG
           echo "leg resumed: $(sudo dmsetup info $LEG | grep State)" ;;

delay)     MS=${2:-20000}; B=$(base); save_table
           swap "0 $(sz) delay $B 0 $MS" ;;
error)     swap "0 $(sz) error" ;;
flakey)    MODE=${2:-error_writes}; B=$(base); save_table
           case $MODE in
             plain) FEAT="0" ;;                              # down interval errors everything
             # <n> counts the feature name *and* its arguments; getting that
             # wrong is what makes dm-flakey reject the table with EINVAL.
             corrupt_read)  FEAT="5 corrupt_bio_byte 32 r 1 0" ;;   # silent read corruption
             corrupt_write) FEAT="5 corrupt_bio_byte 32 w 1 0" ;;   # silent write corruption
             random_read_corrupt|random_write_corrupt) FEAT="2 $MODE 100" ;;
             *)     FEAT="1 $MODE" ;;
           esac
           # up=0 down=1 -> permanently in the "down" interval, so the feature
           # applies to every bio instead of flapping on a timer.
           swap "0 $(sz) flakey $B 0 0 1 $FEAT" ;;
heal)      if [ -s "$SAVED" ]; then swap "$(cat $SAVED)"
           else echo "no saved table; leg is $(sudo dmsetup table $LEG)"; fi
           sudo dmsetup info $LEG | grep State ;;

status)    echo "--- dm ---";       sudo dmsetup table $LEG; sudo dmsetup info $LEG | grep State
           echo "--- iptables ---"; sudo iptables -S $CHAIN 2>/dev/null || echo "(no $CHAIN)"
           echo "--- nvmet ---";    for f in /sys/kernel/config/nvmet/subsystems/*/namespaces/1/enable; do
                                        echo "$f = $(cat $f)"; done ;;
*) echo "unknown action: $1" >&2; exit 2 ;;
esac
