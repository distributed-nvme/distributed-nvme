#!/bin/bash
# dn0_failover.sh -- move the real storage from the cn0 lane to the cn1 lane.
. "$(dirname "$0")/common.sh"
S=$DEV_SECTORS
T0=$(date +%s.%N)
stamp() { printf '%s dn0 %s\n' "$(date +%s.%N)" "$*"; }

# 1. quiesce the cn0 lane.  Flushing suspend (no --noflush): in-flight IO
#    completes against the good table, only new IO is held.
stamp "step1 suspend $D_LIN_CN0"
sudo dmsetup suspend --nolockfs --noudevsync "$D_LIN_CN0"
stamp "step1 done"

# 2. move the cn1 lane off its error device onto the real (delayed) storage
stamp "step2 reload $D_LIN_CN1 -> $D_DELAY"
sudo dmsetup suspend --nolockfs --noflush --noudevsync "$D_LIN_CN1"
printf '0 %s linear /dev/mapper/%s 0\n' "$S" "$D_DELAY" | sudo dmsetup load "$D_LIN_CN1"
sudo dmsetup resume --noudevsync "$D_LIN_CN1"
stamp "step2 done"

# 3. promote ns-to-cn1 into the optimized group (live ana_grpid rewrite --
#    no ns disable, no port relink; cn1 sees it in ~15ms via the ANA AEN)
stamp "step3 ns-to-cn1 ana_grpid -> 1 (optimized)"
echo 1 | sudo tee "$NVMET/subsystems/$NQN_TO_CN1/namespaces/1/ana_grpid" >/dev/null
stamp "step3 done"

# 4.
stamp "step4 sleep 5"
sleep 5

# 5. point the cn0 lane at its error device and release the suspend
stamp "step5 reload $D_LIN_CN0 -> $D_ERR_CN0"
printf '0 %s linear /dev/mapper/%s 0\n' "$S" "$D_ERR_CN0" | sudo dmsetup load "$D_LIN_CN0"
sudo dmsetup resume --noudevsync "$D_LIN_CN0"
stamp "step5 done"

# 6. demote ns-to-cn0
stamp "step6 ns-to-cn0 ana_grpid -> 2 (non-optimized)"
echo 2 | sudo tee "$NVMET/subsystems/$NQN_TO_CN0/namespaces/1/ana_grpid" >/dev/null
stamp "step6 done"

stamp "finished in $(echo "$(date +%s.%N) - $T0" | bc)s"
