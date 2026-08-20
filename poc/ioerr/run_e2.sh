#!/bin/bash
#
# run_e2.sh -- follow-up to test A case (e).
#
# Removing the host from allowed_hosts produced no I/O error at all over 900s,
# because nvmet only consults allowed_hosts in the Connect handler.  This shows
# the fault is latent rather than absent: force one controller reset and the
# same configuration that was serving I/O a second ago refuses to come back.
#
set -u
HOST0=192.168.122.193
CN0=192.168.122.125
HNQN='nqn.2014-08.org.nvmexpress:uuid:1a788e0a-02bd-4ae9-9a4b-e4aad8b04c26'
NQN=nqn.2026-08.dnvio:e2
S=yupeng

h(){ ssh $S@$HOST0 "$@"; }
c(){ ssh $S@$CN0 "$@"; }

c "sudo bash /var/tmp/cnctl.sh init '$HNQN'" >/dev/null
c "sudo bash /var/tmp/cnctl.sh lane-create 0 4440 $NQN cafe0003-0000-4000-8000-000000000000 $CN0 1 100 optimized DNVIOTEST dnvio"
h "sudo bash /var/tmp/hostctl.sh connect $NQN 10 2 5 $CN0:4440"
DEV=$(h "sudo bash /var/tmp/hostctl.sh wait-dev $NQN 15")
echo "dev=$DEV"
h "sudo dmesg -C"; c "sudo dmesg -C"

h "sudo setsid nohup python3 /var/tmp/io_probe.py $DEV 90 /var/tmp/probe-e2.json >/dev/null 2>&1 &"
sleep 5

echo "--- removing host from allowed_hosts at $(date +%s.%N)"
c "sudo bash /var/tmp/cnctl.sh lane-fault 0 $NQN e apply"
sleep 15
echo "--- state after 15s with the fault in place (I/O should still be flowing)"
h "sudo bash /var/tmp/hostctl.sh state $NQN"

CTRL=$(h "ls /sys/class/nvme | while read n; do [ \"\$(cat /sys/class/nvme/\$n/subsysnqn 2>/dev/null)\" = '$NQN' ] && echo \$n; done" | head -1)
RESET_T=$(date +%s.%N)
echo "--- forcing a reconnect: nvme reset /dev/$CTRL at $RESET_T"
h "sudo nvme reset /dev/$CTRL" || true
sleep 45

echo "--- state after the reset"
h "sudo bash /var/tmp/hostctl.sh state $NQN"
echo "--- host dmesg"
h "sudo dmesg -T | grep -iE 'nvme' | grep -viE 'I/O Cmd|Buffer I/O|I/O error, dev' | tail -20"
echo "--- target dmesg"
c "sudo dmesg -T | tail -10"
echo "--- probe (reset_epoch=$RESET_T)"
h "sudo cat /var/tmp/probe-e2.json"
echo "RESET_EPOCH=$RESET_T"

c "sudo bash /var/tmp/cnctl.sh lane-fault 0 $NQN e clear" >/dev/null
h "sudo bash /var/tmp/hostctl.sh disconnect $NQN"
c "sudo bash /var/tmp/cnctl.sh cleanup" >/dev/null
