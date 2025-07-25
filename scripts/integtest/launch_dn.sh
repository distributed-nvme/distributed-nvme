#!/bin/bash

set -ex

trap 'echo $(date)' DEBUG

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/multi_nodes_conf.sh
source $CURR_DIR/utils.sh

if [ "$WORK_DIR" == "" ]; then
    echo "WORK_DIR is empty"
    exit 1
fi

dd if=/dev/zero of=$WORK_DIR/dn.img bs=1M count=4096
sudo losetup /dev/loop240 $WORK_DIR/dn.img

ip_addr=$1

echo "launch dn agent"
sudo --background $CURR_DIR/dnvagent \
     --grpc-network tcp \
     --grpc-address "${ip_addr}:${DN_AGENT_PORT}" \
     --role dn \
     > $WORK_DIR/dn_agent.log 2>&1 &
