#!/bin/bash
# cn_setup.sh -- initiator side: connect both targets and build the RAID1 by hand.
#
# mdadm is deliberately not installed on cn0, so nothing here can silently fall
# back to it. Metadata is written by mdsb.py; assembly and every tunable go
# through sysfs; putting a leg back goes through mdctl.py.
#
# The transport settings are not options. §5.4 of raid1_selfheal_test.md is the
# measurement behind them:
#   md failfast on        -> a wedged-but-connected backend becomes an IO error
#   fast_io_fail_tmo=5    -> second, independent route out of the same trap
#   ctrl_loss_tmo=-1      -> never delete the controller, so a leg can return
set -eu

MD=md0
DEV=/opt/dnv

say() { printf '[cn0] %s\n' "$*"; }

sudo modprobe nvme-tcp
sudo modprobe raid1
[ -s /etc/nvme/hostnqn ] || echo nqn.2026-08.org.dnv:host:cn0 | sudo tee /etc/nvme/hostnqn >/dev/null
[ -s /etc/nvme/hostid ]  || echo cccccccc-0000-0000-0000-0000000000c0 | sudo tee /etc/nvme/hostid >/dev/null
echo 10 | sudo tee /sys/module/nvme_core/parameters/io_timeout >/dev/null

declare -A ADDR=( [dn0]=192.168.122.48 [dn1]=192.168.122.70 )
declare -A NSUUID=( [dn0]=11111111-1111-1111-1111-000000000000
                    [dn1]=22222222-2222-2222-2222-000000000000 )

for n in dn0 dn1; do
    nqn=nqn.2026-08.org.dnv:$n
    if sudo nvme list-subsys 2>/dev/null | grep -q "$nqn"; then
        say "$n already connected"
    else
        sudo nvme connect -t tcp -a "${ADDR[$n]}" -s 4420 -n "$nqn" \
             --reconnect-delay=5 --ctrl-loss-tmo=-1 --fast_io_fail_tmo=5
        say "connected $nqn"
    fi
done

LEG=()
for n in dn0 dn1; do
    p=/dev/disk/by-id/nvme-uuid.${NSUUID[$n]}
    for _ in $(seq 50); do [ -e "$p" ] && break; sleep 0.2; done
    [ -e "$p" ] || { echo "missing $p" >&2; exit 1; }
    LEG+=("$p")
    say "$n -> $p -> /dev/$(basename "$(readlink -f "$p")")"
done

if [ -e /sys/block/$MD/md ]; then
    say "$MD already exists, leaving it alone"
else
    (cd $DEV && sudo python3 mdsb.py --create "${LEG[@]}")
    echo $MD    | sudo tee /sys/module/md_mod/parameters/new_array >/dev/null
    echo 1.2    | sudo tee /sys/block/$MD/md/metadata_version >/dev/null  # must precede new_dev
    for p in "${LEG[@]}"; do
        cat /sys/class/block/$(basename "$(readlink -f "$p")")/dev \
            | sudo tee /sys/block/$MD/md/new_dev >/dev/null
    done
    echo active | sudo tee /sys/block/$MD/md/array_state >/dev/null
fi

# fail_last_dev=1 lets md kick the *second* leg too rather than keeping a
# half-alive array, which is what makes the both-legs-down cases reachable.
# failfast comes from the superblock devflags at assembly, and is re-applied
# here (and by the healer) because new_dev does not carry it over.
sudo python3 -c "import sys; sys.path.insert(0,'$DEV'); import mdctl; print('tunables:', mdctl.tunables('$MD'))"

say "--- /proc/mdstat ---"; cat /proc/mdstat
grep -H . /sys/block/$MD/md/{array_state,degraded,raid_disks,level,fail_last_dev} \
          /sys/block/$MD/md/bitmap/{location,chunksize,metadata,space} 2>/dev/null \
    | sed "s#/sys/block/$MD/md/##"
for d in /sys/block/$MD/md/dev-*; do
    echo "$(basename $d): slot=$(cat $d/slot) state=$(cat $d/state)"
done
say READY
