#!/bin/bash
# teardown.sh — dismantle the rig. Order matters: defuse every dm fault first,
# on the targets, *before* touching nvmet or disconnecting. A stalled backend
# leaves an nvmet request outstanding and then ns disable / controller teardown
# block forever.
set -u
CN=192.168.122.125; DN0=192.168.122.48; DN1=192.168.122.70
S() { ssh -o StrictHostKeyChecking=no yupeng@$1 "${@:2}"; }

echo "=== 1. defuse targets ==="
for h in $DN0 $DN1; do
    S $h 'bash /tmp/fault.sh net-clear; sudo dmsetup info dnv-leg | grep -q SUSPENDED && sudo dmsetup resume --noudevsync dnv-leg; bash /tmp/fault.sh heal' || true
done

echo "=== 2. stop cn0 services and the array ==="
S $CN 'sudo systemctl disable --now dnv-healer dnv-iogen 2>/dev/null;
       sudo rm -f /run/dnv-io-pause
       if [ -d /sys/block/md0/md ]; then
           echo inactive | sudo tee /sys/block/md0/md/array_state >/dev/null
           echo clear    | sudo tee /sys/block/md0/md/array_state >/dev/null
       fi
       cat /proc/mdstat'

echo "=== 3. disconnect nvme ==="
S $CN 'for n in dn0 dn1; do sudo nvme disconnect -n nqn.2026-08.org.dnv:$n; done; sudo nvme list'

echo "=== 4. tear down targets ==="
for h in $DN0 $DN1; do
  S $h 'set -e
    N=/sys/kernel/config/nvmet
    for s in $N/subsystems/nqn.2026-08.org.dnv:*; do
        nqn=$(basename $s)
        sudo rm -f $N/ports/1/subsystems/$nqn
        echo 0 | sudo tee $s/namespaces/1/enable >/dev/null
        sudo rmdir $s/namespaces/1 $s/allowed_hosts/* 2>/dev/null || true
        sudo rm -f $s/allowed_hosts/* 2>/dev/null || true
        sudo rmdir $s
    done
    sudo rmdir $N/ports/1 $N/hosts/* 2>/dev/null || true
    sudo dmsetup remove dnv-leg
    L=$(losetup -j /var/tmp/dnv/*.img | cut -d: -f1); [ -n "$L" ] && sudo losetup -d $L
    sudo rm -rf /var/tmp/dnv /etc/udev/rules.d/58-dnv-test.rules
    sudo udevadm control --reload-rules
    echo "$(hostname) clean"'
done
