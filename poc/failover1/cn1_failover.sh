#!/bin/bash
# cn1_failover.sh -- promote cn1 to the active path.
. "$(dirname "$0")/common.sh"
S=$DEV_SECTORS
D_LIN=$D_LIN_ON-cn1
T0=$(date +%s.%N)
stamp() { printf '%s cn1 %s\n' "$(date +%s.%N)" "$*"; }

CTRL=$(ctrl_for_nqn "$NQN_TO_CN1") || die "no upstream controller"
UPDEV=/dev/$(nvme_head_for "$NQN_TO_CN1") || die "no upstream blockdev"

# 1. wait for dn0 to promote our upstream namespace (local sysfs only)
stamp "step1 waiting for $CTRL ana_state=optimized (now: $(local_ana_state "$CTRL"))"
deadline=$(echo "$(date +%s.%N) + 30" | bc)
while :; do
  st=$(local_ana_state "$CTRL" 2>/dev/null || echo "?")
  [ "$st" = "optimized" ] && break
  if [ "$(echo "$(date +%s.%N) > $deadline" | bc)" = 1 ]; then
    stamp "step1 TIMEOUT, last state=$st"; break
  fi
  sleep 0.005
done
stamp "step1 done, ana_state=$(local_ana_state "$CTRL" 2>/dev/null)"

# 2. swing the local lane onto the now-healthy nvme namespace
stamp "step2 reload $D_LIN -> $UPDEV"
dm_reload "$D_LIN" "0 $S linear $UPDEV 0" 20
stamp "step2 done"

# 3. advertise the path to host0
stamp "step3 ns-path-cn1 ana_grpid -> 1 (optimized)"
echo 1 | sudo tee "$NVMET/subsystems/$NQN_PATH/namespaces/1/ana_grpid" >/dev/null
stamp "step3 done"

stamp "finished in $(echo "$(date +%s.%N) - $T0" | bc)s"
