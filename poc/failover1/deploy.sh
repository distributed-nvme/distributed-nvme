#!/bin/bash
# deploy.sh -- copy the node-side scripts to ~/failover1 on every node.
set -e
cd "$(dirname "$0")"; . ./nodes.sh
FILES="common.sh cleanup.sh dn0_setup.sh cn_setup.sh host0_setup.sh \
       dn0_failover.sh cn0_failover.sh cn1_failover.sh barrier.sh io_load.py"
for ip in $ALL; do
  ssh yupeng@$ip "mkdir -p $REMOTE"
  scp -q $FILES yupeng@$ip:$REMOTE/
  echo "deployed -> $ip"
done
