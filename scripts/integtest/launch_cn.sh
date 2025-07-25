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

ip_addr=$1

echo "launch cn agent"
sudo --background $CURR_DIR/dnvagent \
     --grpc-network tcp \
     --grpc-address "${ip_addr}:${CN_AGENT_PORT}" \
     --role cn \
     > $WORK_DIR/cn_agent.log 2>&1 &
