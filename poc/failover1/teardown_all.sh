#!/bin/bash
# teardown_all.sh -- return the whole fleet to a blank slate.
#
# Order is load-bearing.  Each node's resources are held open by the node above
# it, so teardown has to unwind top-down:
#   host0 disconnect  -> releases the ss-path controllers on cn0/cn1
#   cn0/cn1 nvmet     -> releases dnv-lin-on-cnX
#   cn0/cn1 dm        -> releases the upstream nvme namespace
#   cn0/cn1 disconnect-> releases the ss-to-cnX controllers on dn0
#   dn0 nvmet         -> releases dnv-lin-cnX
#   dn0 dm
set -e
cd "$(dirname "$0")"; . ./nodes.sh
ssh yupeng@$HOST0 "cd $REMOTE && ./cleanup.sh disconnect"
for ip in $CN0 $CN1; do
  ssh yupeng@$ip "cd $REMOTE && ./cleanup.sh nvmet && ./cleanup.sh dm && ./cleanup.sh disconnect"
done
ssh yupeng@$DN0 "cd $REMOTE && ./cleanup.sh nvmet && ./cleanup.sh dm"
echo "=== residue check ==="
for ip in $ALL; do
  ssh yupeng@$ip 'printf "%-16s ports=[%s] subsys=[%s] dm=[%s] ctrls=[%s]\n" \
    "$(hostname -I | cut -d" " -f1)" \
    "$(ls /sys/kernel/config/nvmet/ports 2>/dev/null | tr "\n" " ")" \
    "$(ls /sys/kernel/config/nvmet/subsystems 2>/dev/null | tr "\n" " ")" \
    "$(sudo dmsetup ls 2>/dev/null | grep "^dnv-" | tr "\n" " ")" \
    "$(ls -d /sys/class/nvme/nvme* 2>/dev/null | tr "\n" " ")"'
done
