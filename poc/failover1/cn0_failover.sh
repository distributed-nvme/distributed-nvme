#!/bin/bash
# cn0_failover.sh -- retire cn0 as the active path.
. "$(dirname "$0")/common.sh"
S=$DEV_SECTORS
D_ERR=$D_ERR_ON-cn0
D_LIN=$D_LIN_ON-cn0
T0=$(date +%s.%N)
stamp() { printf '%s cn0 %s\n' "$(date +%s.%N)" "$*"; }

# 1. tell host0 this path is gone
stamp "step1 ns-path-cn0 ana_grpid -> 2 (inaccessible)"
echo 2 | sudo tee "$NVMET/subsystems/$NQN_PATH/namespaces/1/ana_grpid" >/dev/null
stamp "step1 done"

# 2. wait for dn0 to demote our upstream namespace.  Local sysfs only -- this
#    is the ANA log the target pushed to us, we never query dn0.
CTRL=$(ctrl_for_nqn "$NQN_TO_CN0") || die "no upstream controller"
stamp "step2 waiting for $CTRL ana_state=non-optimized (now: $(local_ana_state "$CTRL"))"
deadline=$(echo "$(date +%s.%N) + 30" | bc)
while :; do
  st=$(local_ana_state "$CTRL" 2>/dev/null || echo "?")
  [ "$st" = "non-optimized" ] && break
  if [ "$(echo "$(date +%s.%N) > $deadline" | bc)" = 1 ]; then
    stamp "step2 TIMEOUT, last state=$st"; break
  fi
  sleep 0.005
done
stamp "step2 done, ana_state=$(local_ana_state "$CTRL" 2>/dev/null)"

# 3. cut the local lane over to the error device
stamp "step3 reload $D_LIN -> $D_ERR"
dm_reload "$D_LIN" "0 $S linear /dev/mapper/$D_ERR 0" 20
stamp "step3 done"

stamp "finished in $(echo "$(date +%s.%N) - $T0" | bc)s"
