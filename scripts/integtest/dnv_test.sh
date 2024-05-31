#!/bin/bash

set -ex

trap 'echo -n $(date)' DEBUG

CURR_DIR=$(readlink -f $(dirname $0))
source $CURR_DIR/conf.sh
source $CURR_DIR/utils.sh

ROOT_DIR=$CURR_DIR/../..
BIN_DIR=$ROOT_DIR/_out/linux_amd64
ETCD_BIN=$ROOT_DIR/bin/linux_amd64/etcd

sudo rm -rf $WORK_DIR
mkdir -p $WORK_DIR

echo "launch etcd"
$ETCD_BIN --listen-client-urls "http://localhost:$ETCD_PORT" \
          --advertise-client-urls "http://localhost:$ETCD_PORT" \
          --listen-peer-urls "http://localhost:$ETCD_PEER_PORT" \
          --name etcd0 --data-dir $WORK_DIR/etcd0.data \
          > $WORK_DIR/etcd0.log 2>&1 &

echo "launch dn agent 0"
$BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9020" --role dn \
                  > $WORK_DIR/dn_agent_0.log 2>&1 &

echo "launch dn agent 1"
$BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9021" --role dn \
                  > $WORK_DIR/dn_agent_1.log 2>&1 &

echo "launch cn agent"
$BIN_DIR/dnvagent --grpc-network tcp --grpc-address "127.0.0.1:9120" --role cn \
                  > $WORK_DIR/cn_agent.log 2>&1 &

echo "launch dn worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9220" \
                   --grpc-target "127.0.0.1:9220" \
                   --role dn \
                   > $WORK_DIR/dn_worker.log 2>&1 &

echo "launch cn worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9320" \
                   --grpc-target "127.0.0.1:9320" \
                   --role cn \
                   > $WORK_DIR/cn_worker.log 2>&1 &

echo "launch sp worker"
$BIN_DIR/dnvworker --etcd-endpoints "localhost:$ETCD_PORT" \
                   --grpc-network tcp --grpc-address "127.0.0.1:9420" \
                   --grpc-target "127.0.0.1:9420" \
                   --role sp \
                   > $WORK_DIR/sp_worker.log 2>&1 &

echo "launch api server"
$BIN_DIR/dnvapi --etcd-endpoints "localhost:$ETCD_PORT" \
                --grpc-network tcp --grpc-address "127.0.0.1:9520" \
                > $WORK_DIR/apiserver.log 2>&1 &
